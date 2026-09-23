package sshconn_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"logviewer/internal/logfile"
	"logviewer/internal/sshconn"
	"logviewer/internal/testssh"
)

// 编译期断言：远端文件来源满足 logfile.Source —— 这是两个包之间唯一的契约。
var _ logfile.Source = (*sshconn.RemoteFile)(nil)

// openRemote 连接测试服务端并打开远端文件。
func openRemote(t *testing.T, srv *testssh.Server, remotePath string) (*sshconn.Manager, sshconn.Profile, *sshconn.RemoteFile) {
	t.Helper()
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	f, err := m.OpenFile(p, remotePath)
	if err != nil {
		t.Fatalf("打开远端文件失败：%v", err)
	}
	t.Cleanup(func() { f.Close() })
	return m, p, f
}

func TestRemoteFileStatReadSeek(t *testing.T) {
	root := t.TempDir()
	content := "line-1\nline-2\nline-3\nline-4\n"
	path := testssh.WriteFile(t, root, "logs/app.log", content)
	srv := startServer(t, root)

	_, _, f := openRemote(t, srv, path)

	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", fi.Size(), len(content))
	}
	if fi.Name() != "app.log" || fi.IsDir() {
		t.Fatalf("Name = %q, IsDir = %v", fi.Name(), fi.IsDir())
	}

	// 顺序读（触发泵送管线）
	if got := readAll(t, f, 0); got != content {
		t.Fatalf("顺序读 = %q, want %q", got, content)
	}

	// Seek(0) 后重新读：必须重建管线并给出完整内容（泵送只能从头开始）
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, f, 0); got != content {
		t.Fatalf("Seek 后重读 = %q, want %q", got, content)
	}

	// 随机读
	buf := make([]byte, 6)
	if _, err := f.ReadAt(buf, 7); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "line-2" {
		t.Fatalf("ReadAt = %q, want %q", buf, "line-2")
	}

	// Seek 到文件尾部
	end, err := f.Seek(0, 2) // io.SeekEnd
	if err != nil {
		t.Fatal(err)
	}
	if end != int64(len(content)) {
		t.Fatalf("SeekEnd = %d, want %d", end, len(content))
	}
}

// readAll 从当前位置顺序读完（n>0 表示最多读 n 字节，0 表示不限）。
func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }, limit int) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 7) // 故意用非整块大小，覆盖跨块拼接
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
			if limit > 0 && sb.Len() >= limit {
				break
			}
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("读取失败：%v", err)
		}
	}
	return sb.String()
}

func TestOpenRemoteFileErrors(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "x\n")
	if err := os.MkdirAll(filepath.Join(root, "logs", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, root)
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.OpenFile(p, "/logs"); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("打开目录的错误 = %v，应提示是目录", err)
	}
	if _, err := m.OpenFile(p, "/logs/missing.log"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("打开不存在文件的错误 = %v，应提示不存在", err)
	}

	// 未连接的管理器
	m2 := newManager(t, "")
	if _, err := m2.OpenFile(p, "/logs/app.log"); err == nil || !strings.Contains(err.Error(), "请先连接") {
		t.Fatalf("未连接时的错误 = %v", err)
	}
}

func TestListMissingDir(t *testing.T) {
	srv := startServer(t, t.TempDir())
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.List(p, "/nope/nothing"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("列不存在目录的错误 = %v", err)
	}
}

