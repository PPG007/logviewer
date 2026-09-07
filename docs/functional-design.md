# 结构化日志查看器 — 功能设计文档

> 版本：v1.0
> 状态：已确认（2026-09-05）

## 1. 项目概述

一款基于 **Wails v3** 的桌面端结构化日志（JSONL）查看与检索工具。每个日志文件的每一行是一条 JSON 字符串，软件负责打开、解析、分页展示，并支持单文件内多条件并行检索。

- **定位**：本地轻量版的日志查看器（对标 Grafana Loki / Kibana 的本地替代）
- **核心价值**：打开即用、检索快、多条件多 tab 并行浏览、大文件（500MB 级）流畅

## 2. 目标与非目标

### 2.1 目标（v1）

1. 打开本地 JSONL 日志文件并解析
2. 分页展示（每页 ≤ 100 条）
3. 左侧多文件管理（每个文件一个菜单项）
4. 单文件内多 tab 检索（字段过滤 + 关键字 + 时间范围）
5. 500MB 级文件可用（索引/检索/翻页均流畅）

### 2.2 非目标（v1 不做，见 §12 未来扩展）

- tail 实时跟随
- 多文件按时间合并视图
- 远端日志采集 / 服务端
- 正则检索（推迟到 v1.1）
- 统计聚合面板、查询收藏/保存

## 3. 技术栈

| 层 | 选型 | 说明 |
|---|---|---|
| 桌面壳 | Wails v3（v3.0.0-beta.3） | Go 处理后端，WebView 渲染前端 |
| 后端语言 | Go 1.26 | 文件 I/O、索引、检索引擎 |
| JSON 解析 | `jsoniter` | 整行反序列化；`gjson` 备用做按字段取值 |
| 前端框架 | React 18 + TypeScript + Vite 8 | 模板默认 |
| UI 组件库 | Ant Design（`antd`） | 表格、Tabs、表单、DatePicker 开箱即用 |
| 状态管理 | zustand | 轻量，管理多文件多 tab 状态 |
| 文件读取 | `os.File` + `bufio`（索引）/ `ReadAt`（随机读） | v1.1 可切 mmap（见 §11） |

## 4. 核心概念与数据模型

### 4.1 后端结构体（Go）

```go
type SessionStatus int
const (
    StatusIndexing SessionStatus = iota // 正在建索引
    StatusReady                         // 可浏览/检索
    StatusError                         // 打开失败
)

type FileSession struct {
    ID         string        // 会话 id（前端用它引用文件）
    Path       string        // 绝对路径
    Name       string        // 展示名（文件名）
    TotalLines int64         // 总行数
    Offsets    []int64       // 每行起始字节偏移，len == TotalLines
    Fields     []FieldInfo   // 探测到的顶层字段
    Status     SessionStatus
}

type FieldInfo struct {
    Name string // 字段名
    Type string // "string" | "number" | "bool" | "object" | "array" | "unknown"
}

type Condition struct {
    Field string // 字段名
    Op    string // 见 §6 运算符表
    Value string // 比较值（字符串形式，数值在匹配时转换）
}

type TimeRange struct {
    From *int64 // unix 毫秒，含
    To   *int64 // unix 毫秒，含；nil 表示无上/下界
}

type Query struct {
    Conditions []Condition // 条件之间 AND
    Keyword    string      // 全文子串（与 conditions AND）
    TimeRange  *TimeRange
}

type ParsedLine struct {
    LineNo    int64
    Raw       string         // 原始行文本
    JSON      map[string]any // 解析结果；非法 JSON 时为 nil
    Valid     bool           // 是否为合法 JSON
    Timestamp *int64         // 识别出的时间（unix 毫秒）
    Level     string         // 识别出的级别（见 §5.3），无则空
}

type SearchResult struct {
    TabID string
    Total int64          // 命中总数
    Rows  []ParsedLine   // 当前页
}
```

### 4.2 前端状态（zustand，示意）

