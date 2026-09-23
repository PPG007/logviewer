// Package service 是 Wails binding 层：把 logfile/parse/search 的能力封装为
// LogService 方法（前后端唯一接口），并通过事件推送索引/检索进度。
package service

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"logviewer/internal/filecache"
	"logviewer/internal/logfile"
	"logviewer/internal/parse"
	"logviewer/internal/search"
	"logviewer/internal/sshconn"
	"logviewer/internal/store"
)

// ---------------- DTO（会被 bindings 生成器转成 TS 类型） ----------------

type FileInfo struct {
	ID         string
	Path       string
	Name       string
	Status     string // "indexing" | "ready" | "error"
	TotalLines int64
	Kind       string // "local" | "remote"
	Remote     string // 远端连接标识 user@host:port（本地为空）
	ConnID     uint   // 远端连接 id（本地为 0）
}

type IndexStatus struct {
	FileID  string
	Done    bool
	Percent float64 // 0~100
	Error   string
	// TotalLines 索引完成后的总行数（未完成时为 0）。供前端对账兜底：
	// 事件可能早于订阅就绪，行数靠这里补齐（FileInfo 是打开瞬间的快照，行数恒为 0）。
	TotalLines int64
}

// RecentFile 历史记录项（侧栏「最近打开」列表）。
// 同名不同路径靠 Path/Dir 区分，同名不同主机再靠 Remote 区分。
type RecentFile struct {
	ID           uint
	Path         string
	Name         string
	Dir          string
	TotalLines   int64 // 最近一次索引完成后的行数（未知为 0）
	OpenCount    int
	LastOpenedAt int64  // unix 毫秒
	Exists       bool   // 记录读取时磁盘上是否仍存在；false = 已丢失，UI 标记并可移除记录
	Kind         string // "local" | "remote"
	Remote       string // 远端连接标识 user@host:port
	ConnID       uint   // 远端连接 id（重连用）
	// Connected 远端主机当前是否有活动连接。远端文件的存在性不在列表阶段探测
	// （列表不能因网络卡住），未连接时 UI 显示「未连接」而不是「已丢失」。
	Connected bool
}

// FieldValues 某字段探测到的取值枚举（供检索条件下拉，仅 string 类型字段有）。
// Truncated=true 表示取值过多已截断：Values 为空，UI 应回落为自由输入。
type FieldValues struct {
	Field     string
	Values    []string
	Truncated bool
}

type ParsedLine struct {
	LineNo    int64
	Raw       string
	Valid     bool
	JSON      map[string]any // 非法行为 nil
	Timestamp *int64         // unix 毫秒
	Level     string
	Message   string
}

type PageResult struct {
	Total int64
	Rows  []ParsedLine
}

type SearchResult struct {
	TabID string
	Total int64
	Rows  []ParsedLine // 第一页
}

// ---------------- 事件载荷（main.go 中 RegisterEvent） ----------------

type IndexProgressEvent struct {
	FileID  string
	Percent float64
	Done    bool
	Error   string // 索引失败信息（成功为空）
	// TotalLines 索引完成后的总行数（未完成时为 0）。行数只有在索引扫描结束后才可知，
	// 打开接口返回的 FileInfo.TotalLines 恒为 0，前端以此事件/GetIndexStatus 补正。
	TotalLines int64
}

type SearchProgressEvent struct {
	FileID  string
	TabID   string
	Scanned int64
	Total   int64
	Done    bool
}

// ---------------- LogService ----------------

// pageSizeLimit 每页行数上限（前端选择器上限 100；此处放宽到 500 允许程序化大页读取）。
const pageSizeLimit = 500

// clampPageSize 归一页大小：<1 按 1，>上限按上限。
func clampPageSize(n int) int {
	if n < 1 {
		n = 1
	}
	if n > pageSizeLimit {
		n = pageSizeLimit
	}
	return n
}

// ---------------- 临时日志（手动输入/粘贴创建） ----------------
// 上限口径：最多 50,000 行 / 10 MiB。内容经 IPC 全量传输并即时建索引：
// 行数过大会卡 UI 与索引（故按条数限制）；单行又可能很长，体积单独兜底
// （防御 10MB 单行压垮解析）。上限同时在前端预检（api.ts 常量镜像）。
const (
	maxTempLogLines = 50_000
	maxTempLogBytes = 10 << 20 // 10 MiB
)

// countLines 按 \n 计行数：以 \n 结尾的末行为终止符不计空尾行。
func countLines(s string) int {
	if len(s) == 0 {
		return 0
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			n++
		}
	}
	if s[len(s)-1] != '\n' {
		n++ // 末行无换行符也计一行
	}
	return n
}

