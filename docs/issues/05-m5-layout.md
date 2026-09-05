# M5 · 前端：布局 + 状态

- **状态**：未开始
- **依赖**：M4（需要 bindings 类型，但可先用占位类型并行）
- **关联文档**：[功能设计](../functional-design.md) §7（UI 布局）、§4.2（前端状态）

## 目标

搭出应用骨架：左侧文件列表 + 右侧「文件级 Tabs + 检索级 Tabs」，并用 zustand 建立状态模型。**本里程碑不做真实数据流**，先跑通布局与空状态交互。

## 目录结构

```
frontend/src/
├── main.tsx                # 入口：引入 antd + store
├── App.tsx                 # 顶层布局
├── types.ts                # 与后端 DTO 对齐的类型
├── store/
│   └── useStore.ts         # zustand store
├── layout/
│   ├── FileSidebar.tsx     # 左侧文件列表
│   └── MainPane.tsx        # 右侧：文件级 Tabs + 检索级 Tabs
├── table/
│   └── LogTable.tsx        # 结果表格（M6 填充数据）
└── search/
    └── QueryBuilder.tsx    # 查询构建（M7 实现）
```

## 详细实现

### 1. 引入 antd（`main.tsx`）

```tsx
import React from 'react'
import ReactDOM from 'react-dom/client'
import { ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import App from './App'

ReactDOM.createRoot(document.getElementById('root') as HTMLElement).render(
  <React.StrictMode>
    <ConfigProvider locale={zhCN} theme={{ token: { colorPrimary: '#1677ff' } }}>
      <App />
    </ConfigProvider>
  </React.StrictMode>,
)
```

### 2. 类型（`types.ts`）

与后端 DTO 一一对应（M4 生成 bindings 后可直接复用 bindings 里的类型，此处为手写对齐版，二者择一，避免重复）：

```ts
export interface FileInfo {
  ID: string; Path: string; Name: string
  Status: 'indexing' | 'ready' | 'error'; TotalLines: number
}
export interface ParsedLine {
  LineNo: number; Raw: string; Valid: boolean
  JSON: Record<string, any> | null; Timestamp?: number | null
  Level: string; Message: string
}
export interface PageResult { Total: number; Rows: ParsedLine[] }
export interface SearchResult { TabID: string; Total: number; Rows: ParsedLine[] }
export interface FieldInfo { Name: string; Type: string }
export interface Condition { Field: string; Op: string; Value: string }
export interface TimeRange { From?: number | null; To?: number | null }
export interface Query { Conditions: Condition[]; Keyword: string; TimeRange?: TimeRange | null }
```

### 3. Store（`store/useStore.ts`）

```ts
import { create } from 'zustand'

export interface Tab {
  tabId: string
  title: string
  mode: 'browse' | 'search'
  query?: Query
  page: number
  pageSize: number          // 固定 100
  total: number
  rows: ParsedLine[]
  loading: boolean
  searching: boolean
  searchPercent: number
}

export interface FileView {
  fileId: string
  info: FileInfo
  indexPercent: number
  indexDone: boolean
  fields: FieldInfo[]
  tabs: Tab[]
  activeTabId: string
}

interface State {
  files: FileView[]
  activeFileId: string | null
  // actions
  addFile: (info: FileInfo) => void
  removeFile: (fileId: string) => void
  setActiveFile: (fileId: string) => void
  setIndexProgress: (fileId: string, percent: number, done: boolean) => void
  setFields: (fileId: string, fields: FieldInfo[]) => void
  addTab: (fileId: string, tab: Tab) => void
  removeTab: (fileId: string, tabId: string) => void
  setActiveTab: (fileId: string, tabId: string) => void
  setTab: (fileId: string, tabId: string, patch: Partial<Tab>) => void
}

export const useStore = create<State>((set, get) => ({
  files: [],
  activeFileId: null,
  // ...各 action 实现（常规 zustand 写法，对 files 做 map/update）
}))
```

> tabId 生成：前端用 `crypto.randomUUID()` 或自增计数（`tab-${Date.now()}-${n}`）。

### 4. 布局（`App.tsx`）

```tsx
import { Layout } from 'antd'
import FileSidebar from './layout/FileSidebar'
import MainPane from './layout/MainPane'

export default function App() {
  return (
    <Layout style={{ height: '100vh' }}>
      <FileSidebar />
      <MainPane />
    </Layout>
  )
}
```

- `FileSidebar`：`Sider` 内放「打开文件」按钮 + `Menu`/`List`（每个文件一项，显示文件名 + 状态徽标）。
- `MainPane`：顶部 `Tabs`（文件级，items 来自 `files`）；每个文件 Tab 内容再嵌套一层 `Tabs`（检索级，items 来自该文件的 `tabs`，含一个默认「全部」browse tab 与 `+` 新建入口）。

### 5. 空状态

未打开任何文件时，`MainPane` 显示 `Empty` 组件（提示「点击左侧打开日志文件」）。

## 涉及文件

| 文件 | 操作 |
|---|---|
| `frontend/src/main.tsx` | 修改：引入 antd、ConfigProvider |
| `frontend/src/App.tsx` | 重写：顶层布局 |
| `frontend/src/types.ts` | 新建 |
| `frontend/src/store/useStore.ts` | 新建 |
| `frontend/src/layout/*` | 新建 |
| `frontend/src/table/LogTable.tsx` | 新建（占位） |
| `frontend/src/search/QueryBuilder.tsx` | 新建（占位） |
| 模板 CSS/示例组件 | 删除或替换 |

## 验收标准（DoD）

- [ ] `wails3 dev` 下布局正确渲染（左列表 + 右侧双层 Tabs）
- [ ] 未打开文件时显示空状态
- [ ] store 的 action 逻辑正确（新增/删除/切换文件与 tab 后 UI 联动）

## 风险与注意

1. **antd v5 主题**：无需引入 CSS，`ConfigProvider` 提供主题与中文 locale。
2. **Tabs 嵌套**：文件级 Tabs 与检索级 Tabs 是两层独立组件，各自 `activeKey` 来自 store，避免互相干扰。
3. **状态单一来源**：一切 UI 状态来自 zustand store，组件不自行维护重复状态（尤其 tab 的 page/total/rows）。
