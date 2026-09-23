// Package sshconn 提供到远端主机的 SSH/SFTP 连接：认证、主机指纹校验（TOFU）、
// 按主机复用连接、目录浏览，并把远端文件适配成 logfile.Source 的字节来源。
//
// 敏感信息边界：Profile 承载主机/端口/用户/认证方式/私钥路径，凭据放在 Secret 里传入。
// 本包自身不写入任何存储；凭据是否落盘由上层决定（服务层当前明文存入本机数据库）。
package sshconn

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	// DefaultPort 未指定端口时的 SSH 默认端口。
	DefaultPort = 22
	// defaultDialTimeout 拨号（含握手与认证）超时。
	defaultDialTimeout = 15 * time.Second
	// defaultKeepAlive 保活探测间隔：用于发现「对端已消失但连接未报错」的死连接。
	defaultKeepAlive = 30 * time.Second
	// keepAliveMsg OpenSSH 约定的保活请求。
	keepAliveMsg = "keepalive@openssh.com"
)

// Options 管理器配置。零值可用（不校验主机指纹、使用默认超时）。
type Options struct {
	// KnownHostsPath 本程序自己的 known_hosts：未知主机首次连接后写到这里（TOFU）。
	// 留空表示不做首次信任记录（此时未知主机会被直接拒绝）。
	KnownHostsPath string
	// ReuseUserKnownHosts 同时读取 ~/.ssh/known_hosts（存在才读，只读不写）。
	ReuseUserKnownHosts bool
	// DialTimeout 拨号超时，默认 15s。
	DialTimeout time.Duration
	// KeepAliveInterval 保活探测间隔，默认 30s。
	KeepAliveInterval time.Duration
}

// Profile 连接所需的非敏感信息（可持久化到数据库）。
type Profile struct {
	Host       string
	Port       int
	User       string
	AuthMethod string // AuthPassword / AuthKey
	KeyPath    string // 私钥文件路径（AuthKey 时使用；留空则探测 ~/.ssh 下的默认私钥）
}

// Secret 本次连接使用的凭据。
//
// 本包不负责持久化：是否保存由上层决定（当前实现由服务层明文存入本机数据库）。
// 本包只保证不把口令写进日志与错误信息。
type Secret struct {
	Password   string // AuthPassword 的登录密码
	Passphrase string // AuthKey 的私钥口令（私钥未加密时留空）
}

// Addr host:port（端口非法时回落到 22）。
func (p Profile) Addr() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.port())) }

// Key 连接复用键：同一 user@host:port 共用一条 SSH 连接。
func (p Profile) Key() string { return p.User + "@" + p.Addr() }

// Display 展示用标识，如 root@10.0.0.5:22。
func (p Profile) Display() string { return p.Key() }

func (p Profile) port() int {
	if p.Port <= 0 || p.Port > 65535 {
		return DefaultPort
	}
	return p.Port
}

func (p Profile) validate() error {
	if strings.TrimSpace(p.Host) == "" {
		return errors.New("请填写主机地址")
	}
	if strings.TrimSpace(p.User) == "" {
		return errors.New("请填写用户名")
	}
	return nil
}

// Manager 连接管理器：按 Profile.Key() 复用连接，直到显式断开或探测到连接已死。
type Manager struct {
	opts  Options
	hosts *hostKeys

	// dialMu 串行化「建连并登记」：避免同一主机被并发拨号建出两条连接（后建的那条会泄漏）。
	dialMu sync.Mutex
	mu     sync.Mutex
	conns  map[string]*conn
}

type conn struct {
	profile Profile
	client  *ssh.Client
	sftp    *sftp.Client
	done    chan struct{} // 关闭时通知保活 goroutine 退出

	mu     sync.Mutex
	dead   bool
	closed sync.Once
}

func (c *conn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.dead
}

func (c *conn) markDead() {
	c.mu.Lock()
	c.dead = true
	c.mu.Unlock()
}

// close 关闭连接（幂等）。
func (c *conn) close() {
	c.closed.Do(func() {
		c.markDead()
		close(c.done)
		if c.sftp != nil {
			c.sftp.Close()
		}
		if c.client != nil {
			c.client.Close()
		}
	})
}

// NewManager 创建管理器；KnownHostsPath 的父目录会自动创建。
func NewManager(opts Options) (*Manager, error) {
	hosts, err := newHostKeys(opts.KnownHostsPath, opts.ReuseUserKnownHosts)
	if err != nil {
		return nil, err
	}
	return &Manager{opts: opts, hosts: hosts, conns: make(map[string]*conn)}, nil
}

