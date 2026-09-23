// Package filecache 为远端文件提供本地内容缓存：把读过的远端字节写到本地，
// 下次打开同一份内容时不再走网络。
//
// 设计前提（已确认）：远端指纹（大小 + 修改时间）变化时**整份重下**，不做增量追加。
// 因此缓存文件永远只有两种状态——「某个指纹下字节级完整的副本」与「不存在」：
//   - 写入先落 <hash>.part，读满打开时刻的快照大小后才截断并 rename 成 <hash>.bin；
//   - .part 一律视为不可用（中断、进程崩溃、下次打开都会丢弃），启动时统一清理。
// 这样命中判定只有「命中/丢弃」两态，不存在「前缀可信吗」的中间状态。
package filecache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"logviewer/internal/logfile"
	"logviewer/internal/store"
)

// DefaultLimit 默认缓存总量上限。
const DefaultLimit = 2 << 30 // 2GB

// EnvCacheDir 环境变量：显式指定缓存目录（单测隔离用，与 store.EnvDBPath 同一套路）。
const EnvCacheDir = "LOGVIEWER_CACHE_DIR"

// DefaultDir 默认缓存目录。
//
// 用 UserCacheDir（Windows 上即 %LocalAppData%）而不是 UserConfigDir（%AppData%\Roaming）：
// 漫游配置会被域环境同步到服务器，几 GB 的日志缓存不能放进去。
func DefaultDir() (string, error) {
	if p := os.Getenv(EnvCacheDir); p != "" {
		return p, nil
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return "", fmt.Errorf("无法确定缓存目录：%w", err)
	}
	return filepath.Join(base, "logviewer", "cache"), nil
}

// Request 打开一个缓存来源所需的信息。
type Request struct {
	SourceKey string // 与 store.Source.Key() 一致（远端形如 sftp://user@host:port/path）
	ConnID    uint
	Remote    string // 展示用 user@host:port
	Path      string // 远端 POSIX 路径
	Name      string // 文件名
	// Bypass 强制绕过缓存（用户点「重新加载」时使用：那种场景恰恰是
	// 大小与修改时间都没变但内容确实变了，用缓存会把变化藏起来）。
	Bypass bool
	// Info 远端文件信息，由调用方取一次后传入（判定指纹用；命中时整场只有这一次网络往返）。
	// 之所以不用回调：拒绝目录等策略属于调用方，且这样不会出现「取两次、两次之间远端变了」。
	Info os.FileInfo
	// OpenRemote 打开远端字节来源。惰性调用：命中缓存时完全不会用到。
	OpenRemote func() (logfile.Source, error)
}

// Manager 缓存管理器：目录、上限、使用中引用、逐出。
type Manager struct {
	dir   string
	store *store.Store // 元数据；为 nil 时缓存功能整体不可用

	mu      sync.Mutex
	inUse   map[string]int  // sourceKey → 使用中的会话数（逐出时跳过）
	writers map[string]bool // sourceKey → 是否已有写入者（同一来源只允许一个写缓存）
	limit   int64
	// epoch 代际号：清空缓存/删除主机时递增。在途写入者提交前校验自己的代际，
	// 不一致就丢弃——否则「清空」之后，正在下载的那个文件一提交就把条目"复活"了。
	epoch int64
}

// NewManager 创建管理器并确保目录存在。
func NewManager(dir string, st *store.Store) (*Manager, error) {
	if dir == "" {
		return nil, errors.New("缓存目录为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败：%w", err)
	}
	return &Manager{
		dir: dir, store: st,
		inUse:   make(map[string]int),
		writers: make(map[string]bool),
		limit:   DefaultLimit,
	}, nil
}

// Dir 缓存目录。
func (m *Manager) Dir() string { return m.dir }

// Available 报告缓存是否可用（数据库不可用时整体降级为不缓存）。
func (m *Manager) Available() bool { return m != nil && m.store != nil }

// SetLimit 设置总量上限（<=0 表示不限制）。
func (m *Manager) SetLimit(n int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.limit = n
	m.mu.Unlock()
}

func (m *Manager) limitBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit
}

func (m *Manager) acquire(key string) {
	m.mu.Lock()
	m.inUse[key]++
	m.mu.Unlock()
}

func (m *Manager) release(key string) {
	m.mu.Lock()
	if m.inUse[key] > 0 {
		m.inUse[key]--
	}
	if m.inUse[key] == 0 {
		delete(m.inUse, key)
	}
	m.mu.Unlock()
}

func (m *Manager) busy(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inUse[key] > 0
}

// claimWriter 认领写入权：同一来源同一时刻只允许一个写入者。
// 拿不到就退化为不缓存的直连读（不等待、不共享写句柄）。
func (m *Manager) claimWriter(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writers[key] {
		return false
	}
	m.writers[key] = true
	return true
}

func (m *Manager) releaseWriter(key string) {
	m.mu.Lock()
	delete(m.writers, key)
	m.mu.Unlock()
}

