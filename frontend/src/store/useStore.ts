// zustand 单一状态源（docs/issues/05 §3 / §4.2）：多文件 × 每文件多 tab。
// 每个 tab 都是「查询 tab」：首 tab 为空条件（=全部行，打开文件后自动检索一次），
// 加号（+）复制当前 tab 的过滤条件到新 tab 并立即执行。
// 一切 UI 状态从这里读写；数据加载动作以模块函数形式导出，组件只关心 promise 成败。

import { create } from 'zustand'
import type { FieldInfo, FieldValues, FileInfo, ParsedLine, Query, RecentFile } from '../types'
import { api } from '../api'

/** 每页行数：默认 20，选择器范围 10~100（后端再兜底 [1,500]）。 */
export const DEFAULT_PAGE_SIZE = 20
export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100]

export interface Tab {
  tabId: string
  title: string
  query: Query // 查询构建器编辑中的条件（检索时 cleanQuery 后再提交）
  page: number
  pageSize: number
  total: number
  rows: ParsedLine[]
  loading: boolean
  searching: boolean
  searchPercent: number // 0~100
  searched: boolean // 是否成功跑过一次检索（区分「未检索」与「零命中」空态）
  autoRun?: boolean // 初始「全部」tab 标记：文件索引就绪后自动跑一次空条件检索
}

export interface FileView {
  fileId: string
  info: FileInfo
  indexPercent: number
  indexDone: boolean
  indexError: string // 索引失败信息（成功为空）
  fields: FieldInfo[] | null // 未拉取/拉取失败为 null，UI 按 [] 处理
  fieldValues: Record<string, FieldValues> // 字段名 -> 取值枚举（未请求过则缺 key；Truncated 字段不在此）
  tabs: Tab[]
  activeTabId: string
}

export function emptyQuery(): Query {
  return { Conditions: [], Keyword: '', TimeRange: null }
}

// ---------------- 主题（亮/暗） ----------------
// 暗色同时驱动 main.tsx 的 ConfigProvider algorithm 与 <html data-theme>（自写 CSS 变量）；
// 偏好存 localStorage，重启保持。

const THEME_KEY = 'logviewer.theme'

function readSavedTheme(): boolean {
  try {
    return localStorage.getItem(THEME_KEY) === 'dark'
  } catch {
    return false
  }
}

function cloneQuery(q: Query): Query {
  return {
    Conditions: q.Conditions.map((c) => ({ Field: c.Field, Op: c.Op, Value: c.Value })),
    Keyword: q.Keyword,
    TimeRange: q.TimeRange ? { From: q.TimeRange.From, To: q.TimeRange.To } : null,
  }
}

/** 新建检索 tab：seed 为其初始过滤条件（null = 空条件 → 全部行）。 */
export function newSearchTab(seed: Query | null, over?: Partial<Tab>): Tab {
  const q = cloneQuery(seed ?? emptyQuery())
  return {
    tabId: crypto.randomUUID(),
    title: buildTitle(q),
    query: q,
    page: 1,
    pageSize: DEFAULT_PAGE_SIZE,
    total: 0,
    rows: [],
    loading: false,
    searching: false,
    searchPercent: 0,
    searched: false,
    ...over,
  }
}

// ---------------- store ----------------

interface State {
  files: FileView[]
  activeFileId: string | null
  /** 历史记录（SQLite 持久化，重启后仍在）：侧栏「最近打开」数据源。 */
  recent: RecentFile[]
  dark: boolean // 暗色主题（持久化；同步 ConfigProvider 与 <html data-theme>）
  toggleTheme: () => void
  addFile: (info: FileInfo) => void
  removeFile: (fileId: string) => void
  setActiveFile: (fileId: string) => void
  setRecent: (recent: RecentFile[]) => void
  setIndexProgress: (
    fileId: string,
    percent: number,
    done: boolean,
    error?: string,
    totalLines?: number,
  ) => void
  setFields: (fileId: string, fields: FieldInfo[]) => void
  setFieldValues: (fileId: string, field: string, fv: FieldValues) => void
  /** 复制当前激活 tab 的条件到新 tab 并立即执行检索（+ 按钮）。 */
  addSearchTab: (fileId: string) => Promise<void>
  removeTab: (fileId: string, tabId: string) => void
  setActiveTab: (fileId: string, tabId: string) => void
  setTab: (fileId: string, tabId: string, patch: Partial<Tab>) => void
}

export function findFile(s: { files: FileView[] }, fileId: string): FileView | null {
  return s.files.find((f) => f.fileId === fileId) ?? null
}

export function findTab(file: FileView | null, tabId: string): Tab | null {
  return file?.tabs.find((t) => t.tabId === tabId) ?? null
}

