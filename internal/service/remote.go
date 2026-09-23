package service

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"logviewer/internal/logfile"
	"logviewer/internal/sshconn"
	"logviewer/internal/store"
)

// ---------------- 远端主机 DTO ----------------

// Connection 一台已保存的远端主机。
//
// 注意：**口令不回传前端**。Connection 里只有 HasPassword（是否已保存），
// 连接时由后端自动取用库里保存的明文口令（见 ConnectRemote）。
// 这样明文只存在于数据库与 Go 进程内存，不会多一份进 WebView。
type Connection struct {
	ID         uint
	Name       string
	Host       string
	Port       int
	User       string
	AuthMethod string // "password" | "key"
	KeyPath    string // 私钥文件路径（认证方式为 key 时）
	// Cache 该主机是否启用本地内容缓存；null = 跟随全局设置。
	Cache *bool
	// HasPassword 该主机是否已保存口令（登录密码或私钥口令），编辑表单据此提示「留空则沿用」。
	HasPassword bool
	LastDir    string // 上次浏览的目录（再次打开浏览器时定位到这里）
	LastUsedAt int64  // unix 毫秒
	Connected  bool   // 当前是否有活动连接
}

// Credential 本次连接使用的口令。
// 留空表示「沿用库里已保存的口令」；填了则在连接成功后覆盖保存。
type Credential struct {
	Password   string // 登录密码（认证方式为 password 时）
	Passphrase string // 私钥口令（私钥未加密时留空）
}

// RemoteEntry 远端目录项。
type RemoteEntry struct {
	Name    string
	Path    string
	IsDir   bool
	Size    int64
	ModTime int64  // unix 毫秒
	Mode    string // 权限展示，如 -rw-r--r--
}

// RemoteListing 一次目录浏览的结果。
type RemoteListing struct {
	Path    string // 当前目录（POSIX 绝对路径）
	Parent  string // 上级目录；已在根目录时为空
	Home    string // 远端家目录（快捷跳转用）
	Entries []RemoteEntry
}

// profile 主机配置 → 连接参数。
func profile(rec store.Connection) sshconn.Profile {
	return sshconn.Profile{
		Host:       rec.Host,
		Port:       rec.Port,
		User:       rec.User,
		AuthMethod: rec.AuthMethod,
		KeyPath:    rec.KeyPath,
	}
}

func toConnection(rec store.Connection, connected bool) Connection {
	return Connection{
		ID:         rec.ID,
		Name:       rec.Name,
		Host:       rec.Host,
		Port:       rec.Port,
		User:       rec.User,
		AuthMethod: rec.AuthMethod,
		KeyPath:    rec.KeyPath,
		Cache:      rec.Cache,
		// 只暴露「有没有」，不回传口令本身
		HasPassword: rec.Password != "" || rec.Passphrase != "",
		LastDir:     rec.LastDir,
		LastUsedAt: rec.LastUsedAt.UnixMilli(),
		Connected:  connected,
	}
}

// errNoRemote 远端功能不可用（连接管理器初始化失败）。
func errNoRemote() error {
	return errors.New("远端功能不可用（连接管理器未能初始化）")
}

// ListConnections 已保存的远端主机（最近使用在前），含当前连接状态。
func (s *LogService) ListConnections() ([]Connection, error) {
	if s.store == nil {
		return nil, errNoHistory()
	}
	recs, err := s.store.ListConnections()
	if err != nil {
		return nil, err
	}
	out := make([]Connection, 0, len(recs))
	for _, r := range recs {
		connected := s.remote != nil && s.remote.Connected(profile(r))
		out = append(out, toConnection(r, connected))
	}
	return out, nil
}

