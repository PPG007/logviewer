package service_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"logviewer/internal/search"
	"logviewer/internal/service"
)

// 说明：单测环境无 Wails app（application.Get()==nil），事件发射被静默跳过，
// 进度走 GetIndexStatus 轮询验证。

func writeLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "svc.log")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitReady(t *testing.T, svc *service.LogService, fileID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := svc.GetIndexStatus(fileID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Done {
			if st.Error != "" {
				t.Fatalf("索引失败: %s", st.Error)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("等待索引完成超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenAndBrowse(t *testing.T) {
	var lines []string
	for i := range 200 {
		lv := "INFO"
		if i%10 == 0 {
			lv = "ERROR"
		}
		lines = append(lines, fmt.Sprintf(`{"time":1720000000000,"level":"%s","msg":"line %d","n":%d}`, lv, i, i))
	}
	p := writeLines(t, lines)

	svc := service.NewLogService()
	info, err := svc.OpenFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.Name != "svc.log" {
		t.Fatalf("FileInfo = %+v", info)
	}
	if info.Status != "indexing" && info.Status != "ready" {
		t.Fatalf("初始状态应为 indexing/ready: %s", info.Status)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })

	waitReady(t, svc, info.ID)

	// 状态对账
	st, err := svc.GetIndexStatus(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done || st.Percent != 100 {
		t.Fatalf("IndexStatus = %+v", st)
	}

	// 字段
	fields, err := svc.GetFields(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, f := range fields {
		byName[f.Name] = f.Type
	}
	for k, want := range map[string]string{"time": "number", "level": "string", "msg": "string", "n": "number"} {
		if byName[k] != want {
			t.Errorf("字段 %s = %q, want %q（全字段: %v）", k, byName[k], want, byName)
		}
	}

	// 浏览分页
	pg, err := svc.GetLines(info.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 200 || len(pg.Rows) != 100 {
		t.Fatalf("GetLines Total=%d rows=%d, want 200/100", pg.Total, len(pg.Rows))
	}
	if pg.Rows[0].LineNo != 0 || pg.Rows[0].Level != "ERROR" || pg.Rows[0].Message != "line 0" || !pg.Rows[0].Valid {
		t.Fatalf("首行解析错误: %+v", pg.Rows[0])
	}
	if pg.Rows[0].Timestamp == nil || *pg.Rows[0].Timestamp != 1720000000000 {
		t.Fatalf("时间戳解析错误: %v", pg.Rows[0].Timestamp)
	}
	// 第二页
	pg2, err := svc.GetLines(info.ID, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg2.Rows) != 100 || pg2.Rows[0].LineNo != 100 {
		t.Fatalf("第二页错误: first=%d len=%d", pg2.Rows[0].LineNo, len(pg2.Rows))
	}
	// 越界空
	pg3, err := svc.GetLines(info.ID, 300, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg3.Rows) != 0 {
		t.Fatalf("越界页应为空")
	}
}

func TestSearchAndPage(t *testing.T) {
	var lines []string
	for i := range 500 {
		lv := "INFO"
		if i%5 == 0 {
			lv = "ERROR"
		}
		lines = append(lines, fmt.Sprintf(`{"time":1720000000000,"level":"%s","msg":"line %d","latency":%d}`, lv, i, 100+i))
	}
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	q := search.Query{Conditions: []search.Condition{
		{Field: "level", Op: "=", Value: "ERROR"},
		{Field: "latency", Op: ">", Value: "400"},
	}}
	res, err := svc.Search(info.ID, "tab-a", q, 100)
	if err != nil {
		t.Fatal(err)
	}
	// level=ERROR（i%5==0）且 latency>400（i>300）→ 305..495 步长 5 → 39 行
	if res.Total != 39 {
		t.Fatalf("Search Total = %d, want 39", res.Total)
	}
	if len(res.Rows) != 39 { // 不足一页全量返回
		t.Fatalf("第一页 rows = %d, want 39", len(res.Rows))
	}
	if res.Rows[0].LineNo != 305 {
		t.Fatalf("首条命中行号 = %d, want 305", res.Rows[0].LineNo)
	}

	// 时间范围 + 关键字（"line 499" 是唯一子串；时间范围闭区间含该行时间）
	q2 := search.Query{Keyword: "line 499", TimeRange: &search.TimeRange{From: ptr(int64(1720000000000)), To: ptr(int64(1720000000000))}}
	res2, err := svc.Search(info.ID, "tab-b", q2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Total != 1 || res2.Rows[0].LineNo != 499 {
		t.Fatalf("关键字+时间范围结果错误: total=%d first=%d", res2.Total, res2.Rows[0].LineNo)
	}

	// 翻页（按前端 pageSize=20 语义）：tab-c 共 400 命中，第 3 页 = 命中序 [40..59]
	q3 := search.Query{Conditions: []search.Condition{{Field: "level", Op: "=", Value: "INFO"}}} // 400 条
	if _, err := svc.Search(info.ID, "tab-c", q3, 100); err != nil {
		t.Fatal(err)
	}
	pg, err := svc.GetPage(info.ID, "tab-c", 3, 20)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 400 || len(pg.Rows) != 20 {
		t.Fatalf("GetPage Total=%d rows=%d, want 400/20", pg.Total, len(pg.Rows))
	}
	// 第 3 页首行 = 命中序 40（0-based）的原始行号
	wantLine := int64(0)
	count := 0
	for i := 0; ; i++ {
		if i%5 != 0 {
			if count == 40 {
				wantLine = int64(i)
				break
			}
			count++
		}
	}
	if pg.Rows[0].LineNo != wantLine {
		t.Fatalf("第 3 页首行 = %d, want %d", pg.Rows[0].LineNo, wantLine)
	}
	// 越界页空
	pgEmpty, err := svc.GetPage(info.ID, "tab-c", 999, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pgEmpty.Rows) != 0 {
		t.Fatalf("越界页应为空")
	}
}

func TestCloseFile(t *testing.T) {
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, []string{`{"level":"ERROR","msg":"x"}`}))
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)
	if _, err := svc.Search(info.ID, "tab-x", search.Query{Keyword: "x"}, 100); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}
	// 关闭后所有操作报 file not found
	if _, err := svc.GetLines(info.ID, 0, 10); err == nil {
		t.Fatal("关闭后 GetLines 应报错")
	}
	if _, err := svc.GetIndexStatus(info.ID); err == nil {
		t.Fatal("关闭后 GetIndexStatus 应报错")
	}
	if _, err := svc.Search(info.ID, "tab-x", search.Query{}, 100); err == nil {
		t.Fatal("关闭后 Search 应报错")
	}
	// 重复关闭
	if err := svc.CloseFile(info.ID); err == nil {
		t.Fatal("重复关闭应报 file not found")
	}
}

func TestCancelSearch(t *testing.T) {
	// 无进行中检索时取消是空操作，不报错
	svc := service.NewLogService()
	if err := svc.CancelSearch("ghost-file", "ghost-tab"); err != nil {
		t.Fatal(err)
	}
	info, err := svc.OpenFile(writeLines(t, []string{`{"level":"ERROR"}`}))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(info.ID)
	waitReady(t, svc, info.ID)
	if err := svc.CancelSearch(info.ID, "tab-a"); err != nil {
		t.Fatal(err)
	}
}

func TestBadLineHandling(t *testing.T) {
	svc := service.NewLogService()
	lines := []string{`{"level":"INFO","msg":"good"}`, `this is not json`, `{"level":"ERROR","msg":"err"}`}
	info, err := svc.OpenFile(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(info.ID)
	waitReady(t, svc, info.ID)
	pg, err := svc.GetLines(info.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pg.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(pg.Rows))
	}
	if pg.Rows[1].Valid || pg.Rows[1].JSON != nil {
		t.Fatalf("坏行应 Valid=false JSON=nil: %+v", pg.Rows[1])
	}
	if pg.Rows[1].Raw != "this is not json" {
		t.Fatalf("坏行原始文本 = %q", pg.Rows[1].Raw)
	}
}

func TestRemoveTab(t *testing.T) {
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, []string{`{"level":"ERROR","msg":"x"}`, `{"level":"INFO","msg":"y"}`}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)
	if _, err := svc.Search(info.ID, "tab-x", search.Query{Conditions: []search.Condition{{Field: "level", Op: "=", Value: "ERROR"}}}, 100); err != nil {
		t.Fatal(err)
	}
	// RemoveTab 后：命中缓存释放（Total 0、页空）、再检索可用、文件关闭正常
	if err := svc.RemoveTab(info.ID, "tab-x"); err != nil {
		t.Fatal(err)
	}
	pg, err := svc.GetPage(info.ID, "tab-x", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 0 || len(pg.Rows) != 0 {
		t.Fatalf("RemoveTab 后 GetPage = %+v, want 空", pg)
	}
	if _, err := svc.Search(info.ID, "tab-x", search.Query{Keyword: "y"}, 100); err != nil {
		t.Fatalf("同 id 再次检索应可用: %v", err)
	}
	// 未知文件报错；重复 Remove 是空操作
	if err := svc.RemoveTab("ghost-file", "tab-x"); err == nil {
		t.Fatal("未知文件 RemoveTab 应报错")
	}
	if err := svc.RemoveTab(info.ID, "tab-x"); err != nil {
		t.Fatalf("重复 RemoveTab 应无副作用: %v", err)
	}
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatalf("RemoveTab 后 CloseFile 应正常: %v", err)
	}
}

func ptr(i int64) *int64 { return &i }

// ---------------- 临时日志（OpenTempLog） ----------------

func TestOpenTempLog(t *testing.T) {
	var lines []string
	for i := range 30 {
		lines = append(lines, fmt.Sprintf(`{"time":1720000000000,"level":"WARN","msg":"paste %d"}`, i))
	}
	content := strings.Join(lines, "\n") + "\n"

	svc := service.NewLogService()
	info, err := svc.OpenTempLog(content)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	if info.Name == "" || !strings.HasPrefix(info.Name, "临时日志 ") {
		t.Fatalf("展示名应为「临时日志 …」: %q", info.Name)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("临时文件应已落盘: %v", err)
	}
	waitReady(t, svc, info.ID)

	// 与普通文件一致的浏览/检索链路
	pg, err := svc.GetLines(info.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pg.Total != 30 {
		t.Fatalf("Total = %d, want 30", pg.Total)
	}
	res, err := svc.Search(info.ID, "tab-tmp", search.Query{Conditions: []search.Condition{{Field: "level", Op: "=", Value: "WARN"}}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 30 || len(res.Rows) != 30 {
		t.Fatalf("检索命中 = %d/%d, want 30/30", res.Total, len(res.Rows))
	}

	// 关闭后临时文件被清理（顶部的 t.Cleanup 会再 Close 一次，重复关闭静默返回，无碍）
	path := info.Path
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("CloseFile 后临时文件应被删除: %v", err)
	}
}

func TestOpenTempLogLimits(t *testing.T) {
	svc := service.NewLogService()

	// 空内容
	if _, err := svc.OpenTempLog("   \n\n "); err == nil {
		t.Fatal("空内容应报错")
	}

	// 行数超限（50,000 行）
	overLines := strings.Repeat("a\n", 50_001)
	if _, err := svc.OpenTempLog(overLines); err == nil || !strings.Contains(err.Error(), "行数") {
		t.Fatalf("行数超限应报错且提示行数: %v", err)
	}
	// 恰在上限内可通过（内容不计解析，只验证放行与可关闭）
	atLimit := strings.Repeat("a\n", 50_000-1) + "b"
	info, err := svc.OpenTempLog(atLimit)
	if err != nil {
		t.Fatalf("上限内应可创建: %v", err)
	}
	waitReady(t, svc, info.ID)
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}

	// 体积超限（10 MiB 无换行长文本）
	big := strings.Repeat("x", 10<<20+1)
	if _, err := svc.OpenTempLog(big); err == nil || !strings.Contains(err.Error(), "体积") {
		t.Fatalf("体积超限应报错: %v", err)
	}
}

// ---------------- 字段取值枚举（GetFieldValues） ----------------

func TestFieldValues(t *testing.T) {
	var lines []string
	// level 原样收集（不做大小写归一）："INFO" 与 "info" 是两个独立取值
	levels := []string{"ERROR", "WARN", "info", "INFO", "DEBUG"}
	for i := range 60 {
		lines = append(lines, fmt.Sprintf(
			`{"time":1720000000000,"level":"%s","n":%d,"msg":"m%d","ok":true}`,
			levels[i%len(levels)], i, i))
	}
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	fv, err := svc.GetFieldValues(info.ID, "level")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"DEBUG", "ERROR", "INFO", "WARN", "info"} // 字节序字母排序
	if fv.Truncated || fmt.Sprint(fv.Values) != fmt.Sprint(want) {
		t.Fatalf("level 取值 = %v (truncated=%v), want %v", fv.Values, fv.Truncated, want)
	}

	// 非 string 字段 / 未知字段：空且未截断
	for _, f := range []string{"n", "ok", "ghost"} {
		got, err := svc.GetFieldValues(info.ID, f)
		if err != nil {
			t.Fatal(err)
		}
		if got.Truncated || len(got.Values) != 0 {
			t.Fatalf("字段 %s 取值 = %v (truncated=%v), want 空", f, got.Values, got.Truncated)
		}
	}

	// 关闭后报 file not found
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetFieldValues(info.ID, "level"); err == nil {
		t.Fatal("关闭后 GetFieldValues 应报错")
	}
}

func TestFieldValuesTruncated(t *testing.T) {
	// msg 高基数（distinct 超过 200）→ 截断且整体丢弃
	var lines []string
	for i := range 300 {
		lines = append(lines, fmt.Sprintf(`{"level":"ERROR","msg":"unique-message-%d"}`, i))
	}
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(info.ID)
	waitReady(t, svc, info.ID)

	msg, err := svc.GetFieldValues(info.ID, "msg")
	if err != nil {
		t.Fatal(err)
	}
	if !msg.Truncated || len(msg.Values) != 0 {
		t.Fatalf("msg 应截断为空: %v (truncated=%v)", msg.Values, msg.Truncated)
	}
	// 同文件内枚举字段不受影响
	lv, err := svc.GetFieldValues(info.ID, "level")
	if err != nil {
		t.Fatal(err)
	}
	if lv.Truncated || fmt.Sprint(lv.Values) != "[ERROR]" {
		t.Fatalf("level 应完整: %v (truncated=%v)", lv.Values, lv.Truncated)
	}
}

func TestFieldValuesAtLimit(t *testing.T) {
	// 恰 200 个 distinct 值：不截断、全量返回
	var lines []string
	for i := range 200 {
		lines = append(lines, fmt.Sprintf(`{"tag":"t%d"}`, i))
	}
	svc := service.NewLogService()
	info, err := svc.OpenFile(writeLines(t, lines))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(info.ID)
	waitReady(t, svc, info.ID)

	fv, err := svc.GetFieldValues(info.ID, "tag")
	if err != nil {
		t.Fatal(err)
	}
	if fv.Truncated || len(fv.Values) != 200 {
		t.Fatalf("恰在上限应完整返回: len=%d truncated=%v", len(fv.Values), fv.Truncated)
	}
	if fv.Values[0] != "t0" || fv.Values[199] != "t99" {
		t.Fatalf("字母序异常: %v", fv.Values[:2])
	}
}
