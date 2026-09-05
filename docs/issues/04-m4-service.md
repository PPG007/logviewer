# M4 · 后端：Service 绑定层

- **状态**：未开始
- **依赖**：M1、M2、M3
- **关联文档**：[功能设计](../functional-design.md) §8（Binding API、事件）

## 目标

实现 `internal/service` 包的 `LogService`：把 logfile / parse / search 的能力封装成 **Wails binding 方法**，并通过**事件**向推送进度；接入 `main.go`。这是前后端之间的唯一接口层。

## DTO 定义（会被生成器转成 TS 类型）

```go
package service

type FileInfo struct {
    ID         string
    Path       string
    Name       string
    Status     string // "indexing" | "ready" | "error"
    TotalLines int64
}

type IndexStatus struct {
    FileID    string
    Done      bool
    Percent   float64 // 0~100
    Error     string
}

type ParsedLine struct {
    LineNo    int64
    Raw       string
    Valid     bool
    JSON      map[string]any // 非法行为 nil
    Timestamp *int64          // unix 毫秒
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

type IndexProgressEvent struct {
    FileID  string
    Percent float64
    Done    bool
}

type SearchProgressEvent struct {
    FileID string
    TabID  string
    Scanned int64
    Total   int64
    Done    bool
}
```

## LogService 结构与目标 API

```go
type LogService struct {
    mu       sync.Mutex
    sessions map[string]*logfile.FileSession // fileID -> 会话
    fields   map[string]map[string]string    // fileID -> 字段名 -> 类型
    engine   *search.Engine
}

func NewLogService() *LogService

func (s *LogService) OpenFileDialog() (FileInfo, error)       // 弹系统对话框并打开
func (s *LogService) OpenFile(path string) (FileInfo, error)  // 按路径打开
func (s *LogService) CloseFile(fileID string) error
func (s *LogService) GetIndexStatus(fileID string) (IndexStatus, error)
func (s *LogService) GetFields(fileID string) ([]parse.FieldInfo, error)
func (s *LogService) GetLines(fileID string, start, count int64) (PageResult, error)      // 浏览（无检索）
func (s *LogService) Search(fileID, tabID string, query search.Query) (SearchResult, error)
func (s *LogService) GetPage(fileID, tabID string, page int) (PageResult, error)          // 检索翻页
func (s *LogService) CancelSearch(fileID, tabID string) error
```

> 设计文档 §8.1 为此 API 的契约来源；这里补充了 `OpenFileDialog` 与 `OpenFile(path)`（用于命令行参数/后续拖拽）。

## 详细实现

### 1. 打开文件（对话框 + 索引 + 字段收集）

```go
func (s *LogService) OpenFileDialog() (FileInfo, error) {
    path, err := application.Get().Dialog.OpenFile().
        CanChooseFiles(true).
        SetTitle("选择日志文件").
        AddFilter("日志文件", "*.log;*.jsonl;*.txt;*.json;*.*").
        PromptForSingleSelection()
    if err != nil {
        return FileInfo{}, err
    }
    if path == "" {
        return FileInfo{}, nil // 用户取消
    }
    return s.OpenFile(path)
}

func (s *LogService) OpenFile(path string) (FileInfo, error) {
    acc := map[string]string{} // 字段收集（onLine 回调，与索引同一次扫描）
    onLine := func(lineNo int64, raw string) {
        if m, ok := parse.ParseLine(raw); ok {
            parse.CollectFields(m, acc)
        }
    }
    onProgress := func(done, total int64) { // done/total 单位：字节
        if total > 0 {
            application.Get().Event.Emit("indexProgress", IndexProgressEvent{
                FileID:  "", // 见下方说明：先建会话再回填
                Percent: float64(done) / float64(total) * 100,
            })
        }
    }
    sess, err := logfile.Open(path, onProgress, onLine)
    if err != nil {
        return FileInfo{}, err
    }
    s.mu.Lock()
    s.sessions[sess.ID] = sess
    s.fields[sess.ID] = acc
    s.mu.Unlock()
    return toFileInfo(sess), nil
}
```

> ⚠️ 实现要点：`onProgress` 闭包在 `logfile.Open` 返回前就会被 goroutine 调用，而 `FileID` 需要 `sess.ID`。解决办法：让 `onProgress` 通过闭包变量持有 `fileID`（`Open` 返回后赋值），或把进度事件改为由 `Open` 之后单独的心跳 goroutine 转发。**推荐后者**：`Open` 后起一个 `go` 轮询 `sess.Status()` 直到 ready，期间按 `sess` 的进度（在 `FileSession` 上暴露 `Progress()` 方法或直接用字节进度）emit 事件，最终 emit `Done=true`。M1 的 `ProgressFunc` 与这里的衔接细节在实现时敲定，接口契约（事件名与载荷结构）保持不变。

### 2. 关闭文件

```go
func (s *LogService) CloseFile(fileID string) error {
    s.mu.Lock()
    sess := s.sessions[fileID]
    delete(s.sessions, fileID)
    delete(s.fields, fileID)
    s.mu.Unlock()
    if sess == nil {
        return fmt.Errorf("file not found: %s", fileID)
    }
    return sess.Close()
}
```

