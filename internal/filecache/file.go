package filecache

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"logviewer/internal/logfile"
	"logviewer/internal/store"
)

// File 复合字节来源：本地缓存 + 远端来源，实现 logfile.Source。
//
// 命中缓存（hit）时整场只读本地；未命中（miss）时顺序读从远端取数并**边读边写** .part，
// 读满打开时刻的快照大小后提交为 .bin。随机读（翻页/导出）落在已写区间时直接读本地，
// 因此「索引一遍之后翻页」在远端未命中缓存时也不会重复走网络。
//
// 远端来源是惰性打开的：命中缓存时全程只有判定指纹那一次 stat 往返。
type File struct {
	mgr  *Manager
	req  Request
	info os.FileInfo
	size int64

	partPath string // 本写入者独占的 .part 路径（命中时为 ""）
	epoch    int64  // 打开时的缓存代际（提交前比对，见 Manager.epoch）
	// writerHeld 本 File 是否持有该来源的写入权。只能按它释放：
	// 若不加区分地释放，会让第三个写入者趁虚而入。
	writerHeld bool

	mu      sync.Mutex
	local   *os.File // hit: 只读的 .bin；miss: 可读写的 .part
	written int64    // local 中已就绪的字节数（顺序读的进度）
	pos     int64    // 顺序读位置
	// remotePos 远端来源自己的顺序读位置。两处可能不同步：从本地缓存前缀读完
	// 转向远端时，那些字节并没有消耗远端流，必须先把它定位到 pos 再读，
	// 否则会把开头的内容再读一遍。remotePos < 0 表示位置未知（Seek 后），下次读时重新定位。
	remotePos int64
	done      bool // 已提交或已放弃缓存写入（此后不再写入缓存）
	closed    bool

	openMu sync.Mutex    // 串行化远端来源的惰性打开
	remote logfile.Source
}

func newFile(m *Manager, req Request, fi os.FileInfo, local *os.File, partPath string, hit bool, written int64) *File {
	f := &File{
		mgr: m, req: req, info: fi, size: fi.Size(), local: local,
		partPath: partPath, written: written, epoch: m.currentEpoch(),
	}
	if hit {
		f.done = true // 命中即已完备，不再有提交动作
	}
	return f
}

// 编译期断言：复合来源满足 logfile.Source。
var _ logfile.Source = (*File)(nil)

// Stat 返回**远端**文件信息：调用方的「打开时刻快照」语义（大小、修改时间）
// 与是否启用缓存无关，保持一致。
func (f *File) Stat() (os.FileInfo, error) { return f.info, nil }

// ReadAt 随机读：整段落在已就绪区间内则读本地，否则问远端。
func (f *File) ReadAt(b []byte, off int64) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, os.ErrClosed
	}
	if off >= 0 && off+int64(len(b)) <= f.written && f.local != nil {
		local := f.local
		f.mu.Unlock()
		// 用 ReadAt（pread）而非 Read：不碰文件偏移，可与写入并发
		return local.ReadAt(b, off)
	}
	f.mu.Unlock()

	r, err := f.remoteSource()
	if err != nil {
		return 0, err
	}
	return r.ReadAt(b, off)
}

// Read 顺序读：先消费已就绪的本地前缀，越界后从远端取并写入缓存。
func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, os.ErrClosed
	}
	// 已就绪区间覆盖到快照末尾：再往后就是文件末尾，不必向远端探一次 EOF
	// （命中缓存的会话因此完全不产生网络请求；提交之后同理）。
	// 注意仍要触发提交：EOF 是「顺序读结束」的信号，缓存正是靠它落定。
	if f.pos >= f.size && f.written >= f.size {
		f.mu.Unlock()
		f.commit()
		return 0, io.EOF
	}
	if f.pos < f.written && f.local != nil {
		local := f.local
		off := f.pos
		avail := f.written - off
		f.mu.Unlock()
		// 只读「已就绪」的那一段：越过 written 的部分 .part 里还没有内容，
		// 直接读会拿到 EOF，而调用方会当成整个流结束——远端其实还有数据。
		b := p
		if int64(len(b)) > avail {
			b = b[:avail]
		}
		n, err := local.ReadAt(b, off)
		f.mu.Lock()
		f.pos += int64(n)
		f.mu.Unlock()
		return n, err
	}
	f.mu.Unlock()

	r, err := f.remoteSource()
	if err != nil {
		return 0, err
	}
	// 远端流的位置可能与 f.pos 不一致：先对齐再读（对 SFTP 是纯客户端操作，无往返）
	f.mu.Lock()
	want := f.pos
	needSeek := f.remotePos != want
	f.mu.Unlock()
	if needSeek {
		if _, err := r.Seek(want, io.SeekStart); err != nil {
			return 0, err
		}
		f.mu.Lock()
		f.remotePos = want
		f.mu.Unlock()
	}

	n, err := r.Read(p)
	if n > 0 {
		f.tee(p[:n])
		f.mu.Lock()
		f.remotePos += int64(n)
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.pos += int64(n)
	f.mu.Unlock()
	if err == io.EOF {
		f.commit()
	}
	return n, err
}

