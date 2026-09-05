# 结构化日志查看器 — 实施步骤

> 配套文档：[功能设计](./functional-design.md)
> 本文档按**可验证的里程碑**拆分，每个里程碑有明确产出与验收标准。实施时按 M0 → M8 顺序推进。

## 0. 里程碑总览

| 里程碑 | 主题 | 产出（可验证） |
|---|---|---|
| M0 | 环境就绪 | 能启动的空白 Wails 应用 |
| M1 | 后端：文件会话 + 行偏移索引 | 单测验证索引偏移与随机读一致 |
| M2 | 后端：行解析 + 字段探测 | 单测覆盖典型/坏行 |
| M3 | 后端：检索引擎 | 单测覆盖运算符与取消 |
| M4 | 后端：Service 绑定层 | bindings 可见 `LogService` |
| M5 | 前端：布局 + 状态 | 静态布局可交互（空状态） |
| M6 | 前端：打开文件 + 分页展示 | 打开真实文件可浏览、翻页 |
| M7 | 前端：检索 tab | 多 tab 并存、独立翻页 |
| M8 | 打磨 + 压测 | 500MB 压测达标 |

> 每个里程碑的详细实现（目标、依赖、代码、验收标准、风险）见 [`docs/issues/`](./issues/)（`00`~`08`）。

## 1. 目录结构（目标形态）

```
logviewer/
├── main.go                      # app 入口：注册 LogService、事件、窗口
├── internal/
│   ├── logfile/                 # 文件会话
│   │   ├── index.go             #   []int64 构建、totalLines、状态
│   │   └── reader.go            #   LineReader 接口 + ReadAt 随机读
│   ├── parse/                   # 行解析
│   │   ├── json.go              #   jsoniter 解析
│   │   ├── timestamp.go         #   多格式时间识别
│   │   └── fields.go            #   级别/消息/时间字段探测 + 字段收集
│   ├── search/                  # 检索引擎
│   │   ├── query.go             #   Query/Condition 结构与校验
│   │   ├── matcher.go           #   单行匹配
│   │   └── engine.go            #   顺序扫描 + 命中缓存 + context 取消
│   └── service/                 # Wails Service（binding 层）
│       └── logservice.go        #   方法 + 事件发射
├── frontend/
│   ├── bindings/                # 自动生成，勿手改
│   └── src/
│       ├── layout/              # 左文件列表 + 右 Tabs 骨架
│       ├── table/               # 分页表格 + JSON 展开
│       ├── search/              # 查询构建 UI
│       ├── store/               # zustand store
│       └── types.ts             # 与后端结构对齐的类型
├── docs/                        # 本文档 + 功能设计
└── Taskfile.yml
```

## 2. 里程碑明细

### M0 · 环境就绪

**任务**
- `npm install`
- 安装 `antd`、`@ant-design/icons`（前端依赖）
- 首次 `wails3 dev` 跑通模板

**验收**：应用窗口能启动，模板页面正常显示，Go↔前端链路打通。

---

### M1 · 后端：文件会话 + 行偏移索引（`internal/logfile`）

**任务**
- `Open(path) (*FileSession, error)`：顺序扫描建 `Offsets []int64` + `TotalLines`，后台 goroutine，进度回调
- `ReadLine(session, lineNo) ([]byte, error)`：用 `ReadAt` 按偏移随机读单行
- `ReadLines(session, start, count)`：批量读
- `FileSession` 结构 + 状态机（indexing/ready/error）

**关键点**
- 扫描用 `bufio.Reader`，记录每行起始偏移；注意跨 `\r\n` / `\n` 的行结束符
- `LineReader` 接口预留，为 v1.1 mmap 切换做准备

**验收**：单测——临时文件写入 N 行，`Open` 后 `Offsets` 长度 == N，逐行 `ReadLine` 结果与原始顺序一致。

---

### M2 · 后端：行解析 + 字段探测（`internal/parse`）

**任务**
- `ParseLine(raw) (map[string]any, bool)`：jsoniter 解析，非法返回 `false`
- `ExtractTimestamp(json) *int64`：多格式时间识别（RFC3339 / epoch 秒/毫秒 / 自定义 layout）
- `DetectLevel(json) string`、`DetectMessage(json) string`
- `DetectFields(samples []map[string]any) []FieldInfo`：收集顶层字段名 + 类型

**验收**：单测覆盖——合法 JSON、非法 JSON、无时间字段、多种时间格式、字段类型推断。

---

