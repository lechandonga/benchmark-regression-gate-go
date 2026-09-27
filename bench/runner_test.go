package bench

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// sequenceWorkload returns durations from a fixed sequence, making sampled
// statistics fully reproducible regardless of machine speed.
func sequenceWorkload(name string, seq []int64) Workload {
	var idx atomic.Int64
	return Workload{
		Name: name,
		Run: func(ctx context.Context) (int64, error) {
			i := idx.Add(1) - 1
			return seq[int(i)%len(seq)], nil
		},
	}
}

func TestConcurrentSamplingIsAttributable(t *testing.T) {
	reg := NewRegistry()
	seq := []int64{1000, 1100, 900, 1050, 950, 1020, 980, 1010}
	if err := reg.Register(sequenceWorkload("attr", seq)); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(reg)

	res, err := r.Run(context.Background(), "attr", RunOptions{
		Iterations: 40, Concurrency: 8, Warmup: 2,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Invalid) != 0 {
		t.Fatalf("expected no invalid samples, got %v", res.Invalid)
	}
	if len(res.Valid) != 40 {
		t.Fatalf("expected 40 valid samples, got %d", len(res.Valid))
	}

	// Every iteration index must appear exactly once: samples stay
	// attributable even when 8 workers race.
	seen := map[int]int{}
	workers := map[int]bool{}
	for _, s := range res.Valid {
		seen[s.Iter]++
		workers[s.Worker] = true
		if s.Workload != "attr" {
			t.Errorf("sample attributed to wrong workload %q", s.Workload)
		}
		if s.Duration <= 0 {
			t.Errorf("iter %d: non-positive duration %s", s.Iter, s.Duration)
		}
	}
	for iter := 0; iter < 40; iter++ {
		if seen[iter] != 1 {
			t.Errorf("iter %d seen %d times, want exactly 1", iter, seen[iter])
		}
	}
	if len(workers) < 2 {
		t.Errorf("expected samples from multiple workers, got %v", workers)
	}

	// Repeated runs over the same sequence must yield identical statistics
	// input (reproducible conclusions).
	res2, err := r.Run(context.Background(), "attr", RunOptions{Iterations: 40, Concurrency: 8})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	sum := func(ss []Sample) int64 {
		var total int64
		for _, s := range ss {
			total += int64(s.Duration)
		}
		return total
	}
	if sum(res.Valid) != sum(res2.Valid) {
		t.Errorf("repeated runs diverged: %d vs %d", sum(res.Valid), sum(res2.Valid))
	}
	t.Logf("samples: n=%d workers=%d total=%d ns (reproducible across reruns)",
		len(res.Valid), len(workers), sum(res.Valid))
}

func TestFailureReasonsAreDistinct(t *testing.T) {
	reg := NewRegistry()
	must := func(w Workload) {
		if err := reg.Register(w); err != nil {
			t.Fatal(err)
		}
	}

	must(Workload{Name: "panics", Run: func(ctx context.Context) (int64, error) {
		panic("boom")
	}})
	must(Workload{Name: "sleeps", Run: func(ctx context.Context) (int64, error) {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
			return 1, nil
		}
	}})
	must(Workload{Name: "errs", Run: func(ctx context.Context) (int64, error) {
		return 0, errors.New("disk on fire")
	}})
	must(Workload{Name: "invalid", Run: func(ctx context.Context) (int64, error) {
		return -5, nil
	}})

	r := NewRunner(reg)
	cases := []struct {
		name   string
		opts   RunOptions
		reason FailureReason
	}{
		{"panics", RunOptions{Iterations: 3}, ReasonPanic},
		{"sleeps", RunOptions{Iterations: 3, Timeout: 50 * time.Millisecond}, ReasonTimeout},
		{"errs", RunOptions{Iterations: 3}, ReasonError},
		{"invalid", RunOptions{Iterations: 3}, ReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.Run(context.Background(), tc.name, tc.opts)
			var runErr *RunError
			if !errors.As(err, &runErr) || runErr.Reason != ReasonNoSamples {
				t.Fatalf("expected no_valid_samples run error, got %v", err)
			}
			if len(res.Valid) != 0 {
				t.Fatalf("invalid samples leaked into statistics: %v", res.Valid)
			}
			if len(res.Invalid) != tc.opts.Iterations {
				t.Fatalf("expected %d invalid samples, got %d", tc.opts.Iterations, len(res.Invalid))
			}
			for _, s := range res.Invalid {
				if s.Reason != tc.reason {
					t.Errorf("iter %d: reason=%q, want %q (err=%s)", s.Iter, s.Reason, tc.reason, s.Err)
				}
				t.Logf("invalid sample: workload=%s iter=%d reason=%s err=%q",
					s.Workload, s.Iter, s.Reason, s.Err)
			}
		})
	}
}

func TestMixedValidAndInvalidSamples(t *testing.T) {
	reg := NewRegistry()
	var n atomic.Int64
	err := reg.Register(Workload{Name: "flaky", Run: func(ctx context.Context) (int64, error) {
		i := n.Add(1)
		if i%4 == 0 {
			return 0, fmt.Errorf("transient failure %d", i)
		}
		return 1000, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(reg)
	res, err := r.Run(context.Background(), "flaky", RunOptions{Iterations: 16, Concurrency: 4})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Valid) != 12 || len(res.Invalid) != 4 {
		t.Fatalf("valid=%d invalid=%d, want 12/4", len(res.Valid), len(res.Invalid))
	}
	for _, s := range res.Valid {
		if s.Duration != 1000 {
			t.Errorf("invalid value mixed into valid samples: %v", s)
		}
	}
	for _, s := range res.Invalid {
		if s.Reason != ReasonError {
			t.Errorf("unexpected reason %q", s.Reason)
		}
	}
	t.Logf("mixed run: %d valid (all 1000ns), %d invalid (reason=%s)",
		len(res.Valid), len(res.Invalid), ReasonError)
}

func TestUnknownWorkloadAndBadOptions(t *testing.T) {
	r := NewRunner(NewRegistry())
	_, err := r.Run(context.Background(), "nope", RunOptions{Iterations: 1})
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Reason != ReasonNotFound {
		t.Errorf("want not_found, got %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(sequenceWorkload("w", []int64{1})); err != nil {
		t.Fatal(err)
	}
	r = NewRunner(reg)
	_, err = r.Run(context.Background(), "w", RunOptions{Iterations: 0})
	if !errors.As(err, &runErr) || runErr.Reason != ReasonBadConfig {
		t.Errorf("want bad_options, got %v", err)
	}
	t.Logf("distinct run-level reasons: %s, %s", ReasonNotFound, ReasonBadConfig)
}