// SaveConnection 新增（ID 为 0）或更新一台远端主机。
func (s *LogService) SaveConnection(c Connection) (Connection, error) {
	if s.store == nil {
		return Connection{}, errNoHistory()
	}
	if strings.TrimSpace(c.Host) == "" {
		return Connection{}, errors.New("请填写主机地址")
	}
	if strings.TrimSpace(c.User) == "" {
		return Connection{}, errors.New("请填写用户名")
	}
	if c.AuthMethod == "" {
		c.AuthMethod = sshconn.AuthPassword
	}
	if c.AuthMethod != sshconn.AuthPassword && c.AuthMethod != sshconn.AuthKey {
		return Connection{}, fmt.Errorf("不支持的认证方式：%s", c.AuthMethod)
	}
	if c.Name == "" {
		c.Name = c.User + "@" + c.Host
	}

	updated := store.Connection{
		ID:         c.ID,
		Name:       c.Name,
		Host:       strings.TrimSpace(c.Host),
		Port:       c.Port,
		User:       strings.TrimSpace(c.User),
		AuthMethod: c.AuthMethod,
		KeyPath:    strings.TrimSpace(c.KeyPath),
		Cache:      c.Cache,
	}

	// 只有连接参数真的变了才断开旧连接：改备注名不该把该主机上已打开的文件一起关掉。
	// （连接参数变了则必须断开——旧连接指向的是另一个主机/账号。）
	if c.ID != 0 && s.remote != nil {
		if old, err := s.store.GetConnection(c.ID); err == nil {
			if op := profile(old); s.remote.Connected(op) && op != profile(updated) {
				s.closeSessionsOf(c.ID)
				_ = s.remote.Disconnect(op)
			}
		}
	}

	saved, err := s.store.SaveConnection(updated)
	if err != nil {
		return Connection{}, err
	}
	connected := s.remote != nil && s.remote.Connected(profile(saved))
	return toConnection(saved, connected), nil
}

// DeleteConnection 删除一台远端主机：断开连接、关闭其上的文件会话，
// 并连带删除该主机的历史文件记录（否则「最近打开」会留下打不开的死记录）。
func (s *LogService) DeleteConnection(id uint) error {
	if s.store == nil {
		return errNoHistory()
	}
	// 连接信息先取出来（删除后就查不到了）
	if rec, err := s.store.GetConnection(id); err == nil {
		s.dropCacheForRemote(profile(rec).Display()) // 连带清掉这台主机的本地缓存
		if s.remote != nil {
			s.closeSessionsOf(id)
			_ = s.remote.Disconnect(profile(rec))
		}
	}
	return s.store.DeleteConnection(id)
}

// ConnectRemote 连接一台主机。凭据留空表示沿用库里已保存的；填了则在成功后覆盖保存。
func (s *LogService) ConnectRemote(id uint, cred Credential) error {
	if s.remote == nil {
		return errNoRemote()
	}
	rec, err := s.store.GetConnection(id)
	if err != nil {
		return fmt.Errorf("主机不存在：%w", err)
	}
	password, passphrase := cred.Password, cred.Passphrase
	if password == "" {
		password = rec.Password // 沿用已保存的
	}
	if passphrase == "" {
		passphrase = rec.Passphrase
	}
	if err := s.remote.Connect(profile(rec), sshconn.Secret{
		Password:   password,
		Passphrase: passphrase,
	}); err != nil {
		return err
	}
	// 连接成功才落库：密码错误/连不上时不会把错的覆盖进去
	if cred.Password != "" || cred.Passphrase != "" {
		if err := s.store.SetConnectionSecret(rec.ID, password, passphrase); err != nil {
			log.Printf("保存主机口令失败：%v", err)
		}
	}
	if err := s.store.TouchConnection(rec.ID, ""); err != nil {
		log.Printf("更新主机最近使用时间失败：%v", err)
	}
	return nil
}

// ClearConnectionSecret 清除该主机保存的口令（用户显式要求）。
func (s *LogService) ClearConnectionSecret(id uint) error {
	if s.store == nil {
		return errNoHistory()
	}
	if err := s.store.SetConnectionSecret(id, "", ""); err != nil {
		return err
	}
	// 已建立的连接继续可用，只是下次连接需要重新输入
	return nil
}

// DisconnectRemote 断开连接，并关闭该主机上已打开的文件（断开后它们只会报错）。
func (s *LogService) DisconnectRemote(id uint) error {
	if s.remote == nil {
		return errNoRemote()
	}
	rec, err := s.store.GetConnection(id)
	if err != nil {
		return fmt.Errorf("主机不存在：%w", err)
	}
	s.closeSessionsOf(id)
	return s.remote.Disconnect(profile(rec))
}

