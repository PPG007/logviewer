package sshconn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// 认证方式取值（与前端连接表单一致）。
const (
	AuthPassword = "password"
	AuthKey      = "key"
)

// defaultKeyNames 未指定私钥文件时的探测顺序（与 ssh 客户端习惯一致）。
var defaultKeyNames = []string{"id_ed25519", "id_rsa", "id_ecdsa"}

// authMethods 组装 ssh 认证方法。口令只在此处使用，不写入任何持久化存储。
func authMethods(p Profile, sec Secret) ([]ssh.AuthMethod, error) {
	switch p.AuthMethod {
	case AuthKey:
		signer, err := loadSigner(p.KeyPath, sec.Passphrase)
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	case AuthPassword, "":
		if sec.Password == "" {
			return nil, errors.New("请输入登录密码")
		}
		return []ssh.AuthMethod{ssh.RetryableAuthMethod(ssh.Password(sec.Password), 1)}, nil
	default:
		return nil, fmt.Errorf("不支持的认证方式：%s", p.AuthMethod)
	}
}

// loadSigner 读取并解析私钥；path 为空时按 ~/.ssh 下的默认候选依次探测。
func loadSigner(path, passphrase string) (ssh.Signer, error) {
	var candidates []string
	if strings.TrimSpace(path) != "" {
		candidates = []string{expandHome(strings.TrimSpace(path))}
	} else {
		candidates = defaultKeyPaths()
	}

	var lastErr error
	for _, cand := range candidates {
		raw, err := os.ReadFile(cand)
		if err != nil {
			lastErr = fmt.Errorf("读取私钥失败：%w", err)
			continue
		}
		if passphrase != "" {
			signer, err := ssh.ParsePrivateKeyWithPassphrase(raw, []byte(passphrase))
			if err != nil {
				return nil, fmt.Errorf("解析私钥 %s 失败（请确认私钥口令是否正确）：%w", cand, err)
			}
			return signer, nil
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err == nil {
			return signer, nil
		}
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("私钥 %s 已加密，请填写私钥口令", cand)
		}
		lastErr = fmt.Errorf("解析私钥 %s 失败：%w", cand, err)
	}
	if lastErr == nil {
		lastErr = errors.New("未找到可用的私钥：请指定私钥文件，或确认 ~/.ssh 下存在 id_ed25519 / id_rsa / id_ecdsa")
	}
	return nil, lastErr
}

// defaultKeyPaths ~/.ssh 下的默认私钥候选路径。
func defaultKeyPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(defaultKeyNames))
	for _, n := range defaultKeyNames {
		out = append(out, filepath.Join(home, ".ssh", n))
	}
	return out
}

// expandHome 展开开头的 ~（仅 ~、~/、~\，不处理 ~user 形式）。
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	rest := strings.TrimPrefix(p, "~")
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.TrimPrefix(rest, `\`)
	if rest == "" {
		return home
	}
	return filepath.Join(home, rest)
}