```ts
// 每个「文件」一份，挂在 fileId 下
FileView {
  fileId, name, status, indexPercent
  tabs: Tab[]          // 该文件下的检索 tab
  activeTabId
}
Tab {
  tabId, title          // 如 "level=ERROR"、"查询 2"
  query: Query
  page, pageSize(100), total
  rows: ParsedLine[]    // 当前页
  searching: boolean, searchPercent
}
```

## 5. 功能需求

### 5.1 文件管理

- **打开文件**：选择本地 `.log` / `.jsonl` / 任意文本文件（不限制扩展名）。
- **临时日志**：手动输入/粘贴内容创建临时日志，后端写入系统临时目录后走与普通文件一致的解析/索引/检索；上限 **50,000 行 / 10 MiB**（内容经 IPC 全量传输并即时建索引，防超限卡 UI；前端预检同口径，后端为准），展示名「临时日志 HH:mm:ss」，关闭文件时删除临时文件。
- **多文件**：可同时打开多个文件，左侧列表每个文件占一项，可切换、可关闭。
- **索引状态**：打开后立即在后台建索引，列表项显示进度条；建完前不可检索、不可浏览，仅显示进度（见 §11 决策 9）。
- **关闭文件**：释放该文件的索引与所有 tab 结果。

### 5.2 解析

- **JSONL 解析**：逐行 `json.Unmarshal` 到 `map[string]any`。
- **坏行处理**：非法 JSON 不视为错误，`Valid=false`，展示原始文本并加标记。
- **时间识别**：按优先级探测时间字段（`time` / `timestamp` / `ts` / `@timestamp` / `datetime` / `date`），解析多格式（RFC3339、epoch 秒/毫秒、常见自定义 layout）。识别失败则该行无时间，时间范围过滤时跳过。
- **级别识别**：探测 `level` / `severity` / `lvl` / `log_level`，用于着色与默认列。
- **消息字段识别**：探测 `msg` / `message` / `log` / `content`，作为默认「消息」列。
- **`@` 前缀归一**：上述角色字段名若以 `@` 开头（Elasticsearch/Logstash 风格，如 `@timestamp` / `@level` / `@message`），识别时自动剥离 `@` 后按候选名匹配，无需逐一枚举；同名基础字段与 `@` 变体并存时基础字段优先。
- **字段探测**：收集所有顶层字段名 + 类型，供检索条件下拉选择（采样策略见 §11 决策 5）。

### 5.3 展示

- **分页表格**：每页 ≤ 100 条；显示总行数/总命中数、页码跳转。
- **默认列**：时间 | 级别 | 消息；未识别到消息/级别时回退为「原始行」+ JSON 展开。
- **JSON 展开**：每行可展开查看完整 JSON（Drawer 或 expandable row），键值着色。
- **级别着色**：ERROR/WARN/INFO/DEBUG 等用不同颜色标识（v1.1 或 M8 实现）。
- **复制行**：复制原始行或 JSON（M8）。

### 5.4 检索

- **多 tab**：每个文件可新建多个检索 tab，各自持有独立查询 + 独立分页 + 独立结果，互不影响；共享同一份行索引。
- **查询组成**（三者 AND）：
  1. **字段条件**：字段 + 运算符 + 值，可多条（AND）
  2. **关键字**：对整行原始文本做子串匹配
  3. **时间范围**：基于识别出的时间字段
- **枚举取值下拉**：索引顺带收集各 string 字段的 distinct 取值（每字段上限 200 个，超出即截断不提供），字段条件值为枚举字段时下拉点选防误输；截断 / 非 string 字段回落自由输入。取值原样保留（不做 level 大小写归一），与条件匹配口径一致。
- **执行**：提交后后台顺序扫描，命中行号缓存到该 tab；支持取消、进度显示。
- **翻页**：命中结果分页展示，翻页为内存跳转（瞬时）。

### 5.5 索引与进度

- 打开文件即开始一次顺序扫描，记录每行起始字节偏移，生成 `Offsets`。
- 索引进度、检索进度通过事件推送给前端（见 §8.2）。

## 6. 查询 DSL 规范

查询以 JSON 对象表示（前端 UI 生成，后端反序列化）：

