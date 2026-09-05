# M7 · 前端：检索 tab

- **状态**：未开始
- **依赖**：M6（打开/展示）、M4（后端 Service）
- **关联文档**：[功能设计](../functional-design.md) §5.4（检索）、§6（DSL）、§7（交互）

## 目标

实现单文件内**多检索 tab**：查询构建 UI（字段+运算符+值、关键字、时间范围）、提交检索、命中结果分页、切 tab / 关 tab 时取消扫描、检索进度条。

## 详细实现

### 1. 新建检索 tab（`MainPane.tsx` 的 `+` 入口）

```ts
function newSearchTab(): Tab {
  return {
    tabId: crypto.randomUUID(),
    title: '查询',          // 提交后按条件回填，如 "level=ERROR"
    mode: 'search',
    query: { Conditions: [], Keyword: '', TimeRange: null },
    page: 1, pageSize: 100, total: 0,
    rows: [], loading: false, searching: false, searchPercent: 0,
  }
}
```

> `tabId` 由前端生成（`crypto.randomUUID()`），作为 `Search`/`GetPage`/`CancelSearch` 的参数，后端据此缓存命中行号。

### 2. 查询构建（`search/QueryBuilder.tsx`）

```tsx
import { Button, DatePicker, Input, Select, Space } from 'antd'
import { PlusOutlined, MinusCircleOutlined, SearchOutlined } from '@ant-design/icons'

const OP_OPTIONS = [
  { value: '=', label: '=' },
  { value: '!=', label: '≠' },
  { value: 'contains', label: '包含' },
  { value: '>', label: '>' },
  { value: '<', label: '<' },
]

export default function QueryBuilder({ fields, query, onChange, onSearch }: Props) {
  return (
    <Space direction="vertical" style={{ width: '100%' }}>
      {/* 字段条件（可多条，AND） */}
      {query.Conditions.map((c, i) => (
        <Space key={i}>
          <Select
            style={{ width: 180 }} placeholder="字段"
            value={c.Field} onChange={(v) => updateCond(i, { Field: v })}
            options={fields.map(f => ({ value: f.Name, label: f.Name }))}
            showSearch
          />
          <Select style={{ width: 90 }} value={c.Op}
            onChange={(v) => updateCond(i, { Op: v })}
            options={OP_OPTIONS} />
          <Input style={{ width: 220 }} placeholder="值"
            value={c.Value} onChange={(e) => updateCond(i, { Value: e.target.value })} />
          <Button icon={<MinusCircleOutlined />} onClick={() => removeCond(i)} />
        </Space>
      ))}
      <Space>
        <Button icon={<PlusOutlined />} onClick={addCond}>条件</Button>
        <Input style={{ width: 220 }} placeholder="关键字（整行匹配）"
          value={query.Keyword}
          onChange={(e) => onChange({ ...query, Keyword: e.target.value })} />
        <DatePicker.RangePicker showTime
          onChange={(range) => onChange({ ...query, TimeRange: rangeToMs(range) })} />
        <Button type="primary" icon={<SearchOutlined />} onClick={onSearch}>检索</Button>
      </Space>
    </Space>
  )
}

// RangePicker 的 dayjs 值 → { From, To } unix 毫秒
function rangeToMs(range: any): TimeRange | null {
  if (!range || !range[0] || !range[1]) return null
  return { From: range[0].valueOf(), To: range[1].valueOf() }
}
```

> 字段下拉 `options` 来自 `GetFields`（M6 已拉取到 store 的 `fields`）。用户也可手动输入任意字段名（`showSearch` 支持自由输入）。

### 3. 提交检索

```ts
async function runSearch(fileId: string, tabId: string, query: Query) {
  setTab(fileId, tabId, { searching: true, searchPercent: 0, page: 1 })
  try {
    const res = await LogService.Search(fileId, tabId, query)
    setTab(fileId, tabId, {
      rows: res.Rows, total: res.Total, searching: false, searchPercent: 100,
      title: buildTitle(query), // 如 "level=ERROR" / "关键字: x" / "查询 2"
    })
  } catch (e) {
    // 取消导致的错误静默处理（切 tab / 关 tab 会触发）
    setTab(fileId, tabId, { searching: false })
  }
}
```

### 4. 检索翻页

搜索 tab 的翻页走 `GetPage`（命中行号内存跳转），与浏览 tab 的 `GetLines` 区分：

```ts
// LogTable 里根据 tab.mode 选择分页 handler
const onChange = (page: number) =>
  tab.mode === 'browse'
    ? loadBrowsePage(fileId, tab.tabId, page)   // GetLines
    : loadSearchPage(fileId, tab.tabId, page)   // GetPage

async function loadSearchPage(fileId: string, tabId: string, page: number) {
  const res = await LogService.GetPage(fileId, tabId, page)
  setTab(fileId, tabId, { rows: res.Rows, total: res.Total, page })
}
```

### 5. 进度事件订阅

```ts
Events.On('searchProgress', (e: any) => {
  const p = e.data // { FileID, TabID, Scanned, Total, Done }
  const percent = p.Total > 0 ? Math.round((p.Scanned / p.Total) * 100) : 0
  useStore.getState().setTab(p.FileID, p.TabID, { searchPercent: p.Done ? 100 : percent })
})
```

进度条展示在检索 tab 顶部（`tab.searching` 时显示 `Progress`）。

### 6. 取消扫描

```ts
// 切换检索 tab / 关闭 tab 时：
function cancelIfSearching(fileId: string, tabId: string) {
  const tab = getTab(fileId, tabId)
  if (tab?.searching) LogService.CancelSearch(fileId, tabId)
}
```

在 `setActiveTab`、`removeTab`、`removeFile` 中调用，确保不残留后台扫描。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `frontend/src/search/QueryBuilder.tsx` | 实现查询构建 |
| `frontend/src/table/LogTable.tsx` | 按 tab.mode 区分分页 |
| `frontend/src/layout/MainPane.tsx` | 新建 tab、取消逻辑 |
| `frontend/src/store/useStore.ts` | 补充 search 相关 action |

## 验收标准（DoD）

- [ ] 可新建多个检索 tab，各自独立查询/翻页/进度
- [ ] 字段条件（= ≠ 包含 > <）、关键字、时间范围均生效（对照后端单测语义）
- [ ] 切换/关闭 tab 后，原 tab 的后台扫描被取消，无报错残留
- [ ] 检索进度条随扫描推进，完成后消失

## 风险与注意

1. **取消错误处理**：`Search` 被取消时后端返回 `context.Canceled`，前端须静默处理，不弹错。
2. **tabId 一致性**：`Search`/`GetPage`/`CancelSearch` 必须传同一个 tabId，否则翻页取不到结果。
3. **查询为空**：空 Query（无条件/关键字/时间）等于全量匹配，等价于浏览；可提示用户或直接允许。
4. **翻页越界**：`GetPage` 在 page 超过总页数时返回空 rows（后端 `Page` 已处理），前端据此停用「下一页」。