// TestRemoteIndexThroughLogfile 端到端：远端文件经 logfile 建索引、翻页、顺序扫描。
func TestRemoteIndexThroughLogfile(t *testing.T) {
	root := t.TempDir()
	const n = 5000
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "{\"level\":\"INFO\",\"msg\":\"event %d\",\"n\":%d}\n", i, i)
	}
	content := sb.String()
	path := testssh.WriteFile(t, root, "logs/big.jsonl", content)
	srv := startServer(t, root)
	_, _, f := openRemote(t, srv, path)

	var seen int64
	sess, err := logfile.OpenSource(f, path, "big.jsonl", func(_ int64, _ string) { seen++ })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	if err := sess.WaitReady(); err != nil {
		t.Fatalf("索引失败：%v", err)
	}
	if sess.TotalLines() != n {
		t.Fatalf("TotalLines = %d, want %d", sess.TotalLines(), n)
	}
	if seen != n {
		t.Fatalf("onLine 回调 %d 次, want %d", seen, n)
	}
	if sess.Size() != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", sess.Size(), len(content))
	}

	// 翻页走 ReadAt
	lines, err := sess.ReadLines(1234, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"level":"INFO","msg":"event 1234","n":1234}`,
		`{"level":"INFO","msg":"event 1235","n":1235}`,
		`{"level":"INFO","msg":"event 1236","n":1236}`,
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("ReadLines = %v, want %v", lines, want)
	}
	// 末行（长度以打开时刻大小为界）
	last, err := sess.ReadLines(n-1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0] != fmt.Sprintf(`{"level":"INFO","msg":"event %d","n":%d}`, n-1, n-1) {
		t.Fatalf("末行 = %v", last)
	}

	// 检索走 Scan（Seek(0) + 顺序读，即再次泵送）
	var scanned int64
	if err := sess.Scan(func(lineNo int64, raw string) error {
		scanned++
		if !strings.Contains(raw, "event") {
			t.Fatalf("第 %d 行内容异常：%q", lineNo, raw)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if scanned != n {
		t.Fatalf("Scan 扫描 %d 行, want %d", scanned, n)
	}
}

// 慢链路测试参数：限速 4MB/s、文件 64MB，整趟传输需约 16s。
// 这样「及时中止」（受 pumpCloseWait 上限约束）与「等整趟传完」有数量级差异。
const (
	slowRate = 4 << 20
	slowSize = 64 << 20
)

// writeSlowLog 生成一个大文件并起一个限速的服务端。
func writeSlowLog(t *testing.T) (*testssh.Server, string) {
	t.Helper()
	root := t.TempDir()
	blob := strings.Repeat("0123456789abcdef0123456789abcdef\n", slowSize/33)
	path := testssh.WriteFile(t, root, "logs/huge.log", blob)
	srv := startServer(t, root, func(o *testssh.Options) { o.ThrottleBytesPerSec = slowRate })
	return srv, path
}

// TestRemoteCloseAbortsTransfer 关闭文件必须尽快返回，不能被正在进行的整趟传输阻塞。
func TestRemoteCloseAbortsTransfer(t *testing.T) {
	srv, path := writeSlowLog(t)
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatal(err)
	}
	f, err := m.OpenFile(p, path)
	if err != nil {
		t.Fatal(err)
	}

	// 读一点内容以启动泵送（此时传输正被限速拖住）
	buf := make([]byte, 1024)
	if _, err := f.Read(buf); err != nil {
		t.Fatalf("首次读取失败：%v", err)
	}

	start := time.Now()
	if err := f.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Close 耗时 %v（整趟传输约 16s），说明关闭被传输阻塞了", elapsed)
	}

	// 关闭后不应再能读取
	if _, err := f.Read(buf); err == nil {
		t.Fatal("关闭后 Read 应报错")
	}
	if _, err := f.ReadAt(buf, 0); err == nil {
		t.Fatal("关闭后 ReadAt 应报错")
	}
}

// TestRemoteIndexCancelOnClose 索引中途关闭会话：应尽快结束并报错（不写成 0 行）。
func TestRemoteIndexCancelOnClose(t *testing.T) {
	srv, path := writeSlowLog(t)
	_, _, f := openRemote(t, srv, path)

	sess, err := logfile.OpenSource(f, path, "huge.log", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 等到索引确实开始读（限速下必然还远未读完）
	deadline := time.Now().Add(10 * time.Second)
	for {
		if done, total := sess.Progress(); done > 0 && done < total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待索引进度超时")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	if err := sess.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("索引中关闭耗时 %v（整趟传输约 16s），说明关闭被传输阻塞了", elapsed)
	}
	if err := sess.WaitReady(); err == nil {
		t.Fatal("索引中途关闭后 WaitReady 应返回错误（否则行数会被写成 0）")
	}
}

// TestRemoteTwoFilesShareOneConnection 同一主机打开两个文件共享一条 SSH 连接。
func TestRemoteTwoFilesShareOneConnection(t *testing.T) {
	root := t.TempDir()
	a := testssh.WriteFile(t, root, "a.log", "a1\na2\n")
	b := testssh.WriteFile(t, root, "b.log", "b1\nb2\nb3\n")
	srv := startServer(t, root)
	m, p, fa := openRemote(t, srv, a)

	fb, err := m.OpenFile(p, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fb.Close() })

	for _, tc := range []struct {
		f    *sshconn.RemoteFile
		want int
	}{{fa, 2}, {fb, 3}} {
		sess, err := logfile.OpenSource(tc.f, "x.log", "x.log", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.WaitReady(); err != nil {
			t.Fatal(err)
		}
		if sess.TotalLines() != int64(tc.want) {
			t.Fatalf("TotalLines = %d, want %d", sess.TotalLines(), tc.want)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !m.Connected(p) {
		t.Fatal("关闭文件会话不应断开主机连接")
	}
}
