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

// progressEvery 索引进度上报的字节间隔。
const progressEvery = 1 << 20 // 1MB

// FileSession 一个已打开日志文件的会话。
type FileSession struct {
	ID   string // 会话 id（内部生成）
	Path string
	Name string // 文件名（filepath.Base）

	f        *os.File
	fileSize int64 // 打开时的文件字节数（「打开时刻快照」语义）

	mu         sync.RWMutex
	totalLines int64
	offsets    []int64 // 每行起始字节偏移，len == totalLines
	status     Status
	err        error
	doneBytes  int64         // 索引已读字节数（进度）
	done       chan struct{} // 索引完成（ready/error）时关闭
	closed     bool

	// scanMu 串行化整文件 Scan（*os.File 共享同一读指针）。
	scanMu sync.Mutex
}

// Open 打开文件并启动后台索引 goroutine，立即返回（status=indexing）。
// onLine 可传 nil；回调发生在索引 goroutine 内，须并发安全。
func Open(path string, onLine LineFunc) (*FileSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, errors.New("path is a directory: " + path)
	}
	s := &FileSession{
		ID:       newID(),
		Path:     path,
		Name:     filepath.Base(path),
		f:        f,
		fileSize: fi.Size(),
		status:   StatusIndexing,
		done:     make(chan struct{}),
	}
	go s.index(onLine)
	return s, nil
}

// index 后台建索引：一次顺序扫描记录每行起始偏移。
func (s *FileSession) index(onLine LineFunc) {
	defer close(s.done)
	// onLine 来自外部（字段收集），panic 不应当杀掉整个进程。
	defer func() {
		if r := recover(); r != nil {
			s.mu.Lock()
			s.status = StatusError
			s.err = fmt.Errorf("index panic: %v", r)
			s.mu.Unlock()
		}
	}()

	br := bufio.NewReaderSize(s.f, 1<<20) // 1MB 读缓冲
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
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			s.mu.Lock()
			s.status = StatusError
			s.err = err
			s.mu.Unlock()
			s.setDoneBytes(offset)
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

// WaitReady 阻塞直到索引完成（ready 或 error）。
func (s *FileSession) WaitReady() error {
	<-s.done
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.status == StatusError {
		return s.err
	}
	return nil
}

// Close 关闭底层文件并释放资源，可多次调用。
func (s *FileSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if err := s.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
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
