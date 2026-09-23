package service_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"logviewer/internal/service"
	"logviewer/internal/testssh"
)

// appendLines 追加内容（模拟日志被写入/轮转后的新内容）。
func appendLines(t *testing.T, path string, lines []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

// 本地文件被追加后重新打开：应重建索引看到新行，而不是沿用旧会话。
func TestLocalReopenPicksUpChanges(t *testing.T) {
	path := writeLines(t, []string{`{"level":"INFO","msg":"first"}`})
	svc := service.NewLogService()
	info, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	appendLines(t, path, []string{`{"level":"WARN","msg":"second"}`})

	again, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != info.ID {
		t.Fatalf("会话 id 应保持不变：%q → %q", info.ID, again.ID)
	}
	if again.Status != "indexing" {
		t.Fatalf("文件已变化，重新打开应重建索引，实际状态 = %s", again.Status)
	}
	waitReady(t, svc, again.ID)

	st, err := svc.GetIndexStatus(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalLines != 2 {
		t.Fatalf("TotalLines = %d, want 2（新行未被索引）", st.TotalLines)
	}
	page, err := svc.GetLines(again.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 2 || page.Rows[1].Message != "second" {
		t.Fatalf("新内容未生效：%+v", page.Rows)
	}
}

// 文件没变化时重新打开：直接复用会话，不做无谓的重新读取。
func TestLocalReopenUnchangedReusesSession(t *testing.T) {
	path := writeLines(t, []string{`{"level":"INFO","msg":"only"}`})
	svc := service.NewLogService()
	info, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	again, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != info.ID || again.Status != "ready" {
		t.Fatalf("未变化的文件应直接复用会话：id=%q status=%s", again.ID, again.Status)
	}
}

// ReloadFile 是强制刷新：即使大小与修改时间都没变也重新读取。
func TestReloadFileForcesReindex(t *testing.T) {
	path := writeLines(t, []string{`{"level":"INFO","msg":"same"}`})
	svc := service.NewLogService()
	info, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	// 内容变了但大小不变（模拟同长度改写）：靠大小/时间判断不出来，必须能强制刷新
	if err := os.WriteFile(path, []byte(`{"level":"ERROR","msg":"sme"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReloadFile(info.ID); err != nil {
		t.Fatalf("ReloadFile: %v", err)
	}
	waitReady(t, svc, info.ID)

	page, err := svc.GetLines(info.ID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Level != "ERROR" {
		t.Fatalf("强制刷新未生效：%+v", page.Rows)
	}
}

// 远端文件在服务端被追加后重新打开：必须重新拉取，而不是遇到「重复文件」就不拉了。
func TestRemoteReopenPicksUpChanges(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "line-1\nline-2\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)
	if st, _ := svc.GetIndexStatus(info.ID); st.TotalLines != 2 {
		t.Fatalf("TotalLines = %d, want 2", st.TotalLines)
	}

	// 服务端追加日志
	f, err := os.OpenFile(filepath.Join(root, "logs", "app.log"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line-3\nline-4\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	again, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != info.ID {
		t.Fatalf("会话 id 应保持不变：%q → %q", info.ID, again.ID)
	}
	if again.Status != "indexing" {
		t.Fatalf("远端文件已变化，重新打开应重新拉取，实际状态 = %s", again.Status)
	}
	waitReady(t, svc, again.ID)

	st, err := svc.GetIndexStatus(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalLines != 4 {
		t.Fatalf("TotalLines = %d, want 4（新增的远端内容未被拉取）", st.TotalLines)
	}
	page, err := svc.GetLines(again.ID, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 该文件是纯文本（非 JSON），断言原始行内容
	if len(page.Rows) != 2 || page.Rows[0].Raw != "line-3" || page.Rows[1].Raw != "line-4" {
		t.Fatalf("新增远端内容未生效：%+v", page.Rows)
	}
	// 历史记录里的行数也要跟着更新
	waitRecentLines(t, svc, "/logs/app.log", 4)
}

// 远端文件没变化时不重复拉取（GB 级文件重新点一次不该白传一遍）。
func TestRemoteReopenUnchangedReusesSession(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "a\nb\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	again, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != info.ID || again.Status != "ready" {
		t.Fatalf("未变化的远端文件应直接复用会话：id=%q status=%s", again.ID, again.Status)
	}
}

// 远端文件被轮转（内容变短）后重新打开：行数应减少，不能残留旧行。
func TestRemoteReopenAfterRotate(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/rot.log", "old-1\nold-2\nold-3\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/rot.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)

	// 轮转：原文件被清空重写
	testssh.WriteFile(t, root, "logs/rot.log", "fresh\n")

	again, err := svc.OpenRemoteFile(env.conn.ID, "/logs/rot.log")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, again.ID)
	st, err := svc.GetIndexStatus(again.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalLines != 1 {
		t.Fatalf("TotalLines = %d, want 1（轮转后的旧行未清除）", st.TotalLines)
	}
	page, err := svc.GetLines(again.ID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Raw != "fresh" {
		t.Fatalf("轮转后内容 = %+v", page.Rows)
	}
}

// 断开连接后无法重新加载远端文件（凭据不落盘，必须重新连接）。
func TestRemoteReloadRequiresConnection(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/app.log", "x\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)

	err = svc.DisconnectRemote(env.conn.ID) // 断开时会关闭该主机上的文件会话
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ReloadFile(info.ID); err == nil {
		t.Fatal("会话已随断开关闭，重新加载应报错")
	}

	// 重新连接后可以重新打开并加载
	env.connect(t, svc)
	info2, err := svc.OpenRemoteFile(env.conn.ID, "/logs/app.log")
	if err != nil {
		t.Fatalf("重连后打开失败：%v", err)
	}
	t.Cleanup(func() { svc.CloseFile(info2.ID) })
	waitReady(t, svc, info2.ID)
}

// 不存在的会话 id 应报错而非 panic。
func TestReloadUnknownFile(t *testing.T) {
	svc := service.NewLogService()
	if err := svc.ReloadFile("nope"); err == nil {
		t.Fatal("未知会话重新加载应报错")
	}
}

// 一次性核对：远端重新加载后历史记录里的行数与大小都跟着更新。
func TestRemoteReloadUpdatesHistoryMeta(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "logs/meta.log", "1\n2\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	env.connect(t, svc)

	info, err := svc.OpenRemoteFile(env.conn.ID, "/logs/meta.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	waitReady(t, svc, info.ID)
	waitRecentLines(t, svc, "/logs/meta.log", 2)

	appendLines(t, filepath.Join(root, "logs", "meta.log"), []string{"3", "4", "5"})
	if _, err := svc.OpenRemoteFile(env.conn.ID, "/logs/meta.log"); err != nil {
		t.Fatal(err)
	}
	waitReady(t, svc, info.ID)
	waitRecentLines(t, svc, "/logs/meta.log", 5)

	rec := findRecent(t, svc, "/logs/meta.log")
	if rec.OpenCount < 2 {
		t.Fatalf("OpenCount = %d, want >= 2", rec.OpenCount)
	}
	if rec.Dir != "/logs" || rec.Name != "meta.log" {
		t.Fatalf("远端路径切分错误：%+v", rec)
	}
}

// 保证测试用到的端口解析与主机标识一致（避免 future 改动悄悄改掉展示格式）。
func TestRemoteDisplayFormat(t *testing.T) {
	root := t.TempDir()
	testssh.WriteFile(t, root, "a.log", "x\n")
	svc := service.NewLogService()
	env := newRemoteEnv(t, svc, root)
	host, portStr, _ := net.SplitHostPort(env.srv.Addr)
	want := fmt.Sprintf("%s@%s:%s", env.srv.User, host, portStr)
	if env.conn.Name != "测试主机" {
		t.Fatalf("主机名 = %q", env.conn.Name)
	}
	env.connect(t, svc)
	info, err := svc.OpenRemoteFile(env.conn.ID, "/a.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.CloseFile(info.ID) })
	if info.Remote != want {
		t.Fatalf("Remote = %q, want %q", info.Remote, want)
	}
	if _, err := strconv.Atoi(portStr); err != nil {
		t.Fatalf("端口解析异常：%v", err)
	}
}
