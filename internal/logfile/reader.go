package logfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ReadLines 按行号（0-based）读取 start..start+count-1 行，返回去除行尾 \r\n 的内容。
// 越界返回空切片，不报错。
func (s *FileSession) ReadLines(start, count int64) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, fmt.Errorf("session closed")
	}
	if s.status != StatusReady {
		return nil, fmt.Errorf("session not ready: %v", s.status)
	}
	if start < 0 || start >= s.totalLines || count <= 0 {
		return nil, nil
	}
	if start+count > s.totalLines {
		count = s.totalLines - start
	}
	out := make([]string, 0, count)
	var buf []byte
	for i := int64(0); i < count; i++ {
		lineNo := start + i
		off := s.offsets[lineNo]
		var length int64
		if lineNo+1 < s.totalLines {
			length = s.offsets[lineNo+1] - off
		} else {
			length = s.fileSize - off
		}
		if int64(cap(buf)) < length {
			buf = make([]byte, length)
		}
		b := buf[:length]
		if _, err := s.f.ReadAt(b, off); err != nil && err != io.EOF {
			return nil, err
		}
		out = append(out, trimLineEnd(string(b)))
	}
	return out, nil
}

// Scan 从文件头开始顺序遍历全部行（供检索使用，比逐行 ReadAt 快）。
// fn 返回 error 时中止并原样返回该 error。Scan 内部串行化，可安全地与 ReadAt 混用。
func (s *FileSession) Scan(fn func(lineNo int64, raw string) error) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return fmt.Errorf("session closed")
	}
	if s.status != StatusReady {
		s.mu.RUnlock()
		return fmt.Errorf("session not ready: %v", s.status)
	}
	s.scanMu.Lock()
	defer func() {
		s.scanMu.Unlock()
		s.mu.RUnlock()
	}()
	// *os.File 共享读指针：先 Seek 回文件头（ReadAt 不受影响，但 Scan 用 Read）。
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(s.f, 1<<20)
	var lineNo int64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if err := fn(lineNo, trimLineEnd(string(line))); err != nil {
				return err
			}
			lineNo++
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// trimLineEnd 去掉行尾的 \n 与 \r（兼容 Windows CRLF）。
func trimLineEnd(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
