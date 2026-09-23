package filecache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"logviewer/internal/logfile"
	"logviewer/internal/store"
)

// fakeRemote 假的远端来源：记录被调用的次数，用于断言「命中缓存时完全不碰远端」。
type fakeRemote struct {
	data    []byte
	modTime time.Time
	pos     int64

	reads   int64
	readAts int64
	opens   int64
	closed  bool
}

func (f *fakeRemote) Read(p []byte) (int, error) {
	atomic.AddInt64(&f.reads, 1)
	if f.pos >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.pos:])
	f.pos += int64(n)
	return n, nil
}

func (f *fakeRemote) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt64(&f.readAts, 1)
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeRemote) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.pos = off
	case io.SeekCurrent:
		f.pos += off
	case io.SeekEnd:
		f.pos = int64(len(f.data)) + off
	}
	return f.pos, nil
}

func (f *fakeRemote) Stat() (os.FileInfo, error) {
	return fakeInfo{size: int64(len(f.data)), mod: f.modTime}, nil
}

func (f *fakeRemote) Close() error { f.closed = true; return nil }

type fakeInfo struct {
	size int64
	mod  time.Time
}

func (i fakeInfo) Name() string       { return "app.log" }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() os.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return i.mod }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

var _ logfile.Source = (*fakeRemote)(nil)

// harness 一套「缓存管理器 + 假远端」。
type harness struct {
	t     *testing.T
	mgr   *Manager
	store *store.Store
	dir   string
	key   string
	// remote 当前生效的假远端（NewRemote 可替换，用于模拟内容变化）
	remote *fakeRemote
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	st, err := store.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr, err := NewManager(cacheDir, st)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, mgr: mgr, store: st, dir: cacheDir, key: "sftp://deploy@h:22/var/log/app.log"}
	h.remote = h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0))
	return h
}

// NewRemote 造一个新的假远端（调用即更换内容，用于模拟远端文件被改写）。
func (h *harness) NewRemote(content string, mod time.Time) *fakeRemote {
	h.remote = &fakeRemote{data: []byte(content), modTime: mod}
	return h.remote
}

// partFiles 当前缓存目录下的 .part 文件（写入者独占命名，无法按固定名断言）。
func (h *harness) partFiles() []string {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			out = append(out, e.Name())
		}
	}
	return out
}

func (h *harness) req() Request {
	rm := h.remote
	return Request{
		SourceKey: h.key, ConnID: 3, Remote: "deploy@h:22", Path: "/var/log/app.log", Name: "app.log",
		Info: func() os.FileInfo { fi, _ := rm.Stat(); return fi }(),
		OpenRemote: func() (logfile.Source, error) {
			atomic.AddInt64(&rm.opens, 1)
			return &offsetRemote{inner: rm}, nil
		},
	}
}

// offsetRemote 包装假远端，让每次打开都有独立的读指针（真实远端来源正是如此）。
type offsetRemote struct {
	inner *fakeRemote
	pos   int64
}

func (o *offsetRemote) Read(p []byte) (int, error) {
	atomic.AddInt64(&o.inner.reads, 1)
	if o.pos >= int64(len(o.inner.data)) {
		return 0, io.EOF
	}
	n := copy(p, o.inner.data[o.pos:])
	o.pos += int64(n)
	return n, nil
}

func (o *offsetRemote) ReadAt(p []byte, off int64) (int, error) {
	atomic.AddInt64(&o.inner.readAts, 1)
	if off >= int64(len(o.inner.data)) {
		return 0, io.EOF
	}
	n := copy(p, o.inner.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (o *offsetRemote) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		o.pos = off
	case io.SeekCurrent:
		o.pos += off
	case io.SeekEnd:
		o.pos = int64(len(o.inner.data)) + off
	}
	return o.pos, nil
}

func (o *offsetRemote) Stat() (os.FileInfo, error) { return o.inner.Stat() }
func (o *offsetRemote) Close() error               { return nil }

// drain 顺序读完整个来源。
func drain(t *testing.T, src logfile.Source) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			if err == io.EOF {
				return sb.String()
			}
			t.Fatalf("读取失败：%v", err)
		}
	}
}

