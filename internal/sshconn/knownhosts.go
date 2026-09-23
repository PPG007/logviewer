package sshconn

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyError 主机指纹校验失败（dialError 会原样抛出，不做二次包装）。
type HostKeyError struct{ msg string }

func (e *HostKeyError) Error() string { return e.msg }

// hostKeys 主机指纹校验：先查用户既有 known_hosts 与本程序自己的记录；
// 未记录过的主机按 TOFU（首次信任）记入自己的 known_hosts 文件；
// 与已有记录不符时一律拒绝（防中间人）。
type hostKeys struct {
	check    ssh.HostKeyCallback // 基于文件的校验，可能为 nil（未启用校验）
	tofuPath string              // 首次信任记录的落盘位置，空 = 不记录
	mu       sync.Mutex
	trusted  map[string]ssh.PublicKey // 本次运行中已信任的主机（避免重复追加同一行）
}

func newHostKeys(tofuPath string, reuseUserKnownHosts bool) (*hostKeys, error) {
	h := &hostKeys{tofuPath: tofuPath, trusted: make(map[string]ssh.PublicKey)}
	files := make([]string, 0, 2)
	if reuseUserKnownHosts {
		if home, err := os.UserHomeDir(); err == nil {
			if p := filepath.Join(home, ".ssh", "known_hosts"); fileExists(p) {
				files = append(files, p)
			}
		}
	}
	if tofuPath != "" {
		if err := os.MkdirAll(filepath.Dir(tofuPath), 0o700); err != nil {
			return nil, fmt.Errorf("创建配置目录失败：%w", err)
		}
		// 先确保文件存在且权限为 0600，knownhosts.New 才能读到它。
		f, err := os.OpenFile(tofuPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("初始化 known_hosts 失败：%w", err)
		}
		f.Close()
		files = append(files, tofuPath)
	}
	if len(files) == 0 {
		return h, nil // 既不读也不记：不做校验（由调用方显式选择）
	}
	cb, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("读取 known_hosts 失败：%w", err)
	}
	h.check = cb
	return h, nil
}

// callback 作为 ssh.ClientConfig.HostKeyCallback；hostname 是拨号地址原文（host:port）。
func (h *hostKeys) callback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	if h == nil || h.check == nil {
		return nil
	}
	err := h.check(hostname, remote, key)
	if err == nil {
		return nil
	}
	var ke *knownhosts.KeyError
	if !errors.As(err, &ke) {
		return &HostKeyError{fmt.Sprintf("主机指纹校验失败：%v", err)}
	}
	if len(ke.Want) > 0 {
		// 已知主机但指纹变了：极可能是中间人，也可能是主机重装，一律拒绝并给出两侧指纹。
		return &HostKeyError{fmt.Sprintf(
			"主机 %s 的指纹与记录不符，已拒绝连接：\n  记录中 %s\n  实际   %s\n若主机确已重装，请删除 %s 中该主机的记录后重试",
			hostname, ssh.FingerprintSHA256(ke.Want[0].Key), ssh.FingerprintSHA256(key), h.tofuPath)}
	}

	norm := knownhosts.Normalize(hostname)
	h.mu.Lock()
	prev, seen := h.trusted[norm]
	h.mu.Unlock()
	if seen {
		if ssh.FingerprintSHA256(prev) == ssh.FingerprintSHA256(key) {
			return nil
		}
		return &HostKeyError{fmt.Sprintf("主机 %s 的指纹与本次会话中已信任的记录不符，已拒绝连接", hostname)}
	}

	if h.tofuPath == "" {
		return &HostKeyError{fmt.Sprintf("主机 %s 不在 known_hosts 中，且未启用首次信任记录", hostname)}
	}
	if err := h.remember(norm, key); err != nil {
		return err
	}
	return nil
}

// remember 把新主机指纹追加到本程序自己的 known_hosts（0600）。
func (h *hostKeys) remember(norm string, key ssh.PublicKey) error {
	line := knownhosts.Line([]string{norm}, key)
	h.mu.Lock()
	defer h.mu.Unlock()
	f, err := os.OpenFile(h.tofuPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("记录主机指纹失败：%w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("记录主机指纹失败：%w", err)
	}
	h.trusted[norm] = key
	return nil
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
