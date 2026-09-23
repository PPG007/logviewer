package logfile_test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"logviewer/internal/logfile"
)

// countingSource 统计 ReadAt 调用次数：远端来源上每次 ReadAt 就是一次网络往返，
// 用它断言「翻页不再逐行往返」。
type countingSource struct {
	logfile.Source
	readAts int64
}

func (c *countingSource) ReadAt(b []byte, off int64) (int, error) {
	atomic.AddInt64(&c.readAts, 1)
	return c.Source.ReadAt(b, off)
}

func (c *countingSource) calls() int64 { return atomic.LoadInt64(&c.readAts) }

// openCounting 打开一份内存来源并等索引完成。
func openCounting(t *testing.T, content string) (*logfile.FileSession, *countingSource) {
	t.Helper()
	cs := &countingSource{Source: &memSource{data: []byte(content)}}
	s, err := logfile.OpenSource(cs, "/remote/big.log", "big.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	return s, cs
}

// 一页（远小于窗口）只需一次随机读，而不是每行一次。
func TestReadLinesUsesSingleWindowRead(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	s, cs := openCounting(t, sb.String())
	if s.TotalLines() != 5000 {
		t.Fatalf("TotalLines = %d", s.TotalLines())
	}
	base := cs.calls()

	lines, err := s.ReadLines(1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 100 || lines[0] != "line-1000" || lines[99] != "line-1099" {
		t.Fatalf("内容不对：%v … %v", lines[0], lines[len(lines)-1])
	}
	if got := cs.calls() - base; got != 1 {
		t.Fatalf("100 行的翻页用了 %d 次随机读，应为 1 次（窗口内一次读回）", got)
	}
}

// 跨越多个窗口的大区间：读次数应约为「区间/窗口」，而不是行数。
func TestReadLinesLargeSpanReadsByWindow(t *testing.T) {
	// 每行 100 字节 → 40 万行 ≈ 40MB ≈ 5 个窗口
	const lines = 400_000
	row := strings.Repeat("x", 98) + "\n"
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		sb.WriteString(row)
	}
	s, cs := openCounting(t, sb.String())
	base := cs.calls()

	got, err := s.ReadLines(0, lines) // 导出场景：连续命中合并成一次大区间调用
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != lines {
		t.Fatalf("返回行数 = %d, want %d", len(got), lines)
	}
	if got[0] != strings.Repeat("x", 98) || got[lines-1] != strings.Repeat("x", 98) {
		t.Fatal("内容不正确")
	}
	// 区间约 40MB，窗口 1MB（readWindowBytes）→ 预期约 40 次，远少于 40 万行
	calls := cs.calls() - base
	const wantMax = 50
	if calls > wantMax {
		t.Fatalf("%d 行的连续区间用了 %d 次随机读，应约为「区间/1MB」（≤%d）", lines, calls, wantMax)
	}
	if calls < 2 {
		t.Fatalf("40MB 的区间只用了 %d 次随机读，窗口上限似乎没生效", calls)
	}
}

// 窗口边界处的内容必须与逐行读完全一致（跨窗口拼接不能错位）。
func TestReadLinesWindowBoundary(t *testing.T) {
	// 每行正好 1024 字节，读满 8MB 窗口的边界必然落在行中间
	const lines = 9000 // ≈ 9MB，跨一个窗口边界
	payload := strings.Repeat("y", 1023)
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&sb, "%s%d\n", payload[:1022], i%10)
	}
	s, _ := openCounting(t, sb.String())
	if s.TotalLines() != lines {
		t.Fatalf("TotalLines = %d, want %d", s.TotalLines(), lines)
	}

	all, err := s.ReadLines(0, lines)
	if err != nil {
		t.Fatal(err)
	}
	// 逐行读作为对照（每行单独一个区间，必然在窗口内）
	for _, idx := range []int64{0, 8191, 8192, 8193, 8999} {
		one, err := s.ReadLines(idx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(one) != 1 || one[0] != all[idx] {
			t.Fatalf("第 %d 行逐行读与区间读不一致：%q vs %q", idx, one[0], all[idx])
		}
	}
	// 抽查内容
	if !strings.HasPrefix(all[8192], strings.Repeat("y", 1022)) {
		t.Fatalf("第 8192 行内容异常：%q", all[8192][:20])
	}
}

// 空文件的最后一行为零长度：不能因此漏行或多行。
func TestReadLinesZeroLengthLastLine(t *testing.T) {
	s, _ := openCounting(t, "a\nb\n")
	if got, err := s.ReadLines(0, 3); err != nil || len(got) != 2 {
		t.Fatalf("越界请求应截断到实际行数：%v (err=%v)", got, err)
	}
	// 末行长度为 0 的构造：最后一行是空行
	s2, _ := openCounting(t, "a\n\n")
	got, err := s2.ReadLines(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "" {
		t.Fatalf("内容 = %q", got)
	}
}
