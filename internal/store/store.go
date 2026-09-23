// Package store 用 SQLite(gorm) 持久化「曾经打开过的日志文件」记录：
// 重启软件后侧栏可列出历史文件（区分同名不同路径）并一键重开。
//
// 驱动选用纯 Go 实现（glebarez/sqlite → modernc.org/sqlite），与本项目
// Wails 默认的 CGO_ENABLED=0 构建保持一致：交叉编译无需 C 工具链。
package store

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// maxRecords 记录条数上限：超出后按最近打开时间保留最新的 maxRecords 条。
// 仅为防止记录无限增长（取值远大于实际使用量，正常使用不会触发）。
const maxRecords = 500

// 来源类型。
const (
	KindLocal  = "local"
	KindRemote = "remote"
)

// Source 一个文件的来源：本地路径，或「远端连接 + POSIX 路径」。
//
// Key() 是 files.path_key 的取值，自带来源信息——因此不同主机上的
// /var/log/app.log 是两条独立记录，不会互相覆盖打开次数与行数。
type Source struct {
	Kind   string // KindLocal / KindRemote
	ConnID uint   // 远端连接 id（本地为 0）
	Remote string // 连接标识，如 root@10.0.0.5:22（本地为空）
	Path   string // 本地绝对路径 / 远端 POSIX 路径
}

// LocalSource 本地文件来源。
func LocalSource(path string) Source { return Source{Kind: KindLocal, Path: path} }

// RemoteSource 远端文件来源；remote 为展示用连接标识（user@host:port）。
func RemoteSource(connID uint, remote, path string) Source {
	return Source{Kind: KindRemote, ConnID: connID, Remote: remote, Path: path}
}

// Key 唯一键。本地的取值与既有实现逐字节一致（无需数据迁移）；
// 远端把「哪台主机」并入键，避免同名路径跨主机相互覆盖。
func (s Source) Key() string {
	if s.Kind == KindRemote {
		return "sftp://" + strings.ToLower(s.Remote) + s.Path
	}
	abs, err := Normalize(s.Path)
	if err != nil {
		return PathKey(s.Path)
	}
	return PathKey(abs)
}

// resolved 返回用于展示与存储的路径、文件名、所在目录。
// 远端路径必须按 POSIX 规则切分，不能用 Windows 的 filepath。
func (s Source) resolved() (display, name, dir string) {
	if s.Kind == KindRemote {
		p := path.Clean(s.Path)
		return p, path.Base(p), path.Dir(p)
	}
	abs, err := Normalize(s.Path)
	if err != nil {
		abs = s.Path
	}
	return abs, filepath.Base(abs), filepath.Dir(abs)
}

// DisplayPath 存储与展示用的路径（本地绝对路径、远端 POSIX 路径）。
func (s Source) DisplayPath() string {
	display, _, _ := s.resolved()
	return display
}

// Name 文件名（本地 filepath.Base，远端 path.Base）。
func (s Source) Name() string {
	_, name, _ := s.resolved()
	return name
}

// File 一条「曾打开的日志文件」记录。
// PathKey 是归一化路径加来源（Windows 下忽略大小写），作为唯一索引；
// Path 保留原始路径用于展示与重新打开。
type File struct {
	ID           uint   `gorm:"primaryKey"`
	PathKey      string `gorm:"column:path_key;uniqueIndex;size:512;not null"`
	Path         string `gorm:"size:512;not null"`
	Name         string `gorm:"size:255"` // 文件名（filepath.Base / path.Base）
	Dir          string `gorm:"size:512"` // 所在目录（同名不同路径靠它区分）
	Kind         string `gorm:"size:16;index"` // local / remote（空视为 local，兼容旧记录）
	ConnID       uint   `gorm:"index"`         // 远端连接 id
	Remote       string `gorm:"size:255"`      // 展示用连接标识 user@host:port
	Size         int64  // 打开时刻的文件字节数
	ModTime      int64  // 打开时刻的修改时间（unix 秒）
	TotalLines   int64  // 最近一次索引完成后的行数（未知为 0）
	OpenCount    int    // 累计打开次数
	LastOpenedAt time.Time `gorm:"index"`
}

// Connection 一台已保存的远端主机。
//
// 凭据策略：登录密码与私钥口令**以明文保存在本机数据库**（用户选择，便于免输入），
// 因此数据库文件等同于一份凭据。可用 SetConnectionSecret 清除；
// 数据库位置见 ConfigDir（Windows 上是 %AppData%\Roaming，域环境会被漫游同步）。
type Connection struct {
	ID         uint   `gorm:"primaryKey"`
	Name       string `gorm:"size:128"` // 展示名
	Host       string `gorm:"size:255;not null"`
	Port       int    `gorm:"not null"`
	User       string `gorm:"size:128;not null"`
	AuthMethod string `gorm:"size:16"`  // password / key
	KeyPath    string `gorm:"size:512"` // 私钥路径（认证方式为 key 时）
	// Password / Passphrase 明文保存的登录密码与私钥口令（认证方式为 password / key）。
	Password   string `gorm:"size:512"`
	Passphrase string `gorm:"size:512"`
	// Cache 该主机是否启用本地内容缓存：nil = 跟随全局设置。
	Cache *bool
	LastDir    string `gorm:"size:512"` // 上次浏览的目录（再次浏览时定位到这里）
	LastUsedAt time.Time `gorm:"index"`
}

