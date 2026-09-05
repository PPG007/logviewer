# M2 · 后端：行解析 + 字段探测

- **状态**：未开始
- **依赖**：M0（不依赖 M1，可并行）
- **关联文档**：[功能设计](../functional-design.md) §5.2、§4.1

## 目标

实现 `internal/parse` 包：把单行 JSON 字符串解析为 `map[string]any`，并识别**时间**、**级别**、**消息**字段，收集**字段名与类型**。为展示（列视图）和检索（字段过滤、时间范围）提供能力。

## 目标 API

```go
package parse

// FieldInfo 顶层字段名与推断类型
type FieldInfo struct {
    Name string // "string" | "number" | "bool" | "object" | "array" | "null"
    Type string
}

// ParseLine 解析一行 JSON。合法返回 (map, true)；非法返回 (nil, false)。
func ParseLine(raw string) (map[string]any, bool)

// ExtractTimestamp 识别并解析时间字段，返回 unix 毫秒；无/失败返回 nil。
func ExtractTimestamp(m map[string]any) *int64

// DetectLevel 识别级别字段值（大写归一），无则 ""。
func DetectLevel(m map[string]any) string

// DetectMessage 识别消息字段值（转字符串），无则 ""。
func DetectMessage(m map[string]any) string

// CollectFields 把 m 的顶层字段合并进 acc（name -> type）。
func CollectFields(m map[string]any, acc map[string]string)
```

## 字段名约定（与设计文档 §5.2 一致）

| 角色 | 候选字段名（按优先级） |
|---|---|
| 时间 | `time` `timestamp` `ts` `@timestamp` `datetime` `date` |
| 级别 | `level` `severity` `lvl` `log_level` |
| 消息 | `msg` `message` `log` `content` |

## 详细实现

### 1. 依赖

```bash
go get github.com/json-iterator/go
```

```go
import jsoniter "github.com/json-iterator/go"

var json = jsoniter.ConfigCompatibleWithStandardLibrary
```

### 2. ParseLine

```go
func ParseLine(raw string) (map[string]any, bool) {
    raw = strings.TrimSpace(raw)
    if raw == "" {
        return nil, false
    }
    var m map[string]any
    if err := json.UnmarshalFromString(raw, &m); err != nil {
        return nil, false
    }
    return m, true
}
```

### 3. ExtractTimestamp（多格式）

```go
var timeFieldNames = []string{"time", "timestamp", "ts", "@timestamp", "datetime", "date"}

// 常见文本布局
var timeLayouts = []string{
    time.RFC3339Nano,
    time.RFC3339,
    "2006-01-02 15:04:05.999999999Z07:00",
    "2006-01-02 15:04:05.999999999",
    "2006-01-02T15:04:05.999999999",
    "2006-01-02T15:04:05",
    "2006-01-02 15:04:05",
    "2006/01/02 15:04:05",
}

func ExtractTimestamp(m map[string]any) *int64 {
    for _, name := range timeFieldNames {
        v, ok := m[name]
        if !ok {
            continue
        }
        if ms := toMillis(v); ms != nil {
            return ms
        }
    }
    return nil
}

func toMillis(v any) *int64 {
    var ms int64
    switch x := v.(type) {
    case string:
        // 1) 尝试纯数字（epoch）
        if n, err := strconv.ParseInt(x, 10, 64); err == nil {
            return normalizeEpoch(n)
        }
        // 2) 尝试文本布局
        for _, layout := range timeLayouts {
            if t, err := time.Parse(layout, x); err == nil {
                ms = t.UnixMilli()
                return &ms
            }
        }
        return nil
    case float64:
        return normalizeEpoch(int64(x))
    case int64:
        return normalizeEpoch(x)
    case json.Number: // 若未用 map[string]any 而用 Decoder.UseNumber
        if n, err := x.Int64(); err == nil {
            return normalizeEpoch(n)
        }
    }
    return nil
}

// normalizeEpoch 依据量级判断单位：秒/毫秒/微秒/纳秒。
func normalizeEpoch(n int64) *int64 {
    var ms int64
    switch {
    case n > 1e17: // 纳秒
        ms = n / 1e6
    case n > 1e14: // 微秒
        ms = n / 1e3
    case n > 1e11: // 毫秒
        ms = n
    default: // 秒
        ms = n * 1000
    }
    return &ms
}
```

