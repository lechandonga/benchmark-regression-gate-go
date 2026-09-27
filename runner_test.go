package bench

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerSamplesAttributable(t *testing.T) {
	reg := NewRegistry()
	const nW = 4
	for i := 0; i < nW; i++ {
		id := fmt.Sprintf("w%d", i)
		if err := reg.Register(Workload{ID: id, SourceHash: "h",
			Run: func(context.Context) (int64, int64, int64, error) {
				return 1000, 0, 0, nil
			}}); err != nil {
			t.Fatal(err)
		}
	}
	rn := NewRunner(reg, RunConfig{Runs: 3, Iterations: 5, Concurrency: 4})
	reps, err := rn.RunMany(context.Background(), []string{"w0", "w1", "w2", "w3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != nW {
		t.Fatalf("got %d reports, want %d", len(reps), nW)
	}
	for id, rp := range reps {
		if rp.Aborted || len(rp.Failures) != 0 {
			t.Fatalf("%s aborted=%v failures=%v", id, rp.Aborted, rp.Failures)
		}
		if rp.Stats.N != 15 {
			t.Fatalf("%s samples=%d want 15", id, rp.Stats.N)
		}
		// 每个样本都必须可归因到正确的工作负载/运行/迭代。
		for _, rr := range rp.Runs {
			if rr.WorkloadID != id {
				t.Fatalf("attribution error: run tagged %s in report %s", rr.WorkloadID, id)
			}
			seen := map[int]bool{}
			for _, s := range rr.Samples {
				if s.WorkloadID != id {
					t.Fatalf("sample workload id mismatch: %s in %s", s.WorkloadID, id)
				}
				if s.RunID != rr.RunID {
					t.Fatalf("sample run id mismatch: %d in run %d", s.RunID, rr.RunID)
				}
				if seen[s.Iteration] {
					t.Fatalf("duplicate iteration %d", s.Iteration)
				}
				seen[s.Iteration] = true
			}
		}
		t.Logf("workload=%s total_samples=%d median=%.0f (runs=%d iterations=%d, concurrency=4)",
			id, rp.Stats.N, rp.Stats.Median, len(rp.Runs), 5)
	}
}

func TestRunnerFailureKinds(t *testing.T) {
	reg := NewRegistry()

	reg.Register(Workload{ID: "panics", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		panic("boom")
	}})
	reg.Register(Workload{ID: "slow", SourceHash: "h", Run: func(ctx context.Context) (int64, int64, int64, error) {
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
		}
		return 50_000_000, 0, 0, nil
	}})
	reg.Register(Workload{ID: "bad", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		return 0, 0, 0, nil // 非正耗时 => 无效样本
	}})
	reg.Register(Workload{ID: "errw", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		return 0, 0, 0, errors.New("disk full")
	}})

	rn := NewRunner(reg, RunConfig{
		Runs: 1, Iterations: 3, Concurrency: 4,
		IterationTimeout: 5 * time.Millisecond,
	})
	reps, err := rn.RunMany(context.Background(), []string{"panics", "slow", "bad", "errw"})
	if err != nil {
		t.Fatal(err)
	}

	expect := map[string]FailureKind{
		"panics": FailurePanic,
		"slow":   FailureTimeout,
		"bad":    FailureInvalid,
		"errw":   FailureWorkloadError,
	}
	for id, kind := range expect {
		rp := reps[id]
		if n := rp.FailureOfKind(kind); n != 3 {
			t.Fatalf("%s: expected 3 %s failures, got %d (all=%v)", id, kind, n, rp.Failures)
		}
		if len(rp.AllSamples()) != 0 {
			t.Fatalf("%s: invalid/failed iterations must not enter samples, got %d", id, len(rp.AllSamples()))
		}
		if rp.Stats.N != 0 {
			t.Fatalf("%s: stats.N must be 0, got %d", id, rp.Stats.N)
		}
		f := rp.Failures[0]
		t.Logf("workload=%-7s kind=%-15s message=%q sample_excluded=true stats_n=%d",
			id, f.Kind, f.Message, rp.Stats.N)
	}

	// 未注册工作负载：aborted + missing 原因
	missing := rn.runOneWorkload(context.Background(), "ghost")
	if !missing.Aborted || missing.FailureOfKind(FailureMissing) != 1 {
		t.Fatalf("missing workload: aborted=%v failures=%v", missing.Aborted, missing.Failures)
	}
	t.Logf("workload=ghost   kind=%-15s message=%q aborted=true", FailureMissing, missing.Failures[0].Message)
}

func TestRunnerWarmupAndRepeatability(t *testing.T) {
	var calls int64
	reg := NewRegistry()
	reg.Register(Workload{ID: "w", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		atomic.AddInt64(&calls, 1)
		return 1000, 0, 0, nil
	}})
	rn := NewRunner(reg, RunConfig{Runs: 2, Iterations: 3, Warmup: 2})
	rp, _ := rn.RunOne(context.Background(), "w")
	if calls != int64(2*(3+2)) {
		t.Fatalf("workload called %d times, want %d", calls, 2*(3+2))
	}
	if rp.Stats.N != 6 {
		t.Fatalf("warmup samples must be discarded: N=%d want 6", rp.Stats.N)
	}
	// 重复两次独立运行，结论样本数一致（可复现）。
	rp2, _ := rn.RunOne(context.Background(), "w")
	if rp2.Stats.N != rp.Stats.N || rp2.Stats.Median != rp.Stats.Median {
		t.Fatalf("repeated runs not reproducible: %+v vs %+v", rp.Stats, rp2.Stats)
	}
	t.Logf("warmup=2 runs=2 iterations=3 -> executed=%d recorded=%d median=%.0f, repeat run identical",
		calls, rp.Stats.N, rp.Stats.Median)
}
