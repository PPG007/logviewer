package service_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"logviewer/internal/service"
)

// writeLog 在指定目录写一个日志文件，返回其路径。
func writeLog(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// openLog 打开文件并等待索引就绪，测试结束前关闭会话
// （会话持有文件句柄，Windows 下不关无法删除临时目录）。
func openLog(t *testing.T, svc *service.LogService, path string) service.FileInfo {
	t.Helper()
	info, err := svc.OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", path, err)
	}
	waitReady(t, svc, info.ID)
	t.Cleanup(func() { _ = svc.CloseFile(info.ID) })
	return info
}

// findRecent 按路径在历史记录里查一条。
func findRecent(t *testing.T, svc *service.LogService, path string) service.RecentFile {
	t.Helper()
	recs, err := svc.ListRecentFiles()
	if err != nil {
		t.Fatalf("ListRecentFiles: %v", err)
	}
	for _, r := range recs {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("历史记录中没有 %s（共 %d 条）", path, len(recs))
	return service.RecentFile{}
}

// waitRecentLines 等行数异步回写历史记录（索引完成后由后台 goroutine 写入）。
func waitRecentLines(t *testing.T, svc *service.LogService, path string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := findRecent(t, svc, path).TotalLines; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("历史记录行数未回写：%s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 打开文件写入历史记录：同名不同目录各记一条（靠 Dir 区分），
// 同一路径重复打开复用会话且只记一条；索引完成后行数写入记录与 GetIndexStatus。
func TestRecentFilesRecordAndDedupe(t *testing.T) {
	svc := service.NewLogService()
	lines := []string{`{"level":"INFO","msg":"a"}`, `{"level":"WARN","msg":"b"}`, `{"level":"INFO","msg":"c"}`}
	pa := writeLog(t, t.TempDir(), "app.log", lines)
	pb := writeLog(t, t.TempDir(), "app.log", lines)

	infoA := openLog(t, svc, pa)

	// 「行数显示为 0」的回归点：索引完成后状态里必须带上真实行数
	st, err := svc.GetIndexStatus(infoA.ID)
	if err != nil {
		t.Fatalf("GetIndexStatus: %v", err)
	}
	if st.TotalLines != int64(len(lines)) {
		t.Errorf("IndexStatus.TotalLines = %d, want %d", st.TotalLines, len(lines))
	}

	// 同一路径再次打开：复用会话（同一 fileID），不产生第二条记录
	again, err := svc.OpenFile(pa)
	if err != nil {
		t.Fatalf("OpenFile(a) again: %v", err)
	}
	if again.ID != infoA.ID {
		t.Errorf("同一路径未复用会话：%s != %s", again.ID, infoA.ID)
	}

	infoB := openLog(t, svc, pb)
	if infoB.ID == infoA.ID {
		t.Error("同名不同目录的文件应各自建立会话")
	}

	ra := findRecent(t, svc, pa)
	rb := findRecent(t, svc, pb)
	if ra.Name != "app.log" || rb.Name != "app.log" {
		t.Errorf("文件名不符：%q / %q", ra.Name, rb.Name)
	}
	if ra.Dir == rb.Dir {
		t.Errorf("同名文件未按目录区分：%q / %q", ra.Dir, rb.Dir)
	}
	if !ra.Exists || !rb.Exists {
		t.Error("文件就在磁盘上，Exists 应为 true")
	}

	// 同一路径记录只有一条（打开两次不重复计数，但打开次数累计）
	recs, _ := svc.ListRecentFiles()
	n := 0
	for _, r := range recs {
		if r.Path == pa {
			n++
		}
	}
	if n != 1 {
		t.Errorf("同一路径产生 %d 条记录，want 1", n)
	}
	if ra.OpenCount != 2 {
		t.Errorf("OpenCount = %d, want 2", ra.OpenCount)
	}

	waitRecentLines(t, svc, pa, int64(len(lines)))
}

// 文件从磁盘消失后：列表标记为已丢失，打开返回「文件不存在」，可移除记录。
func TestRecentFileMissing(t *testing.T) {
	svc := service.NewLogService()
	p := writeLog(t, t.TempDir(), "gone.log", []string{`{"msg":"x"}`})
	info := openLog(t, svc, p)
	rec := findRecent(t, svc, p)

	if err := svc.CloseFile(info.ID); err != nil { // 释放句柄后 Windows 才允许删除
		t.Fatalf("CloseFile: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	if got := findRecent(t, svc, p); got.Exists {
		t.Error("磁盘上文件已删除，Exists 应为 false")
	}
	if _, err := svc.OpenRecentFile(rec.ID); err == nil {
		t.Error("打开已丢失的文件应报错")
	} else if !strings.Contains(err.Error(), "文件不存在") {
		t.Errorf("错误信息应说明文件不存在，实际：%v", err)
	}

	if err := svc.RemoveRecentFile(rec.ID); err != nil {
		t.Fatalf("RemoveRecentFile: %v", err)
	}
	recs, err := svc.ListRecentFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == rec.ID {
			t.Fatalf("记录 %d 未被移除", rec.ID)
		}
	}
}

// 从历史记录重新打开：内容与首次打开一致，行数可用（关闭文件不删记录）。
func TestOpenRecentFile(t *testing.T) {
	svc := service.NewLogService()
	lines := []string{`{"level":"INFO","msg":"hello"}`, `{"level":"ERROR","msg":"bad"}`}
	p := writeLog(t, t.TempDir(), "reopen.jsonl", lines)
	info := openLog(t, svc, p)
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}

	rec := findRecent(t, svc, p)
	reopened, err := svc.OpenRecentFile(rec.ID)
	if err != nil {
		t.Fatalf("OpenRecentFile: %v", err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(reopened.ID) })
	waitReady(t, svc, reopened.ID)

	page, err := svc.GetLines(reopened.ID, 0, 10)
	if err != nil {
		t.Fatalf("GetLines: %v", err)
	}
	if page.Total != int64(len(lines)) || len(page.Rows) != len(lines) {
		t.Errorf("重开后行数不符：total=%d rows=%d", page.Total, len(page.Rows))
	}
}

// 临时日志不进历史记录（其临时文件在关闭时删除，记录无意义）。
func TestTempLogNotPersisted(t *testing.T) {
	svc := service.NewLogService()
	before, err := svc.ListRecentFiles()
	if err != nil {
		t.Fatal(err)
	}
	info, err := svc.OpenTempLog(`{"msg":"temp"}`)
	if err != nil {
		t.Fatalf("OpenTempLog: %v", err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(info.ID) })
	after, err := svc.ListRecentFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("临时日志被写入历史记录：%d -> %d 条", len(before), len(after))
	}
}
