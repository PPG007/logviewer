package service_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"logviewer/internal/service"
	"logviewer/internal/testssh"
)

// cacheEnv 一套「已连接的远端主机 + 一个日志文件」。
type cacheEnv struct {
	*remoteEnv
	path string
	// content 当前远端文件内容（追加/改写时同步更新）
	content string
}

func newCacheEnv(t *testing.T, svc *service.LogService, content string) *cacheEnv {
	t.Helper()
	// 整个测试包共用一个数据库：全局开关是持久化的，前一个用例可能关过它。
	// 显式恢复默认值，避免用例之间互相影响。
	if err := svc.SetCacheEnabled(true); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", content)
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)
	return &cacheEnv{remoteEnv: env, path: "/logs/app.log", content: content}
}

// write 改写远端文件（同步更新期望内容）。
func (e *cacheEnv) write(t *testing.T, content string) {
	t.Helper()
	testssh.WriteFile(t, e.root, "logs/app.log", content)
	e.content = content
}

func (e *cacheEnv) append(t *testing.T, extra string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(e.root, "logs", "app.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	e.content += extra
}

// open 打开远端文件并等索引完成，返回 FileInfo。
func (e *cacheEnv) open(t *testing.T, svc *service.LogService) service.FileInfo {
	t.Helper()
	info, err := svc.OpenRemoteFile(e.conn.ID, e.path)
	if err != nil {
		t.Fatalf("打开失败：%v", err)
	}
	waitReady(t, svc, info.ID)
	return info
}

// sourceKeyOf 该测试主机的来源键（与后端 store.Source.Key() 同构：sftp://user@host:port + path）。
// 整个测试包共用一个数据库与缓存目录，因此断言必须按来源键精确匹配——
// 只比路径是不够的（别的用例也用 /logs/app.log，只是端口不同）。
func sourceKeyOf(e *cacheEnv) string {
	ident := e.conn.User + "@" + e.conn.Host + ":" + strconv.Itoa(e.conn.Port)
	return "sftp://" + strings.ToLower(ident) + e.path
}

// 读出全部行内容，用于逐字节比对（只断言「零字节」会被"缓存返回空内容"骗过）。
func readAllLines(t *testing.T, svc *service.LogService, fileID string) string {
	t.Helper()
	st, err := svc.GetIndexStatus(fileID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := svc.GetLines(fileID, 0, st.TotalLines)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, r := range page.Rows {
		sb.WriteString(r.Raw)
		sb.WriteString("\n")
	}
	return sb.String()
}

// 核心性质：第二次打开同一份远端内容，一个字节都不传。
func TestRemoteCacheSecondOpenTransfersNothing(t *testing.T) {
	svc := service.NewLogService()
	content := strings.Repeat("hello world\n", 500)
	env := newCacheEnv(t, svc, content)

	first := env.open(t, svc)
	if got := readAllLines(t, svc, first.ID); got != content {
		t.Fatalf("首次内容不符（长度 %d，want %d）", len(got), len(content))
	}
	firstBytes := env.srv.BytesRead()
	if firstBytes < int64(len(content)) {
		t.Fatalf("首次打开应传输至少 %d 字节，实际 %d（夹具没生效？）", len(content), firstBytes)
	}
	if err := svc.CloseFile(first.ID); err != nil {
		t.Fatal(err)
	}

	// 第二次：命中缓存
	before := env.srv.BytesRead()
	second := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(second.ID) })
	if got := readAllLines(t, svc, second.ID); got != content {
		t.Fatalf("命中缓存时内容不符（长度 %d，want %d）", len(got), len(content))
	}
	if delta := env.srv.BytesRead() - before; delta != 0 {
		t.Fatalf("命中缓存却传输了 %d 字节（应为 0）", delta)
	}
	if second.TotalLines != first.TotalLines {
		t.Fatalf("行数不一致：%d vs %d", second.TotalLines, first.TotalLines)
	}
}