// CacheEntry 一条本地缓存记录：远端文件内容的本地副本。
//
// 缓存文件本体放在缓存目录（见 filecache 包），这里只存判定指纹与索引信息：
// Size/ModTime 是**远端**文件的大小与修改时间，二者与缓存文件一一对应——
// 只有「大小 + 修改时间」都一致且缓存文件长度等于 Size 时才认为命中。
type CacheEntry struct {
	ID         uint   `gorm:"primaryKey"`
	SourceKey  string `gorm:"column:source_key;uniqueIndex;size:512;not null"`
	ConnID     uint   `gorm:"index"`
	Remote     string `gorm:"size:255;index"` // 展示用 user@host:port（删除主机时按它清理）
	Path       string `gorm:"size:512"`
	Name       string `gorm:"size:255"`
	Size       int64  // 远端文件字节数（完整缓存时等于缓存文件长度）
	ModTime    int64  // 远端修改时间（unix 秒）
	FileName   string `gorm:"size:128"` // 缓存目录下的文件名
	LastUsedAt time.Time `gorm:"index"`
}

// Setting 键值配置（缓存开关、上限等）。键值都很短，一张小表即可。
type Setting struct {
	Key   string `gorm:"primaryKey;size:64"`
	Value string `gorm:"size:512"`
}

// Store 记录库句柄。
type Store struct {
	db *gorm.DB

	mu        sync.Mutex
	lastTouch time.Time // 进程内最近一次记录的打开时间（见 nextStamp）
}

// nextStamp 返回严格递增的「打开时间」。
// Windows 下 time.Now() 存在约 0.5ms 量化：连续两次打开可能取到同一时刻，
// 会让「最近打开优先」的排序不稳定，故在进程内做单调修正（并列时顺延 1µs）。
func (s *Store) nextStamp() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !now.After(s.lastTouch) {
		now = s.lastTouch.Add(time.Microsecond)
	}
	s.lastTouch = now
	return now
}

// EnvDBPath 环境变量：显式指定数据库文件位置（单测与多实例隔离用）。
const EnvDBPath = "LOGVIEWER_DB_PATH"

// ConfigDir 本程序的配置目录（数据库、known_hosts 等都放这里）：
// 用户配置目录下 logviewer（Windows %AppData%、macOS ~/Library/Application Support、
// Linux ~/.config）；取不到配置目录时退回系统临时目录。目录会自动创建。
func ConfigDir() (string, error) {
	if p := os.Getenv(EnvDBPath); p != "" {
		if dir := filepath.Dir(p); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			return dir, nil
		}
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "logviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// DefaultPath 默认数据库路径（配置目录下的 logviewer.db）。
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvDBPath); p != "" {
		if _, err := ConfigDir(); err != nil {
			return "", err
		}
		return p, nil
	}
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logviewer.db"), nil
}

// Open 打开（必要时创建）数据库并建表。
func Open(path string) (*Store, error) {
	// _pragma 由驱动透传给 SQLite：WAL 提升读写并发，busy_timeout 避免多进程写锁立即报错。
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(3000)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // GUI 程序无控制台，避免噪音
	})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&File{}, &Connection{}, &CacheEntry{}, &Setting{}); err != nil {
		return nil, err
	}
	// SQLite 单写者：连接池收敛为 1，进程内不会自相竞争写锁。
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return &Store{db: db}, nil
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Normalize 把路径归一化为绝对路径（仅 Abs + Clean，不解析符号链接）。
func Normalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// PathKey 唯一键：Windows 文件系统大小写不敏感，统一小写，
// 避免大小写不同的同一文件产生两条记录。
func PathKey(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// Touch 记录一次打开：新来源插入，已存在则更新文件信息并累加打开次数。
func (s *Store) Touch(src Source, size, modTime int64) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	display, name, dir := src.resolved()
	now := s.nextStamp()
	rec := File{
		PathKey:      src.Key(),
		Path:         display,
		Name:         name,
		Dir:          dir,
		Kind:         src.Kind,
		ConnID:       src.ConnID,
		Remote:       src.Remote,
		Size:         size,
		ModTime:      modTime,
		OpenCount:    1,
		LastOpenedAt: now,
	}
	err := s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "path_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"path":           display,
			"name":           name,
			"dir":            dir,
			"kind":           rec.Kind,
			"conn_id":        rec.ConnID,
			"remote":         rec.Remote,
			"size":           size,
			"mod_time":       modTime,
			"last_opened_at": now,
			"open_count":     gorm.Expr("open_count + 1"),
		}),
	}).Create(&rec).Error
	if err != nil {
		return err
	}
	s.prune()
	return nil
}