```json
{
  "conditions": [
    { "field": "level",    "op": "=",        "value": "ERROR" },
    { "field": "status",   "op": "!=",       "value": "200"   },
    { "field": "latency",  "op": ">",        "value": "1000"  },
    { "field": "msg",      "op": "contains", "value": "timeout" }
  ],
  "keyword": "trace_id=abc123",
  "timeRange": { "from": 1720000000000, "to": 1720003600000 }
}
```

### 运算符表

| op | 语义 | value 类型 |
|---|---|---|
| `=` | 字段值字符串相等 | string |
| `!=` | 不等于 | string |
| `contains` | 字段值包含子串 | string |
| `>` | 数值大于（字段为 number 时比较数值） | number |
| `<` | 数值小于 | number |
| `regex` | 正则匹配（**v1.1**） | string |

### 匹配语义（明确定义，避免歧义）

- 多个 `conditions` 之间是 **AND**；`keyword`、`timeRange` 与 `conditions` 之间也是 **AND**。
- 字段**不存在**时：`=` / `contains` / `>` / `<` 均**不匹配**；`!=` **视为匹配**。
- 字段值为 number，但 `=` / `!=` / `contains` 语义下先转字符串再比较（`>`/`<` 走数值比较）。
- `keyword` 匹配范围：整行原始文本（含字段名与值，简单粗暴，够用）。
- `timeRange`：行无时间戳 → 不匹配；`from`/`to` 为 nil 表示该侧无界。
- 空 `Query`（无条件、无关键字、无时间范围）= 匹配所有行（等价于直接浏览）。

## 7. UI / 交互设计

```
┌────────────┬──────────────────────────────────────────────────┐
│ 文件列表    │  [ fileA.log ] [ fileB.log ]          ← 文件级 Tabs │
│  ▸ fileA    │  ┌────────────────────────────────────────────┐ │
│  ▸ fileB    │  │ [全部] [level=ERROR] [查询3] [+] ← 检索级 Tabs│ │
│             │  │ 字段▾ 运算符▾ 值    [＋条件]   关键字   时间范围 │ │
│  [＋打开]    │  ├────────────────────────────────────────────┤ │
│             │  │ 时间      │级别  │消息          (JSON 展开)   │ │
│             │  │ 10:00:01  │ERROR │...           (分页 ≤100)  │ │
│             │  │ 10:00:02  │INFO  │...                        │ │
│             │  └────────────────────────────────────────────┘ │
│             │          [◀ 1/5123 ▶]  共 512,345 条           │
└────────────┴──────────────────────────────────────────────────┘
```

- **左侧文件列表**：每个已打开文件一项（图标 + 文件名 + 索引进度/状态）；底部「打开」按钮。
- **文件级 Tabs**：切换当前查看的文件。
- **检索级 Tabs**：每个文件内默认一个「全部」tab（无条件浏览），可 `＋` 新建检索 tab。
- **查询构建区**：字段下拉（来自探测结果）+ 运算符下拉 + 值输入，可增删多条条件；另有关键字输入框、时间范围选择器。
- **结果表格**：antd `Table`，分页控件在底部，显示总数与页码跳转。

### 交互细节

- 切换/关闭检索 tab 时，取消该 tab 进行中的扫描（后端 goroutine 支持 context 取消）。
- 索引进行中时，检索入口置灰并提示「索引中 x%」。
- 打开超大文件时，列表项即时出现进度，避免界面无响应。

## 8. Binding API（Go ↔ 前端）

### 8.1 Service 方法

```go
type LogService struct{}

// 打开文件：触发后台索引，立即返回会话信息（含 id 与初始状态）
OpenFile(path string) (FileInfo, error)
// 创建临时日志：内容写入系统临时目录后与 OpenFile 同一解析/索引链路；上限 50,000 行 / 10 MiB
OpenTempLog(content string) (FileInfo, error)
// 关闭文件，释放资源（临时日志一并删除其临时文件）
CloseFile(fileId string) error
// 查询索引进度
GetIndexStatus(fileId string) (IndexStatus, error)
// 获取探测到的字段（供检索条件下拉）
GetFields(fileId string) ([]FieldInfo, error)
// 字段取值枚举（仅 string 字段；Truncated=true 已截断，Values 为空）
GetFieldValues(fileId string, field string) (FieldValues, error)
// 浏览：按行号取一页原始行（无检索条件时用）
GetLines(fileId string, start int64, count int) (PageResult, error)
// 检索：提交查询（tabId 由前端生成），返回第一页；扫描期间 emit searchProgress
Search(fileId string, tabId string, query Query) (SearchResult, error)
// 翻页：从缓存命中行号取第 page 页
GetPage(fileId string, tabId string, page int) (PageResult, error)
// 取消进行中的检索
CancelSearch(fileId string, tabId string) error
```