// 首次打开：读完提交缓存（.bin + 记录），并删除 .part。
func TestMissThenCommit(t *testing.T) {
	h := newHarness(t)
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	got := drain(t, src)
	if got != "hello\nworld\n" {
		t.Fatalf("内容 = %q", got)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	bin := h.mgr.binPath(h.key)
	if fi, err := os.Stat(bin); err != nil || fi.Size() != int64(len(got)) {
		t.Fatalf("缓存文件应已提交且长度一致：%v size=%v", err, fi)
	}
	if left := h.partFiles(); len(left) != 0 {
		t.Fatalf(".part 应已改名，不应残留：%v", left)
	}
	ent, ok, err := h.store.GetCacheEntry(h.key)
	if err != nil || !ok {
		t.Fatalf("应写入缓存记录：ok=%v err=%v", ok, err)
	}
	if ent.Size != int64(len(got)) || ent.ModTime != 1700000000 || ent.Remote != "deploy@h:22" {
		t.Fatalf("记录字段不对：%+v", ent)
	}
}

// 命中缓存：第二次打开完全不碰远端（不读、不打开）。
func TestHitServesLocallyWithoutTouchingRemote(t *testing.T) {
	h := newHarness(t)
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	rm := h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0)) // 内容相同（指纹一致）
	before := atomic.LoadInt64(&rm.reads)
	beforeAt := atomic.LoadInt64(&rm.readAts)

	src2, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	defer src2.Close()
	if got := drain(t, src2); got != "hello\nworld\n" {
		t.Fatalf("命中时内容 = %q", got)
	}
	if got := atomic.LoadInt64(&rm.reads) - before; got != 0 {
		t.Fatalf("命中缓存却发起了 %d 次顺序读", got)
	}
	if got := atomic.LoadInt64(&rm.readAts) - beforeAt; got != 0 {
		t.Fatalf("命中缓存却发起了 %d 次随机读", got)
	}
	if got := atomic.LoadInt64(&rm.opens); got != 0 {
		t.Fatalf("命中缓存却打开了 %d 次远端来源（应惰性打开）", got)
	}
	_ = before
}

// 随机读落在已缓存区间时也走本地（翻页不重复走网络）。
func TestReadAtUsesCache(t *testing.T) {
	h := newHarness(t)
	content := strings.Repeat("abcdefghij", 100) // 1000 字节
	h.NewRemote(content, time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	rm := h.NewRemote(content, time.Unix(1700000000, 0))
	src2, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	defer src2.Close()
	buf := make([]byte, 32)
	if _, err := src2.ReadAt(buf, 500); err != nil {
		t.Fatal(err)
	}
	if string(buf) != content[500:532] {
		t.Fatalf("ReadAt 内容 = %q", buf)
	}
	if got := atomic.LoadInt64(&rm.readAts); got != 0 {
		t.Fatalf("命中缓存的随机读不应走远端，实际 %d 次", got)
	}
}

// 指纹变化 → 当作未命中重下，并替换旧缓存。
func TestFingerprintChangeRefetches(t *testing.T) {
	h := newHarness(t)
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	// 追加内容（大小与修改时间都变）
	rm := h.NewRemote("hello\nworld\nmore\n", time.Unix(1700000100, 0))
	src2, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, src2); got != "hello\nworld\nmore\n" {
		t.Fatalf("指纹变化后内容 = %q（用了陈旧缓存？）", got)
	}
	src2.Close()
	if atomic.LoadInt64(&rm.reads) == 0 {
		t.Fatal("指纹变化应重新拉取")
	}
	ent, _, _ := h.store.GetCacheEntry(h.key)
	if ent.Size != int64(len("hello\nworld\nmore\n")) || ent.ModTime != 1700000100 {
		t.Fatalf("缓存记录未更新：%+v", ent)
	}
	// 再开一次应命中新缓存
	rm2 := h.NewRemote("hello\nworld\nmore\n", time.Unix(1700000100, 0))
	src3, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	defer src3.Close()
	if got := drain(t, src3); got != "hello\nworld\nmore\n" {
		t.Fatalf("内容 = %q", got)
	}
	if atomic.LoadInt64(&rm2.reads) != 0 {
		t.Fatal("应命中新写入的缓存")
	}
}

