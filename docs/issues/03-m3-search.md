# M3 · 后端：检索引擎

- **状态**：未开始
- **依赖**：M1（logfile）、M2（parse）
- **关联文档**：[功能设计](../functional-design.md) §5.4、§6、§11 决策 2/7

## 目标

实现 `internal/search` 包：查询 DSL 定义与校验、单行匹配器、基于顺序扫描的检索引擎（命中行号缓存 + 可取消 + 进度）。

## 核心设计

- 检索 = **顺序扫描**文件（`session.Scan`），逐行解析 + 匹配，命中行号存 `[]int64`（8 字节/命中）。
- 命中结果按 `tabId` 缓存，翻页是内存跳转。
- 支持 `context` 取消（切换/关闭 tab 时中断扫描）。

## 目标 API

```go
package search

type Condition struct {
    Field string
    Op    string // "=" | "!=" | "contains" | ">" | "<"
    Value string
}

type TimeRange struct {
    From *int64 // unix 毫秒，含
    To   *int64 // unix 毫秒，含
}

type Query struct {
    Conditions []Condition // AND
    Keyword    string      // 对原始行做子串匹配
    TimeRange  *TimeRange
}

func (q *Query) Validate() error

type Matcher struct{ q Query }
func NewMatcher(q Query) *Matcher
func (m *Matcher) Match(raw string, parsed map[string]any, ts *int64) bool

type ProgressFunc func(done, total int64) // 单位：行

type Engine struct {
    mu      sync.Mutex
    results map[string][]int64          // tabId -> 命中行号
    cancels map[string]context.CancelFunc
}

func NewEngine() *Engine

// Search 顺序扫描并缓存命中行号。返回命中行号切片（含全部命中）。
func (e *Engine) Search(s *logfile.FileSession, tabID string, q Query, onProgress ProgressFunc) ([]int64, error)
func (e *Engine) Cancel(tabID string)
func (e *Engine) Remove(tabID string)                 // 关闭 tab 时释放结果
func (e *Engine) Page(tabID string, page, pageSize int) ([]int64, error) // page 从 1 开始
func (e *Engine) Total(tabID string) int
```

## 匹配语义（与设计文档 §6 严格一致）

| 规则 | 说明 |
|---|---|
| 组合 | `conditions` 之间 AND；`keyword`、`timeRange` 与 `conditions` 之间 AND |
| 字段不存在 | `=`/`contains`/`>`/`<` → 不匹配；`!=` → **视为匹配** |
| 字符串比较 | `=`/`!=`/`contains` 先把字段值 `fmt.Sprint` 转字符串再比较 |
| 数值比较 | `>`/`<` 把字段值与 `Value` 都 `ParseFloat` 后比较；任一非数值 → 不匹配 |
| 关键字 | 对整行 `raw` 做 `strings.Contains` |
| 时间范围 | `ts == nil`（该行无时间）→ 不匹配；`From`/`To` 为 nil 表示该侧无界 |
| 非法 JSON 行 | `parsed == nil`，字段条件按「字段不存在」处理，仍可命中关键字 |

## 详细实现

### 1. Query 与校验

```go
var allowedOps = map[string]bool{"=": true, "!=": true, "contains": true, ">": true, "<": true}

func (q *Query) Validate() error {
    for _, c := range q.Conditions {
        if c.Field == "" {
            return fmt.Errorf("condition field is empty")
        }
        if !allowedOps[c.Op] {
            return fmt.Errorf("unsupported operator %q", c.Op)
        }
    }
    return nil
}
```

### 2. Matcher

```go
func (m *Matcher) Match(raw string, parsed map[string]any, ts *int64) bool {
    // 1) 时间范围
    if m.q.TimeRange != nil {
        if ts == nil {
            return false
        }
        if m.q.TimeRange.From != nil && *ts < *m.q.TimeRange.From {
            return false
        }
        if m.q.TimeRange.To != nil && *ts > *m.q.TimeRange.To {
            return false
        }
    }
    // 2) 关键字
    if m.q.Keyword != "" && !strings.Contains(raw, m.q.Keyword) {
        return false
    }
    // 3) 字段条件
    for _, c := range m.q.Conditions {
        if !matchCondition(parsed, c) {
            return false
        }
    }
    return true
}

func matchCondition(parsed map[string]any, c Condition) bool {
    v, ok := parsed[c.Field] // parsed 为 nil 时 ok=false
    switch c.Op {
    case "!=":
        if !ok {
            return true
        }
        return fmt.Sprint(v) != c.Value
    case "=":
        if !ok {
            return false
        }
        return fmt.Sprint(v) == c.Value
    case "contains":
        if !ok {
            return false
        }
        return strings.Contains(fmt.Sprint(v), c.Value)
    case ">", "<":
        if !ok {
            return false
        }
        lhs, err1 := strconv.ParseFloat(fmt.Sprint(v), 64)
        rhs, err2 := strconv.ParseFloat(c.Value, 64)
        if err1 != nil || err2 != nil {
            return false
        }
        if c.Op == ">" {
            return lhs > rhs
        }
        return lhs < rhs
    }
    return false
}
```

