package logfile_test

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"logviewer/internal/logfile"
)

// memSource 内存字节来源：只实现 logfile.Source 要求的原语，
// 用来验证「索引/翻页/检索算法确实不依赖 *os.File」。
type memSource struct {
	data    []byte
	pos     int64
	isDir   bool
	closed  bool
	statErr error
	// gate 非 nil 时 Read 一直阻塞，直到 Close 关闭它（模拟慢链路/正在拉取中的远端读）。
	gate chan struct{}
}

func (m *memSource) Read(p []byte) (int, error) {
	if m.gate != nil {
		<-m.gate
		return 0, io.ErrClosedPipe
	}
	if m.pos >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[m.pos:])
	m.pos += int64(n)
	return n, nil
}

func (m *memSource) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memSource) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = off
	case io.SeekCurrent:
		m.pos += off
	case io.SeekEnd:
		m.pos = int64(len(m.data)) + off
	}
	return m.pos, nil
}

func (m *memSource) Stat() (os.FileInfo, error) {
	if m.statErr != nil {
		return nil, m.statErr
	}
	return memInfo{size: int64(len(m.data)), dir: m.isDir}, nil
}

func (m *memSource) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true
	if m.gate != nil {
		close(m.gate) // 关闭须唤醒正在阻塞的读
	}
	return nil
}

type memInfo struct {
	size int64
	dir  bool
}

func (i memInfo) Name() string       { return "mem.log" }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() os.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

// 编译期断言：内存来源满足 logfile.Source。
var _ logfile.Source = (*memSource)(nil)

func TestOpenSourceReadAndScan(t *testing.T) {
	content := "a1\na2\r\na3\n\na5" // 含 CRLF、空行、无末尾换行
	src := &memSource{data: []byte(content)}
	s, err := logfile.OpenSource(src, "/var/log/remote/app.log", "app.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}

	if s.Name != "app.log" || s.Path != "/var/log/remote/app.log" {
		t.Fatalf("Name/Path = %q/%q", s.Name, s.Path)
	}
	want := []string{"a1", "a2", "a3", "", "a5"}
	if got := s.TotalLines(); got != int64(len(want)) {
		t.Fatalf("TotalLines = %d, want %d", got, len(want))
	}
	if got := s.Size(); got != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", got, len(content))
	}

	got, err := s.ReadLines(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != strings.Join(want[1:4], "|") {
		t.Fatalf("ReadLines = %q, want %q", got, want[1:4])
	}

	// Scan 走 Seek(0)+顺序读，须与 ReadLines 结果一致
	src.pos = int64(len(content)) // 打乱读指针，验证 Seek 生效
	var scanned []string
	if err := s.Scan(func(_ int64, raw string) error {
		scanned = append(scanned, raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(scanned, "|") != strings.Join(want, "|") {
		t.Fatalf("Scan = %q, want %q", scanned, want)
	}
}

func TestOpenSourceRejectsDirectory(t *testing.T) {
	src := &memSource{isDir: true}
	if _, err := logfile.OpenSource(src, "/var/log", "log", nil); err == nil {
		t.Fatal("目录来源应报错")
	}
	if !src.closed {
		t.Fatal("失败时 OpenSource 应关闭来源")
	}
}

func TestOpenSourceStatError(t *testing.T) {
	src := &memSource{statErr: os.ErrPermission}
	if _, err := logfile.OpenSource(src, "/root/secret.log", "secret.log", nil); err == nil {
		t.Fatal("Stat 失败应报错")
	}
	if !src.closed {
		t.Fatal("失败时 OpenSource 应关闭来源")
	}
}

// TestCloseDuringIndex 索引进行中关闭会话：须立刻中断并以错误收尾，
// 不能让 WaitReady 误判成功（否则历史记录的行数会被写成 0）。
func TestCloseDuringIndex(t *testing.T) {
	src := &memSource{data: []byte(strings.Repeat("line\n", 200000)), gate: make(chan struct{})}
	s, err := logfile.OpenSource(src, "/remote/big.log", "big.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 此时索引 goroutine 阻塞在首次 Read 上
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitReady(); err == nil {
		t.Fatal("关闭后 WaitReady 应返回错误")
	}
	if s.Status() != logfile.StatusError {
		t.Fatalf("Status = %v, want error", s.Status())
	}
}

// 索引进行中重新加载：应被拒绝（不能在扫描脚下换索引），且不影响进行中的那一轮。
func TestReloadWhileIndexingRejected(t *testing.T) {
	src := &memSource{data: []byte(strings.Repeat("line\n", 1000)), gate: make(chan struct{})}
	s, err := logfile.OpenSource(src, "/remote/x.log", "x.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	opened := false
	open := func() (logfile.Source, error) {
		opened = true
		return &memSource{data: []byte("new\n")}, nil
	}
	if err := s.Reload(open, nil); err == nil {
		t.Fatal("索引进行中重新加载应报错")
	}
	// 拒绝发生在打开之前：连来源都不该去开（开了就要负责关，还会与旧来源重叠）
	if opened {
		t.Fatal("被拒绝时不应调用打开函数")
	}
}
