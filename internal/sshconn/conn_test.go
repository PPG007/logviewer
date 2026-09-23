package sshconn_test

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"logviewer/internal/sshconn"
	"logviewer/internal/testssh"
)

// newManager 创建只使用临时 known_hosts 的管理器（不读用户真实的 ~/.ssh/known_hosts）。
func newManager(t *testing.T, knownHosts string) *sshconn.Manager {
	t.Helper()
	if knownHosts == "" {
		knownHosts = filepath.Join(t.TempDir(), "known_hosts")
	}
	m, err := sshconn.NewManager(sshconn.Options{
		KnownHostsPath:    knownHosts,
		DialTimeout:       10 * time.Second,
		KeepAliveInterval: time.Hour, // 测试期间不需要保活探测
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func profileOf(t *testing.T, s *testssh.Server, auth string) sshconn.Profile {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return sshconn.Profile{Host: host, Port: port, User: s.User, AuthMethod: auth}
}

func startServer(t *testing.T, root string, opts ...func(*testssh.Options)) *testssh.Server {
	t.Helper()
	o := testssh.Options{Root: root, Password: "pw-" + testssh.RandHex(4)}
	for _, f := range opts {
		f(&o)
	}
	return testssh.Start(t, o)
}

func TestConnectPasswordListAndStat(t *testing.T) {
	root := t.TempDir()
	logPath := testssh.WriteFile(t, root, "logs/app.log", "a\nb\nc\n")
	testssh.WriteFile(t, root, "logs/readme.txt", "hi")
	if err := os.MkdirAll(filepath.Join(root, "logs", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := startServer(t, root)

	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	if !m.Connected(p) {
		t.Fatal("Connected = false, want true")
	}

	_, entries, err := m.List(p, "/logs")
	if err != nil {
		t.Fatal(err)
	}
	// 目录优先，同级按名称排序：sub, app.log, readme.txt
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name)
	}
	want := []string{"sub", "app.log", "readme.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("目录项 = %v, want %v", got, want)
	}
	if !entries[0].IsDir {
		t.Fatal("首项应为目录")
	}
	if entries[1].Path != "/logs/app.log" {
		t.Fatalf("Path = %q", entries[1].Path)
	}
	if entries[1].Size != 6 {
		t.Fatalf("Size = %d, want 6", entries[1].Size)
	}
	if entries[1].ModTime == 0 {
		t.Fatal("ModTime 不应为 0")
	}

	fi, err := m.Stat(p, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 6 || fi.IsDir() {
		t.Fatalf("Stat = size %d dir %v", fi.Size(), fi.IsDir())
	}

	// 家目录：测试服务端无 sshd 的 home 概念，Getwd 返回 "." 或根，两者都应是可列目录
	home, err := m.Home(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.List(p, home); err != nil {
		t.Fatalf("列出家目录 %q 失败：%v", home, err)
	}
}

func TestConnectWrongPassword(t *testing.T) {
	srv := startServer(t, t.TempDir())
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	err := m.Connect(p, sshconn.Secret{Password: "wrong"})
	if err == nil {
		t.Fatal("错误密码应连接失败")
	}
	if !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("错误信息 = %v，应提示认证失败", err)
	}
	if m.Connected(p) {
		t.Fatal("失败后不应处于已连接状态")
	}
}

func TestConnectKeyAuth(t *testing.T) {
	dir := t.TempDir()
	_, priv, pub := testssh.ClientKeyPair(t)
	keyPath := testssh.WritePrivateKey(t, dir, priv, "")
	srv := startServer(t, t.TempDir(), func(o *testssh.Options) { o.AuthorizedKey = pub })

	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthKey)
	p.KeyPath = keyPath
	if err := m.Connect(p, sshconn.Secret{}); err != nil {
		t.Fatalf("私钥连接失败：%v", err)
	}
	if _, _, err := m.List(p, "/"); err != nil {
		t.Fatal(err)
	}
}

func TestConnectKeyWithPassphrase(t *testing.T) {
	dir := t.TempDir()
	passphrase := "pp-" + testssh.RandHex(4)
	_, priv, pub := testssh.ClientKeyPair(t)
	keyPath := testssh.WritePrivateKey(t, dir, priv, passphrase)
	srv := startServer(t, t.TempDir(), func(o *testssh.Options) { o.AuthorizedKey = pub })
	p := profileOf(t, srv, sshconn.AuthKey)
	p.KeyPath = keyPath

	// 不填口令：应提示私钥已加密
	m := newManager(t, "")
	err := m.Connect(p, sshconn.Secret{})
	if err == nil || !strings.Contains(err.Error(), "口令") {
		t.Fatalf("未填口令的错误 = %v，应提示需要私钥口令", err)
	}

	// 口令错误：应报解析失败而不是连上
	if err := m.Connect(p, sshconn.Secret{Passphrase: "不存在的口令"}); err == nil {
		t.Fatal("错误口令不应连接成功")
	}

	// 口令正确
	m2 := newManager(t, "")
	if err := m2.Connect(p, sshconn.Secret{Passphrase: passphrase}); err != nil {
		t.Fatalf("带口令的私钥连接失败：%v", err)
	}
}

// TestConnectKeyDefaultPath 未指定私钥文件时按 ~/.ssh 默认候选探测。
func TestConnectKeyDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir 在类 Unix 上读 HOME
	t.Setenv("USERPROFILE", home) // Windows 上读 USERPROFILE
	_, priv, pub := testssh.ClientKeyPair(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testssh.WritePrivateKey(t, sshDir, priv, "")
	srv := startServer(t, t.TempDir(), func(o *testssh.Options) { o.AuthorizedKey = pub })

	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthKey) // KeyPath 留空
	if err := m.Connect(p, sshconn.Secret{}); err != nil {
		t.Fatalf("默认私钥探测失败：%v", err)
	}
}

func TestConnectMissingHostAndUser(t *testing.T) {
	m := newManager(t, "")
	if err := m.Connect(sshconn.Profile{Host: "", User: "u"}, sshconn.Secret{Password: "x"}); err == nil {
		t.Fatal("缺少主机应报错")
	}
	if err := m.Connect(sshconn.Profile{Host: "127.0.0.1", User: ""}, sshconn.Secret{Password: "x"}); err == nil {
		t.Fatal("缺少用户名应报错")
	}
}

func TestListBeforeConnect(t *testing.T) {
	m := newManager(t, "")
	_, _, err := m.List(sshconn.Profile{Host: "127.0.0.1", Port: 1, User: "u"}, "/")
	if err == nil || !strings.Contains(err.Error(), "请先连接") {
		t.Fatalf("未连接时的错误 = %v", err)
	}
}

// TestHostKeyTOFU 未记录的主机首次连接应写入自己的 known_hosts，之后可直接连接；
// 已有记录但指纹不符时必须拒绝。
func TestHostKeyTOFU(t *testing.T) {
	srv := startServer(t, t.TempDir())
	p := profileOf(t, srv, sshconn.AuthPassword)

	kh := filepath.Join(t.TempDir(), "known_hosts")
	m := newManager(t, kh)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatalf("首次连接失败：%v", err)
	}
	raw, err := os.ReadFile(kh)
	if err != nil {
		t.Fatalf("首次连接后应写入 known_hosts：%v", err)
	}
	// 非标准端口会写成 [host]:port 形式（knownhosts 规范）
	if want := knownhosts.Normalize(srv.Addr); !strings.Contains(string(raw), want) {
		t.Fatalf("known_hosts 内容 = %q，应包含主机 %s", raw, want)
	}
	// 记的内容要真能被 knownhosts 解析并与服务端主机密钥匹配（而非只写了一行字）
	cb, err := knownhosts.New(kh)
	if err != nil {
		t.Fatalf("known_hosts 应可解析：%v", err)
	}
	if err := cb(srv.Addr, &net.TCPAddr{}, srv.Signer.PublicKey()); err != nil {
		t.Fatalf("记录的指纹应通过校验：%v", err)
	}

	// 另一个管理器（重新读文件）应能直接连上
	m2 := newManager(t, kh)
	if err := m2.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatalf("已记录主机应能直接连接：%v", err)
	}

	// 同地址但指纹不同：必须拒绝。
	// 另一台服务器（主机密钥不同）的地址上，预置 srv 的指纹，模拟「主机被换掉」。
	other := startServer(t, t.TempDir())
	bad := profileOf(t, other, sshconn.AuthPassword)
	line := knownhosts.Line([]string{knownhosts.Normalize(bad.Addr())}, srv.Signer.PublicKey())
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m3 := newManager(t, kh)
	err = m3.Connect(bad, sshconn.Secret{Password: other.Password})
	if err == nil {
		t.Fatal("指纹不符应拒绝连接")
	}
	if !strings.Contains(err.Error(), "指纹") {
		t.Fatalf("错误信息 = %v，应提示指纹不符", err)
	}
}

func TestDisconnectThenUse(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "a.log", "x\n")
	srv := startServer(t, root)
	m := newManager(t, "")
	p := profileOf(t, srv, sshconn.AuthPassword)
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatal(err)
	}
	if err := m.Disconnect(p); err != nil {
		t.Fatal(err)
	}
	if m.Connected(p) {
		t.Fatal("断开后 Connected 应为 false")
	}
	if _, _, err := m.List(p, "/"); err == nil {
		t.Fatal("断开后列目录应报错")
	}
	// 重新连接
	if err := m.Connect(p, sshconn.Secret{Password: srv.Password}); err != nil {
		t.Fatalf("重连失败：%v", err)
	}
	if _, _, err := m.List(p, "/"); err != nil {
		t.Fatal(err)
	}
}