// SetTotalLines 写入索引完成后的行数（记录不存在时忽略）。
func (s *Store) SetTotalLines(src Source, lines int64) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Model(&File{}).
		Where("path_key = ?", src.Key()).
		Update("total_lines", lines).Error
}

// ListConnections 按最近使用时间倒序返回已保存的远端主机。
func (s *Store) ListConnections() ([]Connection, error) {
	if s == nil {
		return nil, errors.New("store unavailable")
	}
	var out []Connection
	err := s.db.Order("last_used_at DESC, id DESC").Find(&out).Error
	return out, err
}

// GetConnection 按主键取一台远端主机。
func (s *Store) GetConnection(id uint) (Connection, error) {
	if s == nil {
		return Connection{}, errors.New("store unavailable")
	}
	var rec Connection
	if err := s.db.First(&rec, id).Error; err != nil {
		return Connection{}, err
	}
	return rec, nil
}

// SaveConnection 新增（ID 为 0）或更新一台远端主机。
func (s *Store) SaveConnection(c Connection) (Connection, error) {
	if s == nil {
		return Connection{}, errors.New("store unavailable")
	}
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = 22
	}
	if c.Name == "" {
		c.Name = c.User + "@" + c.Host
	}
	c.LastUsedAt = s.nextStamp()
	if c.ID == 0 {
		if err := s.db.Create(&c).Error; err != nil {
			return Connection{}, err
		}
		return c, nil
	}
	// 更新：显式指定列，避免把零值（如清空 KeyPath）当成「未设置」而跳过
	err := s.db.Model(&Connection{}).Where("id = ?", c.ID).Updates(map[string]any{
		"name":         c.Name,
		"host":         c.Host,
		"port":         c.Port,
		"user":         c.User,
		"auth_method":  c.AuthMethod,
		"key_path":     c.KeyPath,
		// 刻意不在这里写 password/passphrase：凭据只由 SetConnectionSecret 写。
		// SaveConnection 的入参来自前端 DTO，而 DTO 不携带口令（口令不回传前端），
		// 在这里写会用一个空串把已保存的口令抹掉。
		"cache": c.Cache,
		"last_used_at": c.LastUsedAt,
	}).Error
	if err != nil {
		return Connection{}, err
	}
	return s.GetConnection(c.ID)
}

// SetConnectionSecret 保存（或清除）该主机的明文凭据。
// 单独一个方法而不是走 SaveConnection：凭据只在连接成功时由连接流程写入，
// 避免编辑主机时误清空（编辑表单不回填口令，留空表示「保持不变」）。
func (s *Store) SetConnectionSecret(id uint, password, passphrase string) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Model(&Connection{}).Where("id = ?", id).Updates(map[string]any{
		"password":   password,
		"passphrase": passphrase,
	}).Error
}

// TouchConnection 记录一次使用（最近使用时间 + 上次浏览目录）。
func (s *Store) TouchConnection(id uint, lastDir string) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	updates := map[string]any{"last_used_at": s.nextStamp()}
	if lastDir != "" {
		updates["last_dir"] = lastDir
	}
	return s.db.Model(&Connection{}).Where("id = ?", id).Updates(updates).Error
}

// DeleteConnection 删除一台远端主机，并连带删除它的全部文件记录
// （否则「最近打开」里会留下指向已删除主机的死记录）。
func (s *Store) DeleteConnection(id uint) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conn_id = ?", id).Delete(&File{}).Error; err != nil {
			return err
		}
		return tx.Delete(&Connection{}, id).Error
	})
}

// List 按最近打开时间倒序返回记录。
func (s *Store) List() ([]File, error) {
	if s == nil {
		return nil, errors.New("store unavailable")
	}
	var out []File
	// 时间戳理论上唯一，仍以 id 兜底，保证排序稳定（不同进程写同一库时可能出现并列）。
	err := s.db.Order("last_opened_at DESC, id DESC").Limit(maxRecords).Find(&out).Error
	return out, err
}

// Get 按主键取一条记录。
func (s *Store) Get(id uint) (File, error) {
	if s == nil {
		return File{}, errors.New("store unavailable")
	}
	var rec File
	if err := s.db.First(&rec, id).Error; err != nil {
		return File{}, err
	}
	return rec, nil
}

// Delete 删除一条记录（用户主动移除历史项）。
func (s *Store) Delete(id uint) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Delete(&File{}, id).Error
}

