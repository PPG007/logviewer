package search

import (
	"fmt"
	"strconv"
	"strings"
)

// Matcher 单行匹配器。语义与设计文档 §6 严格一致：
//   - 多个 conditions 之间 AND；keyword、timeRange 与 conditions 之间 AND
//   - 字段不存在：=/contains/>/< 不匹配；!= 视为匹配
//   - =/!=/contains 先把字段值 fmt.Sprint 转字符串比较；>/< 走数值比较
//   - keyword 对整行原始文本子串匹配
//   - timeRange：行无时间戳（ts==nil）不匹配
//   - 非法 JSON 行 parsed==nil：字段条件按「字段不存在」处理，仍可命中关键字
type Matcher struct {
	q Query
}

func NewMatcher(q Query) *Matcher {
	return &Matcher{q: q}
}

// Match 判断单行是否命中。raw 原始行、parsed 解析结果（非法行为 nil）、ts 行时间戳。
func (m *Matcher) Match(raw string, parsed map[string]any, ts *int64) bool {
	// 1) 时间范围
	if m.q.TimeRange != nil {
		if ts == nil {
			return false
		}
		if m.q.TimeRange.From != nil && *ts < *m.q.TimeRange.From {
			return false
		}
		if m.q.TimeRange.To != nil && *ts > *m.q.TimeRange.To {
			return false
		}
	}
	// 2) 关键字（整行子串）
	if m.q.Keyword != "" && !strings.Contains(raw, m.q.Keyword) {
		return false
	}
	// 3) 字段条件
	for _, c := range m.q.Conditions {
		if !matchCondition(parsed, c) {
			return false
		}
	}
	return true
}

func matchCondition(parsed map[string]any, c Condition) bool {
	v, ok := parsed[c.Field] // parsed 为 nil 时 ok=false
	switch c.Op {
	case "!=":
		if !ok {
			return true
		}
		return fmt.Sprint(v) != c.Value
	case "=":
		if !ok {
			return false
		}
		return fmt.Sprint(v) == c.Value
	case "contains":
		if !ok {
			return false
		}
		return strings.Contains(fmt.Sprint(v), c.Value)
	case ">", "<":
		if !ok {
			return false
		}
		lhs, err1 := strconv.ParseFloat(fmt.Sprint(v), 64)
		rhs, err2 := strconv.ParseFloat(c.Value, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if c.Op == ">" {
			return lhs > rhs
		}
		return lhs < rhs
	}
	return false
}