// Bypass（用户点「重新加载」）：即使指纹一致也重新拉取。
func TestBypassIgnoresCache(t *testing.T) {
	h := newHarness(t)
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	rm := h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0))
	req := h.req()
	req.Bypass = true
	src2, err := h.mgr.Open(req)
	if err != nil {
		t.Fatal(err)
	}
	defer src2.Close()
	if got := drain(t, src2); got != "hello\nworld\n" {
		t.Fatalf("内容 = %q", got)
	}
	if atomic.LoadInt64(&rm.reads) == 0 {
		t.Fatal("Bypass 应绕过缓存重新拉取")
	}
}

// 中断（未读完就关闭）：丢弃 .part，不留半个缓存，也不写记录。
func TestAbortKeepsNothing(t *testing.T) {
	h := newHarness(t)
	h.NewRemote(strings.Repeat("xxxxxxxxxx\n", 1000), time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := src.Read(buf); err != nil {
		t.Fatal(err)
	}
	src.Close() // 没读到 EOF

	if left := h.partFiles(); len(left) != 0 {
		t.Fatalf(".part 应被丢弃，实际残留：%v", left)
	}
	if _, err := os.Stat(h.mgr.binPath(h.key)); !os.IsNotExist(err) {
		t.Fatal("未读完不应提交 .bin")
	}
	if _, ok, _ := h.store.GetCacheEntry(h.key); ok {
		t.Fatal("未读完不应写缓存记录")
	}
}

// 缓存文件被外部删除/截断：长度校验发现不一致 → 当作未命中重下，而不是返回错误内容。
func TestCacheFileTampered(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(path string)
	}{
		{"删除", func(p string) { os.Remove(p) }},
		{"截断", func(p string) { os.WriteFile(p, []byte("short"), 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			src, err := h.mgr.Open(h.req())
			if err != nil {
				t.Fatal(err)
			}
			drain(t, src)
			src.Close()
			tc.damage(h.mgr.binPath(h.key))

			rm := h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0))
			src2, err := h.mgr.Open(h.req())
			if err != nil {
				t.Fatal(err)
			}
			defer src2.Close()
			if got := drain(t, src2); got != "hello\nworld\n" {
				t.Fatalf("内容 = %q", got)
			}
			if atomic.LoadInt64(&rm.reads) == 0 {
				t.Fatal("缓存不可用时应回退到远端重新读取")
			}
		})
	}
}

// 单条超过上限的文件不缓存（不写盘、不留记录），但读取必须正常。
func TestOversizedFileNotCached(t *testing.T) {
	h := newHarness(t)
	h.mgr.SetLimit(8) // 比内容还小的上限
	h.NewRemote(strings.Repeat("z", 100), time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, src); len(got) != 100 {
		t.Fatalf("内容长度 = %d", len(got))
	}
	src.Close()
	if _, err := os.Stat(h.mgr.binPath(h.key)); !os.IsNotExist(err) {
		t.Fatal("超上限的文件不应写缓存")
	}
	if _, ok, _ := h.store.GetCacheEntry(h.key); ok {
		t.Fatal("超上限的文件不应留记录")
	}
}

// 逐出：超上限时按 LRU 淘汰最旧的，使用中的不淘汰。
func TestEvictionByLRU(t *testing.T) {
	h := newHarness(t)
	content := strings.Repeat("a", 100)
	h.mgr.SetLimit(250) // 只装得下 2 条

	write := func(key, content string, mod int64) {
		h.key = key
		h.NewRemote(content, time.Unix(mod, 0))
		src, err := h.mgr.Open(h.req())
		if err != nil {
			t.Fatal(err)
		}
		drain(t, src)
		src.Close()
	}
	write("k1", content, 1700000000)
	write("k2", content, 1700000001)
	time.Sleep(5 * time.Millisecond)
	write("k3", content, 1700000002) // 触发逐出：k1 应被淘汰

	recs, err := h.store.ListCacheEntries()
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(recs))
	for _, r := range recs {
		keys = append(keys, r.SourceKey)
	}
	joined := strings.Join(keys, ",")
	if strings.Contains(joined, "k1") {
		t.Fatalf("最旧的条目应被逐出，实际：%v", joined)
	}
	if !strings.Contains(joined, "k3") {
		t.Fatalf("最新写入的条目不应被逐出：%v", joined)
	}
	// 被逐出的文件也要删掉
	if _, err := os.Stat(h.mgr.binPath("k1")); !os.IsNotExist(err) {
		t.Fatal("逐出后缓存文件应删除")
	}
}

