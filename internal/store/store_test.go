package store

import (
	"path/filepath"
	"runtime"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// 打开两次同一路径只保留一条记录，且打开次数累加（重启后可一键重开的关键语义）。
func TestTouchUpsert(t *testing.T) {
	st := newTestStore(t)
	if err := st.Touch(LocalSource(`C:\logs\app.log`), 100, 1000); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := st.Touch(LocalSource(`C:\logs\app.log`), 200, 2000); err != nil {
		t.Fatalf("Touch again: %v", err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	if got[0].OpenCount != 2 {
		t.Errorf("OpenCount = %d, want 2", got[0].OpenCount)
	}
	if got[0].Size != 200 || got[0].ModTime != 2000 {
		t.Errorf("size/modtime not updated: %+v", got[0])
	}
	if got[0].Name != "app.log" {
		t.Errorf("Name = %q, want app.log", got[0].Name)
	}
}

// 同名不同目录必须是两条记录，且能靠 Dir 区分。
func TestSameNameDifferentDir(t *testing.T) {
	st := newTestStore(t)
	if err := st.Touch(LocalSource(filepath.Join("D:", "a", "app.log")), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(LocalSource(filepath.Join("D:", "b", "app.log")), 1, 1); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 records, got %d", len(got))
	}
	if got[0].Dir == got[1].Dir {
		t.Errorf("同名文件未能区分目录：%q / %q", got[0].Dir, got[1].Dir)
	}
}

// Windows 下大小写不同的同一路径视为同一文件（仅一条记录）。
func TestPathKeyCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅在大小写不敏感的文件系统上成立")
	}
	st := newTestStore(t)
	if err := st.Touch(LocalSource(`C:\Logs\App.log`), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(LocalSource(`c:\logs\app.log`), 1, 1); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
}

func TestSetTotalLinesDeleteClear(t *testing.T) {
	st := newTestStore(t)
	if err := st.Touch(LocalSource(`C:\logs\app.log`), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTotalLines(LocalSource(`C:\logs\app.log`), 1234); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TotalLines != 1234 {
		t.Fatalf("TotalLines 未写入：%+v", got)
	}
	if err := st.Delete(got[0].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.List(); len(got) != 0 {
		t.Fatalf("Delete 后仍有记录：%+v", got)
	}
	if err := st.Touch(LocalSource(`C:\logs\b.log`), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.List(); len(got) != 0 {
		t.Fatalf("Clear 后仍有记录：%+v", got)
	}
}

// 记录按最近打开时间倒序（最新打开的排最前）。
func TestListOrder(t *testing.T) {
	st := newTestStore(t)
	_ = st.Touch(LocalSource(`C:\logs\old.log`), 1, 1)
	_ = st.Touch(LocalSource(`C:\logs\new.log`), 1, 1)
	_ = st.Touch(LocalSource(`C:\logs\old.log`), 1, 1) // 重新打开旧文件 → 应排到最前
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "old.log" {
		t.Fatalf("排序不符合最近打开优先：%+v", got)
	}
}

// 本地来源的键必须与既有实现逐字节一致：否则升级后旧记录会全部「失联」。
func TestLocalKeyUnchanged(t *testing.T) {
	p := `C:\logs\app.log`
	abs, err := Normalize(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := LocalSource(p).Key(), PathKey(abs); got != want {
		t.Fatalf("本地键 = %q, want %q", got, want)
	}
}

// 不同主机上的同一路径必须是三条互不干扰的记录（本地同路径也算一条）。
func TestSamePathDifferentHosts(t *testing.T) {
	st := newTestStore(t)
	if err := st.Touch(RemoteSource(1, "root@10.0.0.5:22", "/var/log/app.log"), 10, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(RemoteSource(2, "root@10.0.0.6:22", "/var/log/app.log"), 20, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(LocalSource(`/var/log/app.log`), 30, 1); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 records, got %d：%+v", len(got), got)
	}
	// 远端记录能区分主机，且行数互不覆盖
	if err := st.SetTotalLines(RemoteSource(1, "root@10.0.0.5:22", "/var/log/app.log"), 111); err != nil {
		t.Fatal(err)
	}
	byRemote := map[string]int64{}
	kinds := map[string]int{}
	for _, f := range got {
		byRemote[f.Remote] = f.TotalLines
		kinds[f.Kind]++
	}
	if kinds[KindRemote] != 2 || kinds[KindLocal] != 1 {
		t.Fatalf("Kind 统计异常：%+v", kinds)
	}
	// 重新读取校验行数只落到 10.0.0.5 那条
	got, _ = st.List()
	for _, f := range got {
		if f.Remote == "root@10.0.0.5:22" && f.TotalLines != 111 {
			t.Errorf("行数未写入该主机记录：%+v", f)
		}
		if f.Remote == "root@10.0.0.6:22" && f.TotalLines != 0 {
			t.Errorf("行数串到了另一台主机：%+v", f)
		}
	}
}

// 远端记录重复打开只累加次数，且 Name/Dir 按 POSIX 规则切分。
func TestRemoteTouchUpsert(t *testing.T) {
	st := newTestStore(t)
	src := RemoteSource(7, "deploy@10.1.2.3:2222", "/srv/logs/service-a/app.log")
	if err := st.Touch(src, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(src, 2, 2); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	if got[0].OpenCount != 2 || got[0].Size != 2 {
		t.Fatalf("重复打开未累加/更新：%+v", got[0])
	}
	if got[0].Name != "app.log" || got[0].Dir != "/srv/logs/service-a" {
		t.Fatalf("远端路径切分错误：name=%q dir=%q", got[0].Name, got[0].Dir)
	}
	if got[0].Kind != KindRemote || got[0].ConnID != 7 || got[0].Remote != "deploy@10.1.2.3:2222" {
		t.Fatalf("来源信息未落库：%+v", got[0])
	}
}

func TestConnectionCRUD(t *testing.T) {
	st := newTestStore(t)
	c, err := st.SaveConnection(Connection{
		Host: "10.0.0.5", Port: 22, User: "root", AuthMethod: "key", KeyPath: "/home/me/.ssh/id_ed25519",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 {
		t.Fatal("新增应回填 ID")
	}
	if c.Name != "root@10.0.0.5" {
		t.Fatalf("未填名称时应生成默认名：%q", c.Name)
	}

	// 更新（含把 KeyPath 清空：显式列更新不应忽略零值）
	c.KeyPath = ""
	c.Name = "生产主机"
	c.Port = 2222
	upd, err := st.SaveConnection(c)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Name != "生产主机" || upd.Port != 2222 || upd.KeyPath != "" {
		t.Fatalf("更新未生效：%+v", upd)
	}

	list, err := st.ListConnections()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "生产主机" {
		t.Fatalf("ListConnections = %+v", list)
	}

	// 上次浏览目录
	if err := st.TouchConnection(c.ID, "/srv/logs"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetConnection(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastDir != "/srv/logs" {
		t.Fatalf("LastDir = %q", got.LastDir)
	}
	if err := st.TouchConnection(c.ID, ""); err != nil { // 空目录不应清掉已有值
		t.Fatal(err)
	}
	got, _ = st.GetConnection(c.ID)
	if got.LastDir != "/srv/logs" {
		t.Fatalf("空目录覆盖了 LastDir：%q", got.LastDir)
	}

	// 删除主机应连带删除它的文件记录
	if err := st.Touch(RemoteSource(c.ID, "root@10.0.0.5:2222", "/srv/logs/app.log"), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(LocalSource(`C:\logs\keep.log`), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteConnection(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetConnection(c.ID); err == nil {
		t.Fatal("删除后不应还能取到该主机")
	}
	files, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "keep.log" {
		t.Fatalf("删除主机应连带删除其文件记录：%+v", files)
	}
}

// 端口非法时回落 22（避免前端传 0 导致连接信息不可用）。
func TestSaveConnectionDefaultPort(t *testing.T) {
	st := newTestStore(t)
	c, err := st.SaveConnection(Connection{Host: "h", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 22 {
		t.Fatalf("Port = %d, want 22", c.Port)
	}
}

// ---------------- 缓存元数据与配置 ----------------

func TestCacheEntryCRUD(t *testing.T) {
	st := newTestStore(t)
	key := "sftp://deploy@10.0.0.5:22/var/log/app.log"
	if _, ok, err := st.GetCacheEntry(key); err != nil || ok {
		t.Fatalf("未写入时应返回 ok=false：ok=%v err=%v", ok, err)
	}

	rec := CacheEntry{
		SourceKey: key, ConnID: 7, Remote: "deploy@10.0.0.5:22",
		Path: "/var/log/app.log", Name: "app.log",
		Size: 1234, ModTime: 1700000000, FileName: "abc123.bin",
	}
	if err := st.SaveCacheEntry(rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.GetCacheEntry(key)
	if err != nil || !ok {
		t.Fatalf("GetCacheEntry: ok=%v err=%v", ok, err)
	}
	if got.Size != 1234 || got.FileName != "abc123.bin" || got.Remote != "deploy@10.0.0.5:22" {
		t.Fatalf("字段未落库：%+v", got)
	}
	if got.LastUsedAt.IsZero() {
		t.Fatal("LastUsedAt 未设置")
	}

	// 指纹变化后覆盖写（同名来源只应有一条记录）
	rec.Size = 5678
	rec.FileName = "abc123.bin"
	if err := st.SaveCacheEntry(rec); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListCacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Size != 5678 {
		t.Fatalf("覆盖写失败：%+v", all)
	}

	// 最近使用时间刷新后仍只有一条
	if err := st.TouchCacheEntry(key); err != nil {
		t.Fatal(err)
	}
	all, _ = st.ListCacheEntries()
	if len(all) != 1 {
		t.Fatalf("Touch 不应产生新记录：%+v", all)
	}

	// 删除返回被删记录（调用方据此删缓存文件）
	del, ok, err := st.DeleteCacheEntry(key)
	if err != nil || !ok || del.FileName != "abc123.bin" {
		t.Fatalf("DeleteCacheEntry: ok=%v err=%v rec=%+v", ok, err, del)
	}
	if _, ok, _ := st.GetCacheEntry(key); ok {
		t.Fatal("删除后仍能取到")
	}
}

// LRU 排序：最近使用的排最前（逐出按逆序淘汰）。
func TestCacheEntryLRUOrder(t *testing.T) {
	st := newTestStore(t)
	mk := func(key string) CacheEntry {
		return CacheEntry{SourceKey: key, Remote: "u@h:22", Path: "/" + key, Name: key, Size: 1, FileName: key + ".bin"}
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := st.SaveCacheEntry(mk(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.TouchCacheEntry("a"); err != nil { // a 变成最近使用
		t.Fatal(err)
	}
	all, err := st.ListCacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].SourceKey != "a" || all[2].SourceKey != "b" {
		t.Fatalf("LRU 排序异常：%+v", all)
	}
}

// 按主机清理：同一 user@host:port 的多条记录（可能来自两条连接记录）应一起清掉。
func TestDeleteCacheEntriesByRemote(t *testing.T) {
	st := newTestStore(t)
	base := func(remote, path string) CacheEntry {
		return CacheEntry{
			SourceKey: "sftp://" + remote + path, Remote: remote,
			Path: path, Name: "app.log", Size: 1, FileName: "x.bin",
		}
	}
	for _, e := range []CacheEntry{
		base("deploy@h1:22", "/a.log"),
		base("deploy@h1:22", "/b.log"),
		base("deploy@h2:22", "/a.log"),
	} {
		if err := st.SaveCacheEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	del, err := st.DeleteCacheEntriesByRemote("deploy@h1:22")
	if err != nil {
		t.Fatal(err)
	}
	if len(del) != 2 {
		t.Fatalf("应删掉 h1 的 2 条，实际 %d：%+v", len(del), del)
	}
	rest, err := st.ListCacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Remote != "deploy@h2:22" {
		t.Fatalf("误删了别的主机：%+v", rest)
	}

	if _, err := st.DeleteCacheEntriesByRemote(""); err != nil {
		t.Fatalf("空 remote 应是无操作而非报错：%v", err)
	}
}

func TestClearCacheEntries(t *testing.T) {
	st := newTestStore(t)
	if err := st.SaveCacheEntry(CacheEntry{SourceKey: "k1", Remote: "u@h:22", FileName: "1.bin"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCacheEntry(CacheEntry{SourceKey: "k2", Remote: "u@h:22", FileName: "2.bin"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := st.ClearCacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 2 {
		t.Fatalf("应返回被清掉的 2 条（调用方要删对应文件），实际 %d", len(cleared))
	}
	if all, _ := st.ListCacheEntries(); len(all) != 0 {
		t.Fatalf("清空后仍有记录：%+v", all)
	}
}

func TestSettings(t *testing.T) {
	st := newTestStore(t)
	if _, ok, err := st.GetSetting(SettingCacheEnabled); err != nil || ok {
		t.Fatalf("未设置时应 ok=false：ok=%v err=%v", ok, err)
	}
	if err := st.SetSetting(SettingCacheEnabled, "1"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := st.GetSetting(SettingCacheEnabled)
	if err != nil || !ok || v != "1" {
		t.Fatalf("GetSetting = %q ok=%v err=%v", v, ok, err)
	}
	// 覆盖写
	if err := st.SetSetting(SettingCacheEnabled, "0"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetSetting(SettingCacheEnabled); v != "0" {
		t.Fatalf("覆盖写失败：%q", v)
	}
	if err := st.SetSetting(SettingCacheLimit, "2147483648"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := st.GetSetting(SettingCacheLimit); v != "2147483648" {
		t.Fatalf("上限未保存：%q", v)
	}
}
