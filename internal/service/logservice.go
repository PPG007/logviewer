// Package service 是 Wails binding 层：把 logfile/parse/search 的能力封装为
// LogService 方法（前后端唯一接口），并通过事件推送索引/检索进度。
package service

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"logviewer/internal/logfile"
	"logviewer/internal/parse"
	"logviewer/internal/search"
)

// ---------------- DTO（会被 bindings 生成器转成 TS 类型） ----------------

type FileInfo struct {
	ID         string
	Path       string
	Name       string
	Status     string // "indexing" | "ready" | "error"
	TotalLines int64
}

type IndexStatus struct {
	FileID  string
	Done    bool
	Percent float64 // 0~100
	Error   string
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
	fields      map[string]map[string]string    // fileID -> 字段名 -> 类型
	fieldValues map[string]map[string]*valSet   // fileID -> 字段名 -> string 取值收集器
	fileTabs    map[string]map[string]struct{}  // fileID -> 该文件注册过的 tabID（CloseFile 时清理）
	tempFiles   map[string]string               // fileID -> 临时日志文件路径（关闭时删除）
	engine      *search.Engine
}

func NewLogService() *LogService {
	return &LogService{
		sessions:    make(map[string]*logfile.FileSession),
		fields:      make(map[string]map[string]string),
		fieldValues: make(map[string]map[string]*valSet),
		fileTabs:    make(map[string]map[string]struct{}),
		tempFiles:   make(map[string]string),
		engine:      search.NewEngine(),
	}
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

// openSession 打开日志路径并注册会话：同一次顺序扫描里完成索引、字段收集与
// 取值收集（枚举下拉数据），立即返回。displayName 非空时覆盖展示名
// （临时日志显示为「临时日志 …」）。
func (s *LogService) openSession(path, displayName string) (FileInfo, error) {
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
	sess, err := logfile.Open(path, onLine)
	if err != nil {
		return FileInfo{}, err
	}
	if displayName != "" {
		sess.Name = displayName // 仅展示用途；Name 在 FileSession 中不被索引路径读取
	}
	info := toFileInfo(sess)
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.fields[sess.ID] = acc
	s.fieldValues[sess.ID] = vals
	s.mu.Unlock()
	// 进度通过心跳事件推送（fileID 此时才可用，避免回调闭包时序问题）。
	s.watchIndex(sess.ID, sess)
	return info, nil
}

// OpenFile 打开指定路径文件（展示名 = 文件名）。
func (s *LogService) OpenFile(path string) (FileInfo, error) {
	return s.openSession(path, "")
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
	info, err := s.openSession(path, fmt.Sprintf("临时日志 %s", time.Now().Format("15:04:05")))
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
func (s *LogService) watchIndex(fileID string, sess *logfile.FileSession) {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			switch sess.Status() {
			case logfile.StatusIndexing:
				emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: sess.Percent(), Done: false})
			case logfile.StatusReady:
				emit("indexProgress", IndexProgressEvent{FileID: fileID, Percent: 100, Done: true})
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

// GetIndexStatus 查询索引进度（供前端打开文件后对账，事件为主、此方法兜底）。
func (s *LogService) GetIndexStatus(fileID string) (IndexStatus, error) {
	sess := s.get(fileID)
	if sess == nil {
		return IndexStatus{}, fmt.Errorf("file not found: %s", fileID)
	}
	st := IndexStatus{FileID: fileID, Percent: sess.Percent()}
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
func (s *LogService) toParsedLines(sess *logfile.FileSession, lineNos []int64) ([]ParsedLine, error) {
	rows := make([]ParsedLine, 0, len(lineNos))
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

func toFileInfo(sess *logfile.FileSession) FileInfo {
	return FileInfo{
		ID:         sess.ID,
		Path:       sess.Path,
		Name:       sess.Name,
		Status:     sess.Status().String(),
		TotalLines: sess.TotalLines(),
	}
}
