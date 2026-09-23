// 与后端 DTO 对齐的 TS 类型（bindings 生成物为 JS+JSDoc，类型以本文件为准，
// 见 docs/issues/05-m5-layout.md §2：手写对齐版与 bindings 二者择一，避免重复）。

export interface FileInfo {
  ID: string
  Path: string
  Name: string
  Status: 'indexing' | 'ready' | 'error'
  TotalLines: number
  Kind: 'local' | 'remote'
  /** 远端连接标识 user@host:port（本地为空）。 */
  Remote: string
  /** 远端连接 id（本地 0）。 */
  ConnID: number
}

export interface IndexStatus {
  FileID: string
  Done: boolean
  Percent: number // 0~100
  Error: string
  /** 索引完成后的总行数（未完成 0）；FileInfo 是打开瞬间的快照，行数靠这里补正。 */
  TotalLines: number
}

/**
 * 历史记录项（侧栏「最近打开」）：同名不同路径靠 Path/Dir 区分，
 * 同名不同主机再靠 Remote 区分。
 */
export interface RecentFile {
  ID: number
  Path: string
  Name: string
  Dir: string
  TotalLines: number // 最近一次索引完成后的行数（未知 0）
  OpenCount: number
  LastOpenedAt: number // unix 毫秒
  /** 读取记录时磁盘上是否仍存在；false = 已丢失，UI 标记并提供移除记录。 */
  Exists: boolean
  Kind: 'local' | 'remote'
  Remote: string
  ConnID: number
  /**
   * 远端主机当前是否有活动连接。远端文件的存在性不在列表阶段探测（列表不能因网络卡住），
   * 未连接时显示「未连接」而不是「已丢失」。
   */
  Connected: boolean
}

// ---------------- 远端主机（SSH/SFTP） ----------------

/** 已保存的远端主机；不含任何口令/私钥材料。 */
export interface Connection {
  ID: number
  Name: string
  Host: string
  Port: number
  User: string
  AuthMethod: 'password' | 'key'
  /** 私钥文件路径（认证方式为 key 时）。 */
  KeyPath: string
  /** 该主机是否启用本地内容缓存；null = 跟随全局设置。 */
  Cache: boolean | null
  /** 该主机是否已保存口令（后端不回传口令本身）。 */
  HasPassword: boolean
  /** 上次浏览的目录（再次打开浏览器时定位到这里）。 */
  LastDir: string
  LastUsedAt: number // unix 毫秒
  Connected: boolean
}

/** 本次连接使用的口令；留空表示沿用后端已保存的。 */
export interface Credential {
  Password: string
  Passphrase: string
}

export interface RemoteEntry {
  Name: string
  Path: string
  IsDir: boolean
  Size: number
  ModTime: number // unix 毫秒
  Mode: string // 权限展示，如 -rw-r--r--
}

// ---------------- 本地内容缓存 ----------------

/** 一条本地缓存条目（缓存管理界面用）。 */
export interface CacheEntryInfo {
  /** 缓存标识（与后端 Source.Key() 同构，删除单条时回传）。 */
  SourceKey: string
  Remote: string
  Path: string
  Name: string
  Size: number
  LastUsedAt: number // unix 毫秒
  /** 当前有会话在使用（清除时会被跳过）。 */
  InUse: boolean
}

/** 缓存总览。 */
export interface CacheInfo {
  /** 全局开关（主机可以再单独覆盖）。 */
  Enabled: boolean
  /** 数据库可用时缓存才可用。 */
  Available: boolean
  Dir: string
  /** 总量上限（字节；<=0 表示不限制）。 */
  Limit: number
  TotalBytes: number
  Entries: CacheEntryInfo[] | null
}

export interface RemoteListing {
  Path: string // 当前目录
  Parent: string // 上级目录；已在根目录时为空
  Home: string // 远端家目录（快捷跳转用）
  Entries: RemoteEntry[]
}

export interface ParsedLine {
  LineNo: number
  Raw: string
  Valid: boolean
  JSON: Record<string, any> | null // 非法行为 null
  Timestamp: number | null // unix 毫秒
  Level: string
  Message: string
}

export interface PageResult {
  Total: number
  Rows: ParsedLine[]
}

export interface SearchResult {
  TabID: string
  Total: number
  Rows: ParsedLine[]
}

export interface FieldInfo {
  Name: string
  Type: string // "string" | "number" | "bool" | "object" | "array" | "unknown"
}

/** 字段取值枚举（检索条件下拉用）：Values 字母序；Truncated=true 时为空，应回落手输。 */
export interface FieldValues {
  Field: string
  Values: string[]
  Truncated: boolean
}

export interface Condition {
  Field: string
  Op: string // "=" | "!=" | "contains" | ">" | "<"
  Value: string
}

export interface TimeRange {
  From: number | null // unix 毫秒，含
  To: number | null // 含；null 表示该侧无界
}

export interface Query {
  Conditions: Condition[]
  Keyword: string
  TimeRange: TimeRange | null
}

// ---------------- 事件载荷（后端 → 前端） ----------------

export interface IndexProgressEvent {
  FileID: string
  Percent: number
  Done: boolean
  Error: string // 索引失败信息（成功为空）
  TotalLines: number // 完成时的总行数（未完成 0）
}

export interface SearchProgressEvent {
  FileID: string
  TabID: string
  Scanned: number
  Total: number
  Done: boolean
}
