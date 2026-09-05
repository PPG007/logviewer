// 命令 genlog：生成约 N MB 的合成 JSONL 日志（docs/issues/08 §5.1）。
// 每行约 400-480 字节、全部为合法 JSON；字段与 parse 包约定对齐：
// time(RFC3339Nano)/level/msg/trace_id/latency_ms/service。
// 级别分布 10% ERROR（首词取错误场景词，便于按场景检索）、20% WARN、50% INFO、20% DEBUG。
//
// 用法: go run ./cmd/genlog -out bench_500mb.jsonl -size 500
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"time"
)

var (
	levels = []string{
		"INFO", "INFO", "INFO", "INFO", "INFO",
		"DEBUG", "DEBUG",
		"WARN", "WARN",
		"ERROR",
	}
	services = []string{
		"api-gateway", "user-service", "order-service",
		"payment-service", "search-service", "cache-service",
	}
	words = []string{
		"request", "response", "user", "session", "auth", "login", "token",
		"order", "payment", "checkout", "cart", "inventory", "shipment",
		"retry", "cache", "redis", "query", "database", "connection",
		"pool", "batch", "worker", "queue", "publish", "subscribe",
		"event", "metric", "health", "check", "deploy", "config",
		"latency", "thread", "buffer", "stream", "index", "gateway",
	}
	// ERROR 行消息首词：制造可检索的场景词
	errWords = []string{"timeout", "connection-reset", "retry-exceeded", "disk-full", "rejected", "rate-limited"}
)

func main() {
	out := flag.String("out", "bench_500mb.jsonl", "输出文件路径")
	sizeMB := flag.Int("size", 500, "目标大小（MB）")
	flag.Parse()

	target := int64(*sizeMB) << 20
	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)

	rng := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0x243f6a8885a308d3)) // 固定种子：可复现
	now := time.Now()

	var bytes, lines int64
	start := time.Now()
	for bytes < target {
		level := levels[rng.IntN(len(levels))]
		nw := 30 + rng.IntN(10) // 30-39 词 → 消息约 250-330 字节
		msg := make([]byte, 0, 320)
		for i := range nw {
			word := words[rng.IntN(len(words))]
			if i == 0 && level == "ERROR" {
				word = errWords[rng.IntN(len(errWords))]
			}
			if i > 0 {
				msg = append(msg, ' ')
			}
			msg = append(msg, word...)
		}
		t := now.Add(-time.Duration(rng.IntN(86400)) * time.Second)
		line := fmt.Sprintf(
			`{"time":"%s","level":"%s","msg":"%s","trace_id":"%016x","latency_ms":%d,"service":"%s"}`+"\n",
			t.Format(time.RFC3339Nano), level, msg, rng.Uint64(),
			rng.IntN(5000), services[rng.IntN(len(services))],
		)
		if _, err := w.WriteString(line); err != nil {
			log.Fatal(err)
		}
		bytes += int64(len(line))
		lines++
	}
	if err := w.Flush(); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "genlog: %s = %d bytes / %d lines (avg %.0f B/line), %.2fs\n",
		*out, bytes, lines, float64(bytes)/float64(lines), time.Since(start).Seconds())
}
