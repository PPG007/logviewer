# M1 · 后端：文件会话 + 行偏移索引

- **状态**：未开始
- **依赖**：M0
- **关联文档**：[功能设计](../functional-design.md) §4.1、§5.5、§9、§11 决策 1/3

## 目标

实现 `internal/logfile` 包：打开文件后在后台建立**行偏移索引**（`[]int64`），支持按行号随机读原始行。这是整个应用的地基——检索和翻页都依赖它。

## 核心设计

- 文件**原始内容不驻留内存**，只保留每行起始字节偏移（8 字节/行，125 万行 ≈ 10MB）。
- 索引建立 = 一次顺序扫描（后台 goroutine），完成后 `status → ready`。
- 展示/翻页用 `ReadAt` 按偏移随机读指定行。
- 检索（M3）单独做顺序扫描，不走逐行 `ReadAt`。

## 目标 API

```go
package logfile

type Status int

const (
    StatusIndexing Status = iota
    StatusReady
    StatusError
)

// ProgressFunc 进度回调，done/total 单位为行。
type ProgressFunc func(done, total int64)

type FileSession struct {
    ID   string // 会话 id（内部生成）
    Path string
    Name string // 文件名（path.Base）

    f          *os.File
    totalLines int64
    offsets    []int64
    status     Status
    err        error
    mu         sync.RWMutex
}

// Open 打开文件并启动后台索引。立即返回（status=indexing）。
// onLine：每读一行回调一次（可传 nil）。调用方用它在这同一次扫描里「顺带」收集字段（见 M4）。
func Open(path string, onProgress ProgressFunc, onLine func(lineNo int64, raw string)) (*FileSession, error)

func (s *FileSession) ID() string
func (s *FileSession) TotalLines() int64
func (s *FileSession) Status() Status
func (s *FileSession) Error() error
func (s *FileSession) WaitReady() error // 阻塞直到 ready 或 error

// ReadLines 按行号（0-based）读取 start..start+count-1 行，返回去除行尾 \r\n 的内容。
func (s *FileSession) ReadLines(start, count int64) ([]string, error)

// Scan 顺序遍历全部行（供检索用，比逐行 ReadAt 快）。fn 返回 error 时中止。
func (s *FileSession) Scan(fn func(lineNo int64, raw string) error) error

func (s *FileSession) Close() error
```

## 详细实现

### 1. 索引构建（`index.go`）

```go
func buildIndex(f *os.File, onProgress ProgressFunc, onLine func(lineNo int64, raw string)) (offsets []int64, err error) {
    br := bufio.NewReaderSize(f, 1<<20) // 1MB 读缓冲
    fi, _ := f.Stat()
    totalBytes := fi.Size()
    var offset int64
    var lines int64
    for {
        line, err := br.ReadBytes('\n')
        if len(line) > 0 {
            offsets = append(offsets, offset)
            raw := trimLineEnd(string(line))
            if onLine != nil {
                onLine(lines, raw) // 顺带回调（字段收集等），单遍扫描
            }
            offset += int64(len(line))
            lines++
            if onProgress != nil && lines%100_000 == 0 {
                onProgress(offset, totalBytes) // done/total 单位：字节
            }
        }
        if err != nil {
            break // io.EOF 或真实错误
        }
    }
    if onProgress != nil {
        onProgress(offset, totalBytes) // 收尾，保证最后一次上报
    }
    return offsets, nil
}
```

> 补充：为了让进度有「总行数/百分比」，可在扫描前用 `f.Stat()` 拿文件字节大小作为 total 估算，或用「已读字节/文件字节」直接算百分比（推荐后者，最准）。此处进度细节可在实现时调整，接口 `ProgressFunc(done, total int64)` 保持不变。

### 2. 随机读（`reader.go`）

```go
func (s *FileSession) ReadLines(start, count int64) ([]string, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()
    if s.status != StatusReady {
        return nil, fmt.Errorf("session not ready: %v", s.status)
    }
    if start < 0 || start >= s.totalLines {
        return nil, nil
    }
    if start+count > s.totalLines {
        count = s.totalLines - start
    }
    out := make([]string, 0, count)
    buf := make([]byte, 0, 4096)
    for i := int64(0); i < count; i++ {
        lineNo := start + i
        off := s.offsets[lineNo]
        var length int64
        if lineNo+1 < int64(len(s.offsets)) {
            length = s.offsets[lineNo+1] - off
        } else {
            fi, _ := s.f.Stat()
            length = fi.Size() - off
        }
        if int64(cap(buf)) < length {
            buf = make([]byte, length)
        }
        b := buf[:length]
        if _, err := s.f.ReadAt(b, off); err != nil && err != io.EOF {
            return nil, err
        }
        out = append(out, trimLineEnd(string(b)))
    }
    return out, nil
}

// trimLineEnd 去掉行尾的 \n 和 \r（兼容 Windows CRLF）。
func trimLineEnd(s string) string {
    s = strings.TrimSuffix(s, "\n")
    s = strings.TrimSuffix(s, "\r")
    return s
}
```