// 使用中的条目不参与逐出。
func TestEvictionSkipsInUse(t *testing.T) {
	h := newHarness(t)
	content := strings.Repeat("b", 100)

	h.key = "inuse"
	h.NewRemote(content, time.Unix(1700000000, 0))
	held, err := h.mgr.Open(h.req()) // 保持打开 = 使用中
	if err != nil {
		t.Fatal(err)
	}
	drain(t, held)
	defer held.Close()

	h.mgr.SetLimit(150)
	h.key = "other"
	h.NewRemote(content, time.Unix(1700000001, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	if _, ok, _ := h.store.GetCacheEntry("inuse"); !ok {
		t.Fatal("使用中的条目不应被逐出")
	}
}

// 清理残留的 .part 与孤儿 .bin。
func TestCleanupStale(t *testing.T) {
	h := newHarness(t)
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	orphan := filepath.Join(h.dir, "deadbeef.bin")
	stale := filepath.Join(h.dir, "cafebabe.part")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.mgr.CleanupStale()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("孤儿 .bin 应被清理")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("残留 .part 应被清理")
	}
	// 有记录的正常缓存不能被误删
	if _, err := os.Stat(h.mgr.binPath(h.key)); err != nil {
		t.Fatalf("正常缓存被误删：%v", err)
	}
	if _, ok, _ := h.store.GetCacheEntry(h.key); !ok {
		t.Fatal("正常记录被误删")
	}
}

// 清空与按主机清理。
func TestClearAndRemoveByRemote(t *testing.T) {
	h := newHarness(t)
	mk := func(key, remote string) {
		h.key = key
		h.NewRemote("x\n", time.Unix(1700000000, 0))
		src, err := h.mgr.Open(h.req())
		if err != nil {
			t.Fatal(err)
		}
		drain(t, src)
		src.Close()
		// Remote 字段由 req() 固定，这里手动改记录以模拟多主机
		ent, _, _ := h.store.GetCacheEntry(key)
		ent.Remote = remote
		if err := h.store.SaveCacheEntry(ent); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "deploy@h1:22")
	mk("b", "deploy@h2:22")

	h.mgr.RemoveByRemote("deploy@h1:22")
	if _, ok, _ := h.store.GetCacheEntry("a"); ok {
		t.Fatal("h1 的缓存应被清理")
	}
	if _, ok, _ := h.store.GetCacheEntry("b"); !ok {
		t.Fatal("h2 的缓存不应受影响")
	}

	if err := h.mgr.Clear(); err != nil {
		t.Fatal(err)
	}
	if recs, _ := h.store.ListCacheEntries(); len(recs) != 0 {
		t.Fatalf("清空后仍有记录：%+v", recs)
	}
	if _, err := os.Stat(h.mgr.binPath("b")); !os.IsNotExist(err) {
		t.Fatal("清空后缓存文件应删除")
	}
}

// 缓存文件创建失败（目录被占）时降级为纯远端读，读取结果不受影响。
func TestCacheWriteFailureDegradesGracefully(t *testing.T) {
	h := newHarness(t)
	// 让缓存文件建不出来（目录不存在），验证降级路径
	h.mgr.dir = filepath.Join(h.dir, "does-not-exist")
	h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatalf("缓存不可用不应导致打开失败：%v", err)
	}
	defer src.Close()
	if got := drain(t, src); got != "hello\nworld\n" {
		t.Fatalf("降级后内容 = %q", got)
	}
	buf := make([]byte, 5)
	if _, err := src.ReadAt(buf, 6); err != nil {
		t.Fatalf("降级后随机读失败：%v", err)
	}
	if string(buf) != "world" {
		t.Fatalf("降级后随机读内容 = %q", buf)
	}
}

