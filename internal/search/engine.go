package search

import (
	"context"
	"sync"

	"logviewer/internal/parse"
)

// Scanner 可顺序遍历的日志源（*logfile.FileSession 满足）。接口化便于测试取消语义。
type Scanner interface {
	TotalLines() int64
	Scan(fn func(lineNo int64, raw string) error) error
}

// ProgressFunc 检索进度回调（单位：行）。done 为已扫描行数，total 为总行数。
type ProgressFunc func(done, total int64)

// cancelEntry 包裹取消函数：以指针身份判断“本检索是否仍是该 tab 的当前扫描”。
type cancelEntry struct {
	cancel context.CancelFunc
}

// Engine 检索引擎：命中行号按 tabID 缓存，翻页为内存跳转；支持 context 取消。
type Engine struct {
	mu      sync.Mutex
	results map[string][]int64 // tabId -> 命中行号
	cancels map[string]*cancelEntry
}

func NewEngine() *Engine {
	return &Engine{
		results: make(map[string][]int64),
		cancels: make(map[string]*cancelEntry),
	}
}

// Search 顺序扫描源文件并缓存命中行号（tabID 已存在时旧结果被替换）。
// 返回全部命中行号（含调用方可能需要的计数）。取消时返回 context.Canceled 且不缓存结果。
func (e *Engine) Search(s Scanner, tabID string, q Query, onProgress ProgressFunc) ([]int64, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &cancelEntry{cancel: cancel}
	e.mu.Lock()
	// 同一 tab 再次检索时先取消旧扫描，避免新旧结果乱序覆盖。
	if old := e.cancels[tabID]; old != nil {
		old.cancel()
	}
	e.cancels[tabID] = entry
	e.mu.Unlock()
	defer func() {
		// 仅当本检索仍是该 tab 的当前句柄时才清理（防止误删新检索的句柄）。
		e.mu.Lock()
		if e.cancels[tabID] == entry {
			delete(e.cancels, tabID)
		}
		e.mu.Unlock()
	}()

	m := NewMatcher(q)
	total := s.TotalLines()
	matched := make([]int64, 0, 1024)
	// 无字段条件且无时间范围时无需逐行 JSON 解析（纯关键字/全量扫描更快）
	needParsed := len(q.Conditions) > 0 || q.TimeRange != nil

	err := s.Scan(func(lineNo int64, raw string) error {
		// 每 4096 行检查一次取消
		if lineNo&4095 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		var parsed map[string]any
		var ts *int64
		if needParsed {
			if p, ok := parse.ParseLine(raw); ok {
				parsed = p
				ts = parse.ExtractTimestamp(p)
			}
		}
		if m.Match(raw, parsed, ts) {
			matched = append(matched, lineNo)
		}
		if onProgress != nil && lineNo&0xFFFF == 0 { // 每 65536 行上报一次
			onProgress(lineNo, total)
		}
		return nil
	})
	if err != nil {
		return nil, err // 取消时 err == context.Canceled
	}

	e.mu.Lock()
	e.results[tabID] = matched
	e.mu.Unlock()
	if onProgress != nil {
		onProgress(total, total)
	}
	return matched, nil
}

// Cancel 取消 tabID 的进行中检索（无则空操作）。
func (e *Engine) Cancel(tabID string) {
	e.mu.Lock()
	if c := e.cancels[tabID]; c != nil {
		c.cancel()
	}
	e.mu.Unlock()
}

// Remove 释放 tabID 的结果与取消句柄（关闭 tab 时调用）。
func (e *Engine) Remove(tabID string) {
	e.mu.Lock()
	if c := e.cancels[tabID]; c != nil {
		c.cancel()
	}
	delete(e.results, tabID)
	delete(e.cancels, tabID)
	e.mu.Unlock()
}

// Page 取 tabID 命中结果的第 page 页（从 1 开始，pageSize 条）。
// 越界或结果不存在返回空切片。
func (e *Engine) Page(tabID string, page, pageSize int) ([]int64, error) {
	if page < 1 {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	matched := e.results[tabID]
	start := (page - 1) * pageSize
	if start >= len(matched) {
		return nil, nil
	}
	return matched[start:min(start+pageSize, len(matched))], nil
}

// Total 返回 tabID 命中总数；结果不存在返回 0。
func (e *Engine) Total(tabID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.results[tabID])
}

// All 返回 tabID 的全部命中行号副本（扫描顺序升序；无结果返回空切片）。用于导出。
func (e *Engine) All(tabID string) []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.results[tabID]...)
}
