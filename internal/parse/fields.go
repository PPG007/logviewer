package parse

import (
	"fmt"
	"strings"

	jsoniter "github.com/json-iterator/go"
)

// FieldInfo 顶层字段名与推断类型。
type FieldInfo struct {
	Name string
	Type string // "string" | "number" | "bool" | "object" | "array" | "null" | "unknown"
}

// 角色字段候选名（按优先级，与设计文档 §5.2 一致）。
var (
	levelFieldNames = []string{"level", "severity", "lvl", "log_level"}
	msgFieldNames   = []string{"msg", "message", "log", "content"}
)

// DetectLevel 识别级别字段值并大写归一；无则返回 ""。
func DetectLevel(m map[string]any) string {
	for _, name := range levelFieldNames {
		if v, ok := m[name]; ok {
			if s, ok := v.(string); ok {
				return strings.ToUpper(s)
			}
		}
	}
	return ""
}

// DetectMessage 识别消息字段值（非字符串转字符串）；无则返回 ""。
func DetectMessage(m map[string]any) string {
	for _, name := range msgFieldNames {
		if v, ok := m[name]; ok {
			if s, ok := v.(string); ok {
				return s
			}
			return fmt.Sprint(v)
		}
	}
	return ""
}

// CollectFields 把 m 的顶层字段合并进 acc（字段名 -> 类型），同名冲突后者覆盖。
func CollectFields(m map[string]any, acc map[string]string) {
	for k, v := range m {
		acc[k] = typeOf(v)
	}
}

func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case float64, float32, int, int64, int32, uint64, jsoniter.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "unknown"
	}
}