// ---------------- 字段取值收集（枚举下拉用） ----------------
// 索引进度里已逐行解析出 map[string]any，这里顺带收集 string 字段的 distinct
// 取值，零额外扫描。每字段最多 maxFieldValues 个：level/status 等枚举字段收全集；
// msg 等高基数字段很快截断并整体丢弃（不保留任意的「前 200 个」避免误导），
// UI 依 Truncated 回落手输 —— 内存始终有界（截断即释放）。
const maxFieldValues = 200

type valSet struct {
	set  map[string]struct{}
	full bool // 已达上限：已清空，停止收集
}

func newValSet() *valSet { return &valSet{set: make(map[string]struct{}, 16)} }

func (v *valSet) add(s string) {
	if v.full {
		return
	}
	if _, ok := v.set[s]; ok {
		return
	}
	if len(v.set) >= maxFieldValues {
		v.full = true
		v.set = nil // 截断即弃：不占用内存
		return
	}
	v.set[s] = struct{}{}
}

func (v *valSet) list() []string {
	out := make([]string, 0, len(v.set))
	for s := range v.set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type LogService struct {
	mu          sync.Mutex
	sessions    map[string]*logfile.FileSession // fileID -> 会话
	sources     map[string]store.Source         // fileID -> 来源（去重、历史记录、重新打开都靠它）
	fields      map[string]map[string]string    // fileID -> 字段名 -> 类型
	fieldValues map[string]map[string]*valSet   // fileID -> 字段名 -> string 取值收集器
	fileTabs    map[string]map[string]struct{}  // fileID -> 该文件注册过的 tabID（CloseFile 时清理）
	tempFiles   map[string]string               // fileID -> 临时日志文件路径（关闭时删除）
	engine      *search.Engine

	// store 历史文件记录库（SQLite）。打开失败时为 nil，功能整体降级为「无历史记录」，
	// 不影响打开/检索等主流程。
	store *store.Store
	// remote 远端 SSH/SFTP 连接管理器。数据库不可用时为 nil（远端功能整体不可用）。
	remote *sshconn.Manager
	// cache 远端文件内容的本地缓存。日志查看器的常态是反复看同一份日志，
	// 缓存命中时重开与翻页都不再走网络；数据库不可用时为 nil（降级为不缓存）。
	cache *filecache.Manager
	// cacheEnabled 全局缓存开关（主机可单独覆盖）；见 cache.go。
	cfgMu        sync.RWMutex
	cacheEnabled bool
}

func NewLogService() *LogService {
	s := &LogService{
		sessions:    make(map[string]*logfile.FileSession),
		sources:     make(map[string]store.Source),
		fields:      make(map[string]map[string]string),
		fieldValues: make(map[string]map[string]*valSet),
		fileTabs:    make(map[string]map[string]struct{}),
		tempFiles:   make(map[string]string),
		engine:      search.NewEngine(),
	}
	if path, err := store.DefaultPath(); err != nil {
		log.Printf("历史记录不可用（无法确定数据库路径）：%v", err)
	} else if db, err := store.Open(path); err != nil {
		log.Printf("历史记录不可用（打开数据库失败）：%v", err)
	} else {
		s.store = db
	}
	// 主机指纹记录放在配置目录：与数据库同级，便于随配置一起备份/清理。
	// 同时复用 ~/.ssh/known_hosts（只读），让已经信任过的主机免于二次确认。
	if dir, err := store.ConfigDir(); err != nil {
		log.Printf("远端功能不可用（无法确定配置目录）：%v", err)
	} else if m, err := sshconn.NewManager(sshconn.Options{
		KnownHostsPath:      filepath.Join(dir, "known_hosts"),
		ReuseUserKnownHosts: true,
	}); err != nil {
		log.Printf("远端功能不可用（初始化连接管理器失败）：%v", err)
	} else {
		s.remote = m
	}
	// 缓存默认开启；初始化失败只是不缓存，远端功能照常可用。
	s.cacheEnabled = true
	s.initCache()
	return s
}

// ServiceShutdown 由 Wails 在退出时调用：关闭远端连接与数据库连接。
func (s *LogService) ServiceShutdown() error {
	if s.remote != nil {
		s.remote.Close()
	}
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

// emit 发送事件；application.Get() 为 nil（如单元测试）时静默跳过。
func emit(name string, payload any) {
	app := application.Get()
	if app == nil {
		return
	}
	app.Event.Emit(name, payload)
}

// dialogCanceled 判断对话框错误是否为用户取消：Windows 下取消对话框返回
// cfd.ErrorCancelled（文本 "cancelled by user"），该包是 wails 的 internal
// 实现，无法 errors.Is，只能按错误文本识别。取消与「未选择」等价，调用方应返回空结果。
func dialogCanceled(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "cancel")
}

// OpenFileDialog 弹出系统文件选择对话框并打开所选文件；用户取消返回空 FileInfo。
func (s *LogService) OpenFileDialog() (FileInfo, error) {
	path, err := application.Get().Dialog.OpenFile().
		CanChooseFiles(true).
		SetTitle("选择日志文件").
		AddFilter("日志文件", "*.log;*.jsonl;*.txt;*.json").
		AddFilter("所有文件", "*.*").
		PromptForSingleSelection()
	if err != nil {
		if dialogCanceled(err) {
			return FileInfo{}, nil // 用户取消：不是错误，不向 UI 报错
		}
		return FileInfo{}, err
	}
	if path == "" {
		return FileInfo{}, nil // 用户取消（其它平台返回空路径）
	}
	return s.OpenFile(path)
}

// newCollectors 字段/取值收集器：复用建索引的同一次顺序扫描顺带收集
// （字段名 + string 字段的枚举取值），避免为下拉数据多扫一遍文件。
// 返回的收集器只应由当轮扫描的 goroutine 写入；读取方须先等 WaitReady。
func newCollectors() (map[string]string, map[string]*valSet, logfile.LineFunc) {
	acc := make(map[string]string)
	vals := make(map[string]*valSet)
	onLine := func(_ int64, raw string) {
		if m, ok := parse.ParseLine(raw); ok {
			parse.CollectFields(m, acc)
			for k, v := range m {
				sv, ok := v.(string) // 仅 string 值参与枚举；number/bool 等保持自由输入
				if !ok {
					continue
				}
				vs := vals[k]
				if vs == nil {
					vs = newValSet()
					vals[k] = vs
				}
				vs.add(sv) // 原样收集（不做 level 大小写归一），与条件匹配口径一致
			}
		}
	}
	return acc, vals, onLine
}

// openSession 打开来源并注册会话：同一次顺序扫描里完成索引、字段收集与
// 取值收集（枚举下拉数据），立即返回。displayName 非空时覆盖展示名
// （临时日志显示为「临时日志 …」）；persist=false 表示临时日志，不进历史记录。
// open 负责提供字节来源（本地文件或远端 SFTP 文件），由调用方决定怎么连。
func (s *LogService) openSession(
	src store.Source, displayName string, persist bool, open func() (logfile.Source, error),
) (FileInfo, error) {
	acc, vals, onLine := newCollectors()
	raw, err := open()
	if err != nil {
		return FileInfo{}, err
	}
	name := src.Name()
	if displayName != "" {
		name = displayName // 仅展示用途；Name 在 FileSession 中不被索引路径读取
	}
	sess, err := logfile.OpenSource(raw, src.DisplayPath(), name, onLine)
	if err != nil {
		return FileInfo{}, err
	}
	info := toFileInfo(sess, src)
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.sources[sess.ID] = src
	s.fields[sess.ID] = acc
	s.fieldValues[sess.ID] = vals
	s.mu.Unlock()
	if persist {
		// 记录文件名/路径/大小/修改时间等：重启后可在「最近打开」里找到（同名不同路径各记一条）。
		s.recordOpen(src, sess)
		s.recordTotalLines(src, sess)
	}
	// 进度通过心跳事件推送（fileID 此时才可用，避免回调闭包时序问题）。
	s.watchIndex(sess.ID, sess)
	return info, nil
}

// OpenFile 打开本地文件（展示名 = 文件名）。同一来源已打开时：文件没变就直接复用
// 会话（避免重复占用索引内存），变了则重建索引（日志被追加后不必先关再开）。
// 同名但不同目录的文件是两个独立会话。
func (s *LogService) OpenFile(path string) (FileInfo, error) {
	src := store.LocalSource(path)
	stat := func() (os.FileInfo, error) { return os.Stat(path) }
	open := func() (logfile.Source, error) { return os.Open(path) }
	if info, ok := s.findOpen(src); ok {
		sess := s.get(info.ID)
		if sess == nil {
			return FileInfo{}, fmt.Errorf("file not found: %s", info.ID)
		}
		info, err := s.reuseOrReload(sess, src, stat, open)
		if err != nil {
			return FileInfo{}, err
		}
		s.recordOpen(src, sess) // 复用会话也算一次打开
		return info, nil
	}
	return s.openSession(src, "", true, open)
}

// reuseOrReload 处理「打开的是一份已经在列表里的文件」：
// 来源侧的大小与修改时间都没变就直接复用现有会话；变了就丢弃旧索引重新读取。
// 重新加载保持会话 id 不变，前端已开的 tab 与检索条件因此得以保留。
//
// 比对只查元信息（stat），不预开来源：开了就要在 Reload 之前关掉，
// 否则新旧两份额来源重叠会踩到缓存的「同一来源一个写入者」限制。
func (s *LogService) reuseOrReload(
	sess *logfile.FileSession,
	src store.Source,
	stat func() (os.FileInfo, error),
	open func() (logfile.Source, error),
) (FileInfo, error) {
	fi, err := stat()
	if err != nil {
		return FileInfo{}, err
	}
	if fi.Size() == sess.Size() && fi.ModTime().Equal(sess.ModTime()) {
		return toFileInfo(sess, src), nil // 内容没变：不做无谓的重新拉取（远端可能是上百 MB）
	}

	acc, vals, onLine := newCollectors()
	if err := sess.Reload(open, onLine); err != nil {
		return FileInfo{}, err
	}
	s.mu.Lock()
	s.fields[sess.ID] = acc
	s.fieldValues[sess.ID] = vals
	s.mu.Unlock()
	s.recordTotalLines(src, sess)
	s.watchIndex(sess.ID, sess)
	// 立刻通知前端「重新开始索引」：文件很小时本轮可能在首次心跳前就结束，
	// 单靠事件里的 done=true 无法让前端区分「重新加载」与「本来就好」。
	emit("indexProgress", IndexProgressEvent{FileID: sess.ID, Percent: 0, Done: false})
	return toFileInfo(sess, src), nil
}

// ReloadFile 强制重新读取并重建索引（不看文件是否变化）。用于日志被追加/轮转后
// 手动刷新，或大小与修改时间都没变但内容确实变了的情形。
// 会话 id 不变，前端已开的 tab 与检索条件保留，但需要重新检索。
func (s *LogService) ReloadFile(fileID string) error {
	sess := s.get(fileID)
	if sess == nil {
		return fmt.Errorf("file not found: %s", fileID)
	}
	s.mu.Lock()
	src := s.sources[fileID]
	s.mu.Unlock()

	// 强制刷新必须绕过缓存，并用新内容重写它：
	// 用户点这个按钮，恰恰是因为「大小与修改时间都没变但内容确实变了」。
	_, open, err := s.accessorsFor(src, true)
	if err != nil {
		return err
	}
	acc, vals, onLine := newCollectors()
	if err := sess.Reload(open, onLine); err != nil {
		return err
	}
	s.mu.Lock()
	s.fields[fileID] = acc
	s.fieldValues[fileID] = vals
	s.mu.Unlock()
	s.recordTotalLines(src, sess)
	s.watchIndex(fileID, sess)
	emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: 0, Done: false})
	return nil
}