// 负向对照：关掉缓存后第二次打开必须重新传输，证明上面的 0 字节确实是缓存带来的。
func TestRemoteCacheDisabledStillTransfers(t *testing.T) {
	svc := service.NewLogService()
	content := strings.Repeat("x\n", 400)
	env := newCacheEnv(t, svc, content)
	if err := svc.SetCacheEnabled(false); err != nil {
		t.Fatal(err)
	}

	first := env.open(t, svc)
	if err := svc.CloseFile(first.ID); err != nil {
		t.Fatal(err)
	}
	before := env.srv.BytesRead()
	second := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(second.ID) })
	if delta := env.srv.BytesRead() - before; delta < int64(len(content)) {
		t.Fatalf("关闭缓存后应重新传输，实际只传了 %d 字节", delta)
	}
}

// 远端内容变化 → 重新拉取、内容正确、缓存被替换（第三次打开又是零传输）。
func TestRemoteCacheRefreshesOnChange(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "line-1\nline-2\n")

	first := env.open(t, svc)
	if err := svc.CloseFile(first.ID); err != nil {
		t.Fatal(err)
	}

	env.append(t, "line-3\n")
	before := env.srv.BytesRead()
	second := env.open(t, svc)
	if got := readAllLines(t, svc, second.ID); got != env.content {
		t.Fatalf("变化后内容 = %q，want %q", got, env.content)
	}
	if delta := env.srv.BytesRead() - before; delta == 0 {
		t.Fatal("内容变了却没有重新传输")
	}
	if err := svc.CloseFile(second.ID); err != nil {
		t.Fatal(err)
	}

	// 缓存已被新内容替换
	before = env.srv.BytesRead()
	third := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(third.ID) })
	if got := readAllLines(t, svc, third.ID); got != env.content {
		t.Fatalf("第三次内容 = %q", got)
	}
	if delta := env.srv.BytesRead() - before; delta != 0 {
		t.Fatalf("新缓存应命中，实际又传了 %d 字节", delta)
	}
}

// 「重新加载」在同大小同时刻原地改写后：本次看到新内容，且**下次打开也不能弹回旧的**。
func TestReloadFileBypassesAndRefreshesCache(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "AAAA\n")

	info := env.open(t, svc)
	if got := readAllLines(t, svc, info.ID); got != "AAAA\n" {
		t.Fatalf("初始内容 = %q", got)
	}

	// 同长度、mtime 秒级相同（同一秒内改写）：指纹判定不出来，只能靠强制刷新
	if err := os.WriteFile(filepath.Join(env.root, "logs", "app.log"), []byte("BBBB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.content = "BBBB\n"

	before := env.srv.BytesRead()
	if err := svc.ReloadFile(info.ID); err != nil {
		t.Fatalf("ReloadFile: %v", err)
	}
	waitReady(t, svc, info.ID)
	if got := readAllLines(t, svc, info.ID); got != "BBBB\n" {
		t.Fatalf("重新加载后内容 = %q，want BBBB", got)
	}
	if env.srv.BytesRead() == before {
		t.Fatal("强制刷新应重新拉取")
	}

	// 关键回归：关闭后重新打开，不能又命中那份旧缓存
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}
	reopened := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(reopened.ID) })
	if got := readAllLines(t, svc, reopened.ID); got != "BBBB\n" {
		t.Fatalf("重新加载后再次打开 = %q，说明缓存里的旧内容又回来了", got)
	}
}

