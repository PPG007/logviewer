// Package logfile 提供文件会话与行偏移索引：
// 打开文件后后台顺序扫描一次，记录每行起始字节偏移（[]int64，8 字节/行），
// 原始内容不驻留内存；展示/翻页用 ReadAt 按偏移随机读，检索用 Scan 顺序遍历。
package logfile

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status 会话状态。
type Status int

const (
	StatusIndexing Status = iota // 正在建索引
	StatusReady                  // 可浏览/检索
	StatusError                  // 打开或索引失败
)

func (s Status) String() string {
	switch s {
	case StatusIndexing:
		return "indexing"
	case StatusReady:
		return "ready"
	case StatusError:
		return "error"
	}
	return "unknown"
}

// LineFunc 每读一行回调一次（字段收集等「顺带」工作复用同一次扫描）。
type LineFunc func(lineNo int64, raw string)

// Source 日志文件的字节来源：本地文件（*os.File）或远端文件（如 SFTP）。
// 只依赖三个 I/O 原语——随机读 ReadAt、顺序读 Seek+Read、元信息 Stat——
// *os.File 与 *sftp.File 都天然满足，因此索引/检索/分页算法不必区分二者。
type Source interface {
	io.ReaderAt
	io.Reader
	io.Seeker
	Stat() (os.FileInfo, error)
	io.Closer
}

// errClosed 索引过程中会话被关闭：状态置 error，避免 WaitReady 被误判为「索引成功」。
var errClosed = errors.New("session closed")

// progressEvery 索引进度上报的字节间隔（也用于检查会话是否已关闭）。
const progressEvery = 1 << 20 // 1MB

// FileSession 一个已打开日志文件的会话。
type FileSession struct {
	ID   string // 会话 id（内部生成）
	Path string // 展示用路径（本地绝对路径 / 远端 POSIX 路径）
	Name string // 文件名（本地 filepath.Base，远端 path.Base）

	f        Source
	fileSize int64     // 打开时的文件字节数（「打开时刻快照」语义）
	modTime  time.Time // 打开时的文件修改时间（历史记录展示用）

	mu         sync.RWMutex
	totalLines int64
	offsets    []int64 // 每行起始字节偏移，len == totalLines
	status     Status
	err        error
	doneBytes  int64         // 索引已读字节数（进度）
	done       chan struct{} // 索引完成（ready/error）时关闭
	closed     bool

	// scanMu 串行化整文件 Scan（顺序读与随机读混用时由下层来源保证互不干扰）。
	scanMu sync.Mutex
}

// Open 打开本地文件并启动后台索引 goroutine，立即返回（status=indexing）。
// onLine 可传 nil；回调发生在索引 goroutine 内，须并发安全。
func Open(path string, onLine LineFunc) (*FileSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return OpenSource(f, path, filepath.Base(path), onLine)
}

// OpenSource 打开任意字节来源并启动后台索引 goroutine。
// displayPath 仅用于展示与去重（远端传 POSIX 路径，不能用 filepath 处理）；
// name 为展示文件名（远端须用 path.Base）。失败时负责关闭 src。
func OpenSource(src Source, displayPath, name string, onLine LineFunc) (*FileSession, error) {
	fi, err := src.Stat()
	if err != nil {
		src.Close()
		return nil, err
	}
	if fi.IsDir() {
		src.Close()
		return nil, errors.New("path is a directory: " + displayPath)
	}
	if name == "" {
		name = displayPath
	}
	s := &FileSession{
		ID:       newID(),
		Path:     displayPath,
		Name:     name,
		f:        src,
		fileSize: fi.Size(), // 末行长度以打开时刻大小为界，故须精确
		modTime:  fi.ModTime(),
		status:   StatusIndexing,
		done:     make(chan struct{}),
	}
	go s.index(onLine)
	return s, nil
}

// Reload 用新的字节来源重建索引：文件被追加、轮转或远端内容变化后调用。
//
// 会话 id 与展示名保持不变，因此前端的 tab、检索条件与选中的文件都不会丢；
// 行偏移、大小、修改时间在扫描完成时整体替换。索引期间状态回到 indexing，
// WaitReady 会重新阻塞到本轮扫描结束。
//
// open 由调用方提供，并且**在旧来源关闭之后才调用**：新旧两份额来源不能同时存在。
// 远端缓存的写入权是「每个来源键同一时刻一个写入者」，重叠会让新来源静默退化成
// 不缓存（重新加载过的内容就写不回缓存，下次打开又弹回旧的）。
func (s *FileSession) Reload(open func() (Source, error), onLine LineFunc) error {
	// 取写锁同时也会等待进行中的 Scan/ReadLines 结束：不能在扫描脚下换索引。
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.status == StatusIndexing {
		return errors.New("正在建立索引，请稍后再试")
	}

	old := s.f
	s.f = nil
	if old != nil {
		old.Close() // 先释放旧来源（本地可能已是被轮转掉的 inode）
	}

	src, err := open() // 慢操作（含远端连接）；此刻读者已被写锁挡住
	if err != nil {
		// 打不开新来源：会话进入错误态，等待调用方处理（重新连接/重新打开）
		s.status = StatusError
		s.err = err
		s.done = make(chan struct{})
		close(s.done)
		return err
	}
	fi, statErr := src.Stat()
	if statErr != nil {
		src.Close()
		s.status = StatusError
		s.err = statErr
		s.done = make(chan struct{})
		close(s.done)
		return statErr
	}
	if fi.IsDir() {
		src.Close()
		err := errors.New("path is a directory: " + s.Path)
		s.status = StatusError
		s.err = err
		s.done = make(chan struct{})
		close(s.done)
		return err
	}

	s.f = src
	s.fileSize = fi.Size()
	s.modTime = fi.ModTime()
	s.status = StatusIndexing
	s.err = nil
	s.doneBytes = 0
	s.totalLines = 0
	s.offsets = nil
	s.done = make(chan struct{}) // 新一轮的完成信号
	go s.index(onLine)
	return nil
}