// accessorsFor 按来源给出「查元信息」与「打开字节来源」两个闭包。
// 分成两个是为了让「比对文件是否变化」只查元信息，不必先开一份来源再关掉。
// bypash 供「重新加载」使用：绕过缓存直连远端，并用新内容重写缓存。
func (s *LogService) accessorsFor(
	src store.Source, bypass bool,
) (func() (os.FileInfo, error), func() (logfile.Source, error), error) {
	if src.Kind != store.KindRemote {
		// 缓存只服务远端：本地文件本身就在磁盘上，再复制一份毫无意义，
		// 而且一旦本地文件「同大小同 mtime 被改写」，缓存会把本地内容也钉成旧值。
		return func() (os.FileInfo, error) { return os.Stat(src.Path) },
			func() (logfile.Source, error) { return os.Open(src.Path) }, nil
	}
	if s.remote == nil {
		return nil, nil, errNoRemote()
	}
	rec, err := s.store.GetConnection(src.ConnID)
	if err != nil {
		return nil, nil, fmt.Errorf("主机不存在：%w", err)
	}
	prof := profile(rec)
	if !s.remote.Connected(prof) {
		return nil, nil, fmt.Errorf("需要先连接 %s 才能重新加载", prof.Display())
	}
	return func() (os.FileInfo, error) { return s.remote.Stat(prof, src.Path) },
		func() (logfile.Source, error) { return s.openRemoteSource(rec, prof, src.Path, bypass) }, nil
}