export const useStore = create<State>()((set, get) => ({
  files: [],
  activeFileId: null,
  recent: [],
  dark: readSavedTheme(),

  toggleTheme() {
    const next = !get().dark
    set({ dark: next })
    try {
      localStorage.setItem(THEME_KEY, next ? 'dark' : 'light')
    } catch {
      /* 存储不可用（隐私模式等）时仅本次会话生效 */
    }
  },

  addFile(info) {
    const existing = findFile(get(), info.ID)
    if (existing) {
      // 同一路径后端复用会话：这里只切到该文件（不重复加入列表）
      set({ activeFileId: info.ID })
      return
    }
    const first = newSearchTab(emptyQuery(), { autoRun: true }) // 首 tab = 全部（空条件）
    const view: FileView = {
      fileId: info.ID,
      info,
      indexPercent: info.Status === 'ready' ? 100 : 0,
      indexDone: info.Status !== 'indexing',
      indexError: '',
      fields: null,
      fieldValues: {},
      tabs: [first],
      activeTabId: first.tabId,
    }
    set((s) => ({
      files: [...s.files, view],
      activeFileId: s.activeFileId ?? info.ID,
    }))
    refreshRecent().catch(() => {}) // 后端已在打开时写入记录，这里同步「最近打开」
  },

  removeFile(fileId) {
    const file = findFile(get(), fileId)
    if (!file) return
    // 先作废该文件所有 tab 的进行中动作（其 promise 后续 settle 时按 stale 静默跳过）
    for (const t of file.tabs) nextGen(t.tabId)
    api.closeFile(fileId).catch(() => {}) // 后端取消全部扫描并释放索引/结果
    set((s) => {
      const files = s.files.filter((f) => f.fileId !== fileId)
      return {
        files,
        activeFileId: s.activeFileId === fileId ? (files[0]?.fileId ?? null) : s.activeFileId,
      }
    })
    refreshRecent().catch(() => {}) // 关闭不删记录：该文件应出现在「最近打开」里
  },

  setActiveFile(fileId) {
    if (!findFile(get(), fileId)) return
    set({ activeFileId: fileId })
  },

  setRecent(recent) {
    set({ recent })
  },

  setIndexProgress(fileId, percent, done, error = '', totalLines) {
    set((s) => ({
      files: s.files.map((f) =>
        f.fileId !== fileId
          ? f
          : {
              ...f,
              indexPercent: done && !error ? 100 : percent,
              indexDone: done,
              indexError: error,
              // 行数只有在索引扫描结束后才可知：打开接口返回的 FileInfo.TotalLines 恒为 0，
              // 这里在完成时补正（否则侧栏一直显示「0 行」）。
              info: done
                ? {
                    ...f.info,
                    Status: error ? 'error' : 'ready',
                    TotalLines: totalLines && totalLines > 0 ? totalLines : f.info.TotalLines,
                  }
                : f.info,
            },
      ),
    }))
  },

  setFields(fileId, fields) {
    set((s) => ({
      files: s.files.map((f) => (f.fileId !== fileId ? f : { ...f, fields })),
    }))
  },

  setFieldValues(fileId, field, fv) {
    set((s) => ({
      files: s.files.map((f) =>
        f.fileId !== fileId ? f : { ...f, fieldValues: { ...f.fieldValues, [field]: fv } },
      ),
    }))
  },

  addSearchTab(fileId) {
    const file = findFile(get(), fileId)
    const src = file ? findTab(file, file.activeTabId) : null
    if (!src) return Promise.resolve()
    // 复制当前激活 tab 的过滤条件与页大小 → 新 tab 立即执行同查询
    const tab = newSearchTab(src.query, { pageSize: src.pageSize })
    set((s) => ({
      files: s.files.map((f) =>
        f.fileId !== fileId ? f : { ...f, tabs: [...f.tabs, tab], activeTabId: tab.tabId },
      ),
    }))
    return runSearch(fileId, tab.tabId)
  },

  removeTab(fileId, tabId) {
    const file = findFile(get(), fileId)
    const tab = findTab(file, tabId)
    if (!file || !tab) return
    if (file.tabs.length <= 1) return // 每个文件至少保留一个 tab
    nextGen(tabId)
    if (tab.searching) api.cancelSearch(fileId, tabId).catch(() => {})
    api.removeTab(fileId, tabId).catch(() => {}) // 释放该 tab 命中缓存（避免多 tab 缓存膨胀）
    set((s) => ({
      files: s.files.map((f) => {
        if (f.fileId !== fileId) return f
        const tabs = f.tabs.filter((t) => t.tabId !== tabId)
        return { ...f, tabs, activeTabId: f.activeTabId === tabId ? tabs[0].tabId : f.activeTabId }
      }),
    }))
  },

  setActiveTab(fileId, tabId) {
    const file = findFile(get(), fileId)
    if (!file || tabId === file.activeTabId) return
    // 切走仍在扫描的 tab：作废其动作、取消后端扫描并复位其状态（docs/issues/07 §6）
    const old = findTab(file, file.activeTabId)
    if (old?.searching) {
      nextGen(old.tabId)
      api.cancelSearch(fileId, old.tabId).catch(() => {})
      set((s) => ({
        files: s.files.map((f) =>
          f.fileId !== fileId
            ? f
            : {
                ...f,
                tabs: f.tabs.map((t) =>
                  t.tabId === old.tabId
                    ? { ...t, searching: false, loading: false, searchPercent: 0 }
                    : t,
                ),
              },
        ),
      }))
    }
    set((s) => ({
      files: s.files.map((f) => (f.fileId !== fileId ? f : { ...f, activeTabId: tabId })),
    }))
  },

  setTab(fileId, tabId, patch) {
    set((s) => ({
      files: s.files.map((f) =>
        f.fileId !== fileId
          ? f
          : {
              ...f,
              tabs: f.tabs.map((t) => (t.tabId === tabId ? { ...t, ...patch } : t)),
            },
      ),
    }))
  },
}))