// ListRemoteDir 列出远端目录；dir 为空时定位到上次浏览的目录，再退回家目录。
// 会顺带记录「上次浏览目录」，下次打开浏览器直接定位。
func (s *LogService) ListRemoteDir(id uint, dir string) (RemoteListing, error) {
	if s.remote == nil {
		return RemoteListing{}, errNoRemote()
	}
	rec, err := s.store.GetConnection(id)
	if err != nil {
		return RemoteListing{}, fmt.Errorf("主机不存在：%w", err)
	}
	prof := profile(rec)
	if !s.remote.Connected(prof) {
		return RemoteListing{}, fmt.Errorf("尚未连接到 %s，请先连接", prof.Display())
	}
	if strings.TrimSpace(dir) == "" {
		dir = rec.LastDir // 可为空：Manager 会退回家目录
	}
	cur, entries, err := s.remote.List(prof, dir)
	if err != nil {
		return RemoteListing{}, err
	}
	out := RemoteListing{
		Path:    cur,
		Entries: make([]RemoteEntry, 0, len(entries)),
	}
	if parent := path.Dir(cur); parent != cur {
		out.Parent = parent // 已在根目录时留空，前端不显示「上级」
	}
	if home, err := s.remote.Home(prof); err == nil {
		out.Home = home
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, RemoteEntry{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir,
			Size: e.Size, ModTime: e.ModTime, Mode: e.Mode,
		})
	}
	if err := s.store.TouchConnection(rec.ID, cur); err != nil {
		log.Printf("记录上次浏览目录失败：%v", err)
	}
	return out, nil
}

// OpenRemoteFile 打开远端文件（需先连接）。同一来源已打开时：远端文件在服务端
// 被追加/轮转后大小或修改时间会变，此时重新拉取并重建索引，而不是沿用旧索引。
func (s *LogService) OpenRemoteFile(id uint, remotePath string) (FileInfo, error) {
	if s.remote == nil {
		return FileInfo{}, errNoRemote()
	}
	rec, err := s.store.GetConnection(id)
	if err != nil {
		return FileInfo{}, fmt.Errorf("主机不存在：%w", err)
	}
	prof := profile(rec)
	if !s.remote.Connected(prof) {
		return FileInfo{}, fmt.Errorf("尚未连接到 %s，请先连接", prof.Display())
	}
	if strings.TrimSpace(remotePath) == "" {
		return FileInfo{}, errors.New("请选择要打开的文件")
	}
	p := path.Clean(remotePath)
	src := store.RemoteSource(rec.ID, prof.Display(), p)
	stat := func() (os.FileInfo, error) { return s.remote.Stat(prof, p) }
	open := func() (logfile.Source, error) { return s.openRemoteSource(rec, prof, p, false) }
	if info, ok := s.findOpen(src); ok {
		sess := s.get(info.ID)
		if sess == nil {
			return FileInfo{}, fmt.Errorf("file not found: %s", info.ID)
		}
		info, err := s.reuseOrReload(sess, src, stat, open)
		if err != nil {
			return FileInfo{}, err
		}
		s.recordOpen(src, sess) // 复用会话也算一次打开
		return info, nil
	}
	return s.openSession(src, "", true, open)
}

// PickKeyFile 弹出文件选择对话框挑私钥文件；用户取消返回空串。
func (s *LogService) PickKeyFile() (string, error) {
	app := application.Get()
	if app == nil {
		return "", errors.New("文件对话框不可用（无 GUI 环境）")
	}
	path, err := app.Dialog.OpenFile().
		CanChooseFiles(true).
		SetTitle("选择私钥文件").
		AddFilter("所有文件", "*.*").
		PromptForSingleSelection()
	if err != nil {
		if dialogCanceled(err) {
			return "", nil // 取消不是错误
		}
		return "", err
	}
	return path, nil
}

// closeSessionsOf 关闭某台远端主机上的全部文件会话（断开/删除主机时调用）。
func (s *LogService) closeSessionsOf(connID uint) {
	s.mu.Lock()
	ids := make([]string, 0, 2)
	for fileID, src := range s.sources {
		if src.Kind == store.KindRemote && src.ConnID == connID {
			ids = append(ids, fileID)
		}
	}
	s.mu.Unlock()
	for _, fileID := range ids {
		if err := s.CloseFile(fileID); err != nil {
			log.Printf("关闭远端文件会话失败：%v", err)
		}
	}
}