> 注意：`jsoniter` 默认把数字解析为 `float64`（非整数型字段的值走 `float64` 分支）。大整数精度问题见「风险 3」。

### 4. DetectLevel / DetectMessage

```go
var levelFieldNames = []string{"level", "severity", "lvl", "log_level"}
var msgFieldNames = []string{"msg", "message", "log", "content"}

func DetectLevel(m map[string]any) string {
    for _, name := range levelFieldNames {
        if v, ok := m[name]; ok {
            if s, ok := v.(string); ok {
                return strings.ToUpper(s)
            }
        }
    }
    return ""
}

func DetectMessage(m map[string]any) string {
    for _, name := range msgFieldNames {
        if v, ok := m[name]; ok {
            if s, ok := v.(string); ok {
                return s
            }
            return fmt.Sprint(v) // 非字符串则格式化
        }
    }
    return ""
}
```

### 5. CollectFields

```go
func CollectFields(m map[string]any, acc map[string]string) {
    for k, v := range m {
        acc[k] = typeOf(v)
    }
}

func typeOf(v any) string {
    switch v.(type) {
    case nil:
        return "null"
    case bool:
        return "bool"
    case string:
        return "string"
    case float64, int64, json.Number:
        return "number"
    case map[string]any:
        return "object"
    case []any:
        return "array"
    default:
        return "unknown"
    }
}
```

## 涉及文件

| 文件 | 操作 |
|---|---|
| `internal/parse/json.go` | 新建：ParseLine、jsoniter 实例 |
| `internal/parse/timestamp.go` | 新建：ExtractTimestamp、toMillis、normalizeEpoch |
| `internal/parse/fields.go` | 新建：DetectLevel、DetectMessage、CollectFields、FieldInfo |
| `internal/parse/parse_test.go` | 新建：单测 |
| `go.mod` | 新增 `github.com/json-iterator/go` |

## 单元测试（必做）

1. `ParseLine`：合法 JSON、非法 JSON、空行、带首尾空白、嵌套对象
2. `ExtractTimestamp`：RFC3339、`2006-01-02 15:04:05`、epoch 秒/毫秒/微秒/纳秒、字符串 epoch、无时间字段、时间字段值非法
3. `DetectLevel`：`level`、`severity` 大写归一、无级别
4. `DetectMessage`：`msg`/`message` 字符串、非字符串消息
5. `CollectFields`：各类型推断、多行合并去重

## 验收标准（DoD）

- [ ] `go test ./internal/parse/` 通过
- [ ] 典型生产日志行能正确抽出时间/级别/消息
- [ ] 非法 JSON 返回 `false` 而非 panic

## 风险与注意

1. **`@timestamp` 等特殊字段名**：JSON key 可能是 `"@timestamp"`，`map[string]any` 直接 `m["@timestamp"]` 即可，无需特殊处理。
2. **时区**：无时区的文本时间（如 `2006-01-02 15:04:05`）按 `time.Local` 解析，可能导致时间范围过滤跨时区偏差——实现时记录这个假设，必要时支持用户指定时区（v1.1）。
3. **大整数精度**：`jsoniter` 默认把数字转 `float64`，> 2^53 的整型 ID 会丢精度。v1 先接受（见 implementation-plan §5 风险 4），若日志有 64 位 ID 需求，改用 `jsoniter.Config{UseNumber: true}` 并在类型判断里兼容 `json.Number`。
4. **字段探测只做顶层**：嵌套字段（如 `"http":{"status":200}` 中的 `http.status`）v1 不支持，`CollectFields` 把整个 `http` 记为 `object`。
