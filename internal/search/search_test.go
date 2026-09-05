package search_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"logviewer/internal/logfile"
	"logviewer/internal/parse"
	"logviewer/internal/search"
)

func ptr(i int64) *int64 { return &i }

func mustMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	m, ok := parse.ParseLine(raw)
	if !ok {
		t.Fatalf("测试数据非法: %s", raw)
	}
	return m
}

// ---------------- Matcher 语义（对照设计 §6） ----------------

func matchOne(t *testing.T, raw string, q search.Query) bool {
	t.Helper()
	if err := q.Validate(); err != nil {
		t.Fatalf("查询非法: %v", err)
	}
	m, ok := parse.ParseLine(raw)
	var ts *int64
	if ok {
		ts = parse.ExtractTimestamp(m)
	}
	return search.NewMatcher(q).Match(raw, m, ts)
}

func TestMatcherOperators(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		q    search.Query
		want bool
	}{
		// =
		{"相等命中", `{"level":"ERROR"}`, search.Query{Conditions: []search.Condition{{"level", "=", "ERROR"}}}, true},
		{"相等不中", `{"level":"INFO"}`, search.Query{Conditions: []search.Condition{{"level", "=", "ERROR"}}}, false},
		{"数值转字符串相等", `{"status":200}`, search.Query{Conditions: []search.Condition{{"status", "=", "200"}}}, true},
		{"数值不相等", `{"status":201}`, search.Query{Conditions: []search.Condition{{"status", "=", "200"}}}, false},
		{"bool 相等", `{"ok":true}`, search.Query{Conditions: []search.Condition{{"ok", "=", "true"}}}, true},
		// !=
		{"不等命中", `{"status":201}`, search.Query{Conditions: []search.Condition{{"status", "!=", "200"}}}, true},
		{"不等不中", `{"status":200}`, search.Query{Conditions: []search.Condition{{"status", "!=", "200"}}}, false},
		// contains
		{"包含命中", `{"msg":"request timeout"}`, search.Query{Conditions: []search.Condition{{"msg", "contains", "timeout"}}}, true},
		{"包含不中", `{"msg":"request ok"}`, search.Query{Conditions: []search.Condition{{"msg", "contains", "timeout"}}}, false},
		// > <
		{"大于命中", `{"latency":1500}`, search.Query{Conditions: []search.Condition{{"latency", ">", "1000"}}}, true},
		{"大于不中", `{"latency":500}`, search.Query{Conditions: []search.Condition{{"latency", ">", "1000"}}}, false},
		{"小于命中", `{"latency":500}`, search.Query{Conditions: []search.Condition{{"latency", "<", "1000"}}}, true},
		{"等值不中(开区间)", `{"latency":1000}`, search.Query{Conditions: []search.Condition{{"latency", ">", "1000"}}}, false},
		{"非数值不中", `{"latency":"slow"}`, search.Query{Conditions: []search.Condition{{"latency", ">", "1000"}}}, false},
		{"字段数值非法值不中", `{"latency":1500}`, search.Query{Conditions: []search.Condition{{"latency", ">", "abc"}}}, false},
		{"字符串值比较两边都非数值不中", `{"a":"xyz"}`, search.Query{Conditions: []search.Condition{{"a", ">", "1"}}}, false},
		{"对象字段转字符串 contains", `{"data":{"x":1}}`, search.Query{Conditions: []search.Condition{{"data", "contains", "map[x:1]"}}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchOne(t, c.raw, c.q); got != c.want {
				t.Fatalf("Match = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMatcherMissingField(t *testing.T) {
	raw := `{"level":"ERROR","msg":"hi"}`
	missing := []struct {
		op   string
		want bool
	}{
		{"=", false}, {"contains", false}, {">", false}, {"<", false}, {"!=", true},
	}
	for _, c := range missing {
		q := search.Query{Conditions: []search.Condition{{"nope", c.op, "x"}}}
		if got := matchOne(t, raw, q); got != c.want {
			t.Errorf("字段不存在 op=%s: got %v, want %v", c.op, got, c.want)
		}
	}
}

func TestMatcherNullValue(t *testing.T) {
	raw := `{"a":null}`
	// key 存在值为 null：按「存在」处理，fmt.Sprint(nil)=="<nil>"
	if !matchOne(t, raw, search.Query{Conditions: []search.Condition{{"a", "=", "<nil>"}}}) {
		t.Error(`a=null 应命中 "=" "<nil>"`)
	}
	if matchOne(t, raw, search.Query{Conditions: []search.Condition{{"a", "=", "null"}}}) {
		t.Error(`a=null 不应命中 "=" "null"`)
	}
	if !matchOne(t, raw, search.Query{Conditions: []search.Condition{{"a", "!=", "null"}}}) {
		t.Error(`a=null 应命中 "!=" "null"（存在且值不同）`)
	}
}

func TestMatcherKeywordAndInvalid(t *testing.T) {
	// 关键字命中原始文本（含字段名与值）
	if !matchOne(t, `{"trace_id":"abc123","msg":"x"}`, search.Query{Keyword: "trace_id"}) {
		t.Error("关键字应命中原始文本")
	}
	// 非法 JSON 行：字段条件不命中，关键字可命中
	invalid := `this line contains trace_id=abc123 but is not json`
	if matchOne(t, invalid, search.Query{Conditions: []search.Condition{{"level", "=", "ERROR"}}}) {
		t.Error("非法行不应命中字段条件")
	}
	if !matchOne(t, invalid, search.Query{Keyword: "abc123"}) {
		t.Error("非法行应命中关键字")
	}
	// 非法 JSON 行 + != 字段条件 = 字段不存在 → 匹配（语义成立）
	if !matchOne(t, invalid, search.Query{Conditions: []search.Condition{{"level", "!=", "ERROR"}}}) {
		t.Error("非法行 + != 应视为字段不存在而匹配")
	}
}

func TestMatcherTimeRange(t *testing.T) {
	base := int64(1_720_000_000_000) // 2024-07-03 左右
	raw := fmt.Sprintf(`{"time":%d}`, base)
	// 边界含
	if !matchOne(t, raw, search.Query{TimeRange: &search.TimeRange{From: &base, To: &base}}) {
		t.Error("from==to==ts 应命中")
	}
	if !matchOne(t, raw, search.Query{TimeRange: &search.TimeRange{From: ptr(base - 1), To: ptr(base + 1)}}) {
		t.Error("ts 在范围内应命中")
	}
	if matchOne(t, raw, search.Query{TimeRange: &search.TimeRange{From: ptr(base + 1), To: nil}}) {
		t.Error("ts < from 不命中")
	}
	if matchOne(t, raw, search.Query{TimeRange: &search.TimeRange{From: nil, To: ptr(base - 1)}}) {
		t.Error("ts > to 不命中")
	}
	// 无时间戳 + 时间范围 → 不命中
	if matchOne(t, `{"a":1}`, search.Query{TimeRange: &search.TimeRange{From: nil, To: nil}}) {
		t.Error("无时间戳行 + 时间范围应不命中")
	}
	// 无时间范围时无时间戳行可命中
	if !matchOne(t, `{"a":1}`, search.Query{}) {
		t.Error("空查询应命中所有行")
	}
	// 文本时间戳
	if !matchOne(t, `{"time":"2024-07-01T00:00:00Z"}`, search.Query{TimeRange: &search.TimeRange{From: ptr(1719792000000), To: ptr(1719792000000)}}) {
		t.Error("文本时间戳范围应命中")
	}
}

func TestMatcherCombination(t *testing.T) {
	raw := `{"time":1720000000000,"level":"ERROR","latency":2000,"msg":"timeout at 10:00"}`
	q := search.Query{
		Conditions: []search.Condition{
			{"level", "=", "ERROR"},
			{"latency", ">", "1000"},
		},
		Keyword: "timeout",
		TimeRange: &search.TimeRange{
			From: ptr(1720000000000), To: ptr(1720000000000),
		},
	}
	if !matchOne(t, raw, q) {
		t.Error("全部条件满足应命中")
	}
	// 任一条件不满足 → 不命中
	q2 := q
	q2.Keyword = "nonexistent"
	if matchOne(t, raw, q2) {
		t.Error("关键字不满足应不命中")
	}
	q3 := q
	q3.TimeRange = &search.TimeRange{From: ptr(1720000000001), To: nil}
	if matchOne(t, raw, q3) {
		t.Error("时间范围不满足应不命中")
	}
}

// ---------------- Query 校验 ----------------

func TestQueryValidate(t *testing.T) {
	good := []search.Query{
		{},
		{Conditions: []search.Condition{{"level", "=", "ERROR"}}},
		{Conditions: []search.Condition{{"latency", ">", "1"}, {"msg", "contains", "x"}}},
		{Keyword: "k", TimeRange: &search.TimeRange{}},
	}
	for i, q := range good {
		if err := q.Validate(); err != nil {
			t.Errorf("good query #%d: unexpected error %v", i, err)
		}
	}
	bad := []struct {
		name string
		q    search.Query
	}{
		{"空字段名", search.Query{Conditions: []search.Condition{{"", "=", "x"}}}},
		{"非法运算符", search.Query{Conditions: []search.Condition{{"a", "regex", "x"}}}},
		{"空运算符", search.Query{Conditions: []search.Condition{{"a", "", "x"}}}},
	}
	for _, c := range bad {
		if err := c.q.Validate(); err == nil {
			t.Errorf("%s 应校验失败", c.name)
		}
	}
}

// ---------------- Engine（真实文件） ----------------

func writeJSONLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "data.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openSession(t *testing.T, lines []string) *logfile.FileSession {
	t.Helper()
	s, err := logfile.Open(writeJSONLines(t, lines), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	return s
}

// 编译期断言：FileSession 满足 search.Scanner。
var _ search.Scanner = (*logfile.FileSession)(nil)

func TestEngineSearchAndPage(t *testing.T) {
	var lines []string
	// 10 行：level=ERROR 出现在行 1、4、8（0-based）
	for i := range 10 {
		lv := "INFO"
		if i == 1 || i == 4 || i == 8 {
			lv = "ERROR"
		}
		lines = append(lines, fmt.Sprintf(`{"time":%d,"level":"%s","latency":%d,"msg":"line %d"}`, 1_720_000_000_000+i, lv, 100+i, i))
	}
	s := openSession(t, lines)
	e := search.NewEngine()

	matched, err := e.Search(s, "tab-1", search.Query{Conditions: []search.Condition{{"level", "=", "ERROR"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 3 || matched[0] != 1 || matched[1] != 4 || matched[2] != 8 {
		t.Fatalf("命中行号 = %v, want [1 4 8]", matched)
	}
	if e.Total("tab-1") != 3 {
		t.Fatalf("Total = %d, want 3", e.Total("tab-1"))
	}
	page, err := e.Page("tab-1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0] != 1 || page[1] != 4 {
		t.Fatalf("第 1 页 = %v, want [1 4]", page)
	}
	page, err = e.Page("tab-1", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0] != 8 {
		t.Fatalf("第 2 页 = %v, want [8]", page)
	}
	// 越界页空
	page, _ = e.Page("tab-1", 99, 2)
	if page != nil {
		t.Fatalf("越界页 = %v, want nil", page)
	}
	// 不存在的 tab
	if page, _ := e.Page("nope", 1, 10); page != nil {
		t.Fatalf("未知 tab 页 = %v, want nil", page)
	}
	if e.Total("nope") != 0 {
		t.Fatalf("未知 tab Total = %d, want 0", e.Total("nope"))
	}

	// 同 tab 再次检索：旧结果被替换
	matched, err = e.Search(s, "tab-1", search.Query{Conditions: []search.Condition{{"latency", ">", "104"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 5 {
		t.Fatalf("第二次检索命中 = %d, want 5（latency>104 共 5 行）", len(matched))
	}

	// Remove 后结果释放
	e.Remove("tab-1")
	if e.Total("tab-1") != 0 {
		t.Fatal("Remove 后 Total 应为 0")
	}
}

func TestEngineKeywordHitsInvalidLines(t *testing.T) {
	lines := []string{
		`{"level":"INFO","msg":"ok"}`,
		`garbage with needle inside`,
		`{"level":"ERROR","msg":"needle here"}`,
		`plain text needle`,
	}
	s := openSession(t, lines)
	e := search.NewEngine()
	matched, err := e.Search(s, "kw", search.Query{Keyword: "needle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 3 || matched[0] != 1 || matched[1] != 2 || matched[2] != 3 {
		t.Fatalf("关键字命中 = %v, want [1 2 3]", matched)
	}
}

func TestEngineEmptyQueryMatchesAll(t *testing.T) {
	lines := []string{`{"a":1}`, `not json`, `{"b":2}`}
	s := openSession(t, lines)
	e := search.NewEngine()
	matched, err := e.Search(s, "all", search.Query{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 3 {
		t.Fatalf("空查询应命中全部，got %v", matched)
	}
}

func TestEngineSearchErrorNoCache(t *testing.T) {
	s := openSession(t, []string{`{"level":"ERROR"}`})
	e := search.NewEngine()
	// 非法查询不产生缓存
	if _, err := e.Search(s, "bad", search.Query{Conditions: []search.Condition{{"a", "regex", "x"}}}, nil); err == nil {
		t.Fatal("非法运算符应报错")
	}
	if e.Total("bad") != 0 {
		t.Fatal("非法查询不应有缓存")
	}
}

// ---------------- Engine 取消（门控 Scanner，确定性） ----------------

type gatedScanner struct {
	entered chan struct{}
	release chan struct{}
	once    bool
}

func (g *gatedScanner) TotalLines() int64 { return 1_000_000 }

func (g *gatedScanner) Scan(fn func(int64, string) error) error {
	if !g.once {
		g.once = true
		close(g.entered)
		<-g.release
	}
	for i := int64(0); i < g.TotalLines(); i++ {
		if err := fn(i, `{"level":"INFO","msg":"x"}`); err != nil {
			return err
		}
	}
	return nil
}

func TestEngineCancel(t *testing.T) {
	gs := &gatedScanner{entered: make(chan struct{}), release: make(chan struct{})}
	e := search.NewEngine()
	done := make(chan error, 1)
	go func() {
		_, err := e.Search(gs, "c1", search.Query{Keyword: "x"}, nil)
		done <- err
	}()
	<-gs.entered // 扫描已开始并被门控阻塞
	e.Cancel("c1")
	close(gs.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后错误 = %v, want context.Canceled", err)
	}
	if e.Total("c1") != 0 {
		t.Fatal("取消后不应有结果缓存")
	}
	if page, _ := e.Page("c1", 1, 10); page != nil {
		t.Fatal("取消后 Page 应为空")
	}
	// cancels 已清理：再次 Cancel 不 panic
	e.Cancel("c1")
	e.Remove("c1")
}

func TestEngineAll(t *testing.T) {
	s := openSession(t, []string{`{"level":"INFO"}`, `{"level":"ERROR"}`, `{"level":"INFO"}`})
	e := search.NewEngine()
	if _, err := e.Search(s, "all-tab", search.Query{Conditions: []search.Condition{{"level", "=", "INFO"}}}, nil); err != nil {
		t.Fatal(err)
	}
	all := e.All("all-tab")
	if len(all) != 2 || all[0] != 0 || all[1] != 2 {
		t.Fatalf("All = %v, want [0 2]", all)
	}
	// 返回副本：外部修改不影响引擎内部
	all[0] = 99
	if got := e.All("all-tab"); got[0] != 0 {
		t.Fatalf("All 副本被外部污染: %v", got)
	}
	if len(e.All("nope")) != 0 {
		t.Fatal("未知 tab All 长度应为 0")
	}
}

func TestEngineCancelBeforeScanStarts(t *testing.T) {
	// 取消已完成的搜索是空操作；未注册的 tab 取消也不报错
	e := search.NewEngine()
	e.Cancel("ghost")
}
