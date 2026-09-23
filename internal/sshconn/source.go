package sshconn

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// errRemoteClosed 本地主动关闭导致的读中断（不是远端故障）。
var errRemoteClosed = errors.New("远端文件已关闭")

// pumpCloseWait 关闭时等待泵送 goroutine 收尾的上限。
//
// 关掉读端能让 WriteTo 立刻停止写，但它返回前还要 wg.Wait() 等已发出的 SFTP 读请求
// 全部回来（在途窗口约 maxConcurrentRequests × maxPacket ≈ 2MB）。慢链路上这几 MB
// 可能要好几秒，而「关闭文件」是用户点一下就要有反馈的操作，不能被链路拖住；
// 超时后直接返回，残留的 goroutine 会自行收尾并关闭它自己的句柄。
const pumpCloseWait = 500 * time.Millisecond

// RemoteFile 远端文件的字节来源，满足 logfile.Source（ReadAt / Read / Seek / Stat / Close）。
//
// 两种读法走两条互不干扰的通道：
//   - 顺序读（索引整文件、Scan 检索）：每次泵送新开一个 SFTP 句柄，用 File.WriteTo 写入
//     io.Pipe —— 库内部按文件大小开并发工作池分块取数，吞吐取决于链路带宽而非单包 RTT；
//   - 随机读（翻页、导出）：独立的常驻句柄走 ReadAt。
//
// 必须分开的原因是 sftp.File 的 WriteTo 会全程持有该文件对象的写锁，同一句柄上的 ReadAt
// 会被整趟传输阻塞；而 Close 与该句柄上的读写也不是并发安全的，故每趟泵送用完即弃。
type RemoteFile struct {
	client *sftp.Client
	path   string
	info   os.FileInfo // 打开时刻的快照（大小/修改时间语义与本地一致）

	mu      sync.Mutex
	pos     int64
	pr      *io.PipeReader
	pw      *io.PipeWriter
	pumping bool
	pumped  chan struct{} // 当前泵送 goroutine 结束
	closed  bool

	rndMu sync.RWMutex
	rnd   *sftp.File // 随机读句柄
}

// OpenFile 打开远端文件并返回字节来源。
func (m *Manager) OpenFile(p Profile, remotePath string) (*RemoteFile, error) {
	cl, err := m.client(p)
	if err != nil {
		return nil, err
	}
	fi, err := cl.Stat(remotePath)
	if err != nil {
		return nil, remoteError(err, fmt.Sprintf("读取 %s", remotePath))
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s 是目录，请选择日志文件", remotePath)
	}
	rnd, err := cl.Open(remotePath)
	if err != nil {
		return nil, remoteError(err, fmt.Sprintf("打开 %s", remotePath))
	}
	return &RemoteFile{client: cl, path: remotePath, info: fi, rnd: rnd}, nil
}

// Stat 打开时刻的文件信息快照。
func (f *RemoteFile) Stat() (os.FileInfo, error) { return f.info, nil }

// ReadAt 按偏移读取（翻页、导出用），与顺序读互不干扰。
func (f *RemoteFile) ReadAt(b []byte, off int64) (int, error) {
	f.rndMu.RLock()
	defer f.rndMu.RUnlock() // 与 Close 互斥：sftp.File 的读写不能与 Close 并发
	if f.rnd == nil {
		return 0, os.ErrClosed
	}
	return f.rnd.ReadAt(b, off)
}

// Read 从当前位置顺序读取；首次调用（或 Seek 之后）会重建泵送管线。
func (f *RemoteFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, os.ErrClosed
	}
	if !f.pumping {
		f.startPumpLocked()
	}
	pr := f.pr
	f.mu.Unlock()

	n, err := pr.Read(p)

	f.mu.Lock()
	f.pos += int64(n)
	if err != nil && f.pr == pr {
		f.pumping = false // 管线已结束：下次 Read 重新泵送
	}
	f.mu.Unlock()
	return n, err
}

// Seek 定位读指针；下一次 Read 会从新位置重新泵送。
func (f *RemoteFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.pos + offset
	case io.SeekEnd:
		abs = f.info.Size() + offset
	default:
		return 0, errors.New("无效的 whence")
	}
	if abs < 0 {
		return 0, errors.New("定位到负偏移")
	}
	f.pos = abs
	if f.pumping {
		if f.pr != nil {
			f.pr.CloseWithError(errRemoteClosed) // 中断旧管线
		}
		f.pumping = false
	}
	return abs, nil
}

// Close 关闭来源：先中断泵送，再关闭句柄。可多次调用。
func (f *RemoteFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	pr, pumping, pumped := f.pr, f.pumping, f.pumped
	f.pr, f.pw, f.pumping = nil, nil, false
	f.mu.Unlock()

	// 关掉读端 → WriteTo 的下一次写失败并立即退出（不能直接关句柄：那样会阻塞到整趟传完）。
	if pumping && pr != nil {
		pr.CloseWithError(errRemoteClosed)
	}
	if pumped != nil {
		select {
		case <-pumped: // 泵送已收尾（它自己会关闭该趟的句柄）
		case <-time.After(pumpCloseWait):
		}
	}

	f.rndMu.Lock()
	rnd := f.rnd
	f.rnd = nil
	f.rndMu.Unlock()
	if rnd == nil {
		return nil
	}
	if err := rnd.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// startPumpLocked 启动顺序泵送（须持 f.mu）。每趟用新句柄，避免与其他读争用文件锁。
func (f *RemoteFile) startPumpLocked() {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	f.pr, f.pw = pr, pw
	f.pumping = true
	f.pumped = done
	go f.pump(f.pos, pw, done)
}

// pump 顺序泵送完成后关闭 pw（错误经 pw 传给读端），并释放本趟句柄。
func (f *RemoteFile) pump(start int64, pw *io.PipeWriter, done chan struct{}) {
	defer close(done)
	seq, err := f.client.Open(f.path)
	if err != nil {
		pw.CloseWithError(remoteError(err, "打开远端文件"))
		return
	}
	defer seq.Close()

	if start <= 0 {
		// 新句柄偏移为 0：WriteTo 内部按文件大小并发分块读
		_, err = seq.WriteTo(pw)
	} else {
		err = copyFrom(seq, pw, start)
	}
	pw.CloseWithError(err) // err 为 nil 时等价于 Close
}

// copyFrom 从 off 起顺序拷贝（WriteTo 只从当前偏移开始，非 0 起点用分块 ReadAt）。
func copyFrom(seq *sftp.File, w io.Writer, off int64) error {
	buf := make([]byte, 1<<20)
	for {
		n, err := seq.ReadAt(buf, off)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			off += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