其中 `PageResult = { total int64; rows []ParsedLine }`。

### 8.2 事件（后端 → 前端）

| 事件 | 载荷 | 说明 |
|---|---|---|
| `indexProgress` | `{ fileId, percent, done }` | 索引进度 |
| `searchProgress` | `{ fileId, tabId, scanned, total, done }` | 检索扫描进度 |

（沿用模板中 `application.RegisterEvent[T]` + `app.Event.Emit` 的机制。）

## 9. 性能与非功能需求

| 指标 | 目标 | 备注 |
|---|---|---|
| 目标文件规模 | 500MB / ≈125 万行 | 平均 400 字节/行 |
| 索引耗时 | < 5s（SSD，冷） | 一次顺序扫描 |
| 索引内存 | ≈ 10MB（`[]int64`） | 8 字节/行 |
| 检索扫描 | 冷 < 3s；热（页缓存）< 100ms | v1.1 引入 mmap 提热 |
| 翻页 | < 50ms | 命中行号内存跳转 |
| 峰值内存 | < 200MB | 多文件 + 多 tab 命中缓存 |
| 分页大小 | 100 条/页 | 固定上限 |

## 10. 错误处理与边界情况

| 情况 | 处理 |
|---|---|
| 非法 JSON 行 | `Valid=false`，展示原始文本并标记，不影响整体 |
| 空行 | 计入行号，按非法 JSON 处理 |
| 编码非 UTF-8 | 假设 UTF-8；解析失败降级为原始文本展示 |
| 文件无读取权限 | 打开时报错，会话进入 `StatusError` |
| 打开期间文件被外部修改 | v1 采用「打开时刻快照」语义（索引基于打开时的内容），不自动重载；v1.1 提供手动「重新加载」 |
| 检索中关闭文件/切换 tab | context 取消，结果丢弃 |

## 11. 关键设计决策

> 已确认（2026-09-05）。

1. **文件内容不驻留内存**：仅保留 `[]int64` 行偏移索引 + 各 tab 命中行号，原始内容留在磁盘，展示时 `ReadAt` 随机读。✅ 已定
2. **查询在 Go 侧执行**（顺序扫描 + 命中缓存），前端只渲染当前页。✅ 已定
3. **mmap 优化**：v1 先用「每次检索重新顺序扫描」实现，读文件抽象为 `LineReader` 接口；v1.1 无缝替换为 mmap（跨平台库 `github.com/edsrzf/mmap-go`），借助 OS 页缓存把热检索降到几十毫秒。✅ 已定（作为 v1.1 项）
4. **打开文件方式** ✅ 已确认：以「系统文件选择对话框」为主（`application.Get().Dialog.OpenFile()`），拖拽、命令行参数后续再加。
5. **时间字段识别** ✅ 已确认：「自动探测 + 手动覆盖」。
6. **字段探测采样** ✅ 已确认：「建索引时顺带全量收集字段名」（复用那次扫描，字段最全）。
7. **查询 DSL 首版运算符** ✅ 已确认：`= / != / contains / > / <` + 关键字 + 时间范围；`regex` 推迟到 v1.1。
8. **进度显示** ✅ 已确认：索引、检索均做实时进度条。
9. **索引期间是否可浏览已索引部分** ✅ 已确认：「不可浏览，仅显示进度」。

## 12. 未来扩展（v1.1+）

- mmap + OS 页缓存（热检索提速）
- `regex` 运算符
- tail 实时跟随
- 多文件按时间合并视图
- 命中结果导出（CSV/JSON）
- 查询收藏/保存、级别统计面板
- 手动「重新加载」文件