// recordOpen 写一条历史记录（打开次数 +1、刷新最近打开时间）。失败只记日志，
// 不影响打开主流程；store 不可用（数据库打开失败）时整体降级为空操作。
func (s *LogService) recordOpen(src store.Source, sess *logfile.FileSession) {
	if s.store == nil {
		return
	}
	if err := s.store.Touch(src, sess.Size(), sess.ModTime().Unix()); err != nil {
		log.Printf("写入历史记录失败：%v", err)
	}
}

// recordTotalLines 等索引结束后把总行数回写历史记录（行数只有索引完成时才可知）。
func (s *LogService) recordTotalLines(src store.Source, sess *logfile.FileSession) {
	if s.store == nil {
		return
	}
	go func() {
		if err := sess.WaitReady(); err != nil {
			return // 索引失败：不写行数
		}
		if err := s.store.SetTotalLines(src, sess.TotalLines()); err != nil {
			log.Printf("写入历史记录行数失败：%v", err)
		}
	}()
}

// findOpen 按来源键查找已打开的会话（本地忽略大小写，远端含主机维度）。
func (s *LogService) findOpen(src store.Source) (FileInfo, bool) {
	key := src.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if s.sources[id].Key() == key {
			return toFileInfo(sess, s.sources[id]), true
		}
	}
	return FileInfo{}, false
}