// currentEpoch 当前代际号（写入者打开时记下，提交前比对）。
func (m *Manager) currentEpoch() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch
}

// bumpEpoch 使在途写入全部作废（清空缓存/删除主机时调用）。
func (m *Manager) bumpEpoch() {
	m.mu.Lock()
	m.epoch++
	m.mu.Unlock()
}

// binName 缓存文件名：来源键的哈希（避免路径里的斜杠/冒号等不宜作文件名的字符）。
func binName(sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(sum[:16])
}

func (m *Manager) binPath(sourceKey string) string {
	return filepath.Join(m.dir, binName(sourceKey)+".bin")
}

// newPartPath 生成**每个写入者独占**的临时文件名。
// 固定名会让两个写入者（同进程的重入、或多进程）写同一个 inode：
// 先完成的一方 rename 走，后一方还在往同一个 inode 写——已发布的 .bin 会继续被改。
func (m *Manager) newPartPath(sourceKey string) string {
	return filepath.Join(m.dir, fmt.Sprintf("%s.%d.%s.part", binName(sourceKey), os.Getpid(), randHex(6)))
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Open 打开一个来源：命中缓存则全程本地读，否则边读边写缓存。
//
// 调用方负责在不再使用时 Close；本方法内部已登记引用计数，逐出时不会动使用中的条目。
func (m *Manager) Open(req Request) (logfile.Source, error) {
	if !m.Available() {
		return nil, errors.New("缓存不可用")
	}
	fi := req.Info
	if fi == nil {
		return nil, errors.New("缺少远端文件信息")
	}

	if !req.Bypass {
		if ent, ok, err := m.store.GetCacheEntry(req.SourceKey); err == nil && ok &&
			ent.Size == fi.Size() && ent.ModTime == fi.ModTime().Unix() {
			if f, err := m.openHit(req, fi, ent); err == nil {
				return f, nil
			} else if !errors.Is(err, errCacheMiss) {
				log.Printf("缓存不可用（%s）：%v", req.Path, err)
			}
			m.dropEntry(req.SourceKey) // 文件缺失/长度不符：当作未命中
		}
	}
	return m.beginWrite(req, fi)
}

// cacheable 判断该文件是否值得缓存。
//
// 修改时间为 0（或负）的文件不缓存：SFTP v3 的 mtime 是秒级 uint32，退化成 0 时
// 指纹只剩大小可比，轮转成同长度的文件会被误判命中。
func cacheable(fi os.FileInfo) bool { return fi.ModTime().Unix() > 0 }

// errCacheMiss 缓存文件缺失或长度不符（按未命中处理）。
var errCacheMiss = errors.New("缓存未命中")

// openHit 命中路径：只读打开 .bin，整场读都不碰网络。
func (m *Manager) openHit(req Request, fi os.FileInfo, ent store.CacheEntry) (*File, error) {
	f, err := os.Open(filepath.Join(m.dir, ent.FileName))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCacheMiss, err)
	}
	got, err := f.Stat()
	if err != nil || got.Size() != ent.Size {
		f.Close()
		return nil, fmt.Errorf("%w: 长度 %d ≠ 记录 %d", errCacheMiss, got.Size(), ent.Size)
	}
	m.acquire(req.SourceKey)
	if err := m.store.TouchCacheEntry(req.SourceKey); err != nil {
		log.Printf("刷新缓存最近使用时间失败：%v", err)
	}
	return newFile(m, req, fi, f, "", true, ent.Size), nil
}

// beginWrite 未命中路径：开一个 .part 边读边写，读满快照大小后提交。
// 以下情况退化为「不缓存的直连读」——读取本身永远不受影响：
// 	- 比上限还大的文件（缓存了也会立刻被逐出，白写一遍磁盘）
// 	- 修改时间不可用（见 cacheable）
// 	- 同一来源已有写入者（不等待、不共享写句柄）
// 	- 缓存文件建不出来（目录不可写、磁盘满）
func (m *Manager) beginWrite(req Request, fi os.FileInfo) (*File, error) {
	// 写入权先认领：拿不到就让位给已经在写的那一个
	if !m.claimWriter(req.SourceKey) {
		return newFile(m, req, fi, nil, "", false, 0), nil
	}
	release := func() { m.releaseWriter(req.SourceKey) }

	if lim := m.limitBytes(); lim > 0 && fi.Size() > lim {
		release()
		return newFile(m, req, fi, nil, "", false, 0), nil
	}
	if !cacheable(fi) {
		release()
		return newFile(m, req, fi, nil, "", false, 0), nil
	}
	partPath := m.newPartPath(req.SourceKey)
	part, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		log.Printf("创建缓存文件失败（%s）：%v", req.Path, err)
		release()
		return newFile(m, req, fi, nil, "", false, 0), nil
	}
	m.acquire(req.SourceKey)
	f := newFile(m, req, fi, part, partPath, false, 0)
	f.writerHeld = true
	return f, nil
}