// index 后台建索引：一次顺序扫描记录每行起始偏移。
func (s *FileSession) index(onLine LineFunc) {
	// 本次扫描的来源与完成信号在本轮开始时取定：Reload 会替换这两者。
	s.mu.RLock()
	f, done := s.f, s.done
	s.mu.RUnlock()
	defer close(done)
	// onLine 来自外部（字段收集），panic 不应当杀掉整个进程。
	defer func() {
		if r := recover(); r != nil {
			s.mu.Lock()
			s.status = StatusError
			s.err = fmt.Errorf("index panic: %v", r)
			s.mu.Unlock()
		}
	}()

	br := bufio.NewReaderSize(f, 1<<20) // 1MB 读缓冲
	var (
		offsets  []int64
		lines    int64
		offset   int64
		lastProg int64
	)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			offsets = append(offsets, offset)
			offset += int64(len(line))
			if onLine != nil {
				onLine(lines, trimLineEnd(string(line)))
			}
			lines++
			if offset-lastProg >= progressEvery {
				lastProg = offset
				s.setDoneBytes(offset)
				// 会话已关闭：立刻停止读取（远端索引可能正拉取上百 MB，不能等读完）。
				if s.isClosed() {
					s.fail(errClosed, offset)
					return
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			if s.isClosed() {
				err = errClosed // 关闭导致底层读取中断：报「已关闭」而非底层错误
			}
			s.fail(err, offset)
			return
		}
	}

	s.mu.Lock()
	s.offsets = offsets
	s.totalLines = lines
	s.status = StatusReady
	s.mu.Unlock()
	s.setDoneBytes(offset)
}

func (s *FileSession) setDoneBytes(b int64) {
	s.mu.Lock()
	s.doneBytes = b
	s.mu.Unlock()
}

// fail 把会话标记为索引失败（关闭导致的中断也走这里，保证 WaitReady 返回错误）。
func (s *FileSession) fail(err error, doneBytes int64) {
	s.mu.Lock()
	s.status = StatusError
	s.err = err
	s.doneBytes = doneBytes
	s.mu.Unlock()
}

func (s *FileSession) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Size 打开时刻的文件字节数（快照语义，不随文件后续追加变化）。
func (s *FileSession) Size() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileSize
}

// ModTime 打开时刻的文件修改时间。
func (s *FileSession) ModTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.modTime
}

// TotalLines 总行数（索引完成前为 0）。
func (s *FileSession) TotalLines() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalLines
}

// Status 当前状态。
func (s *FileSession) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// Error 出错时的错误（StatusError 时非 nil）。
func (s *FileSession) Error() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// Progress 返回索引进度（已读字节, 文件总字节）。非索引态返回 (总字节, 总字节)。
func (s *FileSession) Progress() (done, total int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status != StatusIndexing {
		return s.fileSize, s.fileSize
	}
	return s.doneBytes, s.fileSize
}

// Percent 索引进度百分比 0~100。
func (s *FileSession) Percent() float64 {
	done, total := s.Progress()
	if total <= 0 {
		return 100
	}
	return float64(done) / float64(total) * 100
}

// WaitReady 阻塞直到索引（或重新加载）完成（ready 或 error）。
func (s *FileSession) WaitReady() error {
	// 完成信号会被 Reload 替换，取通道本身也要持锁。
	s.mu.RLock()
	done := s.done
	s.mu.RUnlock()
	<-done
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status == StatusError {
		return s.err
	}
	return nil
}

// Close 关闭底层来源并释放资源，可多次调用。
// 关闭后正在进行的索引会在下次检查点（每 1MB）或底层读失败时退出。
func (s *FileSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	src := s.f
	s.mu.Unlock()
	if src == nil {
		return nil // 重新加载失败后可能没有来源
	}
	if err := src.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

// newID 生成随机会话 id。
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // 熵源不可用，直接失败
	}
	return hex.EncodeToString(b)
}
