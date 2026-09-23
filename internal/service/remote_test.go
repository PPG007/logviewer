package service_test

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"logviewer/internal/search"
	"logviewer/internal/service"
	"logviewer/internal/testssh"
)

// remoteEnv 一个内嵌 SSH 服务端 + 已保存并连上的主机配置。
type remoteEnv struct {
	srv      *testssh.Server
	conn     service.Connection
	root     string
	password string
}

// newRemoteEnv 起测试服务端，并在 svc 里保存主机配置（不连接）。
func newRemoteEnv(t *testing.T, svc *service.LogService, root string) *remoteEnv {
	t.Helper()
	password := "pw-" + testssh.RandHex(4)
	srv := testssh.Start(t, testssh.Options{Root: root, Password: password})
	host, portStr, err := net.SplitHostPort(srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := svc.SaveConnection(service.Connection{
		Name: "测试主机", Host: host, Port: port, User: srv.User, AuthMethod: "password",
	})
	if err != nil {
		t.Fatalf("SaveConnection: %v", err)
	}
	return &remoteEnv{srv: srv, conn: conn, root: root, password: password}
}

func (e *remoteEnv) connect(t *testing.T, svc *service.LogService) {
	t.Helper()
	if err := svc.ConnectRemote(e.conn.ID, service.Credential{Password: e.password}); err != nil {
		t.Fatalf("ConnectRemote: %v", err)
	}
}

// findConn 按 ID 取主机（整个测试包共用一个数据库，不能断言全表内容）。
func findConn(t *testing.T, svc *service.LogService, id uint) (service.Connection, bool) {
	t.Helper()
	conns, err := svc.ListConnections()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.ID == id {
			return c, true
		}
	}
	return service.Connection{}, false
}

// 端到端：连接 → 浏览目录 → 打开远端文件 → 索引 → 翻页 → 检索。
func TestRemoteBrowseOpenAndSearch(t *testing.T) {
	var lines []string
	for i := 0; i < 300; i++ {
		lv := "INFO"
		if i%10 == 0 {
			lv = "ERROR"
		}
		lines = append(lines, fmt.Sprintf(`{"time":1720000000000,"level":"%s","msg":"line %d","n":%d}`, lv, i, i))
	}
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", strings.Join(lines, "\n")+"\n")
	testssh.WriteFile(t, root, "logs/other.txt", "hello")
	testssh.WriteFile(t, root, "logs/readme.md", "doc")

	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	listing, err := svc.ListRemoteDir(env.conn.ID, "/logs")
	if err != nil {
		t.Fatalf("ListRemoteDir: %v", err)
	}
	if listing.Path != "/logs" {
		t.Fatalf("Path = %q, want /logs", listing.Path)
	}
	if listing.Parent != "/" {
		t.Fatalf("Parent = %q, want /", listing.Parent)
	}
	if listing.Home == "" {
		t.Fatal("Home 不应为空（前端快捷跳转用）")
	}
	names := make([]string, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		names = append(names, e.Name)
	}
	want := []string{"app.log", "other.txt", "readme.md"} // 无子目录，按名称排序
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("目录项 = %v, want %v", names, want)
	}
	if listing.Entries[0].Size == 0 || listing.Entries[0].ModTime == 0 {
		t.Fatalf("目录项缺少大小/时间：%+v", listing.Entries[0])
	}
	// 目录浏览应记住位置
	conn, ok := findConn(t, svc, env.conn.ID)
	if !ok {
		t.Fatal("主机未出现在列表中")
	}
	if conn.LastDir != "/logs" {
		t.Fatalf("LastDir = %q, want /logs", conn.LastDir)
	}
	if !conn.Connected {
		t.Fatal("列表应显示已连接")
	}

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatalf("OpenRemoteFile: %v", err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	if info.Kind != "remote" || info.Remote != env.conn.User+"@"+env.conn.Host+":"+strconv.Itoa(env.conn.Port) {
		t.Fatalf("FileInfo 来源信息 = %+v", info)
	}
	if info.Name != "app.log" {
		t.Fatalf("Name = %q", info.Name)
	}
	waitReady(t, svc, info.ID)

	st, err := svc.GetIndexStatus(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalLines != 300 {
		t.Fatalf("TotalLines = %d, want 300", st.TotalLines)
	}

	page, err := svc.GetLines(info.ID, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 300 || len(page.Rows) != 3 {
		t.Fatalf("GetLines = total %d rows %d", page.Total, len(page.Rows))
	}
	if page.Rows[0].LineNo != 10 || !strings.Contains(page.Rows[0].Message, "line 10") {
		t.Fatalf("首行 = %+v", page.Rows[0])
	}

	// 检索走 Scan：远端源需支持 Seek(0) + 顺序读
	res, err := svc.Search(info.ID, "tab-1", search.Query{
		Conditions: []search.Condition{{Field: "level", Op: "=", Value: "ERROR"}},
	}, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 30 {
		t.Fatalf("命中 = %d, want 30", res.Total)
	}
	if len(res.Rows) != 20 {
		t.Fatalf("首页行数 = %d, want 20", len(res.Rows))
	}

	// 同一个文件再次打开应复用会话（不重复占索引内存）
	again, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != info.ID {
		t.Fatalf("应复用会话：%q vs %q", again.ID, info.ID)
	}
}

// 未连接、密码错误、打开目录等情况要给出可读错误。
func TestRemoteErrors(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "x\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)

	// 未连接
	if _, err := svc.ListRemoteDir(env.conn.ID, "/logs"); err == nil || !strings.Contains(err.Error(), "请先连接") {
		t.Fatalf("未连接时列目录的错误 = %v", err)
	}
	if _, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log"); err == nil || !strings.Contains(err.Error(), "请先连接") {
		t.Fatalf("未连接时打开文件的错误 = %v", err)
	}

	// 密码错误
	err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("错误密码的错误 = %v", err)
	}

	// 连上后：目录不存在 / 打开目录 / 打开不存在的文件
	env.connect(t, svc)
	if _, err := svc.ListRemoteDir(env.conn.ID, "/nope"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("列不存在目录的错误 = %v", err)
	}
	if _, err := svc.OpenRemoteFile(env.conn.ID, "/logs"); err == nil || !strings.Contains(err.Error(), "目录") {
		t.Fatalf("打开目录的错误 = %v", err)
	}
	if _, err := svc.OpenRemoteFile(env.conn.ID, "/logs/missing.log"); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("打开不存在文件的错误 = %v", err)
	}

	// 不存在的主机 id
	if err := svc.ConnectRemote(99999, service.Credential{Password: "x"}); err == nil {
		t.Fatal("不存在的主机应报错")
	}
}