### 3. 顺序扫描（供检索，`reader.go`）

```go
func (s *FileSession) Scan(fn func(lineNo int64, raw string) error) error {
    s.mu.RLock()
    defer s.mu.RUnlock()
    if s.status != StatusReady {
        return fmt.Errorf("session not ready")
    }
    // 注意：不能用共享的 bufio 读取位置，需从文件头重新开始
    if _, err := s.f.Seek(0, io.SeekStart); err != nil {
        return err
    }
    br := bufio.NewReaderSize(s.f, 1<<20)
    var lineNo int64
    for {
        line, err := br.ReadBytes('\n')
        if len(line) > 0 {
            if err := fn(lineNo, trimLineEnd(string(line))); err != nil {
                return err
            }
            lineNo++
        }
        if err != nil {
            if err == io.EOF {
                return nil
            }
            return err
        }
    }
}
```

### 4. 打开与后台索引（`index.go`）

```go
func Open(path string, onProgress ProgressFunc, onLine func(lineNo int64, raw string)) (*FileSession, error) {
    f, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    s := &FileSession{
        ID:     newID(),          // crypto/rand 或自增计数生成
        Path:   path,
        Name:   filepath.Base(path),
        f:      f,
        status: StatusIndexing,
    }
    go func() {
        offsets, err := buildIndex(f, onProgress, onLine)
        s.mu.Lock()
        if err != nil {
            s.status = StatusError
            s.err = err
        } else {
            s.offsets = offsets
            s.totalLines = int64(len(offsets))
            s.status = StatusReady
        }
        s.mu.Unlock()
        if onProgress != nil {
            onProgress(s.totalLines, s.totalLines) // 完成
        }
    }()
    return s, nil
}
```

## 涉及文件

| 文件 | 操作 |
|---|---|
| `internal/logfile/index.go` | 新建：Session 结构、Open、buildIndex、状态 |
| `internal/logfile/reader.go` | 新建：ReadLines、Scan、trimLineEnd |
| `internal/logfile/index_test.go` | 新建：单测 |
| `go.mod` | 无新增依赖（仅标准库） |

## 单元测试（必做）

用 `t.TempDir()` 写临时文件，覆盖：

1. 常规多行、末尾无换行符
2. 末尾有换行符（应不产生「幽灵空行」）
3. `\r\n`（Windows 换行）
4. 含空行
5. 单行长于 1MB 读缓冲（`ReadBytes` 会扩容，应正确处理）
6. `ReadLines` 越界（start ≥ totalLines 返回空、start+count 截断）

断言：`TotalLines()` 正确；逐行 `ReadLines(lineNo, 1)` 结果与写入的原始内容（去行尾）一致。

## 验收标准（DoD）

- [ ] `go test ./internal/logfile/` 通过
- [ ] `Open` 后 `WaitReady()` 返回 nil，`Status()==StatusReady`
- [ ] 索引内存 = 8 字节/行（`len(offsets)` 与行数一致）
- [ ] 500MB 合成文件索引耗时 < 5s（可在 M8 正式压测，此处先小文件验证正确性）

## 风险与注意

1. **行结束符**：`\r\n` 若处理不当，`ReadAt` 偏移会错位一行。`trimLineEnd` 必须同时去 `\n` 与 `\r`。
2. **并发**：`Offsets` 在索引期间被后台写、可能被读，务必用 `mu` 保护；`ReadLines`/`Scan` 要求 `status==ready` 才允许访问。
3. **`Scan` 的 `Seek`**：`*os.File` 共享同一读指针，`Scan` 与 `ReadAt` 混用时要小心——`ReadAt` 不受文件偏移影响（原子定位），但 `Scan` 用 `Read` 会移动偏移，故 `Scan` 前需 `Seek(0)`。
4. **进度 total 未知问题**：`ProgressFunc` 的 total 用「已读字节/文件总字节」来报百分比最可靠（见实现 1 的补充说明）。
