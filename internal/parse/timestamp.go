package parse

import (
	"strconv"
	"time"

	jsoniter "github.com/json-iterator/go"
)

// timeFieldNames 时间字段候选（按优先级探测）。
// 只列不带 @ 的基础名：@timestamp 等 "@" 前缀变体由 fieldValue 自动归一为对应基础名。
var timeFieldNames = []string{"time", "timestamp", "ts", "datetime", "date"}

// timeLayouts 常见文本时间布局。
// 注意：无时区布局（如 "2006-01-02 15:04:05"）按 time.Local 解析，
// 跨时区过滤可能偏差——v1 接受该假设（见 issue M2 风险 2）。
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006/01/02 15:04:05",
}

// ExtractTimestamp 按优先级探测时间字段并解析，返回 unix 毫秒；无/解析失败返回 nil。
func ExtractTimestamp(m map[string]any) *int64 {
	for _, name := range timeFieldNames {
		if v, ok := fieldValue(m, name); ok {
			if ms := toMillis(v); ms != nil {
				return ms
			}
		}
	}
	return nil
}

func toMillis(v any) *int64 {
	var ms int64
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		// 1) 纯数字字符串按 epoch 处理
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return normalizeEpoch(n)
		}
		// 2) 常见文本布局。ParseInLocation：带时区的布局用时区偏移，
		// 无时区布局按本地时区解析（time.Parse 会错误地按 UTC）。
		for _, layout := range timeLayouts {
			if t, err := time.ParseInLocation(layout, x, time.Local); err == nil {
				ms = t.UnixMilli()
				return &ms
			}
		}
		return nil
	case float64:
		return normalizeEpoch(int64(x))
	case int:
		return normalizeEpoch(int64(x))
	case int64:
		return normalizeEpoch(x)
	case jsoniter.Number: // 若解码器配置了 UseNumber
		if n, err := x.Int64(); err == nil {
			return normalizeEpoch(n)
		}
	}
	return nil
}

// normalizeEpoch 依量级判断单位：纳秒/微秒/毫秒/秒 → unix 毫秒。
func normalizeEpoch(n int64) *int64 {
	var ms int64
	switch {
	case n > 1e17: // 纳秒
		ms = n / 1e6
	case n > 1e14: // 微秒
		ms = n / 1e3
	case n > 1e11: // 毫秒
		ms = n
	default: // 秒
		ms = n * 1000
	}
	return &ms
}