// ---------------- 竞态防护 ----------------
// 每个 tab 一把「代次」计数器：取消/关闭/重新检索都会 +1，
// 早先发起的 promise 在 settle 时若代次已过期则直接丢弃（不污染新状态、不弹错）。

const genByTab = new Map<string, number>()

function nextGen(tabId: string): number {
  const g = (genByTab.get(tabId) ?? 0) + 1
  genByTab.set(tabId, g)
  return g
}

function isStale(tabId: string, gen: number): boolean {
  return (genByTab.get(tabId) ?? 0) !== gen
}

// ---------------- 数据加载动作（检索/翻页/取消） ----------------

/** 翻页：GetPage 命中行号内存跳转，页大小随 tab.pageSize（用户选择器）。 */
export async function loadSearchPage(fileId: string, tabId: string, page: number): Promise<void> {
  const st = useStore.getState()
  const tab = findTab(findFile(st, fileId), tabId)
  if (!tab) return
  const gen = nextGen(tabId)
  st.setTab(fileId, tabId, { page, loading: true })
  const res = await api.getPage(fileId, tabId, page, tab.pageSize)
  if (isStale(tabId, gen)) return
  useStore.getState().setTab(fileId, tabId, {
    rows: res.Rows,
    total: res.Total,
    page,
    loading: false,
  })
}

/** 查询 → 展示标题（如 "level=ERROR · \"auth\""；空条件 = 全部）。 */
export function buildTitle(query: Query): string {
  const parts: string[] = []
  const opSymbol: Record<string, string> = { '=': '=', '!=': '≠', contains: ' 包含 ', '>': '>', '<': '<' }
  for (const c of query.Conditions) {
    if (c.Field && c.Op) parts.push(`${c.Field}${opSymbol[c.Op] ?? c.Op}${c.Value}`)
  }
  if (query.Keyword) parts.push(`"${query.Keyword}"`)
  if (query.TimeRange) parts.push('时间范围')
  if (!parts.length) return '全部'
  const title = parts.join(' · ')
  return title.length > 28 ? `${title.slice(0, 28)}…` : title
}

function cleanQuery(raw: Query): Query {
  return {
    Conditions: raw.Conditions.filter((c) => c.Field.trim() !== '' && c.Op !== '')
      .map((c) => ({ Field: c.Field.trim(), Op: c.Op, Value: c.Value })),
    Keyword: raw.Keyword.trim(),
    TimeRange: raw.TimeRange && (raw.TimeRange.From !== null || raw.TimeRange.To !== null)
      ? { From: raw.TimeRange.From, To: raw.TimeRange.To }
      : null,
  }
}

/** 提交检索。成功回填第一页（行数 = tab.pageSize）与标题；失败先复位状态再抛出，调用方决定是否提示。 */
export async function runSearch(fileId: string, tabId: string): Promise<void> {
  const st = useStore.getState()
  const tab = findTab(findFile(st, fileId), tabId)
  if (!tab) return
  const query = cleanQuery(tab.query)
  const pageSize = tab.pageSize
  const gen = nextGen(tabId)
  st.setTab(fileId, tabId, {
    searching: true,
    searchPercent: 0,
    loading: true,
    searched: false,
    rows: [],
    total: 0,
    page: 1,
  })
  try {
    const res = await api.search(fileId, tabId, query, pageSize)
    if (isStale(tabId, gen)) return
    const cur = useStore.getState()
    if (!findTab(findFile(cur, fileId), tabId)) return // tab 已关闭
    cur.setTab(fileId, tabId, {
      rows: res.Rows,
      total: res.Total,
      page: 1,
      loading: false,
      searching: false,
      searchPercent: 100,
      searched: true,
      title: buildTitle(query),
    })
  } catch (err) {
    // 过期 = 已被取消/关闭/重新检索接管：状态由动作方复位，这里静默结束
    if (isStale(tabId, gen)) return
    const cur = useStore.getState()
    if (!findTab(findFile(cur, fileId), tabId)) return
    cur.setTab(fileId, tabId, { searching: false, loading: false, searchPercent: 0 })
    throw err // 真实失败抛给调用方提示
  }
}