// 历史记录：远端条目带来源信息；未连接时点击给出「需要先连接」；连上后可打开。
func TestRemoteRecentFiles(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "a\nb\nc\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)

	rec := findRecent(t, svc, "/logs/app.log")
	if rec.Kind != "remote" || rec.ConnID != env.conn.ID {
		t.Fatalf("历史记录缺少来源信息：%+v", rec)
	}
	if !rec.Connected {
		t.Fatal("已连接的主机在历史记录中应标记为已连接")
	}
	if rec.Name != "app.log" || rec.Dir != "/logs" {
		t.Fatalf("远端路径切分错误：name=%q dir=%q", rec.Name, rec.Dir)
	}
	waitRecentLines(t, svc, "/logs/app.log", 3)

	// 已连接：直接打开
	again, err := svc.OpenRecentFile(rec.ID)
	if err != nil {
		t.Fatalf("OpenRecentFile: %v", err)
	}
	if again.ID != info.ID {
		t.Fatalf("应复用会话：%q vs %q", again.ID, info.ID)
	}

	// 断开后：文件会话被关闭，历史记录变为未连接，再次点击提示需先连接
	if err := svc.DisconnectRemote(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetIndexStatus(info.ID); err == nil {
		t.Fatal("断开主机应关闭其上的文件会话")
	}
	rec = findRecent(t, svc, "/logs/app.log")
	if rec.Connected {
		t.Fatal("断开后应标记为未连接")
	}
	if _, err := svc.OpenRecentFile(rec.ID); err == nil || !strings.Contains(err.Error(), "需要先连接") {
		t.Fatalf("未连接时打开历史文件的错误 = %v", err)
	}
	// 本地记录（不存在）仍走原有的「文件不存在」路径
	// 远端记录不应被误判为「已丢失」（列表阶段不探测远端存在性）
	if !rec.Exists {
		t.Fatal("远端记录不应在列表阶段被标记为已丢失")
	}
}

// 删除主机会连带删除它的历史文件记录（避免留下打不开的死记录）。
func TestDeleteConnectionRemovesItsFiles(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "x\ny\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)
	local := writeLines(t, []string{"keep"})
	linfo, err := svc.OpenFile(local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(linfo.ID) })

	if err := svc.DeleteConnection(env.conn.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := findConn(t, svc, env.conn.ID); ok {
		t.Fatal("删除后该主机仍在列表中")
	}
	recent, err := svc.ListRecentFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recent {
		if r.ConnID == env.conn.ID {
			t.Fatalf("删除主机应连带删除其文件记录：%+v", r)
		}
	}
	// 本地记录不应受影响
	found := false
	for _, r := range recent {
		if r.Path == local {
			found = true
		}
	}
	if !found {
		t.Fatalf("本地记录被误删：%+v", recent)
	}
}

// 编辑主机配置后应断开旧连接，避免仍指向旧地址。
func TestSaveConnectionDropsOldLink(t *testing.T) {
	svc := service.NewLogService()
	root := t.TempDir()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	updated := env.conn
	updated.Port = env.conn.Port + 1 // 改成一个连不通的端口
	saved, err := svc.SaveConnection(updated)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Connected {
		t.Fatal("改动配置后不应仍显示已连接")
	}
	conn, ok := findConn(t, svc, env.conn.ID)
	if !ok {
		t.Fatal("主机未出现在列表中")
	}
	if conn.Connected {
		t.Fatalf("旧连接未断开：%+v", conn)
	}
	if _, err := svc.ListRemoteDir(env.conn.ID, "/"); err == nil || !strings.Contains(err.Error(), "请先连接") {
		t.Fatalf("改动后应要求重新连接，实际错误 = %v", err)
	}
}

// 保存主机时的入参校验。
func TestSaveConnectionValidation(t *testing.T) {
	svc := service.NewLogService()
	if _, err := svc.SaveConnection(service.Connection{User: "u"}); err == nil {
		t.Fatal("缺少主机应报错")
	}
	if _, err := svc.SaveConnection(service.Connection{Host: "h"}); err == nil {
		t.Fatal("缺少用户名应报错")
	}
	if _, err := svc.SaveConnection(service.Connection{Host: "h", User: "u", AuthMethod: "totp"}); err == nil {
		t.Fatal("不支持的认证方式应报错")
	}
	// 默认值：认证方式缺省为密码，端口缺省为 22，名称缺省为 user@host
	c, err := svc.SaveConnection(service.Connection{Host: "h", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthMethod != "password" || c.Port != 22 || c.Name != "u@h" {
		t.Fatalf("默认值未生效：%+v", c)
	}
}

// 索引中途关闭远端文件：会话应尽快结束（不卡在传输上）。
func TestRemoteCloseFileMidIndex(t *testing.T) {
	root := t.TempDir()
	blob := strings.Repeat("0123456789abcdef0123456789abcdef\n", (32<<20)/33)
	testssh.WriteFile(t, root, "logs/huge.log", blob)
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	// 用限速服务端重来一遍，确保索引远未完成时关闭
	env.srv.Close()
	throttled := testssh.Start(t, testssh.Options{
		Root: root, Password: env.password, ThrottleBytesPerSec: 4 << 20,
	})
	host, portStr, _ := net.SplitHostPort(throttled.Addr)
	port, _ := strconv.Atoi(portStr)
	if _, err := svc.SaveConnection(service.Connection{
		ID: env.conn.ID, Name: "慢链路", Host: host, Port: port, User: throttled.User, AuthMethod: "password",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConnectRemote(env.conn.ID, service.Credential{Password: env.password}); err != nil {
		t.Fatal(err)
	}

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/huge.log")
	if err != nil {
		t.Fatal(err)
	}
	// 等索引确实开始（限速下必然远未读完）
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := svc.GetIndexStatus(info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if st.Percent > 0 && !st.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等待索引进度超时")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	if err := svc.CloseFile(info.ID); err != nil {
		t.Fatalf("CloseFile: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("索引中关闭耗时 %v，说明关闭被传输阻塞了", elapsed)
	}
}