// Seek 定位顺序读位置；下一次 Read 从该处继续（本地已就绪则读本地）。
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, errors.New("无效的 whence")
	}
	if abs < 0 {
		return 0, errors.New("定位到负偏移")
	}
	f.pos = abs
	f.remotePos = -1 // 位置未知：下次从远端读之前会重新定位
	return abs, nil
}

func (f *File) remoteSource() (logfile.Source, error) {
	f.openMu.Lock()
	defer f.openMu.Unlock()
	f.mu.Lock()
	r, closed := f.remote, f.closed
	f.mu.Unlock()
	if r != nil {
		return r, nil
	}
	if closed {
		return nil, os.ErrClosed
	}
	nr, err := f.req.OpenRemote() // 慢操作：不持 f.mu
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.remote = nr
	f.mu.Unlock()
	return nr, nil
}

// tee 把刚从远端读到的字节追加写入缓存。
// 缓存写入失败绝不影响读取：放弃缓存（关掉并删掉 .part）后本次会话退化为纯远端读。
func (f *File) tee(b []byte) {
	f.mu.Lock()
	local, off := f.local, f.written
	f.mu.Unlock()
	if local == nil {
		return
	}
	n, err := local.WriteAt(b, off)
	if err != nil {
		f.abortCache(err)
		return
	}
	f.mu.Lock()
	f.written += int64(n)
	short := n < len(b)
	f.mu.Unlock()
	if short {
		f.abortCache(io.ErrShortWrite)
	}
}

// commit 顺序读到达 EOF：读满快照大小就提交缓存，否则丢弃。
func (f *File) commit() {
	f.mu.Lock()
	if f.done || f.local == nil {
		f.mu.Unlock()
		return
	}
	f.done = true
	local, written, size := f.local, f.written, f.size
	f.mu.Unlock()

	if written < size {
		// 没能读满打开时刻的快照（文件被截断、读取中断）：这份缓存不可信，丢弃
		f.abortCache(fmt.Errorf("只读到 %d 字节，不足快照大小 %d", written, size))
		return
	}

	// 快照之外的字节（读取过程中远端被追加）不属于本次快照，截掉再提交
	// —— 缓存只承诺覆盖 [0, size)，与索引的「打开时刻快照」语义严格对齐。
	if err := local.Truncate(size); err != nil {
		f.abortCache(err)
		return
	}
	if err := local.Sync(); err != nil {
		f.abortCache(err)
		return
	}

	// 期间用户可能已清空缓存或删除主机：这份不再发布，否则条目会「复活」
	if f.epoch != f.mgr.currentEpoch() {
		f.abortCache(errors.New("缓存已被清空或删除，放弃本次写入"))
		return
	}

	binPath, partPath := f.mgr.binPath(f.req.SourceKey), f.partPath
	// 换名之前先把 local 摘掉：期间的读取会回落到远端，不会用到即将失效的句柄
	f.mu.Lock()
	f.local = nil
	f.mu.Unlock()
	local.Close()
	_ = os.Remove(binPath) // 覆盖旧缓存（正常情况下前面的逻辑已删过记录）
	if err := os.Rename(partPath, binPath); err != nil {
		log.Printf("提交缓存失败（%s）：%v", f.req.Path, err)
		_ = os.Remove(partPath)
		return
	}
	bin, err := os.Open(binPath)
	if err != nil {
		log.Printf("缓存提交后打开失败（%s）：%v", f.req.Path, err)
		_ = os.Remove(binPath)
		return
	}
	f.mu.Lock()
	f.local = bin
	f.written = size
	f.mu.Unlock()

	if err := f.mgr.store.SaveCacheEntry(store.CacheEntry{
		SourceKey: f.req.SourceKey,
		ConnID:    f.req.ConnID,
		Remote:    f.req.Remote,
		Path:      f.req.Path,
		Name:      f.req.Name,
		Size:      size,
		ModTime:   f.info.ModTime().Unix(),
		FileName:  binPath[len(f.mgr.dir)+1:],
	}); err != nil {
		log.Printf("记录缓存元数据失败（%s）：%v", f.req.Path, err)
	}
	f.mgr.evictIfNeeded()
}

// abortCache 放弃本次缓存写入：关掉并删除 .part，之后只走远端。
func (f *File) abortCache(cause error) {
	f.mu.Lock()
	local := f.local
	f.local = nil
	f.done = true
	f.mu.Unlock()
	if local == nil {
		return
	}
	local.Close()
	_ = os.Remove(f.partPath)
	log.Printf("放弃缓存（%s）：%v", f.req.Path, cause)
}

// Close 关闭来源：未提交的 .part 一律丢弃（按设计不做增量续传）。
func (f *File) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	local, done := f.local, f.done
	f.local = nil
	r := f.remote
	f.mu.Unlock()

	var err error
	if local != nil {
		err = local.Close()
		if !done {
			_ = os.Remove(f.partPath)
		}
	}
	if r != nil {
		if cerr := r.Close(); err == nil {
			err = cerr
		}
	}
	f.mgr.release(f.req.SourceKey)
	if f.writerHeld {
		f.mgr.releaseWriter(f.req.SourceKey)
	}
	return err
}
