// 命令 bench：对合成大日志跑一遍核心链路压测，对照功能设计 §9 目标输出 PASS/FAIL
// （docs/issues/08 §5.2）。需先运行 cmd/genlog 生成文件。
//
// 用法: go run ./cmd/bench -file bench_500mb.jsonl
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"logviewer/internal/search"
	"logviewer/internal/service"
)

const (
	targetIndex = 5.0  // s：索引 < 5s
	targetCold  = 3.0  // s：冷扫描检索 < 3s
	targetPage  = 50.0 // ms：翻页 < 50ms
	targetMem   = 200  // MB：峰值内存 < 200MB
	// 热检索 < 100ms 依赖 mmap/OS 页缓存，为 v1.1 目标（v1 记录实测值）。
)

type row struct {
	name   string
	val    string
	target string
	pass   bool
}

var rows []row

func add(name, val, target string, pass bool) {
	rows = append(rows, row{name, val, target, pass})
}

func main() {
	file := flag.String("file", "bench_500mb.jsonl", "待测日志文件")
	pages := flag.Int("pages", 10, "翻页采样次数")
	flag.Parse()

	fi, err := os.Stat(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench: 找不到文件（先运行 cmd/genlog）：", err)
		os.Exit(1)
	}

	svc := service.NewLogService()
	runtime.GC()

	// 5ms 采样 HeapAlloc/HeapSys 峰值（后台 goroutine，测试期间持续）
	var peakAlloc, peakSys atomic.Uint64
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				updateMax(&peakAlloc, m.HeapAlloc)
				updateMax(&peakSys, m.HeapSys)
			}
		}
	}()

	fmt.Printf("环境: %s/%s, go %s, %d 核, 文件 %d MB（%.1f MB）\n",
		runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.NumCPU(),
		fi.Size()>>20, float64(fi.Size())/(1<<20))
	begin := time.Now()

	// ---- 1) 索引（OpenFile → 索引完成；GetFields 内部 WaitReady 阻塞到就绪）----
	t0 := time.Now()
	info, err := svc.OpenFile(*file)
	if err != nil {
		fatal(err)
	}
	if _, err := svc.GetFields(info.ID); err != nil { // 返回时索引必已完成
		fatal(err)
	}
	indexDur := time.Since(t0)
	// 行数：以就绪后的 GetLines.Total 为准（OpenFile 返回时刻 total 可能仍是 0）
	browse, err := svc.GetLines(info.ID, 0, 0)
	if err != nil {
		fatal(err)
	}
	totalLines := browse.Total
	add("索引耗时", msf(indexDur), fmt.Sprintf("< %.0fs", targetIndex), indexDur.Seconds() < targetIndex)
	add("索引内存(理论)", fmt.Sprintf("%.1f MB (offsets %d × 8B)", float64(totalLines*8)/(1<<20), totalLines), "≈ 10MB", false)

	// ---- 2) 检索 ----
	kwQ := search.Query{Keyword: "order"}
	kwCold, d1 := runSearch(svc, info.ID, "bench-kw-cold", kwQ)
	add("关键字冷检索 “order”", fmt.Sprintf("%s / %d 命中", msf(d1), kwCold), fmt.Sprintf("< %.0fs", targetCold), d1.Seconds() < targetCold)
	_, d2 := runSearch(svc, info.ID, "bench-kw-hot", kwQ)
	add("关键字热检索（同文件二次）", msf(d2), "< 100ms（v1.1 参考）", d2.Seconds() < 0.1)

	condQ := search.Query{Conditions: []search.Condition{{Field: "level", Op: "=", Value: "ERROR"}}}
	errCold, d3 := runSearch(svc, info.ID, "bench-lv-cold", condQ)
	add("字段条件冷检索 level=ERROR", fmt.Sprintf("%s / %d 命中", msf(d3), errCold), fmt.Sprintf("< %.0fs", targetCold), d3.Seconds() < targetCold)
	_, d4 := runSearch(svc, info.ID, "bench-lv-hot", condQ)
	add("字段条件热检索（同文件二次）", msf(d4), "< 100ms（v1.1 参考）", d4.Seconds() < 0.1)

	now := time.Now()
	from := now.Add(-24 * time.Hour).UnixMilli()
	to := now.UnixMilli()
	trQ := search.Query{TimeRange: &search.TimeRange{From: &from, To: &to}}
	allHits, d5 := runSearch(svc, info.ID, "bench-tr", trQ)
	add("时间范围冷检索(全量解析)", fmt.Sprintf("%s / %d 命中(全部行)", msf(d5), allHits), "< 3s（最重路径参考）", d5.Seconds() < targetCold)

	// ---- 3) 翻页 ----
	// 命中缓存热翻页：取 level=ERROR 结果的连续页
	var pgSum time.Duration
	for p := 2; p <= 2+*pages; p++ {
		t0 = time.Now()
		if _, err := svc.GetPage(info.ID, "bench-lv-cold", p, 100); err != nil {
			fatal(err)
		}
		pgSum += time.Since(t0)
	}
	pgAvg := pgSum / time.Duration(*pages)
	add("检索翻页 GetPage 平均", fmt.Sprintf("%.2f ms (%d 页 × 100 行)", float64(pgAvg.Microseconds())/1000, *pages), fmt.Sprintf("< %.0f ms", targetPage), pgAvg.Milliseconds() < int64(targetPage))

	// 浏览翻页 GetLines（原始行，含解析）
	var blSum time.Duration
	for i := 0; i < *pages; i++ {
		t0 = time.Now()
		if _, err := svc.GetLines(info.ID, totalLines/2+int64(i)*100, 100); err != nil {
			fatal(err)
		}
		blSum += time.Since(t0)
	}
	blAvg := blSum / time.Duration(*pages)
	add("浏览翻页 GetLines 平均", fmt.Sprintf("%.2f ms (%d 次 × 100 行)", float64(blAvg.Microseconds())/1000, *pages), fmt.Sprintf("< %.0f ms", targetPage), blAvg.Milliseconds() < int64(targetPage))

	// ---- 4) 内存 ----
	close(stop)
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	add("内存：HeapAlloc 采样峰值", mb(peakAlloc.Load()), fmt.Sprintf("< %d MB", targetMem), peakAlloc.Load() < targetMem<<20)
	add("内存：HeapSys 采样峰值", mb(peakSys.Load()), fmt.Sprintf("< %d MB（驻留参考）", targetMem), false)
	add("内存：测试结束 HeapAlloc", mb(m.HeapAlloc), "—", false)

	if err := svc.CloseFile(info.ID); err != nil {
		fatal(err)
	}

	// ---- 输出 ----
	fmt.Printf("\n%-46s %-26s %-24s %s\n", "指标", "实测", "目标(设计 §9)", "判定")
	fmt.Println("────────────────────────────────────────────────────────────────────────────────")
	for _, r := range rows {
		mark := ""
		switch {
		case r.pass:
			mark = "[PASS]"
		case r.target == "—":
			mark = "  -  "
		case r.target == "≈ 10MB" || r.target == "< 100ms（v1.1 参考）" || r.target == "< 200 MB（驻留参考）":
			mark = "  *  " // 说明性条目
		case r.target == "< 3s（最重路径参考）":
			mark = passMark(r.pass)
		default:
			mark = "[FAIL]"
		}
		fmt.Printf("%-46s %-26s %-24s %s\n", r.name, r.val, r.target, mark)
	}
	fmt.Printf("\n总耗时 %.1fs\n", time.Since(begin).Seconds())
	fmt.Println("注: 热检索 <100ms 为 v1.1 目标（需 mmap/页缓存优化）；说明性条目不参与判定；* 仅提示。")
}

func updateMax(a *atomic.Uint64, v uint64) {
	for {
		old := a.Load()
		if v <= old || a.CompareAndSwap(old, v) {
			return
		}
	}
}

func runSearch(svc *service.LogService, fileID, tabID string, q search.Query) (hits int64, d time.Duration) {
	t0 := time.Now()
	res, err := svc.Search(fileID, tabID, q, 100)
	if err != nil {
		fatal(err)
	}
	return res.Total, time.Since(t0)
}

func passMark(p bool) string {
	if p {
		return "[PASS]"
	}
	return "[FAIL]"
}

func msf(d time.Duration) string {
	return fmt.Sprintf("%.0f ms", float64(d.Microseconds())/1000)
}

func mb(b uint64) string {
	return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench: ", err)
	os.Exit(1)
}
