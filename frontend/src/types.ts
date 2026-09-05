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
}

export interface SearchProgressEvent {
  FileID: string
  TabID: string
  Scanned: number
  Total: number
  Done: boolean
}
