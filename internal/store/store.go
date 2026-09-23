// Package store 用 SQLite(gorm) 持久化「曾经打开过的日志文件」记录：
// 重启软件后侧栏可列出历史文件（区分同名不同路径）并一键重开。
//
// 驱动选用纯 Go 实现（glebarez/sqlite → modernc.org/sqlite），与本项目
// Wails 默认的 CGO_ENABLED=0 构建保持一致：交叉编译无需 C 工具链。
package store

import (
	"errors"
	"os"
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

// File 一条「曾打开的日志文件」记录。
// PathKey 是归一化路径（Windows 下忽略大小写），作为唯一索引；
// Path 保留原始绝对路径用于展示与重新打开。
type File struct {
	ID           uint   `gorm:"primaryKey"`
	PathKey      string `gorm:"column:path_key;uniqueIndex;size:512;not null"`
	Path         string `gorm:"size:512;not null"`
	Name         string `gorm:"size:255"` // 文件名（filepath.Base）
	Dir          string `gorm:"size:512"` // 所在目录（同名不同路径靠它区分）
	Size         int64  // 打开时刻的文件字节数
	ModTime      int64  // 打开时刻的修改时间（unix 秒）
	TotalLines   int64  // 最近一次索引完成后的行数（未知为 0）
	OpenCount    int    // 累计打开次数
	LastOpenedAt time.Time `gorm:"index"`
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

// DefaultPath 默认数据库路径：用户配置目录下 logviewer/logviewer.db
// （Windows %AppData%、macOS ~/Library/Application Support、Linux ~/.config）；
// 取不到配置目录时退回系统临时目录。
func DefaultPath() (string, error) {
	if p := os.Getenv(EnvDBPath); p != "" {
		if dir := filepath.Dir(p); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
		}
		return p, nil
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "logviewer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := db.AutoMigrate(&File{}); err != nil {
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

// Touch 记录一次打开：新路径插入，已存在则更新文件信息并累加打开次数。
func (s *Store) Touch(path string, size, modTime int64) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	abs, err := Normalize(path)
	if err != nil {
		return err
	}
	now := s.nextStamp()
	rec := File{
		PathKey:      PathKey(abs),
		Path:         abs,
		Name:         filepath.Base(abs),
		Dir:          filepath.Dir(abs),
		Size:         size,
		ModTime:      modTime,
		OpenCount:    1,
		LastOpenedAt: now,
	}
	err = s.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "path_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"path":           abs,
			"name":           rec.Name,
			"dir":            rec.Dir,
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
func (s *Store) SetTotalLines(path string, lines int64) error {
	if s == nil {
		return errors.New("store unavailable")
	}
	abs, err := Normalize(path)
	if err != nil {
		return err
	}
	return s.db.Model(&File{}).
		Where("path_key = ?", PathKey(abs)).
		Update("total_lines", lines).Error
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

// prune 只保留最近 maxRecords 条（尽力而为，失败不影响主流程）。
func (s *Store) prune() {
	s.db.Exec(
		"DELETE FROM files WHERE id NOT IN (SELECT id FROM files ORDER BY last_opened_at DESC LIMIT ?)",
		maxRecords,
	)
}
