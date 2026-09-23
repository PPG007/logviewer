package logfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// readWindowBytes 单次连续读取的字节上限：窗口内一次读回、再按偏移切分。
//
// 远端来源每行一次 ReadAt 就是一次网络往返，逐行读在跨地域链路上不可用
// （一页 20 行 ≈ 1 秒）；连续区间读把往返降到「区间/窗口」次，同时内存有界。
// 窗口不宜过大：ReadLines 全程持读锁（与 Close/Reload 互斥），1MB 在慢链路上
// 已可能占用数百毫秒；而常见的整页读取（500 行×百字节级）远小于一个窗口，一次往返即可。
const readWindowBytes = 1 << 20 // 1MB

// lineEnd 第 lineNo 行的结束偏移（最后一行的结束即文件末尾快照）。
func (s *FileSession) lineEnd(lineNo int64) int64 {
	if lineNo+1 < s.totalLines {
		return s.offsets[lineNo+1]
	}
	return s.fileSize
}

// ReadLines 按行号（0-based）读取 start..start+count-1 行，返回去除行尾 \r\n 的内容。
// 越界返回空切片，不报错。返回长度恒等于实际请求的行数。
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
	for i := int64(0); i < count; {
		// 窗口：从第 i 行起尽量多收，直到再加一行就超过 readWindowBytes。
		// 单个超长行也必须整行收进窗口（否则永远读不完）。
		winStart := s.offsets[start+i]
		winEnd := s.lineEnd(start + i)
		j := i + 1
		for j < count {
			end := s.lineEnd(start + j)
			if end-winStart > readWindowBytes {
				break
			}
			winEnd = end
			j++
		}

		n := winEnd - winStart
		if int64(cap(buf)) < n {
			buf = make([]byte, n)
		}
		var win []byte
		if n > 0 {
			win = buf[:n]
			got, err := s.f.ReadAt(win, winStart)
			if err != nil && err != io.EOF {
				return nil, err
			}
			if int64(got) < n {
				// 文件在索引之后被截断或改写：行偏移已不可信。
				// 若把缺的部分当空行返回，会静默给出错误内容——明确报错，让用户重新加载。
				return nil, fmt.Errorf("文件内容已变化（读取到 %d 字节，少于索引记录的 %d），请重新加载", got, n)
			}
		}

		for k := i; k < j; k++ {
			lineNo := start + k
			off := s.offsets[lineNo] - winStart
			if off >= int64(len(win)) {
				out = append(out, "")
				continue
			}
			end := s.lineEnd(lineNo) - winStart
			if end > int64(len(win)) {
				end = int64(len(win))
			}
			out = append(out, trimLineEnd(string(win[off:end])))
		}
		i = j
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
	// 顺序读共享读指针：先 Seek 回文件头（ReadAt 不受影响，但 Scan 用 Read）。
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