### 3. Engine

```go
func (e *Engine) Search(s *logfile.FileSession, tabID string, q Query, onProgress ProgressFunc) ([]int64, error) {
    if err := q.Validate(); err != nil {
        return nil, err
    }
    ctx, cancel := context.WithCancel(context.Background())
    e.mu.Lock()
    e.cancels[tabID] = cancel
    e.mu.Unlock()
    defer func() {
        e.mu.Lock()
        delete(e.cancels, tabID)
        e.mu.Unlock()
    }()

    m := NewMatcher(q)
    total := s.TotalLines()
    matched := make([]int64, 0, 1024)

    err := s.Scan(func(lineNo int64, raw string) error {
        // 每 4096 行检查一次取消
        if lineNo&4095 == 0 {
            select {
            case <-ctx.Done():
                return ctx.Err()
            default:
            }
        }
        parsed, ok := parse.ParseLine(raw)
        var ts *int64
        if ok {
            ts = parse.ExtractTimestamp(parsed)
        }
        if m.Match(raw, parsed, ts) {
            matched = append(matched, lineNo)
        }
        if onProgress != nil && lineNo&0xFFFF == 0 { // 每 65536 行上报一次
            onProgress(lineNo, total)
        }
        return nil
    })
    if err != nil {
        return nil, err // 取消时 err == context.Canceled
    }

    e.mu.Lock()
    e.results[tabID] = matched
    e.mu.Unlock()
    if onProgress != nil {
        onProgress(total, total)
    }
    return matched, nil
}

func (e *Engine) Cancel(tabID string) {
    e.mu.Lock()
    if c, ok := e.cancels[tabID]; ok {
        c()
    }
    e.mu.Unlock()
}

func (e *Engine) Remove(tabID string) {
    e.mu.Lock()
    delete(e.results, tabID)
    delete(e.cancels, tabID)
    e.mu.Unlock()
}

func (e *Engine) Page(tabID string, page, pageSize int) ([]int64, error) {
    e.mu.Lock()
    defer e.mu.Unlock()
    matched := e.results[tabID]
    start := (page - 1) * pageSize
    if start >= len(matched) {
        return nil, nil
    }
    end := start + pageSize
    if end > len(matched) {
        end = len(matched)
    }
    return matched[start:end], nil
}

func (e *Engine) Total(tabID string) int {
    e.mu.Lock()
    defer e.mu.Unlock()
    return len(e.results[tabID])
}
```

> 注意：`Engine` 中 `results`/`cancels` 的访问用 `mu` 保护；`matched` 在 `Search` 内是局部切片，最后一次性存入，避免了边扫边写锁。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `internal/search/query.go` | 新建：Condition/TimeRange/Query/Validate |
| `internal/search/matcher.go` | 新建：Matcher、matchCondition |
| `internal/search/engine.go` | 新建：Engine 及方法 |
| `internal/search/search_test.go` | 新建：单测 |

## 单元测试（必做）

1. `Validate`：非法运算符、空字段名报错；合法通过
2. `Match`：`=`/`!=`/`contains`/`>`/`<` 各自；字段不存在 + `!=` 例外；数值比较非法值；关键字；时间范围（含边界、`ts=nil`）；空 Query 全匹配
3. `Engine.Search`：用临时文件构造数据，验证命中行号正确、`Page`/`Total` 正确、`Cancel` 后返回 `context.Canceled` 且无缓存
4. 非法 JSON 行 + 关键字：关键字仍能命中非法行

## 验收标准（DoD）

- [ ] `go test ./internal/search/` 通过
- [ ] 运算符与匹配语义与设计文档 §6 完全一致
- [ ] 取消语义正确（取消后不残留 `cancels`/`results`）

## 风险与注意

1. **取消粒度**：扫描循环必须周期性检查 `ctx.Done()`（上例每 4096 行），否则取消响应慢。
2. **`Session.Scan` 会 `Seek(0)`**：一次只有一个检索在跑是安全的；若要并发检索同一文件，需注意共享文件读指针（v1 不做并发检索，靠 `Cancel` + 顺序提交）。
3. **命中全量时内存**：最坏命中 == 总行数，125 万行 × 8B ≈ 10MB，可接受。
4. **字段缺失 vs 空值**：`parsed[field]` 返回的 `ok` 只表示 key 是否存在；若 key 存在但值为 `null`，`fmt.Sprint(nil)` 是 `"<nil>"`，语义上按存在处理（`=` `"<nil>"` 才会命中），实现时注意与预期一致并加测试。