// dropEntry 删除一条缓存记录及其文件。
func (m *Manager) dropEntry(sourceKey string) {
	ent, ok, err := m.store.DeleteCacheEntry(sourceKey)
	if err != nil {
		log.Printf("删除缓存记录失败：%v", err)
		return
	}
	if ok && ent.FileName != "" && !m.busy(sourceKey) {
		_ = os.Remove(filepath.Join(m.dir, ent.FileName))
	}
}

// Remove 删除指定来源的缓存（正在使用中的会被跳过）。
func (m *Manager) Remove(sourceKey string) {
	if !m.Available() || m.busy(sourceKey) {
		return
	}
	m.bumpEpoch()
	m.dropEntry(sourceKey)
}

// RemoveByRemote 删除某台主机名下的全部缓存（删除主机时调用）。
// 按 Remote 而非 ConnID 匹配：两条连接记录可以指向同一个 user@host:port。
func (m *Manager) RemoveByRemote(remote string) {
	if !m.Available() || remote == "" {
		return
	}
	m.bumpEpoch() // 作废在途写入，避免提交后条目"复活"
	ents, err := m.store.DeleteCacheEntriesByRemote(remote)
	if err != nil {
		log.Printf("清理主机缓存记录失败：%v", err)
		return
	}
	for _, e := range ents {
		if e.FileName != "" && !m.busy(e.SourceKey) {
			_ = os.Remove(filepath.Join(m.dir, e.FileName))
		}
	}
}

// Clear 清空全部缓存（清不掉的文件留在目录里，下次启动清理）。
func (m *Manager) Clear() error {
	if !m.Available() {
		return errors.New("缓存不可用")
	}
	m.bumpEpoch() // 同上：正在下载的那个文件不能在清空之后又把条目写回来
	ents, err := m.store.ClearCacheEntries()
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.FileName != "" && !m.busy(e.SourceKey) {
			_ = os.Remove(filepath.Join(m.dir, e.FileName))
		}
	}
	return nil
}

// CleanupStale 启动时清理：残留的 .part（中断/崩溃留下，一律不可用）与
// 没有对应记录的孤儿 .bin（记录被删但文件没删掉）。
func (m *Manager) CleanupStale() {
	if m == nil || m.dir == "" {
		return
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	known := map[string]bool{}
	if m.Available() {
		if recs, err := m.store.ListCacheEntries(); err == nil {
			for _, r := range recs {
				known[r.FileName] = true
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".part"):
			_ = os.Remove(filepath.Join(m.dir, name))
		case strings.HasSuffix(name, ".bin") && !known[name]:
			_ = os.Remove(filepath.Join(m.dir, name))
		}
	}
}

// EntryInfo 缓存条目（管理界面用）。
type EntryInfo struct {
	SourceKey    string
	Remote       string
	Path         string
	Name         string
	Size         int64
	ModTime      int64
	LastUsedAt   int64 // unix 毫秒
	InUse        bool  // 当前有会话正在使用（清除时会被跳过）
}

// Stats 缓存统计。
type Stats struct {
	Dir        string
	Limit      int64
	TotalBytes int64
	Entries    []EntryInfo
}

// Stats 返回缓存统计与条目列表（最近使用在前）。
func (m *Manager) Stats() (Stats, error) {
	if !m.Available() {
		return Stats{}, errors.New("缓存不可用")
	}
	recs, err := m.store.ListCacheEntries()
	if err != nil {
		return Stats{}, err
	}
	out := Stats{Dir: m.dir, Limit: m.limitBytes(), Entries: make([]EntryInfo, 0, len(recs))}
	for _, r := range recs {
		out.TotalBytes += r.Size
		out.Entries = append(out.Entries, EntryInfo{
			SourceKey:  r.SourceKey,
			Remote:     r.Remote,
			Path:       r.Path,
			Name:       r.Name,
			Size:       r.Size,
			ModTime:    r.ModTime,
			LastUsedAt: r.LastUsedAt.UnixMilli(),
			InUse:      m.busy(r.SourceKey),
		})
	}
	return out, nil
}

// EnforceLimit 立即按上限逐出（设置变更、启动时调用）。
func (m *Manager) EnforceLimit() {
	if m == nil || !m.Available() {
		return
	}
	m.evictIfNeeded()
}

// evictIfNeeded 超过上限时按 LRU 逐出（跳过使用中的条目）。
func (m *Manager) evictIfNeeded() {
	limit := m.limitBytes()
	if limit <= 0 || !m.Available() {
		return
	}
	recs, err := m.store.ListCacheEntries() // 最近使用在前
	if err != nil {
		return
	}
	var total int64
	for _, r := range recs {
		total += r.Size
	}
	if total <= limit {
		return
	}
	// 从最旧的开始删
	for i := len(recs) - 1; i >= 0 && total > limit; i-- {
		r := recs[i]
		if m.busy(r.SourceKey) {
			continue
		}
		if _, ok, err := m.store.DeleteCacheEntry(r.SourceKey); err == nil && ok {
			if r.FileName != "" {
				_ = os.Remove(filepath.Join(m.dir, r.FileName))
			}
			total -= r.Size
		}
	}
}
