# M6 · 前端：打开文件 + 分页展示

- **状态**：未开始
- **依赖**：M5（布局/状态）、M4（后端 Service）
- **关联文档**：[功能设计](../functional-design.md) §5.1、§5.3

## 目标

打通「打开文件 → 索引 → 分页浏览」完整链路：打开对话框选文件、文件列表显示索引进度、主区用 antd `Table` 分页（100/页）展示日志，行可展开查看完整 JSON。

## 详细实现

### 1. 事件订阅（`App.tsx` 或 `useStore.ts` 初始化处）

```ts
import { Events } from '@wailsio/runtime'

Events.On('indexProgress', (e: any) => {
  const p = e.data // { FileID, Percent, Done }
  useStore.getState().setIndexProgress(p.FileID, p.Percent, p.Done)
})
```

> 注意：Wails v3 事件回调收到的 payload 在 `.data` 字段（参考模板 `Events.On('time', v => v.data)`）。

### 2. 打开文件（`FileSidebar.tsx`）

```tsx
import { LogService } from '../../bindings/logviewer'

async function handleOpen() {
  const info = await LogService.OpenFileDialog()
  if (!info || !info.ID) return // 取消
  useStore.getState().addFile(info)
  // 索引完成后拉取字段（供 M7 查询下拉）
  if (info.Status === 'ready') loadFields(info.ID)
}
```

- 打开后立即 `addFile`，文件列表项出现，状态为「索引中」+ 进度条（来自 `indexProgress` 事件）。
- 索引完成（`indexDone`）后调用 `GetFields` 拉字段列表并写入 store。

### 3. 浏览：分页加载（`LogTable.tsx`）

首次进入某文件的「全部」tab，或翻页时调用：

```ts
const pageSize = 100
async function loadPage(fileId: string, page: number) {
  const start = (page - 1) * pageSize
  const res = await LogService.GetLines(fileId, start, pageSize)
  useStore.getState().setTab(fileId, activeTabId, {
    rows: res.Rows, total: res.Total, page, loading: false,
  })
}
```

### 4. 表格（`LogTable.tsx`）

```tsx
import { Table, Tag, Typography } from 'antd'
import dayjs from 'dayjs'

const columns = [
  { title: '行号', dataIndex: 'LineNo', width: 90 },
  {
    title: '时间', dataIndex: 'Timestamp', width: 190,
    render: (t?: number | null) => t ? dayjs(t).format('YYYY-MM-DD HH:mm:ss.SSS') : '-',
  },
  {
    title: '级别', dataIndex: 'Level', width: 110,
    render: (lv: string) => lv ? <Tag color={levelColor(lv)}>{lv}</Tag> : '-',
  },
  { title: '消息', dataIndex: 'Message', ellipsis: true },
]

function LogTable({ fileId, tab }: { fileId: string; tab: Tab }) {
  return (
    <Table
      size="small"
      rowKey="LineNo"
      columns={columns}
      dataSource={tab.rows}
      loading={tab.loading}
      pagination={{
        current: tab.page,
        pageSize: tab.pageSize,
        total: tab.total,
        showSizeChanger: false,
        showQuickJumper: true,
        onChange: (page) => loadPage(fileId, tab.tabId, page),
      }}
      expandable={{
        expandedRowRender: (row) => (
          <pre style={{ maxHeight: 300, overflow: 'auto' }}>
            {row.Valid ? JSON.stringify(row.JSON, null, 2) : row.Raw}
          </pre>
        ),
        rowExpandable: (row) => true,
      }}
    />
  )
}

const levelColor = (lv: string) =>
  ({ ERROR: 'red', WARN: 'orange', WARNING: 'orange', INFO: 'blue', DEBUG: 'default', TRACE: 'default' } as any)[lv] ?? 'default'
```

> `dayjs` 是 antd 的既有依赖，可直接 `import dayjs from 'dayjs'`（无需额外安装）。若需更丰富的 JSON 查看（键值着色、可折叠），可后续替换为 `@uiw/react-json-view`（M8）。

### 5. 文件列表项状态（`FileSidebar.tsx`）

- `indexing`：`<Progress percent={indexPercent} size="small" />`
- `ready`：文件名 + 行数 `TotalLines`
- `error`：红色错误提示

## 涉及文件

| 文件 | 操作 |
|---|---|
| `frontend/src/layout/FileSidebar.tsx` | 实现打开 + 进度显示 |
| `frontend/src/table/LogTable.tsx` | 实现表格 + 分页 + 展开 |
| `frontend/src/store/useStore.ts` | 补充 `loadPage` 相关 action/逻辑 |

## 验收标准（DoD）

- [ ] 点击「打开」弹出系统对话框，选中文件后左侧出现该文件项
- [ ] 索引期间显示进度；完成后文件项显示行数
- [ ] 主区表格分页展示日志，翻页/跳页正确，每页 ≤ 100 条
- [ ] 行可展开看到完整 JSON（非法行显示原始文本）
- [ ] 时间列格式正确、级别列着色正确

## 风险与注意

1. **`GetLines` 返回的 `Total`** 是文件总行数（浏览模式），分页据此显示总页数。
2. **整数精度**：`LineNo`/`Total`/`Timestamp` 用 JS `number` 承载（本场景量级远小于 2^53，安全）。
3. **翻页响应**：`GetLines` 走 `ReadAt` 随机读，100 行瞬时返回，无需前端缓存。
4. **打开后状态**：`OpenFileDialog` 返回的 `info.Status` 可能已是 `indexing`，进度由事件驱动更新，避免前端轮询。
