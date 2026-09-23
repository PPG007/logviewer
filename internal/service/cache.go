package service

import (
	"errors"
	"fmt"
	"log"
	"path"
	"strconv"

	"logviewer/internal/filecache"
	"logviewer/internal/logfile"
	"logviewer/internal/sshconn"
	"logviewer/internal/store"
)

// ---------------- 缓存 DTO ----------------

// CacheEntryInfo 一条本地缓存条目（缓存管理界面用）。
type CacheEntryInfo struct {
	SourceKey  string
	Remote     string // user@host:port
	Path       string
	Name       string
	Size       int64
	LastUsedAt int64 // unix 毫秒
	InUse      bool  // 当前有会话在使用（清除时会被跳过）
}

// CacheInfo 缓存总览。
type CacheInfo struct {
	Enabled    bool // 全局开关（主机可以再单独覆盖）
	Available  bool // 数据库可用、缓存功能才可用
	Dir        string
	Limit      int64 // 总量上限（字节；<=0 表示不限制）
	TotalBytes int64
	Entries    []CacheEntryInfo
}

// ---------------- 全局配置 ----------------

// loadCacheSettings 从数据库读取缓存开关与上限（缺省：开启、2GB）。
func (s *LogService) loadCacheSettings() {
	if s.store != nil {
		if v, ok, err := s.store.GetSetting(store.SettingCacheEnabled); err == nil && ok {
			s.cacheEnabled = v != "0"
		}
		if v, ok, err := s.store.GetSetting(store.SettingCacheLimit); err == nil && ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				s.cache.SetLimit(n)
			}
		}
	}
}

// cacheEnabledFor 该主机是否使用缓存：主机自身设置优先，未设置则跟随全局。
func (s *LogService) cacheEnabledFor(rec store.Connection) bool {
	if !s.cacheAvailable() || !s.cacheOn() {
		return false
	}
	if rec.Cache != nil {
		return *rec.Cache
	}
	return true
}

func (s *LogService) cacheAvailable() bool { return s.cache != nil && s.cache.Available() }

func (s *LogService) cacheOn() bool {
	if !s.cacheAvailable() {
		return false
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cacheEnabled
}

// SetCacheEnabled 全局开关缓存。只影响后续写入与新会话，不打断进行中的读取与索引。
func (s *LogService) SetCacheEnabled(enabled bool) error {
	if s.store == nil {
		return errNoHistory()
	}
	v := "0"
	if enabled {
		v = "1"
	}
	if err := s.store.SetSetting(store.SettingCacheEnabled, v); err != nil {
		return err
	}
	s.cfgMu.Lock()
	s.cacheEnabled = enabled
	s.cfgMu.Unlock()
	return nil
}

// SetCacheLimit 设置缓存总量上限（字节；<=0 表示不限制），并按新上限立即逐出。
func (s *LogService) SetCacheLimit(limit int64) error {
	if s.store == nil {
		return errNoHistory()
	}
	if limit < 0 {
		limit = 0
	}
	if err := s.store.SetSetting(store.SettingCacheLimit, strconv.FormatInt(limit, 10)); err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.SetLimit(limit)
		s.cache.EnforceLimit()
	}
	return nil
}

// GetCacheInfo 缓存总览（管理界面用）。
func (s *LogService) GetCacheInfo() (CacheInfo, error) {
	info := CacheInfo{Enabled: s.cacheOn(), Available: s.cacheAvailable()}
	if !s.cacheAvailable() {
		return info, nil
	}
	st, err := s.cache.Stats()
	if err != nil {
		return info, err
	}
	info.Dir, info.Limit, info.TotalBytes = st.Dir, st.Limit, st.TotalBytes
	info.Entries = make([]CacheEntryInfo, 0, len(st.Entries))
	for _, e := range st.Entries {
		info.Entries = append(info.Entries, CacheEntryInfo{
			SourceKey: e.SourceKey, Remote: e.Remote, Path: e.Path, Name: e.Name,
			Size: e.Size, LastUsedAt: e.LastUsedAt, InUse: e.InUse,
		})
	}
	return info, nil
}

// ClearCache 清空全部缓存。
func (s *LogService) ClearCache() error {
	if !s.cacheAvailable() {
		return errors.New("缓存不可用")
	}
	return s.cache.Clear()
}

// RemoveCacheEntry 删除单条缓存。
func (s *LogService) RemoveCacheEntry(sourceKey string) error {
	if !s.cacheAvailable() {
		return errors.New("缓存不可用")
	}
	if sourceKey == "" {
		return errors.New("缺少缓存标识")
	}
	s.cache.Remove(sourceKey)
	return nil
}

// openRemoteSource 打开远端来源：缓存可用且该主机启用时走缓存，否则直连。
//
// bypass 供「重新加载」使用：那种场景恰恰是大小与修改时间都没变但内容变了，
// 用缓存会把变化藏起来；同时它也会用新内容重写缓存，避免下次打开又弹回旧的。
func (s *LogService) openRemoteSource(rec store.Connection, prof sshconn.Profile, remotePath string, bypass bool) (logfile.Source, error) {
	fi, err := s.remote.Stat(prof, remotePath)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		// 目录不是可打开的日志文件：在进入缓存层之前就拒绝，文案与直连路径保持一致
		return nil, fmt.Errorf("%s 是目录，请选择日志文件", remotePath)
	}
	if !s.cacheEnabledFor(rec) {
		return s.remote.OpenFile(prof, remotePath)
	}
	src, err := s.cache.Open(filecache.Request{
		SourceKey: store.RemoteSource(rec.ID, prof.Display(), remotePath).Key(),
		ConnID:    rec.ID,
		Remote:    prof.Display(),
		Path:      remotePath,
		Name:      path.Base(remotePath),
		Bypass:    bypass,
		Info:      fi,
		OpenRemote: func() (logfile.Source, error) {
			return s.remote.OpenFile(prof, remotePath)
		},
	})
	if err != nil {
		// 缓存层出问题不该挡住建索引：退回直连
		log.Printf("缓存打开失败，改用直连（%s）：%v", remotePath, err)
		return s.remote.OpenFile(prof, remotePath)
	}
	return src, nil
}

// dropCacheForRemote 清理某台主机的全部缓存（删除主机时调用）。
func (s *LogService) dropCacheForRemote(remote string) {
	if !s.cacheAvailable() || remote == "" {
		return
	}
	s.cache.RemoveByRemote(remote)
}

// initCache 创建缓存管理器并读取配置；失败时远端功能仍可用，只是不缓存。
func (s *LogService) initCache() {
	dir, err := filecache.DefaultDir()
	if err != nil {
		log.Printf("本地缓存不可用（无法确定缓存目录）：%v", err)
		return
	}
	mgr, err := filecache.NewManager(dir, s.store)
	if err != nil {
		log.Printf("本地缓存不可用（初始化失败）：%v", err)
		return
	}
	s.cache = mgr
	s.loadCacheSettings()
	if !s.cacheOn() {
		return
	}
	// 启动时清理残留：中断/崩溃留下的 .part 与没有记录的孤儿 .bin
	s.cache.CleanupStale()
	s.cache.EnforceLimit()
}