### M3 · 后端：检索引擎（`internal/search`）

**任务**
- `Query`/`Condition` 结构 + 校验（非法运算符/字段报错）
- `Matcher.Match(json map[string]any) bool`：实现 §6 全部运算符与匹配语义
- `Engine.Search(session, query) ([]int64, error)`：goroutine 顺序扫描，命中行号返回
- 命中缓存：`tabId → []int64`；`context` 取消；进度回调

**验收**：单测覆盖——各运算符、AND 组合、字段不存在语义、`!=` 例外、时间范围、取消（提前 cancel 后结果丢弃）。

---

### M4 · 后端：Service 绑定层（`internal/service` + `main.go`）

**任务**
- `LogService` 实现 §8.1 全部方法
- 会话/结果缓存管理（多文件、多 tab 的 `map[fileId]...`、`map[tabId][]int64`）
- 注册事件 `indexProgress`、`searchProgress`
- `main.go` 中 `application.NewService(&LogService{})`，替换模板 `GreetService`

**验收**：`wails3 dev` 后，`frontend/bindings/logviewer` 中能看到 `LogService` 及其方法签名与类型。

---

### M5 · 前端：布局 + 状态（`frontend/src`）

**任务**
- 引入 antd，清掉模板示例 UI
- 布局组件：左「文件列表」+ 右「文件级 Tabs」+ 文件内「检索级 Tabs」
- zustand store：`files[]`、`activeFileId`、每个文件 `tabs[]`、每个 tab 的 `{query, page, pageSize, total, rows, searching}`
- 空状态：未打开文件时的占位

**验收**：`wails3 dev` 下布局可交互（新增 tab、切换 tab、空状态），无后端调用。

---

### M6 · 前端：打开文件 + 分页展示

**任务**
- 「打开文件」按钮 → 系统对话框 → `OpenFile`
- 文件列表项显示文件名 + 索引进度（监听 `indexProgress` 事件）
- 主区 antd `Table` 分页（100/页）：默认列 = 时间/级别/消息，行可展开 JSON（Drawer）
- 监听 `GetLines`/`GetPage` 完成态更新 store

**验收**：打开真实日志文件，浏览/翻页正常，索引进度可见。

---

### M7 · 前端：检索 tab

**任务**
- 「新建检索 tab」→ 查询构建区（字段下拉 + 运算符 + 值；关键字；时间范围）
- 提交 → `Search` → 命中结果分页展示；翻页调 `GetPage`
- 切 tab / 关 tab 时调 `CancelSearch` 取消进行中的扫描
- 检索进度条（监听 `searchProgress`）

**验收**：多 tab 并存、各自独立翻页、切换互不影响，取消生效。

---

### M8 · 打磨 + 压测

**任务**
- 级别着色（ERROR/WARN/INFO/DEBUG）
- 复制原始行 / 复制 JSON
- 命中结果导出（CSV/JSON）
- 用脚本生成 500MB JSONL 压测：索引耗时、检索耗时、翻页延迟、峰值内存，对照 §9 目标

**验收**：500MB 压测指标达标；交互无卡顿。

## 3. 开发工作流

- **开发**：`wails3 dev`（热重载 + 自动重新生成 bindings）
- **改 Service 后**：`wails3 generate bindings` 重新生成前端类型
- **后端单测**：`go test ./internal/...`
- **前端构建**：`npm run build`（`tsc && vite build`）

## 4. 测试策略

- **后端单测**：`logfile`（索引正确性）、`parse`（解析/时间）、`search`（匹配语义/取消）各自覆盖
- **合成日志生成器**：脚本产出带可控字段（时间/级别/消息/随机字段）的 JSONL，用于开发与 500MB 压测
- **手工验收**：每个里程碑的「验收」项

## 5. 风险与注意事项

1. **Wails v3 为 beta**：API 可能随版本变动，锁定当前 `v3.0.0-beta.3`，升级时注意 breaking change。
2. **bindings 是生成物**：不要手改 `frontend/bindings/`，改 Go 结构后重新生成。
3. **行结束符**：Windows `\r\n` 需在索引扫描中正确处理，否则偏移会错位。
4. **大数精度**：JSON 大整数经 Go `map[string]any` 转 `float64` 再序列化到前端，可能丢精度；如日志含 64 位整型 ID，需在解析层特殊处理（v1 可先接受，遇到再议）。
5. **并发安全**：多 tab 并发检索读写共享缓存，需加锁或按 tab 隔离。
