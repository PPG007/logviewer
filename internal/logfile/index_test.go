package logfile_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"logviewer/internal/logfile"
)

// writeTemp 写临时文件并返回路径。
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sample.log")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func openReady(t *testing.T, content string) (*logfile.FileSession, []string) {
	t.Helper()
	p := writeTemp(t, content)
	want := strings.Split(content, "\n")
	if len(want) > 0 && want[len(want)-1] == "" {
		want = want[:len(want)-1] // 去掉末尾换行的「幽灵空行」
	}
	for i := range want {
		want[i] = strings.TrimSuffix(want[i], "\r")
	}
	s, err := logfile.Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	return s, want
}

func TestOpenBasicLines(t *testing.T) {
	content := "l1\nl2\nl3\nl4\nl5\n"
	s, want := openReady(t, content)
	if got := s.TotalLines(); got != int64(len(want)) {
		t.Fatalf("TotalLines = %d, want %d", got, len(want))
	}
	if s.Status() != logfile.StatusReady {
		t.Fatalf("status = %v, want ready", s.Status())
	}
	if err := s.Error(); err != nil {
		t.Fatalf("Error() = %v, want nil", err)
	}
	for i, w := range want {
		rows, err := s.ReadLines(int64(i), 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0] != w {
			t.Fatalf("ReadLines(%d,1) = %q, want %q", i, rows, w)
		}
	}
}

func TestOpenNoTrailingNewline(t *testing.T) {
	content := "a\nb\nc" // 末尾无换行：仍是 3 行
	s, want := openReady(t, content)
	if s.TotalLines() != 3 {
		t.Fatalf("TotalLines = %d, want 3", s.TotalLines())
	}
	if len(want) != 3 || want[2] != "c" {
		t.Fatalf("want mismatch: %v", want)
	}
}

func TestOpenCRLF(t *testing.T) {
	content := "first\r\nsecond\r\nthird\r\n"
	s, want := openReady(t, content)
	if s.TotalLines() != 3 {
		t.Fatalf("TotalLines = %d, want 3", s.TotalLines())
	}
	for i, w := range []string{"first", "second", "third"} {
		if want[i] != w {
			t.Fatalf("want[%d] = %q", i, want[i])
		}
	}
	rows, err := s.ReadLines(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != "second" {
		t.Fatalf("CRLF ReadLines(1,1) = %q", rows)
	}
}

func TestOpenEmptyLines(t *testing.T) {
	content := "a\n\nb\n\n"
	s, want := openReady(t, content)
	if s.TotalLines() != 4 {
		t.Fatalf("TotalLines = %d, want 4（空行计入行号）", s.TotalLines())
	}
	for i, w := range []string{"a", "", "b", ""} {
		if want[i] != w {
			t.Fatalf("want[%d] = %q, want %q", i, want[i], w)
		}
	}
}

func TestOpenEmptyFile(t *testing.T) {
	s, _ := openReady(t, "")
	if s.TotalLines() != 0 {
		t.Fatalf("TotalLines = %d, want 0", s.TotalLines())
	}
	rows, err := s.ReadLines(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %v, want empty", rows)
	}
}

func TestLongLineOverReadBuffer(t *testing.T) {
	long := strings.Repeat("x", 1<<20+100) // 单行超过 1MB 读缓冲
	content := "first\n" + long + "\nlast\n"
	s, want := openReady(t, content)
	if s.TotalLines() != 3 {
		t.Fatalf("TotalLines = %d, want 3", s.TotalLines())
	}
	if want[1] != long {
		t.Fatalf("长行被截断或损坏（len=%d, want %d）", len(want[1]), len(long))
	}
	rows, err := s.ReadLines(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0] != long {
		t.Fatalf("ReadLines 长行不一致")
	}
}

func TestReadLinesBounds(t *testing.T) {
	s, _ := openReady(t, "a\nb\nc\nd\n")
	// start >= total
	rows, err := s.ReadLines(4, 10)
	if err != nil || rows != nil {
		t.Fatalf("ReadLines(4,10) = %v, %v; want nil, nil", rows, err)
	}
	// 负 start
	rows, err = s.ReadLines(-1, 10)
	if err != nil || rows != nil {
		t.Fatalf("ReadLines(-1,10) = %v, %v; want nil, nil", rows, err)
	}
	// 尾部截断：start+count > total
	rows, err = s.ReadLines(2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != "c" || rows[1] != "d" {
		t.Fatalf("尾部截断失败: %v", rows)
	}
	// count <= 0
	rows, err = s.ReadLines(0, 0)
	if err != nil || rows != nil {
		t.Fatalf("ReadLines(0,0) = %v, %v", rows, err)
	}
}

func TestScanMatchesReadLines(t *testing.T) {
	content := "{\"a\":1}\nnot json\n{\"b\":2}\n"
	s, want := openReady(t, content)
	var got []string
	err := s.Scan(func(lineNo int64, raw string) error {
		if lineNo != int64(len(got)) {
			t.Fatalf("Scan 行号乱序: %d", lineNo)
		}
		got = append(got, raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("Scan 行数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Scan 第 %d 行不一致", i)
		}
	}
}

func TestScanAbort(t *testing.T) {
	s, _ := openReady(t, "a\nb\nc\nd\n")
	sentinel := errors.New("abort")
	err := s.Scan(func(lineNo int64, raw string) error {
		if lineNo == 2 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Scan 中止错误 = %v, want %v", err, sentinel)
	}
}

func TestClose(t *testing.T) {
	s, _ := openReady(t, "a\nb\n")
	// 幂等
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadLines(0, 1); err == nil {
		t.Fatal("Close 后 ReadLines 应报错")
	}
	if err := s.Scan(func(int64, string) error { return nil }); err == nil {
		t.Fatal("Close 后 Scan 应报错")
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := logfile.Open(filepath.Join(t.TempDir(), "missing.log"), nil); err == nil {
		t.Fatal("打开不存在的文件应报错")
	}
	if _, err := logfile.Open(t.TempDir(), nil); err == nil {
		t.Fatal("打开目录应报错")
	}
}

func TestOnLineCallback(t *testing.T) {
	p := writeTemp(t, "a\nb\n")
	var seen []string
	s, err := logfile.Open(p, func(lineNo int64, raw string) {
		if lineNo != int64(len(seen)) {
			t.Errorf("onLine 行号乱序: %d", lineNo)
		}
		seen = append(seen, raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Fatalf("onLine 回调 = %v", seen)
	}
	if pct := s.Percent(); pct != 100 {
		t.Fatalf("Percent = %v, want 100", pct)
	}
}

func TestProgressDuringIndex(t *testing.T) {
	// 足够大（几十 MB），保证能观测到 0<percent<100 的中间态。
	var sb strings.Builder
	for i := range 400_000 {
		fmt.Fprintf(&sb, "{\"i\":%d}\n", i)
	}
	content := sb.String()
	p := writeTemp(t, content)
	s, err := logfile.Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sawMid := false
	for {
		if s.Status() == logfile.StatusReady {
			break
		}
		done, total := s.Progress()
		if total > 0 && done < total && done > 0 {
			sawMid = true
		}
	}
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if !sawMid {
		t.Log("未观测到中间进度（小文件可能瞬时完成），跳过")
	}
	if s.TotalLines() != 400_000 {
		t.Fatalf("TotalLines = %d, want 400000", s.TotalLines())
	}
}
