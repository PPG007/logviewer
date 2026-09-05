// Package search 实现查询 DSL 与检索引擎：顺序扫描 + 命中行号缓存 + context 取消 + 进度。
package search

import (
	"fmt"
)

// 运算符白名单（v1 不含 regex，见设计 §6）。
var allowedOps = map[string]bool{"=": true, "!=": true, "contains": true, ">": true, "<": true}

// Condition 单条字段条件。
type Condition struct {
	Field string // 字段名
	Op    string // "=" | "!=" | "contains" | ">" | "<"
	Value string // 比较值（字符串形式，数值匹配时转换）
}

// TimeRange 时间范围（unix 毫秒，闭区间；nil 字段表示该侧无界）。
type TimeRange struct {
	From *int64
	To   *int64
}

// Query 一次查询：conditions 之间 AND；keyword、timeRange 与 conditions 之间 AND。
type Query struct {
	Conditions []Condition
	Keyword    string // 对整行原始文本做子串匹配
	TimeRange  *TimeRange
}

// Validate 校验查询合法性。
func (q *Query) Validate() error {
	for _, c := range q.Conditions {
		if c.Field == "" {
			return fmt.Errorf("condition field is empty")
		}
		if !allowedOps[c.Op] {
			return fmt.Errorf("unsupported operator %q (v1 支持 = != contains > <)", c.Op)
		}
	}
	return nil
}