// 缓存管理：列出条目、清空。
func TestCacheInfoAndClear(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "a\nb\nc\n")
	info := env.open(t, svc)
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}

	ci, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	if !ci.Available || !ci.Enabled {
		t.Fatalf("缓存应可用且默认开启：%+v", ci)
	}
	if ci.Dir == "" || ci.Limit <= 0 {
		t.Fatalf("缓存目录/上限未填：%+v", ci)
	}
	found := false
	for _, e := range ci.Entries {
		if e.SourceKey == sourceKeyOf(env) {
			found = true
		}
	}
	if !found {
		t.Fatalf("缓存条目里没有 %s：%+v", env.path, ci.Entries)
	}
	if ci.TotalBytes <= 0 {
		t.Fatalf("总占用应大于 0：%+v", ci)
	}

	if err := svc.ClearCache(); err != nil {
		t.Fatal(err)
	}
	ci2, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	if len(ci2.Entries) != 0 || ci2.TotalBytes != 0 {
		t.Fatalf("清空后仍有条目：%+v", ci2)
	}
	// 清空后重新打开应重新传输
	before := env.srv.BytesRead()
	again := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(again.ID) })
	if env.srv.BytesRead() == before {
		t.Fatal("清空缓存后应重新传输")
	}
}

// 删除主机连带清掉它的缓存。
func TestDeleteConnectionClearsCache(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "x\ny\n")
	info := env.open(t, svc)
	waitReady(t, svc, info.ID)

	ci, err := svc.GetCacheInfo()
	if err != nil || len(ci.Entries) == 0 {
		t.Fatalf("应有缓存条目：%+v（err=%v）", ci, err)
	}
	if err := svc.DeleteConnection(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	ci2, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ci2.Entries {
		if e.SourceKey == sourceKeyOf(env) {
			t.Fatalf("删除主机应连带清理其缓存：%+v", e)
		}
	}
}

// 单条缓存可单独删除。
func TestRemoveCacheEntry(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "1\n2\n")
	info := env.open(t, svc)
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatal(err)
	}

	ci, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	var key string
	for _, e := range ci.Entries {
		if e.SourceKey == sourceKeyOf(env) {
			key = e.SourceKey
		}
	}
	if key == "" {
		t.Fatalf("找不到缓存条目：%+v", ci.Entries)
	}
	if err := svc.RemoveCacheEntry(key); err != nil {
		t.Fatal(err)
	}
	ci2, _ := svc.GetCacheInfo()
	for _, e := range ci2.Entries {
		if e.SourceKey == key {
			t.Fatal("条目未被删除")
		}
	}
	if err := svc.RemoveCacheEntry(""); err == nil {
		t.Fatal("缺少标识应报错")
	}
}

// 按主机关闭缓存：该主机不产生缓存，其他主机不受影响。
func TestPerHostCacheToggle(t *testing.T) {
	svc := service.NewLogService()
	env := newCacheEnv(t, svc, "p\nq\n")

	off := false
	c, err := svc.SaveConnection(service.Connection{
		ID: env.conn.ID, Name: env.conn.Name, Host: env.conn.Host, Port: env.conn.Port,
		User: env.conn.User, AuthMethod: env.conn.AuthMethod, Cache: &off,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Cache == nil || *c.Cache {
		t.Fatalf("按主机的关闭开关未落库：%+v", c)
	}
	env.connect(t, svc) // 改配置会断开旧连接

	info := env.open(t, svc)
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	ci, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ci.Entries {
		if e.SourceKey == sourceKeyOf(env) {
			t.Fatalf("该主机已关闭缓存，不应有条目：%+v", e)
		}
	}
}

// 缓存目录必须是可配置的：测试不得写进真实用户目录。
func TestCacheDirIsolatedInTests(t *testing.T) {
	svc := service.NewLogService()
	ci, err := svc.GetCacheInfo()
	if err != nil {
		t.Fatal(err)
	}
	if ci.Dir == "" {
		t.Fatal("缓存目录为空")
	}
	if !strings.Contains(strings.ToLower(ci.Dir), "logviewer-test") &&
		!strings.Contains(strings.ToLower(ci.Dir), os.TempDir()[0:4]) {
		t.Fatalf("测试缓存目录应在临时目录下（LOGVIEWER_CACHE_DIR 未生效？）：%s", ci.Dir)
	}
}

// 口令明文落库：连接成功后保存，之后连接不必再输入；可显式清除。
func TestConnectionSecretPersisted(t *testing.T) {
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, t.TempDir())
	// newRemoteEnv 里已带口令连接过一次（凭据在 connect 时传入）
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: env.password}); err != nil {
		t.Fatal(err)
	}
	conn, ok := findConn(t, svc, env.conn.ID)
	if !ok {
		t.Fatal("主机未出现在列表中")
	}
	if !conn.HasPassword {
		t.Fatal("连接成功后应标记「已保存口令」")
	}

	// 断开后仅凭库里保存的口令重连（不再传凭据）
	if err := svc.DisconnectRemote(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{}); err != nil {
		t.Fatalf("应能用已保存的口令重连：%v", err)
	}
	if _, err := svc.ListRemoteDir(env.conn.ID, "/"); err != nil {
		t.Fatalf("重连后应可用：%v", err)
	}

	// 清除后：不再标记，且空凭据重连会失败（要求重新输入）
	if err := svc.ClearConnectionSecret(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	conn, _ = findConn(t, svc, env.conn.ID)
	if conn.HasPassword {
		t.Fatal("清除后不应再标记「已保存口令」")
	}
	if err := svc.DisconnectRemote(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{}); err == nil {
		t.Fatal("清除口令后空凭据不应连上")
	}
}