// Clear 清空全部记录。
func (s *Store) Clear() error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Where("1 = 1").Delete(&File{}).Error
}

// ---------------- 本地缓存元数据 ----------------

// 缓存配置的键名。
const (
	SettingCacheEnabled = "cache_enabled"
	SettingCacheLimit   = "cache_limit_bytes"
)

// GetCacheEntry 按来源键取缓存记录；不存在返回 ok=false（不是错误）。
func (s *Store) GetCacheEntry(sourceKey string) (CacheEntry, bool, error) {
	if s == nil {
		return CacheEntry{}, false, errors.New("store unavailable")
	}
	var rec CacheEntry
	err := s.db.Where("source_key = ?", sourceKey).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CacheEntry{}, false, nil
	}
	if err != nil {
		return CacheEntry{}, false, err
	}
	return rec, true, nil
}

// SaveCacheEntry 写入/更新一条缓存记录（按来源键唯一），并刷新最近使用时间。
func (s *Store) SaveCacheEntry(e CacheEntry) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	e.LastUsedAt = s.nextStamp()
	return s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "source_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"conn_id":      e.ConnID,
			"remote":       e.Remote,
			"path":         e.Path,
			"name":         e.Name,
			"size":         e.Size,
			"mod_time":     e.ModTime,
			"file_name":    e.FileName,
			"last_used_at": e.LastUsedAt,
		}),
	}).Create(&e).Error
}

// TouchCacheEntry 刷新最近使用时间（命中缓存时调用，供 LRU 逐出排序）。
func (s *Store) TouchCacheEntry(sourceKey string) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Model(&CacheEntry{}).
		Where("source_key = ?", sourceKey).
		Update("last_used_at", s.nextStamp()).Error
}

// ListCacheEntries 按最近使用时间倒序返回缓存记录（LRU 逐出按此逆序淘汰）。
func (s *Store) ListCacheEntries() ([]CacheEntry, error) {
	if s == nil {
		return nil, errors.New("store unavailable")
	}
	var out []CacheEntry
	err := s.db.Order("last_used_at DESC, id DESC").Find(&out).Error
	return out, err
}

// DeleteCacheEntry 删除一条记录并返回它（调用方据此删除缓存文件）；不存在返回 ok=false。
func (s *Store) DeleteCacheEntry(sourceKey string) (CacheEntry, bool, error) {
	rec, ok, err := s.GetCacheEntry(sourceKey)
	if err != nil || !ok {
		return CacheEntry{}, false, err
	}
	if err := s.db.Delete(&CacheEntry{}, rec.ID).Error; err != nil {
		return CacheEntry{}, false, err
	}
	return rec, true, nil
}

// ClearCacheEntries 清空全部缓存记录并返回被清掉的（调用方据此删除缓存文件）。
func (s *Store) ClearCacheEntries() ([]CacheEntry, error) {
	if s == nil {
		return nil, errors.New("store unavailable")
	}
	all, err := s.ListCacheEntries()
	if err != nil {
		return nil, err
	}
	if err := s.db.Where("1 = 1").Delete(&CacheEntry{}).Error; err != nil {
		return nil, err
	}
	return all, nil
}

// DeleteCacheEntriesByRemote 删除某台主机（user@host:port）名下的全部缓存记录。
//
// 按 Remote 而不是 ConnID 匹配：两条连接记录可以指向同一个 user@host:port，
// 按 id 删会漏掉另一条记录写下的缓存。
func (s *Store) DeleteCacheEntriesByRemote(remote string) ([]CacheEntry, error) {
	if s == nil {
		return nil, errors.New("store unavailable")
	}
	if remote == "" {
		return nil, nil
	}
	var all []CacheEntry
	if err := s.db.Where("remote = ?", remote).Find(&all).Error; err != nil {
		return nil, err
	}
	if err := s.db.Where("remote = ?", remote).Delete(&CacheEntry{}).Error; err != nil {
		return nil, err
	}
	return all, nil
}

// GetSetting 读配置项；不存在返回 ok=false。
func (s *Store) GetSetting(key string) (string, bool, error) {
	if s == nil {
		return "", false, errors.New("store unavailable")
	}
	var rec Setting
	err := s.db.Where("key = ?", key).First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return rec.Value, true, nil
}

// SetSetting 写配置项（不存在则新建）。
func (s *Store) SetSetting(key, value string) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"value": value}),
	}).Create(&Setting{Key: key, Value: value}).Error
}

// prune 只保留最近 maxRecords 条（尽力而为，失败不影响主流程）。
func (s *Store) prune() {
	s.db.Exec(
		"DELETE FROM files WHERE id NOT IN (SELECT id FROM files ORDER BY last_opened_at DESC LIMIT ?)",
		maxRecords,
	)
}
