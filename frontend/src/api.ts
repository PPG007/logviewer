// bindings 薄封装：把生成的 JS 绑定方法收敛为带类型的 async API。
// 生成物路径为嵌套包结构，统一在这里引用，组件只依赖本模块。

import * as svc from '../bindings/logviewer/internal/service/logservice.js'
import type {
  FieldInfo,
  FieldValues,
  FileInfo,
  IndexStatus,
  PageResult,
  Query,
  SearchResult,
} from './types'

// binding 拒绝时的错误可能是 Error / string / 其它对象，统一取可读文本。
export function errText(e: unknown): string {
  if (e instanceof Error) return e.message
  if (typeof e === 'string') return e
  try {
    return JSON.stringify(e)
  } catch {
    return String(e)
  }
}

// 临时日志上限（与 internal/service/logservice.go 常量镜像，后端为准，此处预检用）。
export const TEMP_LOG_MAX_LINES = 50_000
export const TEMP_LOG_MAX_BYTES = 10 << 20 // 10 MiB

export const api = {
  /** 系统对话框选择并打开；用户取消返回 null。 */
  async openFileDialog(): Promise<FileInfo | null> {
    const info = (await svc.OpenFileDialog()) as FileInfo
    return info?.ID ? info : null
  },
  async openFile(path: string): Promise<FileInfo> {
    return (await svc.OpenFile(path)) as FileInfo
  },
  /** 手动输入内容创建临时日志：后端落临时文件后走与普通文件一致的解析/索引。 */
  async openTempLog(content: string): Promise<FileInfo> {
    return (await svc.OpenTempLog(content)) as FileInfo
  },
  async closeFile(fileId: string): Promise<void> {
    await svc.CloseFile(fileId)
  },
  async removeTab(fileId: string, tabId: string): Promise<void> {
    await svc.RemoveTab(fileId, tabId)
  },
  async cancelSearch(fileId: string, tabId: string): Promise<void> {
    await svc.CancelSearch(fileId, tabId)
  },
  async getIndexStatus(fileId: string): Promise<IndexStatus> {
    return (await svc.GetIndexStatus(fileId)) as IndexStatus
  },
  async getFields(fileId: string): Promise<FieldInfo[]> {
    return (await svc.GetFields(fileId)) as FieldInfo[]
  },
  /** 字段取值枚举（仅 string 字段有；Truncated=true → UI 回落自由输入）。 */
  async getFieldValues(fileId: string, field: string): Promise<FieldValues> {
    return (await svc.GetFieldValues(fileId, field)) as FieldValues
  },
  async search(fileId: string, tabId: string, query: Query, pageSize: number): Promise<SearchResult> {
    return (await svc.Search(fileId, tabId, query, pageSize)) as SearchResult
  },
  async getPage(fileId: string, tabId: string, page: number, pageSize: number): Promise<PageResult> {
    return (await svc.GetPage(fileId, tabId, page, pageSize)) as PageResult
  },
  /** 导出 tab 全部命中到用户选择的文件；取消返回 null。 */
  async exportMatches(fileId: string, tabId: string): Promise<string | null> {
    const p = (await svc.ExportMatches(fileId, tabId)) as string
    return p || null
  },
}