// 通过 logfile 端到端：建索引 → 翻页 → 检索，第二次打开全程本地。
func TestThroughLogfile(t *testing.T) {
	h := newHarness(t)
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	content := sb.String()
	h.NewRemote(content, time.Unix(1700000000, 0))

	open := func() *logfile.FileSession {
		src, err := h.mgr.Open(h.req())
		if err != nil {
			t.Fatal(err)
		}
		sess, err := logfile.OpenSource(src, "/var/log/app.log", "app.log", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.WaitReady(); err != nil {
			t.Fatal(err)
		}
		return sess
	}

	first := open()
	if first.TotalLines() != 500 {
		t.Fatalf("TotalLines = %d", first.TotalLines())
	}
	lines, err := first.ReadLines(10, 3)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "line 10|line 11|line 12" {
		t.Fatalf("内容 = %v", lines)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二次：指纹一致，应全程本地（远端零读取）
	rm := h.NewRemote(content, time.Unix(1700000000, 0))
	second := open()
	defer second.Close()
	if second.TotalLines() != 500 {
		t.Fatalf("第二次 TotalLines = %d", second.TotalLines())
	}
	if lines, err := second.ReadLines(499, 1); err != nil || lines[0] != "line 499" {
		t.Fatalf("末行 = %v (err=%v)", lines, err)
	}
	if got := atomic.LoadInt64(&rm.reads) + atomic.LoadInt64(&rm.readAts); got != 0 {
		t.Fatalf("命中缓存时远端读取次数应为 0，实际 %d", got)
	}
	if got := atomic.LoadInt64(&rm.opens); got != 0 {
		t.Fatalf("命中缓存不应打开远端来源，实际 %d 次", got)
	}
}

// 「重新加载」必须真正换掉缓存内容：否则刷新一次、下次打开又弹回旧内容。
// 这是 size+mtime 都没变、只有内容变了的场景（用户点重新加载的典型理由）。
func TestBypassRewritesCacheForNextOpen(t *testing.T) {
	h := newHarness(t)
	h.NewRemote("OLD-CONTENT\n", time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	// 同长度、同 mtime 的原地改写：指纹判定不出来，只能靠 Bypass
	h.NewRemote("NEW-CONTENT\n", time.Unix(1700000000, 0))
	req := h.req()
	req.Bypass = true
	src2, err := h.mgr.Open(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, src2); got != "NEW-CONTENT\n" {
		t.Fatalf("Bypass 读到 = %q", got)
	}
	src2.Close()

	// 关键：普通打开（不 Bypass）也必须看到新内容——缓存已被替换，而不是留着旧的
	src3, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	defer src3.Close()
	if got := drain(t, src3); got != "NEW-CONTENT\n" {
		t.Fatalf("重新加载后再次打开 = %q，说明缓存里的旧内容又回来了", got)
	}
	ent, ok, _ := h.store.GetCacheEntry(h.key)
	if !ok || ent.Size != int64(len("NEW-CONTENT\n")) {
		t.Fatalf("缓存记录未更新：ok=%v %+v", ok, ent)
	}
}

// 命中缓存后走 Scan（Seek(0) + 顺序读）：游标必须正确，不能读到错位内容。
func TestScanOverCachedSource(t *testing.T) {
	h := newHarness(t)
	var sb strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&sb, "row-%03d\n", i)
	}
	content := sb.String()
	h.NewRemote(content, time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	// 命中路径：先整体读一遍，再 Seek(0) 重读（Scan 正是这个模式）
	rm := h.NewRemote(content, time.Unix(1700000000, 0))
	hit, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	defer hit.Close()
	if got := drain(t, hit); got != content {
		t.Fatalf("首次顺序读长度 = %d，want %d", len(got), len(content))
	}
	if _, err := hit.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var lines []string
	buf := make([]byte, 0)
	one := make([]byte, 1)
	cur := ""
	for {
		n, err := hit.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if one[0] == '\n' {
				lines = append(lines, cur)
				cur = ""
			} else {
				cur += string(one[0])
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(lines) != 300 || lines[0] != "row-000" || lines[299] != "row-299" {
		t.Fatalf("Seek(0) 后重读异常：%d 行，首 %q 末 %q", len(lines), lines[0], lines[len(lines)-1])
	}
	if got := atomic.LoadInt64(&rm.reads) + atomic.LoadInt64(&rm.readAts); got != 0 {
		t.Fatalf("命中缓存的 Scan 不应碰远端，实际 %d 次", got)
	}
}

// 在途写入期间清空缓存：不能在提交时把条目"复活"。
func TestClearDuringWriteDoesNotResurrect(t *testing.T) {
	h := newHarness(t)
	h.NewRemote(strings.Repeat("data\n", 100), time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := src.Read(buf); err != nil { // 读一点，处于「写入中」
		t.Fatal(err)
	}
	if err := h.mgr.Clear(); err != nil {
		t.Fatal(err)
	}
	// 继续读完并关闭：代际已变，不应发布
	drain(t, src)
	src.Close()

	if _, ok, _ := h.store.GetCacheEntry(h.key); ok {
		t.Fatal("清空之后在途写入不应把条目写回来")
	}
	if _, err := os.Stat(h.mgr.binPath(h.key)); !os.IsNotExist(err) {
		t.Fatal("清空之后不应留下缓存文件")
	}
	if left := h.partFiles(); len(left) != 0 {
		t.Fatalf(".part 应被丢弃：%v", left)
	}
}

// 同一来源的第二个写入者：不共享写句柄，退化为直连读；最终只应有一个 .bin。
func TestSecondWriterDegrades(t *testing.T) {
	h := newHarness(t)
	h.NewRemote("hello\nworld\n", time.Unix(1700000000, 0))

	first, err := h.mgr.Open(h.req()) // 认领写入权
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.mgr.Open(h.req()) // 拿不到写入权
	if err != nil {
		t.Fatalf("第二个打开不应失败：%v", err)
	}
	// 第二个仍能正确读取（直连远端），但不会写缓存
	if got := drain(t, second); got != "hello\nworld\n" {
		t.Fatalf("第二个会话内容 = %q", got)
	}
	second.Close()
	if _, err := os.Stat(h.mgr.binPath(h.key)); !os.IsNotExist(err) {
		t.Fatal("第二个会话不应产出缓存文件")
	}

	drain(t, first)
	first.Close()
	if _, err := os.Stat(h.mgr.binPath(h.key)); err != nil {
		t.Fatalf("第一个会话应正常提交：%v", err)
	}
}

// 修改时间不可用（0）的文件不缓存：指纹只剩大小可比，同长度轮转会误判命中。
func TestUnusableModTimeNotCached(t *testing.T) {
	h := newHarness(t)
	h.NewRemote("hello\nworld\n", time.Unix(0, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, src); got != "hello\nworld\n" {
		t.Fatalf("内容 = %q", got)
	}
	src.Close()
	if _, err := os.Stat(h.mgr.binPath(h.key)); !os.IsNotExist(err) {
		t.Fatal("mtime 为 0 的文件不应缓存")
	}
	if left := h.partFiles(); len(left) != 0 {
		t.Fatalf("不应留下 .part：%v", left)
	}
}

// 命中判定只看「大小 + 修改时间」：只改 mtime（同长度原地改写）也必须重下。
func TestModTimeChangeAloneRefetches(t *testing.T) {
	h := newHarness(t)
	h.NewRemote("AAAA\n", time.Unix(1700000000, 0))
	src, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, src)
	src.Close()

	rm := h.NewRemote("BBBB\n", time.Unix(1700000001, 0)) // 同长度、mtime 变
	src2, err := h.mgr.Open(h.req())
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, src2); got != "BBBB\n" {
		t.Fatalf("内容 = %q（用了陈旧缓存？）", got)
	}
	src2.Close()
	if atomic.LoadInt64(&rm.reads) == 0 {
		t.Fatal("mtime 变化应触发重下")
	}
}

// 并发打开同一来源：不 panic、内容正确、只产出一个 .bin。
func TestConcurrentOpens(t *testing.T) {
	h := newHarness(t)
	h.NewRemote(strings.Repeat("payload\n", 200), time.Unix(1700000000, 0))

	const n = 8
	done := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			src, err := h.mgr.Open(h.req())
			if err != nil {
				done <- "ERR:" + err.Error()
				return
			}
			got := drain(t, src)
			src.Close()
			done <- got
		}()
	}
	want := strings.Repeat("payload\n", 200)
	for i := 0; i < n; i++ {
		if got := <-done; got != want {
			t.Fatalf("并发打开拿到错误内容（长度 %d，want %d）", len(got), len(want))
		}
	}
	if left := h.partFiles(); len(left) != 0 {
		t.Fatalf("并发打开后不应残留 .part：%v", left)
	}
	bin := h.mgr.binPath(h.key)
	fi, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("应产出一个 .bin：%v", err)
	}
	if fi.Size() != int64(len(want)) {
		t.Fatalf(".bin 长度 = %d, want %d", fi.Size(), len(want))
	}
}
