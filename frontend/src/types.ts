// 与后端 DTO 对齐的 TS 类型（bindings 生成物为 JS+JSDoc，类型以本文件为准，
// 见 docs/issues/05-m5-layout.md §2：手写对齐版与 bindings 二者择一，避免重复）。

export interface FileInfo {
  ID: string
  Path: string
  Name: string
  Status: 'indexing' | 'ready' | 'error'
  TotalLines: number
}

export interface IndexStatus {
  FileID: string
  Done: boolean
  Percent: number // 0~100
  Error: string
  /** 索引完成后的总行数（未完成 0）；FileInfo 是打开瞬间的快照，行数靠这里补正。 */
  TotalLines: number
}

/** 历史记录项（侧栏「最近打开」）：同名不同路径靠 Path/Dir 区分。 */
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
