package parse

import (
	"testing"
	"time"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		valid   bool
		wantKey string // 合法时断言某字段存在
	}{
		{"合法普通对象", `{"level":"info","msg":"hi"}`, true, "level"},
		{"带首尾空白", "  {\"a\":1}\r\n", true, "a"},
		{"嵌套对象", `{"http":{"status":200},"arr":[1,2]}`, true, "http"},
		{"空对象", "{}", true, ""},
		{"非法 JSON", `{"a": }`, false, ""},
		{"纯文本", "hello world", false, ""},
		{"空行", "", false, ""},
		{"只有空白", "   \n", false, ""},
		{"数组行(顶层非对象)", `[1,2,3]`, false, ""}, // 数组无法反序列化到 map
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := ParseLine(c.raw)
			if ok != c.valid {
				t.Fatalf("ParseLine(%q) ok=%v, want %v", c.raw, ok, c.valid)
			}
			if c.valid && c.wantKey != "" {
				if _, exists := m[c.wantKey]; !exists {
					t.Fatalf("缺少字段 %s: %v", c.wantKey, m)
				}
			}
		})
	}
	// 顶层数组应解析失败（map 反序列化）
	if m, ok := ParseLine("[1,2]"); ok {
		t.Fatalf("顶层数组不应解析为 map: %v", m)
	}
}

func TestParseLineNeverPanics(t *testing.T) {
	bad := []string{"{", "}", "[", `{"a":1`, "\x00\x01", `{"a":"\x00"}`}
	for _, raw := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseLine(%q) panic: %v", raw, r)
				}
			}()
			ParseLine(raw)
		}()
	}
}

func mustMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	m, ok := ParseLine(raw)
	if !ok {
		t.Fatalf("测试数据非法: %s", raw)
	}
	return m
}

func TestExtractTimestamp(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *int64 // nil 表示不识别
	}{
		// RFC3339 / RFC3339Nano
		{"RFC3339", `{"time":"2024-07-01T10:00:00+08:00"}`, ptr(int64(1719799200000))},
		{"RFC3339Nano", `{"time":"2024-07-01T10:00:00.123Z"}`, ptr(int64(1719828000123))},
		// 无时区布局按 time.Local 解析：期望值由本地时区推出
		{"空格布局", `{"time":"2024-07-01 10:00:00"}`, localMillis(2024, 7, 1, 10, 0, 0)},
		{"斜杠布局", `{"time":"2024/07/01 10:00:00"}`, localMillis(2024, 7, 1, 10, 0, 0)},
		// epoch 秒/毫秒/微秒/纳秒
		{"epoch 秒", `{"ts":1720000000}`, ptr(int64(1720000000000))},
		{"epoch 毫秒", `{"ts":1720000000123}`, ptr(int64(1720000000123))},
		{"epoch 微秒", `{"ts":1720000000123456}`, ptr(int64(1720000000123))},
		{"epoch 纳秒", `{"ts":1720000000123456789}`, ptr(int64(1720000000123))},
		{"epoch 秒字符串", `{"ts":"1720000000"}`, ptr(int64(1720000000000))},
		// 字段优先级与缺省
		{"timestamp 优先", `{"ts":1,"timestamp":1720000000}`, ptr(int64(1720000000000))},
		{"@timestamp", `{"@timestamp":"2024-07-01T10:00:00.000Z"}`, ptr(int64(1719828000000))},
		{"datetime", `{"datetime":"2024-07-01T10:00:00.000Z"}`, ptr(int64(1719828000000))},
		{"date 字段", `{"date":"2024-07-01T10:00:00.000Z"}`, ptr(int64(1719828000000))},
		{"无时间字段", `{"a":1}`, nil},
		{"时间值非法字符串", `{"time":"not-a-time"}`, nil},
		{"时间值为 bool", `{"time":true}`, nil},
		{"时间值为空串", `{"time":""}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractTimestamp(mustMap(t, c.raw))
			if c.want == nil {
				if got != nil {
					t.Fatalf("ExtractTimestamp = %d, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ExtractTimestamp = nil, want %d", *c.want)
			}
			if *got != *c.want {
				t.Fatalf("ExtractTimestamp = %d, want %d", *got, *c.want)
			}
		})
	}
}

// localMillis 按本地时区构造 unix 毫秒（与「无时区布局按 Local 解析」的实现对齐）。
func localMillis(y int, mo time.Month, d, h, mi, s int) *int64 {
	ms := time.Date(y, mo, d, h, mi, s, 0, time.Local).UnixMilli()
	return &ms
}

func ptr(i int64) *int64 { return &i }

func TestDetectLevel(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"level":"error"}`, "ERROR"},
		{`{"level":"Info"}`, "INFO"}, // 大写归一
		{`{"severity":"warn"}`, "WARN"},
		{`{"lvl":1,"severity":"debug"}`, "DEBUG"}, // 数值 level 跳过，走 severity
		{`{"level":"debug"}`, "DEBUG"},
		{`{"log_level":"trace"}`, "TRACE"},
		{`{"msg":"no level here"}`, ""},
		{`{"level":42}`, ""}, // 数值不作为级别
	}
	for _, c := range cases {
		if got := DetectLevel(mustMap(t, c.raw)); got != c.want {
			t.Errorf("DetectLevel(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestDetectMessage(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"msg":"hello"}`, "hello"},
		{`{"message":"world"}`, "world"},
		{`{"log":"log body"}`, "log body"},
		{`{"content":"内容"}`, "内容"},
		{`{"msg":"first","message":"second"}`, "first"}, // 优先级
		{`{"msg":42}`, "42"},                            // 非字符串格式化
		{`{"msg":true}`, "true"},
		{`{"a":"b"}`, ""},
		{`{"msg":{"nested":1}}`, "map[nested:1]"}, // 对象消息格式化为文本
	}
	for _, c := range cases {
		if got := DetectMessage(mustMap(t, c.raw)); got != c.want {
			t.Errorf("DetectMessage(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestCollectFields(t *testing.T) {
	m1 := mustMap(t, `{"a":"s","b":1,"c":true,"d":{"x":1},"e":[1,2],"f":null,"g":1.5}`)
	acc := map[string]string{}
	CollectFields(m1, acc)
	want1 := map[string]string{
		"a": "string", "b": "number", "c": "bool",
		"d": "object", "e": "array", "f": "null", "g": "number",
	}
	for k, v := range want1 {
		if acc[k] != v {
			t.Errorf("CollectFields %s = %q, want %q", k, acc[k], v)
		}
	}

	// 多行合并：新字段加入、同名覆盖
	m2 := mustMap(t, `{"a":123,"h":"new"}`)
	CollectFields(m2, acc)
	if acc["h"] != "string" {
		t.Errorf("h = %q, want string", acc["h"])
	}
	if acc["a"] != "number" {
		t.Errorf("a = %q, want number（后者覆盖）", acc["a"])
	}
	// 每行不同 key 时全部保留
	acc2 := map[string]string{}
	CollectFields(mustMap(t, `{"x":1}`), acc2)
	CollectFields(mustMap(t, `{"y":"s"}`), acc2)
	if len(acc2) != 2 || acc2["x"] != "number" || acc2["y"] != "string" {
		t.Errorf("多行合并去重错误: %v", acc2)
	}
}