// 口令错误时不覆盖库里已保存的正确口令。
func TestWrongPasswordDoesNotOverwrite(t *testing.T) {
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, t.TempDir())
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: env.password}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DisconnectRemote(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: "wrong"}); err == nil {
		t.Fatal("错误密码应失败")
	}
	// 库里仍是正确口令：空凭据仍能连上
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{}); err != nil {
		t.Fatalf("错密码不应覆盖已保存的正确口令：%v", err)
	}
}

// 回归：编辑保存主机不能抹掉已保存的口令。
// （曾经 SaveConnection 会写入来自前端 DTO 的空口令，把库里的口令覆盖没，
//  表现为「断开后编辑一下，就再也连不上，提示请输入密码」。）
func TestEditConnectionKeepsSavedSecret(t *testing.T) {
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, t.TempDir())
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: env.password}); err != nil {
		t.Fatal(err)
	}
	conn, _ := findConn(t, svc, env.conn.ID)
	if !conn.HasPassword {
		t.Fatal("先决条件不成立：连接成功后应已保存口令")
	}

	// 模拟「编辑」：前端回传的 DTO 不带口令，只改备注名
	conn.Name = "改过名字的主机"
	conn.HasPassword = true // DTO 里是布尔标记，不是口令本身
	saved, err := svc.SaveConnection(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.HasPassword {
		t.Fatal("编辑保存后口令被抹掉了")
	}

	// 断开后仅凭库里的口令重连（这正是用户点「连接」按钮走的路径）
	if err := svc.DisconnectRemote(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{}); err != nil {
		t.Fatalf("编辑保存后应仍能用已保存的口令连接：%v", err)
	}
	if _, err := svc.ListRemoteDir(env.conn.ID, "/"); err != nil {
		t.Fatalf("重连后应可用：%v", err)
	}
}

// 改备注名不应断开连接、也不应关掉该主机上已打开的文件；
// 改了连接参数（端口/主机/用户）才必须断开。
func TestEditMetaKeepsConnection(t *testing.T) {
	svc := service.NewLogService()
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "a\nb\n")
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)

	// 只改备注名：连接与已打开的文件都应保持
	renamed := env.conn
	renamed.Name = "生产-改名"
	saved, err := svc.SaveConnection(renamed)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Connected {
		t.Fatal("只改备注名不应断开连接")
	}
	if _, err := svc.GetIndexStatus(info.ID); err != nil {
		t.Fatalf("只改备注名不应关掉已打开的文件：%v", err)
	}

	// 改端口：属于连接参数变化，必须断开
	moved := saved
	moved.Port = saved.Port + 1
	after, err := svc.SaveConnection(moved)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connected {
		t.Fatal("改了端口应断开旧连接")
	}
	if _, err := svc.GetIndexStatus(info.ID); err == nil {
		t.Fatal("连接参数变化后，该主机的文件会话应被关闭")
	}
}