### 3. 查询与浏览

```go
func (s *LogService) GetIndexStatus(fileID string) (IndexStatus, error) {
    sess := s.get(fileID)
    if sess == nil {
        return IndexStatus{}, fmt.Errorf("file not found")
    }
    // 依据 sess.Status() 计算 percent（ready 则 100）
    ...
}

func (s *LogService) GetFields(fileID string) ([]parse.FieldInfo, error) {
    sess := s.get(fileID)
    if sess == nil {
        return nil, fmt.Errorf("file not found")
    }
    if err := sess.WaitReady(); err != nil { // 字段在索引完成后才完整
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

func (s *LogService) GetLines(fileID string, start, count int64) (PageResult, error) {
    sess := s.get(fileID)
    if sess == nil {
        return PageResult{}, fmt.Errorf("file not found")
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

func (s *LogService) Search(fileID, tabID string, query search.Query) (SearchResult, error) {
    sess := s.get(fileID)
    if sess == nil {
        return SearchResult{}, fmt.Errorf("file not found")
    }
    onProgress := func(done, total int64) {
        application.Get().Event.Emit("searchProgress", SearchProgressEvent{
            FileID: fileID, TabID: tabID, Scanned: done, Total: total,
        })
    }
    matched, err := s.engine.Search(sess, tabID, query, onProgress)
    if err != nil {
        return SearchResult{}, err
    }
    application.Get().Event.Emit("searchProgress", SearchProgressEvent{
        FileID: fileID, TabID: tabID, Scanned: matchedTotal(matched), Total: total, Done: true,
    })
    pageNos, _ := s.engine.Page(tabID, 1, 100)
    rows, err := s.toParsedLines(sess, pageNos)
    if err != nil {
        return SearchResult{}, err
    }
    return SearchResult{TabID: tabID, Total: int64(len(matched)), Rows: rows}, nil
}

func (s *LogService) GetPage(fileID, tabID string, page int) (PageResult, error) {
    sess := s.get(fileID)
    if sess == nil {
        return PageResult{}, fmt.Errorf("file not found")
    }
    pageNos, err := s.engine.Page(tabID, page, 100)
    if err != nil {
        return PageResult{}, err
    }
    rows, err := s.toParsedLines(sess, pageNos)
    if err != nil {
        return PageResult{}, err
    }
    return PageResult{Total: int64(s.engine.Total(tabID)), Rows: rows}, nil
}

func (s *LogService) CancelSearch(fileID, tabID string) error {
    s.engine.Cancel(tabID)
    return nil
}
```

### 4. 行号 → ParsedLine（展示用）

```go
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
```

## main.go 接线

1. **删除** `greetservice.go`（模板示例）。
2. **删除** `init()` 里的 `application.RegisterEvent[string]("time")` 与 `main` 里的 `time` 事件 goroutine。
3. 注册事件与 Service：

```go
func init() {
    application.RegisterEvent[service.IndexProgressEvent]("indexProgress")
    application.RegisterEvent[service.SearchProgressEvent]("searchProgress")
}

func main() {
    app := application.New(application.Options{
        Name:        "logviewer",
        Description: "结构化日志查看器",
        Services: []application.Service{
            application.NewService(service.NewLogService()),
        },
        Assets: application.AssetOptions{
            Handler: application.AssetFileServerFS(assets),
        },
        Mac: application.MacOptions{
            ApplicationShouldTerminateAfterLastWindowClosed: true,
        },
    })
    // ...窗口配置保持不变...
    err := app.Run()
    if err != nil {
        log.Fatal(err)
    }
}
```

## 涉及文件

| 文件 | 操作 |
|---|---|
| `internal/service/logservice.go` | 新建：DTO、LogService 及其方法 |
| `main.go` | 修改：注册 Service 与事件，删模板示例 |
| `greetservice.go` | 删除 |

## 验收标准（DoD）

- [ ] `wails3 generate bindings` 后，`frontend/bindings/logviewer` 中出现 `LogService`、`FileInfo`、`ParsedLine`、`Query`、`SearchResult` 等类型与方法
- [ ] `go build ./...` 通过
- [ ] 从 Go 侧写一个临时 main 测试：`OpenFile` → 等 ready → `GetLines`/`Search`/`GetPage` 返回正确

## 风险与注意

1. **bindings 是生成物**：改 DTO/方法签名后必须重跑 `wails3 generate bindings`，不要手改 `frontend/bindings/`。
2. **`application.Get()`**：Service 方法内用 `application.Get()` 拿 app 来 emit 事件 / 开对话框；需确认在 app 运行期间调用。
3. **事件载荷类型**：`RegisterEvent[T]` 的 T 必须与 `Emit` 的载荷类型一致，且是导出类型（生成器据此生成前端类型）。
4. **进度衔接**：`logfile.ProgressFunc`（字节）与 `IndexProgressEvent.Percent`（0~100）之间的换算在 Service 层完成；字段收集与索引同一次扫描完成（`onLine` 回调），无需二次扫描。
5. **并发**：`sessions`/`fields` 用 `mu` 保护；`Search` 内部已由 engine 管理并发与取消。
