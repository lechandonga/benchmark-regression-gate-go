// Command benchgate runs reproducible benchmarks, gates regressions against
// a baseline and caches results for incremental reruns.
//
// Usage:
//
//	benchgate run      [flags] [workload...]   sample and gate against baseline
//	benchgate baseline [flags] [workload...]   (re-)establish the baseline
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/lechandonga/benchmark-regression-gate-go/baseline"
	"github.com/lechandonga/benchmark-regression-gate-go/bench"
	"github.com/lechandonga/benchmark-regression-gate-go/cache"
	"github.com/lechandonga/benchmark-regression-gate-go/gate"
	"github.com/lechandonga/benchmark-regression-gate-go/stats"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(os.Args[2:])
	case "baseline":
		code = cmdBaseline(os.Args[2:])
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: benchgate <run|baseline> [flags] [workload...]\n")
}

func newFlagSet(name string) (*flag.FlagSet, *bench.RunOptions, *stats.CompareOptions, *string, *string, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	ro := &bench.RunOptions{}
	co := &stats.CompareOptions{}
	fs.IntVar(&ro.Iterations, "iter", 30, "measured samples per workload")
	fs.IntVar(&ro.Concurrency, "conc", 1, "concurrent sampling workers")
	fs.IntVar(&ro.Warmup, "warmup", 3, "discarded warmup samples")
	fs.DurationVar(&ro.Timeout, "timeout", 10*time.Second, "per-sample timeout")
	fs.Float64Var(&co.NoiseBand, "noise", 0.05, "minimum relative noise tolerance")
	fs.Float64Var(&co.MinRegression, "min-regression", 0.10, "minimum relative slowdown counted as regression")
	cachePath := fs.String("cache", ".benchgate/cache.json", "result cache file")
	basePath := fs.String("baseline-file", ".benchgate/baseline.json", "baseline file")
	noCache := fs.Bool("no-cache", false, "ignore cached results and re-sample")
	return fs, ro, co, cachePath, basePath, noCache
}

func buildGate(ro *bench.RunOptions, co *stats.CompareOptions, cachePath, basePath string, noCache bool) *gate.Gate {
	reg := bench.NewRegistry()
	registerDemoWorkloads(reg)
	return &gate.Gate{
		Registry:     reg,
		Runner:       bench.NewRunner(reg),
		Cache:        cache.New(cachePath),
		Baselines:    baseline.NewStore(basePath),
		Compare:      *co,
		DisableCache: noCache,
	}
}

func cmdRun(args []string) int {
	fs, ro, co, cachePath, basePath, noCache := newFlagSet("run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	g := buildGate(ro, co, *cachePath, *basePath, *noCache)

	reports, err := g.Run(context.Background(), fs.Args(), *ro)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	failed := false
	for _, rep := range reports {
		printReport(rep)
		if rep.Failed() {
			failed = true
		}
	}
	if failed {
		fmt.Println("GATE: FAIL (regression detected)")
		return 1
	}
	fmt.Println("GATE: PASS")
	return 0
}

func cmdBaseline(args []string) int {
	fs, ro, co, cachePath, basePath, _ := newFlagSet("baseline")
	var allowRegression bool
	var tolerance float64
	fs.BoolVar(&allowRegression, "allow-regression", false, "allow baseline to move slower")
	fs.Float64Var(&tolerance, "tolerance", 0, "relative slowdown tolerated without --allow-regression")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	g := buildGate(ro, co, *cachePath, *basePath, true)

	results, err := g.UpdateBaseline(context.Background(), fs.Args(), *ro, baseline.UpdateOptions{
		AllowRegression: allowRegression,
		Tolerance:       tolerance,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	rejected := false
	for _, r := range results {
		status := "updated"
		if r.Rejected {
			status = "REJECTED"
			rejected = true
		}
		fmt.Printf("%-16s %s: %s\n", r.Workload, status, r.Reason)
	}
	if rejected {
		return 1
	}
	return 0
}

func printReport(rep gate.Report) {
	src := "fresh"
	if rep.FromCache {
		src = "cache"
	}
	s := rep.Summary
	fmt.Printf("== %s (%s, fingerprint %.12s)\n", rep.Workload, src, rep.Fingerprint)
	fmt.Printf("   samples: n=%d median=%s mean=%s stddev=%s cv=%.3f min=%s max=%s\n",
		s.N, s.Median, s.Mean, s.StdDev, s.CV, s.Min, s.Max)
	for _, inv := range rep.Invalid {
		fmt.Printf("   invalid sample iter=%d worker=%d reason=%s err=%s\n",
			inv.Iter, inv.Worker, inv.Reason, inv.Err)
	}
	d := rep.Decision
	fmt.Printf("   verdict: %s (ratio=%.3f threshold=%.1f%%) %s\n",
		d.Verdict, d.Ratio, d.Threshold*100, d.Reason)
}
