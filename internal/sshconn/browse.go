package sshconn

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/pkg/sftp"
)

// Entry 远端目录项（前端表格直接渲染）。
type Entry struct {
	Name    string
	Path    string // 远端绝对路径（POSIX）
	IsDir   bool
	Size    int64
	ModTime int64  // unix 毫秒
	Mode    string // 权限展示，如 -rw-r--r--
}

// List 列出远端目录：目录在前，同级按名称不区分大小写排序。
// dir 为空时取远端家目录；返回实际列出的目录（POSIX 归一后的绝对路径）与目录项。
func (m *Manager) List(p Profile, dir string) (string, []Entry, error) {
	cl, err := m.client(p)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(dir) == "" {
		if dir, err = m.Home(p); err != nil {
			return "", nil, err
		}
	}
	dir = path.Clean(dir)
	fis, err := cl.ReadDir(dir)
	if err != nil {
		return "", nil, remoteError(err, fmt.Sprintf("读取目录 %s", dir))
	}
	entries := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		entries = append(entries, Entry{
			Name:    fi.Name(),
			Path:    path.Join(dir, fi.Name()),
			IsDir:   fi.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime().UnixMilli(),
			Mode:    fi.Mode().String(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return dir, entries, nil
}

// Home 远端家目录（即登录用户的 home）。
func (m *Manager) Home(p Profile) (string, error) {
	cl, err := m.client(p)
	if err != nil {
		return "", err
	}
	wd, err := cl.Getwd()
	if err != nil || wd == "" {
		return "/", nil // 拿不到工作目录时退回根目录，由用户自行导航
	}
	return path.Clean(wd), nil
}

// Stat 远端文件信息（供打开前确认存在、是否为目录）。
func (m *Manager) Stat(p Profile, remotePath string) (os.FileInfo, error) {
	cl, err := m.client(p)
	if err != nil {
		return nil, err
	}
	fi, err := cl.Stat(remotePath)
	if err != nil {
		return nil, remoteError(err, fmt.Sprintf("读取 %s", remotePath))
	}
	return fi, nil
}

// remoteError 把 SFTP/网络错误翻译成可读提示。
// SFTP 的错误是 *sftp.StatusError（带 SSH_FX_* 状态码），不能用 errors.Is(os.ErrNotExist) 判断。
func remoteError(err error, what string) error {
	var se *sftp.StatusError
	if errors.As(err, &se) {
		switch se.FxCode() {
		case sftp.ErrSSHFxNoSuchFile:
			return fmt.Errorf("%s 失败：路径不存在", what)
		case sftp.ErrSSHFxPermissionDenied:
			return fmt.Errorf("%s 失败：权限不足", what)
		case sftp.ErrSSHFxNoConnection, sftp.ErrSSHFxConnectionLost:
			return fmt.Errorf("%s 失败：连接已断开，请重新连接", what)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s 失败：路径不存在", what)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%s 失败：权限不足", what)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
		return fmt.Errorf("%s 失败：连接已断开，请重新连接", what)
	}
	return fmt.Errorf("%s 失败：%w", what, err)
}