// OpenTempLog 创建临时日志：内容写入系统临时目录后走与普通文件一致的解析/索引流程。
// 上限：50,000 行 / 10 MiB（前端预检同口径，此处为准）。
func (s *LogService) OpenTempLog(content string) (FileInfo, error) {
	if strings.TrimSpace(content) == "" {
		return FileInfo{}, fmt.Errorf("临时日志内容为空")
	}
	if len(content) > maxTempLogBytes {
		return FileInfo{}, fmt.Errorf("临时日志体积超过上限：最多 %d MB", maxTempLogBytes>>20)
	}
	if n := countLines(content); n > maxTempLogLines {
		return FileInfo{}, fmt.Errorf("临时日志行数超过上限：最多 %d 行（当前 %d 行）", maxTempLogLines, n)
	}
	tmp, err := os.CreateTemp("", "logviewer-temp-*.jsonl")
	if err != nil {
		return FileInfo{}, fmt.Errorf("创建临时文件失败：%w", err)
	}
	path := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(path)
		return FileInfo{}, fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(path)
		return FileInfo{}, fmt.Errorf("写入临时文件失败：%w", err)
	}
	info, err := s.openSession(
		store.LocalSource(path),
		fmt.Sprintf("临时日志 %s", time.Now().Format("15:04:05")),
		false,
		func() (logfile.Source, error) { return os.Open(path) },
	)
	if err != nil {
		os.Remove(path) // 打开失败不留垃圾文件
		return FileInfo{}, err
	}
	s.mu.Lock()
	s.tempFiles[info.ID] = path
	s.mu.Unlock()
	return info, nil
}

// watchIndex 轮询会话状态并以 indexProgress 事件推送进度，直到 ready/error。
// 索引完成时才拿得到总行数，故随完成事件一并推送（前端据此补正行数显示）。
func (s *LogService) watchIndex(fileID string, sess *logfile.FileSession) {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			switch sess.Status() {
			case logfile.StatusIndexing:
				emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: sess.Percent(), Done: false})
			case logfile.StatusReady:
				emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: 100, Done: true, TotalLines: sess.TotalLines()})
				return
			default:
				errMsg := ""
				if sess.Error() != nil {
					errMsg = sess.Error().Error()
				}
				emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: 0, Done: true, Error: errMsg})
				return
			}
		}
	}()
}

// CloseFile 关闭文件，取消其所有检索并释放索引与结果缓存；
// 临时日志的临时文件一并删除。
func (s *LogService) CloseFile(fileID string) error {
	s.mu.Lock()
	sess := s.sessions[fileID]
	delete(s.sessions, fileID)
	delete(s.sources, fileID)
	delete(s.fields, fileID)
	delete(s.fieldValues, fileID)
	tabs := s.fileTabs[fileID]
	delete(s.fileTabs, fileID)
	tmpPath := s.tempFiles[fileID]
	delete(s.tempFiles, fileID)
	s.mu.Unlock()

	if sess == nil {
		return fmt.Errorf("file not found: %s", fileID)
	}
	for tabID := range tabs {
		s.engine.Remove(tabID) // 取消进行中的扫描并释放结果
	}
	err := sess.Close()
	if tmpPath != "" {
		_ = os.Remove(tmpPath) // 尽力而为；残留由系统临时目录自行清理
	}
	return err
}

func (s *LogService) get(fileID string) *logfile.FileSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[fileID]
}

// ---------------- 历史记录（「最近打开」） ----------------

// errNoHistory 历史记录不可用（数据库打开失败）时统一返回的错误。
func errNoHistory() error {
	return fmt.Errorf("历史记录不可用（数据库未能初始化）")
}

