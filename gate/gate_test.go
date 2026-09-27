package gate

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/lechandonga/benchmark-regression-gate-go/baseline"
	"github.com/lechandonga/benchmark-regression-gate-go/bench"
	"github.com/lechandonga/benchmark-regression-gate-go/cache"
	"github.com/lechandonga/benchmark-regression-gate-go/stats"
)

// testEnv pins the environment so fingerprints are reproducible in tests.
var testEnv = map[string]string{"go_version": "test", "goos": "testos", "goarch": "testarch", "cpus": "4"}

// countingWorkload returns a fixed duration per call and counts invocations.
type countingWorkload struct {
	calls atomic.Int64
}

func (cw *countingWorkload) workload(name string, cfg any, ns int64) bench.Workload {
	return bench.Workload{
		Name:   name,
		Config: cfg,
		Run: func(ctx context.Context) (int64, error) {
			cw.calls.Add(1)
			return ns, nil
		},
	}
}

func newGate(t *testing.T, reg *bench.Registry) *Gate {
	t.Helper()
	dir := t.TempDir()
	return &Gate{
		Registry:  reg,
		Runner:    bench.NewRunner(reg),
		Cache:     cache.New(filepath.Join(dir, "cache.json")),
		Baselines: baseline.NewStore(filepath.Join(dir, "baseline.json")),
		Compare:   stats.CompareOptions{NoiseBand: 0.05, MinRegression: 0.10},
		Env:       testEnv,
	}
}

func logReport(t *testing.T, rep Report) {
	t.Helper()
	s := rep.Summary
	t.Logf("workload=%s from_cache=%v fingerprint=%.12s", rep.Workload, rep.FromCache, rep.Fingerprint)
	t.Logf("  samples: n=%d median=%s mean=%s stddev=%s cv=%.3f",
		s.N, s.Median, s.Mean, s.StdDev, s.CV)
	for _, inv := range rep.Invalid {
		t.Logf("  invalid sample: iter=%d worker=%d reason=%s err=%s", inv.Iter, inv.Worker, inv.Reason, inv.Err)
	}
	d := rep.Decision
	t.Logf("  verdict=%s ratio=%.3f threshold=%.1f%% reason=%s",
		d.Verdict, d.Ratio, d.Threshold*100, d.Reason)
}

