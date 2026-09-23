// Package testssh 提供测试用的内嵌 SSH/SFTP 服务端（仅供测试使用，不进主程序）。
//
// 只监听回环地址，SFTP 子系统通过 os.Root 限定在给定目录内：客户端既无法用
// 绝对路径也无法用 ../ 或符号链接逃逸到宿主机其他位置。
package testssh

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Options 启动参数。
type Options struct {
	// Root SFTP 会话的根目录（客户端看到的 "/" 即此目录），必填。
	Root string
	// User 允许登录的用户名，默认 tester。
	User string
	// Password 密码认证口令；留空表示关闭密码认证。
	Password string
	// AuthorizedKey 非 nil 时同时接受该公钥认证。
	AuthorizedKey ssh.PublicKey
	// HostSigner 主机密钥；留空则每次启动生成新的 ed25519 密钥（便于测试指纹变化）。
	HostSigner ssh.Signer
	// ThrottleBytesPerSec 大于 0 时把 SFTP 读限速到该速率。
	// 本地环回太快，无法验证「关闭时是否及时中止传输」，需借此模拟慢链路。
	ThrottleBytesPerSec int64
}

// Server 一个运行中的测试 SSH 服务端。
type Server struct {
	Addr     string // 127.0.0.1:port
	User     string
	Password string
	Signer   ssh.Signer
	Root     string

	cfg       *ssh.ServerConfig
	root      *os.Root
	rate      *rateLimiter
	bytesRead int64 // 服务端累计读出的字节数（原子；BytesRead 读取）
	listener  net.Listener
	mu        sync.Mutex
	conns     []net.Conn
}

// BytesRead 返回服务端累计向客户端送出的文件字节数。
// 用于断言「命中缓存时没有传输数据」：比较操作前后的差值即可，不依赖耗时推断。
func (s *Server) BytesRead() int64 { return atomic.LoadInt64(&s.bytesRead) }

// rateLimiter 按字节数限速：跨并发请求统一排队，模拟带宽瓶颈（而非单包延迟）。
type rateLimiter struct {
	bytesPerSec int64
	mu          sync.Mutex
	next        time.Time
}

func (r *rateLimiter) wait(n int) {
	if r == nil || r.bytesPerSec <= 0 {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if r.next.Before(now) {
		r.next = now
	}
	d := r.next.Sub(now)
	r.next = r.next.Add(time.Duration(int64(n)) * time.Second / time.Duration(r.bytesPerSec))
	r.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

// countingReaderAt 统计服务端读出的字节数（可选限速），并把 Close 透传给底层文件。
//
// 字节计数用于断言「第二次打开没有传输数据」这类性质：测试读 srv.BytesRead() 的差值即可，
// 不必依赖耗时推断。
type countingReaderAt struct {
	f     io.ReaderAt
	rate  *rateLimiter
	count *int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if c.rate != nil {
		c.rate.wait(len(p))
	}
	n, err := c.f.ReadAt(p, off)
	if n > 0 && c.count != nil {
		atomic.AddInt64(c.count, int64(n))
	}
	return n, err
}

func (c *countingReaderAt) Close() error {
	if closer, ok := c.f.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Start 启动测试服务端，并在测试结束时自动关闭。
func Start(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Root == "" {
		t.Fatal("testssh: Root 不能为空")
	}
	if opts.User == "" {
		opts.User = "tester"
	}
	if opts.Password == "" && opts.AuthorizedKey == nil {
		t.Fatal("testssh: 至少要启用一种认证方式")
	}
	signer := opts.HostSigner
	if signer == nil {
		signer = NewSigner(t)
	}
	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		t.Fatalf("testssh: 打开根目录失败：%v", err)
	}

	cfg := &ssh.ServerConfig{ServerVersion: "SSH-2.0-logviewer-test"}
	if opts.Password != "" {
		want := []byte(opts.Password)
		cfg.PasswordCallback = func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == opts.User && subtle.ConstantTimeCompare(pw, want) == 1 {
				return nil, nil
			}
			return nil, fmt.Errorf("密码不正确")
		}
	}
	if opts.AuthorizedKey != nil {
		want := opts.AuthorizedKey.Marshal()
		cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == opts.User && bytes.Equal(key.Marshal(), want) {
				return nil, nil
			}
			return nil, fmt.Errorf("公钥不被接受")
		}
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testssh: 监听失败：%v", err)
	}
	s := &Server{
		Addr: ln.Addr().String(), User: opts.User, Password: opts.Password,
		Signer: signer, Root: opts.Root, cfg: cfg, root: root, listener: ln,
		rate: &rateLimiter{bytesPerSec: opts.ThrottleBytesPerSec},
	}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close 关闭服务端与所有活动连接（幂等）。
func (s *Server) Close() {
	s.mu.Lock()
	if s.listener == nil {
		s.mu.Unlock()
		return
	}
	ln := s.listener
	conns := s.conns
	s.listener = nil
	s.mu.Unlock()

	ln.Close()
	for _, c := range conns {
		c.Close()
	}
	s.root.Close()
}

func (s *Server) serve() {
	for {
		nc, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.listener == nil { // 已关闭
			s.mu.Unlock()
			nc.Close()
			return
		}
		s.conns = append(s.conns, nc)
		s.mu.Unlock()
		go s.handle(nc)
	}
}

func (s *Server) handle(nc net.Conn) {
	defer nc.Close()
	_, chans, reqs, err := ssh.NewServerConn(nc, s.cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.UnknownChannelType, "仅支持 session")
			continue
		}
		channel, requests, err := ch.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(channel, requests)
	}
}

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.Type != "subsystem" {
			req.Reply(false, nil)
			continue
		}
		var payload struct{ Name string }
		ssh.Unmarshal(req.Payload, &payload)
		if payload.Name != "sftp" {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		h := rootedHandler{root: s.root, rate: s.rate, bytesRead: &s.bytesRead}
		srv := sftp.NewRequestServer(ch, sftp.Handlers{
			FileGet:  h,
			FilePut:  h,
			FileCmd:  h,
			FileList: h,
		})
		srv.Serve()
		srv.Close()
		ch.Close()
		return
	}
}