// ListRecentFiles 返回历史文件记录（最近打开在前），并探测文件在磁盘上是否仍存在。
func (s *LogService) ListRecentFiles() ([]RecentFile, error) {
	if s.store == nil {
		return nil, errNoHistory()
	}
	recs, err := s.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]RecentFile, 0, len(recs))
	// 只探测本地路径：远端记录若逐条连网 stat，侧栏会被网络拖住。
	// 远端的存在性留到用户点击时（连接后 stat）判定，未连接时 UI 显示「未连接」。
	localIdx := make([]int, 0, len(recs))
	localPaths := make([]string, 0, len(recs))
	for _, r := range recs {
		kind := r.Kind
		if kind == "" {
			kind = store.KindLocal // 兼容早期版本写入的记录（无 kind 列）
		}
		out = append(out, RecentFile{
			ID:           r.ID,
			Path:         r.Path,
			Name:         r.Name,
			Dir:          r.Dir,
			TotalLines:   r.TotalLines,
			OpenCount:    r.OpenCount,
			LastOpenedAt: r.LastOpenedAt.UnixMilli(),
			Exists:       true, // 先按「存在」乐观置位，探测超时/失败不误报已丢失
			Kind:         kind,
			Remote:       r.Remote,
			ConnID:       r.ConnID,
			Connected:    kind == store.KindRemote && s.remote != nil && s.remote.ConnectedKey(r.Remote),
		})
		if kind == store.KindLocal {
			localIdx = append(localIdx, len(out)-1)
			localPaths = append(localPaths, r.Path)
		}
	}
	for i, missing := range probeExists(localPaths) {
		out[localIdx[i]].Exists = !missing
	}
	return out, nil
}

// OpenRecentFile 打开历史记录中的文件；本地文件已不在磁盘时返回明确错误，
// 前端据此提示「文件不存在」并给出移除记录的选项。
//
// 远端记录需要先连接：未连接时返回提示，由前端引导去连接（口令已保存时连接表单留空即可）。
func (s *LogService) OpenRecentFile(id uint) (FileInfo, error) {
	if s.store == nil {
		return FileInfo{}, errNoHistory()
	}
	rec, err := s.store.Get(id)
	if err != nil {
		return FileInfo{}, fmt.Errorf("历史记录不存在：%w", err)
	}
	if rec.Kind == store.KindRemote {
		if s.remote == nil {
			return FileInfo{}, errNoRemote()
		}
		if !s.remote.ConnectedKey(rec.Remote) {
			return FileInfo{}, fmt.Errorf("需要先连接 %s 才能打开该文件", rec.Remote)
		}
		return s.OpenRemoteFile(rec.ConnID, rec.Path)
	}
	if _, err := os.Stat(rec.Path); err != nil {
		if missingFile(err) {
			return FileInfo{}, fmt.Errorf("文件不存在：%s", rec.Path)
		}
		return FileInfo{}, fmt.Errorf("无法访问文件：%s（%v）", rec.Path, err)
	}
	return s.OpenFile(rec.Path)
}

// RemoveRecentFile 从历史记录中移除一条（关闭文件不删记录，用户显式移除才删）。
func (s *LogService) RemoveRecentFile(id uint) error {
	if s.store == nil {
		return errNoHistory()
	}
	return s.store.Delete(id)
}

// ClearRecentFiles 清空全部历史记录。
func (s *LogService) ClearRecentFiles() error {
	if s.store == nil {
		return errNoHistory()
	}
	return s.store.Clear()
}

// existenceWait 存在性探测的整体等待上限：网络路径不可达时 os.Stat 可能阻塞数十秒，
// 超时未返回的记录按「存在」处理（打开时仍会给出准确错误），避免侧栏整体卡住。
const existenceWait = 1500 * time.Millisecond

// probeExists 并发探测各路径是否已不在磁盘上，返回与 paths 等长的 missing 标志。
// 结果经缓冲 channel 回收：超时后仍在跑的 goroutine 写满缓冲即退出，不泄漏也不会与调用方竞争。
func probeExists(paths []string) []bool {
	missing := make([]bool, len(paths))
	if len(paths) == 0 {
		return missing
	}
	type result struct {
		idx     int
		missing bool
	}
	ch := make(chan result, len(paths))
	for i, p := range paths {
		go func(i int, p string) {
			_, err := os.Stat(p)
			ch <- result{idx: i, missing: missingFile(err)}
		}(i, p)
	}
	timeout := time.After(existenceWait)
	for range paths {
		select {
		case r := <-ch:
			missing[r.idx] = r.missing
		case <-timeout:
			return missing
		}
	}
	return missing
}

