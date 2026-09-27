// Command benchdemo 演示工作负载注册、采样、回归判定、
// 缓存增量重跑与基线保护的端到端用法。
//
// 运行方式：
//
//	go run ./example
//
// 产物写入 .benchstate/（基线 JSON、缓存 JSON、审计 JSONL），
// 删除该目录即可从零开始。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	bench "github.com/lechandonga/benchmark-regression-gate-go"
)

func main() {
	stateDir := flag.String("state", ".benchstate", "directory for baselines, cache and audit log")
	slowdown := flag.Float64("slowdown", 0, "artificial slowdown factor for this run (e.g. 0.25 = 25% slower)")
	flag.Parse()

	// slowdown 模拟实现变更，因此它必须进入 SourceHash，
	// 否则结果缓存会把“新版本”误判为未变化而复用旧样本。
	sourceHash := fmt.Sprintf("v1-slow%.2f", *slowdown)

	reg := bench.NewRegistry()
	// 注册两个工作负载：模拟不同基础耗时与轻微噪声。
	reg.Register(bench.Workload{
		ID: "encode.1k", SourceHash: sourceHash, Deps: []string{"codec@2.3.1"},
		Description: "encode a 1KiB payload",
		Run: func(ctx context.Context) (int64, int64, int64, error) {
			// 用真实但极短的 sleep 模拟工作；±3% 抖动由 time.Sleep 自身产生。
			base := time.Duration(float64(2*time.Millisecond) * (1 + *slowdown))
			start := time.Now()
			time.Sleep(base)
			return time.Since(start).Nanoseconds(), 1024, 1, nil
		},
	})
	reg.Register(bench.Workload{
		ID: "hash.1m", SourceHash: sourceHash, Deps: []string{"sha256@stdlib"},
		Run: func(ctx context.Context) (int64, int64, int64, error) {
			base := time.Duration(float64(500*time.Microsecond) * (1 + *slowdown))
			start := time.Now()
			time.Sleep(base)
			return time.Since(start).Nanoseconds(), 1_000_000, 1, nil
		},
	})

	store, err := bench.NewFileBaselineStore(*stateDir + "/baselines")
	if err != nil {
		log.Fatal(err)
	}
	cache, err := bench.NewFileResultCache(*stateDir + "/cache")
	if err != nil {
		log.Fatal(err)
	}
	audit, err := bench.NewJSONLAuditLog(*stateDir + "/audit.jsonl")
	if err != nil {
		log.Fatal(err)
	}

	svc := bench.NewService(
		reg,
		bench.NewBaselineManager(store),
		cache,
		audit,
		bench.ServiceConfig{
			Run: bench.RunConfig{
				Runs: 3, Iterations: 7, Warmup: 1,
				Concurrency: 2, IterationTimeout: time.Second,
			},
			Gate:            bench.DefaultGateConfig(),
			UseCache:        true,
			UpdateBaselines: true,
		},
	)

	report, err := svc.Evaluate(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}

	failed := report.Failed()
	for _, id := range reg.List() {
		d := report.Decisions[id]
		rp := report.Reports[id]
		fmt.Fprintf(os.Stdout, "%-10s verdict=%-11s ratio=%.3f z=%6.2f median=%dns samples=%d failures=%d\n",
			id, d.Verdict, d.Ratio, d.ZScore, int64(rp.Stats.Median), rp.Stats.N, len(rp.Failures))
		for _, reason := range d.Reasons {
			fmt.Fprintf(os.Stdout, "             - %s\n", reason)
		}
	}

	if len(failed) > 0 {
		fmt.Fprintf(os.Stdout, "\nGATE FAILED: %v\n", failed)
		os.Exit(1)
	}
	fmt.Println("\nGATE OK")
}