// rootedHandler 把 SFTP 请求映射到 os.Root 内（只读；越界路径由 os.Root 拒绝）。
type rootedHandler struct {
	root      *os.Root
	rate      *rateLimiter
	bytesRead *int64 // 服务端读出的总字节数（原子）
}

// rel 把客户端发来的绝对路径映射为 root 内的相对路径（"/" → "."）。
func rel(p string) string {
	clean := path.Clean("/" + strings.TrimPrefix(strings.ReplaceAll(p, `\`, "/"), "/"))
	r := strings.TrimPrefix(clean, "/")
	if r == "" {
		return "."
	}
	return filepath.FromSlash(r)
}

func (h rootedHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f, err := h.root.Open(rel(r.Filepath))
	if err != nil {
		return nil, err
	}
	var rate *rateLimiter
	if h.rate != nil && h.rate.bytesPerSec > 0 {
		rate = h.rate
	}
	return &countingReaderAt{f: f, rate: rate, count: h.bytesRead}, nil
}

func (h rootedHandler) Filewrite(*sftp.Request) (io.WriterAt, error) {
	return nil, sftp.ErrSSHFxOpUnsupported // 测试服务端只读
}

func (h rootedHandler) Filecmd(*sftp.Request) error {
	return sftp.ErrSSHFxOpUnsupported
}

func (h rootedHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		f, err := h.root.Open(rel(r.Filepath))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		fis, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		return listerAt(fis), nil
	case "Stat":
		fi, err := h.root.Stat(rel(r.Filepath))
		if err != nil {
			return nil, err
		}
		return listerAt{fi}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(out []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[offset:])
	if n < len(out) {
		return n, io.EOF
	}
	return n, nil
}

// NewSigner 生成一次性 ed25519 主机密钥。
func NewSigner(t *testing.T) ssh.Signer {
	t.Helper()
	signer, _, _ := ClientKeyPair(t)
	return signer
}

// ClientKeyPair 生成客户端密钥对：signer 用于客户端认证，public 传给 Options.AuthorizedKey。
func ClientKeyPair(t *testing.T) (ssh.Signer, crypto.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, priv, signer.PublicKey()
}

// WritePrivateKey 把私钥写成 OpenSSH 格式文件（passphrase 为空表示不加密），返回文件路径。
func WritePrivateKey(t *testing.T, dir string, key crypto.PrivateKey, passphrase string) string {
	t.Helper()
	var (
		block *pem.Block
		err   error
	)
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(key, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatalf("testssh: 编码私钥失败：%v", err)
	}
	p := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// RandHex 生成 n 字节的随机十六进制串（用于互不相同的测试口令/目录名）。
func RandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// WriteFile 在 root 下写一个文件（自动建父目录），返回相对 root 的 POSIX 路径。
func WriteFile(t testing.TB, root, relPath, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return "/" + strings.TrimPrefix(path.Clean("/"+relPath), "/")
}