// missingFile 判断 Stat 错误是否为「文件不存在」（含路径中间层不是目录的情形）。
func missingFile(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// GetIndexStatus 查询索引进度（供前端打开文件后对账，事件为主、此方法兜底）。
func (s *LogService) GetIndexStatus(fileID string) (IndexStatus, error) {
	sess := s.get(fileID)
	if sess == nil {
		return IndexStatus{}, fmt.Errorf("file not found: %s", fileID)
	}
	st := IndexStatus{FileID: fileID, Percent: sess.Percent(), TotalLines: sess.TotalLines()}
	switch sess.Status() {
	case logfile.StatusReady:
		st.Done = true
		st.Percent = 100
	case logfile.StatusError:
		st.Done = true
		if sess.Error() != nil {
			st.Error = sess.Error().Error()
		}
	default:
		st.Done = false
	}
	return st, nil
}

// GetFields 返回探测到的顶层字段（索引完成后才完整，未完成则阻塞等待）。
func (s *LogService) GetFields(fileID string) ([]parse.FieldInfo, error) {
	sess := s.get(fileID)
	if sess == nil {
		return nil, fmt.Errorf("file not found: %s", fileID)
	}
	if err := sess.WaitReady(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	m := s.fields[fileID]
	s.mu.Unlock()
	out := make([]parse.FieldInfo, 0, len(m))
	for k, v := range m {
		out = append(out, parse.FieldInfo{Name: k, Type: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetFieldValues 返回字段的取值枚举（字母序）。索引完成前阻塞等待；
// 仅收集 string 值：字段不存在/非 string/取值过多截断时 Values 为空，
// Truncated 标识截断（调用方据此回落为自由输入）。
func (s *LogService) GetFieldValues(fileID, field string) (FieldValues, error) {
	sess := s.get(fileID)
	if sess == nil {
		return FieldValues{}, fmt.Errorf("file not found: %s", fileID)
	}
	if err := sess.WaitReady(); err != nil {
		return FieldValues{}, err
	}
	// 索引结束后 vals 不再被 onLine 写入，注册后只读，无需额外加锁
	s.mu.Lock()
	vs := s.fieldValues[fileID][field]
	s.mu.Unlock()
	if vs == nil {
		return FieldValues{Field: field, Values: []string{}}, nil
	}
	if vs.full {
		return FieldValues{Field: field, Values: []string{}, Truncated: true}, nil
	}
	return FieldValues{Field: field, Values: vs.list()}, nil
}

// GetLines 浏览模式：按行号取一页原始行。
func (s *LogService) GetLines(fileID string, start, count int64) (PageResult, error) {
	sess := s.get(fileID)
	if sess == nil {
		return PageResult{}, fmt.Errorf("file not found: %s", fileID)
	}
	rawLines, err := sess.ReadLines(start, count)
	if err != nil {
		return PageResult{}, err
	}
	rows := make([]ParsedLine, 0, len(rawLines))
	for i, raw := range rawLines {
		rows = append(rows, buildParsedLine(start+int64(i), raw))
	}
	return PageResult{Total: sess.TotalLines(), Rows: rows}, nil
}

// Search 提交检索（tabID 由前端生成），后台顺序扫描；期间 emit searchProgress。
// pageSize 决定首屏返回行数；取消时返回 context 取消错误（前端静默处理）。
func (s *LogService) Search(fileID, tabID string, query search.Query, pageSize int) (SearchResult, error) {
	sess := s.get(fileID)
	if sess == nil {
		return SearchResult{}, fmt.Errorf("file not found: %s", fileID)
	}
	s.mu.Lock()
	if s.fileTabs[fileID] == nil {
		s.fileTabs[fileID] = make(map[string]struct{})
	}
	s.fileTabs[fileID][tabID] = struct{}{}
	s.mu.Unlock()

	onProgress := func(done, total int64) {
		emit("searchProgress", SearchProgressEvent{FileID: fileID, TabID: tabID, Scanned: done, Total: total})
	}
	matched, err := s.engine.Search(sess, tabID, query, onProgress)
	if err != nil {
		return SearchResult{}, err // 取消或会话错误：不缓存、无结果
	}
	emit("searchProgress", SearchProgressEvent{FileID: fileID, TabID: tabID, Scanned: int64(len(matched)), Total: int64(len(matched)), Done: true})

	pageNos, _ := s.engine.Page(tabID, 1, clampPageSize(pageSize))
	rows, err := s.toParsedLines(sess, pageNos)
	if err != nil {
		return SearchResult{}, err
	}
	return SearchResult{TabID: tabID, Total: int64(len(matched)), Rows: rows}, nil
}

// GetPage 检索模式翻页：命中行号内存跳转，pageSize 由前端选择器决定。
func (s *LogService) GetPage(fileID, tabID string, page, pageSize int) (PageResult, error) {
	sess := s.get(fileID)
	if sess == nil {
		return PageResult{}, fmt.Errorf("file not found: %s", fileID)
	}
	pageNos, err := s.engine.Page(tabID, page, clampPageSize(pageSize))
	if err != nil {
		return PageResult{}, err
	}
	rows, err := s.toParsedLines(sess, pageNos)
	if err != nil {
		return PageResult{}, err
	}
	return PageResult{Total: int64(s.engine.Total(tabID)), Rows: rows}, nil
}

// CancelSearch 取消进行中的检索（无则空操作）。
func (s *LogService) CancelSearch(fileID, tabID string) error {
	s.engine.Cancel(tabID)
	return nil
}

// RemoveTab 关闭检索 tab：取消进行中的扫描并释放该 tab 的命中缓存
// （补充于 §8.1：关 tab 只取消会残留命中行号缓存，多 tab 高频增删会放大内存）。
func (s *LogService) RemoveTab(fileID, tabID string) error {
	s.mu.Lock()
	sess := s.sessions[fileID]
	if tabs := s.fileTabs[fileID]; tabs != nil {
		delete(tabs, tabID)
		if len(tabs) == 0 {
			delete(s.fileTabs, fileID)
		}
	}
	s.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("file not found: %s", fileID)
	}
	s.engine.Remove(tabID)
	return nil
}

// ExportMatches 把 tabId 的全部命中原始行写入用户选择的文件（JSONL，逐行原样，行尾统一 \n）。
// 用户取消返回空路径且无错误。
func (s *LogService) ExportMatches(fileID, tabID string) (string, error) {
	app := application.Get()
	if app == nil {
		return "", fmt.Errorf("application not ready") // 单测环境无对话框
	}
	sess := s.get(fileID)
	if sess == nil {
		return "", fmt.Errorf("file not found: %s", fileID)
	}
	hits := s.engine.All(tabID)
	path, err := app.Dialog.SaveFile().
		SetFilename("search-results.jsonl").
		SetMessage(fmt.Sprintf("将把该 tab 的全部命中原始行写入所选文件（共 %d 条）", len(hits))).
		AddFilter("JSONL 日志", "*.jsonl").
		AddFilter("文本文件", "*.txt").
		AddFilter("所有文件", "*.*").
		PromptForSingleSelection()
	if err != nil {
		if dialogCanceled(err) {
			return "", nil // 用户取消：不是错误，不向 UI 报错
		}
		return "", err
	}
	if path == "" {
		return "", nil // 用户取消（其它平台返回空路径）
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	if err := exportHits(w, sess, hits); err != nil {
		return "", err
	}
	return path, w.Flush()
}

// exportHits 把升序命中行号写入 w：相邻行合并为一次 ReadLines 批量读，减少随机读调用次数。
// 导出内容为命中行的原始文本 + 换行（不重新序列化 JSON）。
func exportHits(w io.Writer, sess *logfile.FileSession, hits []int64) error {
	for i := 0; i < len(hits); {
		j := i
		for j+1 < len(hits) && hits[j+1] == hits[j]+1 {
			j++
		}
		lines, err := sess.ReadLines(hits[i], int64(j-i+1))
		if err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := io.WriteString(w, l); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		i = j + 1
	}
	return nil
}

// toParsedLines 命中行号 -> ParsedLine 列表。
//
// 命中行号通常密集（同一页里相邻），因此合并成一次连续读取：
// 远端来源上逐行读是每行一次网络往返，一页 20 行就是 20 次。
// 跨度过大时（散落的命中）退回逐行，避免为几个行号拉一大段。
func (s *LogService) toParsedLines(sess *logfile.FileSession, lineNos []int64) ([]ParsedLine, error) {
	rows := make([]ParsedLine, 0, len(lineNos))
	if len(lineNos) == 0 {
		return rows, nil
	}
	lo, hi := lineNos[0], lineNos[0]
	for _, no := range lineNos {
		if no < lo {
			lo = no
		}
		if no > hi {
			hi = no
		}
	}
	const maxSpan = 4096 // 跨度超过这个行数就不值得为少数命中整段拉取
	if hi-lo <= maxSpan {
		raws, err := sess.ReadLines(lo, hi-lo+1)
		if err != nil {
			return nil, err
		}
		for _, no := range lineNos {
			idx := no - lo
			if idx < 0 || int(idx) >= len(raws) {
				continue
			}
			rows = append(rows, buildParsedLine(no, raws[idx]))
		}
		return rows, nil
	}

	for _, no := range lineNos {
		raws, err := sess.ReadLines(no, 1)
		if err != nil {
			return nil, err
		}
		if len(raws) == 0 {
			continue
		}
		rows = append(rows, buildParsedLine(no, raws[0]))
	}
	return rows, nil
}

func buildParsedLine(lineNo int64, raw string) ParsedLine {
	pl := ParsedLine{LineNo: lineNo, Raw: raw}
	m, ok := parse.ParseLine(raw)
	if !ok {
		return pl // Valid=false, JSON=nil
	}
	pl.Valid = true
	pl.JSON = m
	pl.Timestamp = parse.ExtractTimestamp(m)
	pl.Level = parse.DetectLevel(m)
	pl.Message = parse.DetectMessage(m)
	return pl
}

func toFileInfo(sess *logfile.FileSession, src store.Source) FileInfo {
	return FileInfo{
		ID:         sess.ID,
		Path:       sess.Path,
		Name:       sess.Name,
		Status:     sess.Status().String(),
		TotalLines: sess.TotalLines(),
		Kind:       src.Kind,
		Remote:     src.Remote,
		ConnID:     src.ConnID,
	}
}