/** 取消检索并复位 tab 状态（用户点「取消」时用）。 */
export async function cancelTabSearch(fileId: string, tabId: string): Promise<void> {
  const st = useStore.getState()
  const tab = findTab(findFile(st, fileId), tabId)
  if (!tab || !tab.searching) return
  nextGen(tabId)
  try {
    await api.cancelSearch(fileId, tabId)
  } catch {
    /* 取消是尽力而为，错误静默 */
  }
  const cur = useStore.getState()
  if (findTab(findFile(cur, fileId), tabId)) {
    cur.setTab(fileId, tabId, { searching: false, loading: false, searchPercent: 0 })
  }
}

// ---------------- 打开后对账 + 字段拉取 ----------------

const fieldsInflight = new Set<string>()

/** 拉取字段并写入 store；同一文件并发只拉一次，失败允许下次重试。 */
export function ensureFields(fileId: string): void {
  if (fieldsInflight.has(fileId)) return
  fieldsInflight.add(fileId)
  api
    .getFields(fileId)
    .then((fields) => useStore.getState().setFields(fileId, fields))
    .catch(() => {
      /* 字段列表拉取失败：查询构建器退化为自由输入，不阻塞浏览 */
    })
    .finally(() => fieldsInflight.delete(fileId))
}

const fieldValuesInflight = new Map<string, Promise<FieldValues | null>>()

/**
 * 取字段取值枚举（文件级缓存，成功即写 store）。返回 null = 无枚举或拉取失败，
 * 调用方回落自由输入；失败不缓存（下次再试）。
 */
export function fetchFieldValues(fileId: string, field: string): Promise<FieldValues | null> {
  const file = findFile(useStore.getState(), fileId)
  const cached = file?.fieldValues[field]
  if (cached !== undefined) return Promise.resolve(cached)
  const key = fileId + '\x00' + field // NUL 分隔防撞键
  let p = fieldValuesInflight.get(key)
  if (!p) {
    p = api
      .getFieldValues(fileId, field)
      .then((fv) => {
        const cur = useStore.getState()
        if (findFile(cur, fileId)) cur.setFieldValues(fileId, field, fv) // 文件已关则丢弃
        return fv
      })
      .catch(() => null)
    fieldValuesInflight.set(key, p)
    p.finally(() => fieldValuesInflight.delete(key))
  }
  return p
}

/** 打开文件后对账一次索引进度（事件为主、此方法兜底，防事件竞态漏更新）。 */
export async function syncIndex(fileId: string): Promise<void> {
  const st = await api.getIndexStatus(fileId)
  const cur = useStore.getState()
  if (!findFile(cur, fileId)) return // 已在此前被关闭
  if (st.Done) {
    cur.setIndexProgress(fileId, st.Error ? 0 : 100, true, st.Error, st.TotalLines)
    if (!st.Error) ensureFields(fileId)
  }
}

// ---------------- 历史记录（SQLite 持久化） ----------------
// 记录由后端在打开文件时写入；这里只负责把列表同步到 store 与「重新打开」动作。

/** 拉取历史记录并写入 store；失败（数据库不可用等）静默为空列表，不影响主流程。 */
export async function refreshRecent(): Promise<void> {
  try {
    useStore.getState().setRecent(await api.listRecentFiles())
  } catch {
    useStore.getState().setRecent([])
  }
}

/** 从历史记录重新打开文件（重建索引）；文件已丢失时抛出（调用方提示并可移除记录）。 */
export async function openRecentFile(id: number): Promise<void> {
  const info = await api.openRecentFile(id)
  useStore.getState().addFile(info) // 同一路径已打开时只切过去
  await syncIndex(info.ID).catch(() => {}) // 事件推送为主，此调用兜底对账
  await refreshRecent() // 打开次数/最近打开时间有变化
}

/** 从历史记录中移除一条（例如磁盘上已丢失的文件）。 */
export async function removeRecentFile(id: number): Promise<void> {
  await api.removeRecentFile(id)
  await refreshRecent()
}

/** 清空全部历史记录。 */
export async function clearRecentFiles(): Promise<void> {
  await api.clearRecentFiles()
  useStore.getState().setRecent([])
}
