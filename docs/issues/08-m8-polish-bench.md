# M8 · 打磨 + 压测

- **状态**：未开始
- **依赖**：M7
- **关联文档**：[功能设计](../functional-design.md) §9（性能目标）、§10（边界）

## 目标

补齐体验细节（级别着色、复制、导出、JSON 查看器），并用合成 500MB 日志做压测，对照性能目标验收。

## 详细实现

### 1. 级别着色（完整映射）

```ts
const LEVEL_COLOR: Record<string, string> = {
  ERROR: 'red', FATAL: 'magenta', CRITICAL: 'magenta',
  WARN: 'orange', WARNING: 'orange',
  INFO: 'blue', DEBUG: 'default', TRACE: 'default',
}
```

同时给整行加淡色背景（`rowClassName` 按级别），便于扫读。

### 2. 复制

```ts
// 复制原始行 / 复制 JSON（表格操作列或右键菜单）
async function copy(text: string) {
  await navigator.clipboard.writeText(text)
}
```

- 表格加「操作」列：复制原始行、复制 JSON。
- 展开面板内也可放复制按钮。

### 3. JSON 查看器（可选增强）

替换展开面板里的 `<pre>` 为 `@uiw/react-json-view`（键值着色、可折叠）：

```bash
npm install @uiw/react-json-view
```

```tsx
import JsonView from '@uiw/react-json-view'
// expandedRowRender: row.Valid ? <JsonView value={row.JSON} /> : <pre>{row.Raw}</pre>
```

### 4. 导出命中结果（可选后端扩展）

若需导出「全部命中」（而非仅当前页），给 `LogService` 加：

```go
// ExportMatches 把 tabId 的全部命中原始行写入用户选择的目标文件，返回路径。
func (s *LogService) ExportMatches(fileID, tabID string) (string, error)
```

实现：`application.Get().Dialog.SaveFile()` 选路径 → 遍历 `engine` 的命中行号 → `session.ReadLines` 逐段写。v1 可先只做「导出当前页」（纯前端，无后端改动）。

### 5. 压测

#### 5.1 合成日志生成器（`cmd/genlog/main.go` 或临时脚本）

```go
// 生成 500MB 左右 JSONL：每行约 400 字节
func main() {
    f, _ := os.Create("bench_500mb.jsonl")
    w := bufio.NewWriterSize(f, 1<<20)
    var n int
    for {
        line := fmt.Sprintf(`{"time":"%s","level":"%s","msg":"%s","trace_id":"%016x","latency_ms":%d,"service":"%s"}`+"\n",
            time.Now().Add(time.Duration(rand.Intn(86400))*time.Second).Format(time.RFC3339Nano),
            levels[rand.Intn(len(levels))], msgs[rand.Intn(len(msgs))], rand.Uint64(),
            rand.Intn(5000), services[rand.Intn(len(services))])
        w.WriteString(line)
        n += len(line)
        if n >= 500<<20 { break } // 500MB
    }
    w.Flush()
}
```

#### 5.2 指标测量与目标（对照设计 §9）

| 指标 | 目标 | 测量方式 |
|---|---|---|
| 索引耗时 | < 5s | `OpenFile` 到 `indexProgress.Done` 的墙钟时间 |
| 索引内存 | ≈ 10MB | `runtime.ReadMemStats` 前后差，或 offsets 长度×8 估算 |
| 检索（冷扫描） | < 3s | 首次 `Search` 全量匹配耗时 |
| 检索（热） | < 100ms | 二次相同检索（OS 页缓存命中后） |
| 翻页 | < 50ms | `GetPage` 单次耗时 |
| 峰值内存 | < 200MB | 多文件 + 多 tab 命中缓存下 `ReadMemStats` |

> 热检索提速依赖 mmap/OS 页缓存（v1.1，见设计 §11 决策 3）；v1 先验证冷扫 < 3s 达标。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `frontend/src/table/LogTable.tsx` | 着色、复制、JSON 查看器 |
| `cmd/genlog/main.go` | 新建：合成日志生成器 |
| `internal/service/logservice.go` | （可选）`ExportMatches` |

## 验收标准（DoD）

- [ ] 级别着色与整行高亮正确
- [ ] 复制原始行/JSON 可用
- [ ] 500MB 压测指标达到 §9 目标（至少：索引 < 5s、冷检索 < 3s、翻页 < 50ms、内存 < 200MB）
- [ ] 交互无卡顿（滚动/翻页/切换 tab）

## 风险与注意

1. **剪贴板 API**：`navigator.clipboard` 需在安全上下文（Wails WebView 通常满足）；失败时降级 `document.execCommand('copy')`。
2. **压测环境差异**：SSD vs HDD、有无杀毒扫描都会影响 I/O 指标，压测记录环境信息。
3. **热检索**：v1 无 mmap，二次检索仍走磁盘读（但 OS 页缓存已生效），「热 < 100ms」作为 v1.1 目标，v1 记录实测值即可。

## 实测记录（2026-09-05）

环境：Windows 11（22 核 / amd64），Go 1.26.5，SSD；文件 `bench_500mb.jsonl`
= 500.0 MB / 1,351,956 行（avg 388 B/行，genlog 固定种子生成）。

复现：`go run ./cmd/genlog -out bench_500mb.jsonl -size 500` → `go run ./cmd/bench -file bench_500mb.jsonl`

| 指标 | 实测 | 目标（设计 §9） | 判定 |
|---|---|---|---|
| 索引耗时（1 次顺序扫描） | 2016 ms | < 5s | ✅ |
| 索引内存（offsets × 8B） | 10.3 MB（135 万行） | ≈ 10MB | ✅ |
| 关键字冷检索（“order”，91 万命中） | 388 ms | < 3s | ✅ |
| 字段条件冷检索（level=ERROR，13.5 万命中） | 2078 ms | < 3s | ✅ |
| 时间范围冷检索（全行解析最重路径） | 2017 ms | < 3s | ✅ |
| 热检索（同文件二次；关键字/字段条件） | 380 / 2119 ms | < 100ms（v1.1） | ⏳ 留待 v1.1 |
| 检索翻页 GetPage ×10 | 0.53 ms/页 | < 50ms | ✅ |
| 浏览翻页 GetLines ×10 | 0.36 ms/页 | < 50ms | ✅ |
| 峰值内存 HeapAlloc（索引+5 个检索 tab 常驻） | 97.3 MB | < 200MB | ✅ |
| HeapSys 采样峰值 | 123.7 MB | — | 参考 |

结论：全部 v1 目标达标。冷检索逐行解析路径（level=ERROR / 时间范围 ≈ 2.1s）是上界，
纯关键字路径 < 0.4s。热检索因 v1 每次整文件重扫且每次解析，未达 v1.1 的 <100ms，
验证后按设计 §11 决策 3（mmap/页缓存）留待 v1.1。
