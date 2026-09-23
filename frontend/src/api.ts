// bindings 薄封装：把生成的 JS 绑定方法收敛为带类型的 async API。
// 生成物路径为嵌套包结构，统一在这里引用，组件只依赖本模块。

import * as svc from '../bindings/logviewer/internal/service/logservice.js'
import type {
  CacheInfo,
  Connection,
  Credential,
  FieldInfo,
  FieldValues,
  FileInfo,
  IndexStatus,
  PageResult,
  Query,
  RecentFile,
  RemoteListing,
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
  /** 强制重新读取并重建索引（文件被追加/轮转后刷新用）；会话 id 不变。 */
  async reloadFile(fileId: string): Promise<void> {
    await svc.ReloadFile(fileId)
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

  // ---------------- 历史记录（持久化，重启后可见） ----------------

  /** 历史文件记录（最近打开在前，含磁盘存在性探测）；数据库不可用时抛错。 */
  async listRecentFiles(): Promise<RecentFile[]> {
    return ((await svc.ListRecentFiles()) ?? []) as RecentFile[]
  },
  /** 打开历史记录中的文件（重新建索引）；文件已丢失时抛错。 */
  async openRecentFile(id: number): Promise<FileInfo> {
    return (await svc.OpenRecentFile(id)) as FileInfo
  },
  async removeRecentFile(id: number): Promise<void> {
    await svc.RemoveRecentFile(id)
  },
  async clearRecentFiles(): Promise<void> {
    await svc.ClearRecentFiles()
  },

  // ---------------- 远端主机（SSH/SFTP） ----------------

  /** 已保存的远端主机（最近使用在前，含连接状态）。 */
  async listConnections(): Promise<Connection[]> {
    return ((await svc.ListConnections()) ?? []) as Connection[]
  },
  /** 新增（ID 为 0）或更新一台远端主机；不涉及任何口令。 */
  async saveConnection(c: Connection): Promise<Connection> {
    return (await svc.SaveConnection(c)) as Connection
  },
  /** 删除主机：断开连接并连带删除其历史文件记录。 */
  async deleteConnection(id: number): Promise<void> {
    await svc.DeleteConnection(id)
  },
  /**
   * 连接（已有可用连接时直接复用）。凭据留空表示沿用后端已保存的口令；
   * 填了则在连接成功后明文保存到本机数据库。
   */
  async connectRemote(id: number, cred: Credential): Promise<void> {
    await svc.ConnectRemote(id, cred)
  },
  /** 清除该主机已保存的口令（下次连接需重新输入）。 */
  async clearConnectionSecret(id: number): Promise<void> {
    await svc.ClearConnectionSecret(id)
  },
  async disconnectRemote(id: number): Promise<void> {
    await svc.DisconnectRemote(id)
  },
  /** 列出远端目录；dir 为空时定位到上次浏览的目录（再退回远端家目录）。 */
  async listRemoteDir(id: number, dir: string): Promise<RemoteListing> {
    return (await svc.ListRemoteDir(id, dir)) as RemoteListing
  },
  /** 打开远端文件（需先连接）；同名同主机的文件复用现有会话。 */
  async openRemoteFile(id: number, remotePath: string): Promise<FileInfo> {
    return (await svc.OpenRemoteFile(id, remotePath)) as FileInfo
  },
  /** 打开远端文件对话框（选择私钥文件）；取消返回 null。 */
  async pickKeyFile(): Promise<string | null> {
    const p = (await svc.PickKeyFile()) as string
    return p || null
  },

  // ---------------- 远端文件本地缓存 ----------------

  /** 缓存总览（开关、目录、上限、占用、条目列表）。 */
  async getCacheInfo(): Promise<CacheInfo> {
    return (await svc.GetCacheInfo()) as CacheInfo
  },
  /** 全局开关；只影响后续写入与新会话，不打断进行中的读取与索引。 */
  async setCacheEnabled(enabled: boolean): Promise<void> {
    await svc.SetCacheEnabled(enabled)
  },
  /** 总量上限（字节；0 表示不限制），会立即按新上限逐出。 */
  async setCacheLimit(limit: number): Promise<void> {
    await svc.SetCacheLimit(limit)
  },
  async clearCache(): Promise<void> {
    await svc.ClearCache()
  },
  /** 删除单条缓存（正在使用中的会被后端跳过）。 */
  async removeCacheEntry(sourceKey: string): Promise<void> {
    await svc.RemoveCacheEntry(sourceKey)
  },
}