// Connect 建立（或复用）到该主机的连接。已连接直接返回；连接已死则重连。
// 认证失败、主机指纹不符等都会返回可直接展示的中文错误。
func (m *Manager) Connect(p Profile, sec Secret) error {
	if err := p.validate(); err != nil {
		return err
	}
	m.dialMu.Lock()
	defer m.dialMu.Unlock()

	key := p.Key()
	m.mu.Lock()
	existing := m.conns[key]
	m.mu.Unlock()
	if existing != nil {
		if existing.alive() {
			return nil
		}
		m.forget(existing)
	}

	c, err := m.dial(p, sec)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.conns[key] = c
	m.mu.Unlock()
	go m.keepAlive(c)
	return nil
}

// Disconnect 显式断开该主机的连接（幂等）。
func (m *Manager) Disconnect(p Profile) error {
	m.mu.Lock()
	c := m.conns[p.Key()]
	delete(m.conns, p.Key())
	m.mu.Unlock()
	if c != nil {
		c.close()
	}
	return nil
}

// Connected 报告该主机当前是否有可用连接。
func (m *Manager) Connected(p Profile) bool { return m.ConnectedKey(p.Key()) }

// ConnectedKey 按连接标识（Profile.Key()，形如 user@host:port）查询连接状态。
// 历史记录里只有标识字符串，用它可以避免为每条记录反查数据库。
func (m *Manager) ConnectedKey(key string) bool {
	m.mu.Lock()
	c := m.conns[key]
	m.mu.Unlock()
	return c != nil && c.alive()
}

// Close 关闭全部连接（应用退出时调用）。
func (m *Manager) Close() error {
	m.mu.Lock()
	all := make([]*conn, 0, len(m.conns))
	for k, c := range m.conns {
		all = append(all, c)
		delete(m.conns, k)
	}
	m.mu.Unlock()
	for _, c := range all {
		c.close()
	}
	return nil
}

// forget 把死连接从表中移除并关闭。
func (m *Manager) forget(c *conn) {
	m.mu.Lock()
	if m.conns[c.profile.Key()] == c {
		delete(m.conns, c.profile.Key())
	}
	m.mu.Unlock()
	c.close()
}

// client 取出已连接的 SFTP 客户端；未连接时提示先连接。
func (m *Manager) client(p Profile) (*sftp.Client, error) {
	m.mu.Lock()
	c := m.conns[p.Key()]
	m.mu.Unlock()
	if c == nil || !c.alive() {
		return nil, fmt.Errorf("尚未连接到 %s，请先连接", p.Display())
	}
	return c.sftp, nil
}

func (m *Manager) dial(p Profile, sec Secret) (*conn, error) {
	auths, err := authMethods(p, sec)
	if err != nil {
		return nil, err
	}
	timeout := m.opts.DialTimeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	client, err := ssh.Dial("tcp", p.Addr(), &ssh.ClientConfig{
		User:            p.User,
		Auth:            auths,
		HostKeyCallback: m.hosts.callback,
		Timeout:         timeout,
	})
	if err != nil {
		return nil, dialError(p, err)
	}
	// UseConcurrentReads 让 File.WriteTo 走并发分块读（默认即开，此处显式声明意图）：
	// 远端索引整文件时吞吐取决于链路带宽，而不是单包往返。
	sc, err := sftp.NewClient(client, sftp.UseConcurrentReads(true))
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("启动远端 SFTP 子系统失败：%w", err)
	}
	return &conn{profile: p, client: client, sftp: sc, done: make(chan struct{})}, nil
}

// keepAlive 周期性探测连接；发现传输层错误就标记为死，下次使用会自动重连。
func (m *Manager) keepAlive(c *conn) {
	interval := m.opts.KeepAliveInterval
	if interval <= 0 {
		interval = defaultKeepAlive
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			// 服务器对未知请求回 failure 属正常；只有 err != nil 才代表链路已断。
			if _, _, err := c.client.SendRequest(keepAliveMsg, true, nil); err != nil {
				c.markDead()
				return
			}
		}
	}
}

// dialError 把底层错误翻译成可读提示：网络类与认证类在 UI 上最需要区分。
func dialError(p Profile, err error) error {
	var hkErr *HostKeyError
	if errors.As(err, &hkErr) {
		return hkErr // 指纹问题：hostKeys 已给出完整提示，不要再包一层
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"),
		strings.Contains(msg, "no supported methods remain"),
		strings.Contains(msg, "permission denied"):
		return fmt.Errorf("认证失败：用户名或凭据不正确（%s）", p.Key())
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("连接超时：%s 无响应，请检查地址与网络", p.Addr())
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("连接被拒绝：%s 上的 SSH 服务未监听", p.Addr())
	}
	return fmt.Errorf("连接 %s 失败：%w", p.Addr(), err)
}
