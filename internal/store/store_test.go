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
	if err := st.Touch(`C:\logs\app.log`, 100, 1000); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := st.Touch(`C:\logs\app.log`, 200, 2000); err != nil {
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
	if err := st.Touch(filepath.Join("D:", "a", "app.log"), 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(filepath.Join("D:", "b", "app.log"), 1, 1); err != nil {
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
	if err := st.Touch(`C:\Logs\App.log`, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Touch(`c:\logs\app.log`, 1, 1); err != nil {
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
	if err := st.Touch(`C:\logs\app.log`, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTotalLines(`C:\logs\app.log`, 1234); err != nil {
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
	if err := st.Touch(`C:\logs\b.log`, 1, 1); err != nil {
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
	_ = st.Touch(`C:\logs\old.log`, 1, 1)
	_ = st.Touch(`C:\logs\new.log`, 1, 1)
	_ = st.Touch(`C:\logs\old.log`, 1, 1) // 重新打开旧文件 → 应排到最前
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "old.log" {
		t.Fatalf("排序不符合最近打开优先：%+v", got)
	}
}
