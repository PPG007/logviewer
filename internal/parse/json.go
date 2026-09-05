// Package parse 提供单行日志解析能力：JSON 反序列化、时间/级别/消息识别、顶层字段收集。
package parse

import (
	"strings"

	jsoniter "github.com/json-iterator/go"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// ParseLine 解析一行 JSON 文本为 map。合法返回 (map, true)；非法（含空行）返回 (nil, false)。
func ParseLine(raw string) (map[string]any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var m map[string]any
	if err := json.UnmarshalFromString(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}