func TestIncrementalRerunUsesCache(t *testing.T) {
	reg := bench.NewRegistry()
	cw := &countingWorkload{}
	if err := reg.Register(cw.workload("w", map[string]any{"n": 100}, 1000)); err != nil {
		t.Fatal(err)
	}
	g := newGate(t, reg)
	opts := bench.RunOptions{Iterations: 10, Concurrency: 2}
	ctx := context.Background()

	// First run: fresh sampling.
	reps, err := g.Run(ctx, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if reps[0].FromCache {
		t.Fatal("first run must sample fresh")
	}
	freshCalls := cw.calls.Load()
	if freshCalls != 10 {
		t.Fatalf("expected 10 samples, got %d", freshCalls)
	}
	logReport(t, reps[0])

	// Second run, unchanged config/env: no re-execution, identical stats.
	reps2, err := g.Run(ctx, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reps2[0].FromCache {
		t.Fatal("unchanged workload must be served from cache")
	}
	if cw.calls.Load() != freshCalls {
		t.Errorf("unchanged workload was re-executed: calls %d -> %d", freshCalls, cw.calls.Load())
	}
	if reps2[0].Summary != reps[0].Summary {
		t.Errorf("cached summary differs: %+v vs %+v", reps2[0].Summary, reps[0].Summary)
	}
	logReport(t, reps2[0])

	// Environment change invalidates the cache.
	g.Env = map[string]string{"go_version": "test", "goos": "testos", "goarch": "testarch", "cpus": "8"}
	reps3, err := g.Run(ctx, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if reps3[0].FromCache {
		t.Error("environment change must invalidate the cache")
	}
	if cw.calls.Load() != freshCalls+10 {
		t.Errorf("expected re-sampling after env change, calls=%d", cw.calls.Load())
	}
	t.Logf("env change -> cache invalidated, re-sampled (calls=%d)", cw.calls.Load())
}

func TestConfigChangeInvalidatesCache(t *testing.T) {
	reg := bench.NewRegistry()
	cw := &countingWorkload{}
	if err := reg.Register(cw.workload("w", map[string]any{"n": 100}, 1000)); err != nil {
		t.Fatal(err)
	}
	g := newGate(t, reg)
	opts := bench.RunOptions{Iterations: 5}
	ctx := context.Background()

	if _, err := g.Run(ctx, nil, opts); err != nil {
		t.Fatal(err)
	}
	// Re-register under a changed config (new registry, same gate cache).
	reg2 := bench.NewRegistry()
	if err := reg2.Register(cw.workload("w", map[string]any{"n": 200}, 1000)); err != nil {
		t.Fatal(err)
	}
	g.Registry = reg2
	g.Runner = bench.NewRunner(reg2)

	reps, err := g.Run(ctx, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	if reps[0].FromCache {
		t.Error("config change must invalidate the cache")
	}
	t.Logf("config change -> cache invalidated, fingerprint=%.12s", reps[0].Fingerprint)
}

func TestGateVerdictsEndToEnd(t *testing.T) {
	ctx := context.Background()
	opts := bench.RunOptions{Iterations: 20, Concurrency: 4}

	// speed controls the workload's reported duration; the baseline is
	// established at 1000ns, then the knob is turned per scenario.
	build := func(speed *atomic.Int64) (*Gate, *bench.Registry) {
		reg := bench.NewRegistry()
		w := bench.Workload{
			Name:   "svc",
			Config: map[string]any{"v": 1},
			Run:    func(ctx context.Context) (int64, error) { return speed.Load(), nil },
		}
		if err := reg.Register(w); err != nil {
			t.Fatal(err)
		}
		return newGate(t, reg), reg
	}

	run := func(g *Gate) Report {
		reps, err := g.Run(ctx, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(reps) != 1 {
			t.Fatalf("expected 1 report, got %d", len(reps))
		}
		logReport(t, reps[0])
		return reps[0]
	}

	t.Run("no_baseline_then_stable_then_regression", func(t *testing.T) {
		var speed atomic.Int64
		speed.Store(1000)
		g, _ := build(&speed)

		if rep := run(g); rep.Decision.Verdict != stats.VerdictNoBaseline {
			t.Fatalf("want no_baseline, got %s", rep.Decision.Verdict)
		}
		if _, err := g.UpdateBaseline(ctx, nil, opts, baseline.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		g.DisableCache = true // force fresh sampling for each scenario below

		speed.Store(1030) // +3%: normal fluctuation
		if rep := run(g); rep.Decision.Verdict != stats.VerdictStable {
			t.Errorf("fluctuation must be stable, got %s", rep.Decision.Verdict)
		}
		speed.Store(800) // improvement: must not fail
		if rep := run(g); rep.Decision.Verdict != stats.VerdictImproved {
			t.Errorf("want improved, got %s", rep.Decision.Verdict)
		}
		speed.Store(1250) // +25%: clear regression
		if rep := run(g); rep.Decision.Verdict != stats.VerdictRegression || !rep.Failed() {
			t.Errorf("want regression failure, got %s", rep.Decision.Verdict)
		}
	})

	t.Run("baseline_update_rejects_regression", func(t *testing.T) {
		var speed atomic.Int64
		speed.Store(1000)
		g, _ := build(&speed)
		if _, err := g.UpdateBaseline(ctx, nil, opts, baseline.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		speed.Store(1500)
		results, err := g.UpdateBaseline(ctx, nil, opts, baseline.UpdateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !results[0].Rejected {
			t.Errorf("baseline regression must be rejected: %+v", results[0])
		}
		t.Logf("baseline update rejected: %s", results[0].Reason)

		results, err = g.UpdateBaseline(ctx, nil, opts, baseline.UpdateOptions{AllowRegression: true})
		if err != nil {
			t.Fatal(err)
		}
		if !results[0].Updated {
			t.Errorf("explicit override must update: %+v", results[0])
		}
		t.Logf("baseline override: %s", results[0].Reason)
	})

	t.Run("concurrent_repeated_runs_reproducible", func(t *testing.T) {
		var speed atomic.Int64
		speed.Store(1000)
		g, _ := build(&speed)
		if _, err := g.UpdateBaseline(ctx, nil, opts, baseline.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		g.DisableCache = true
		// The same workload evaluated repeatedly under concurrency must
		// yield the same verdict every time.
		for i := 0; i < 5; i++ {
			rep := run(g)
			if rep.Decision.Verdict != stats.VerdictStable {
				t.Fatalf("iteration %d: verdict %s, want stable", i, rep.Decision.Verdict)
			}
		}
		t.Logf("5 repeated concurrent runs: all stable (reproducible)")
	})
}
