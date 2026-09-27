package bench

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FailureReason distinguishes why a sample (or a whole workload run) failed.
// Invalid samples never enter statistics; the reason always travels with
// the observation so conclusions stay attributable.
type FailureReason string

const (
	ReasonNone      FailureReason = ""
	ReasonPanic     FailureReason = "panic"
	ReasonTimeout   FailureReason = "timeout"
	ReasonError     FailureReason = "execution_error"
	ReasonInvalid   FailureReason = "invalid_sample"
	ReasonNotFound  FailureReason = "workload_not_found"
	ReasonNoSamples FailureReason = "no_valid_samples"
	ReasonBadConfig FailureReason = "bad_options"
	ReasonCanceled  FailureReason = "canceled"
)

// RunError attributes a whole-run failure to a specific reason.
type RunError struct {
	Reason FailureReason
	Err    string
}

func (e *RunError) Error() string {
	return fmt.Sprintf("bench: run failed (%s): %s", e.Reason, e.Err)
}

// Sample is one attributed observation of a workload. Every sample, valid
// or not, records its workload, global iteration and worker slot so that
// results from concurrent or repeated runs remain attributable.
type Sample struct {
	Workload  string
	Iter      int
	Worker    int
	StartedAt time.Time
	Duration  time.Duration
	OK        bool
	Reason    FailureReason
	Err       string
}

// RunOptions controls how a workload is sampled.
type RunOptions struct {
	// Iterations is the total number of measured samples across all workers.
	Iterations int
	// Concurrency is the number of parallel worker slots. Defaults to 1.
	Concurrency int
	// Timeout bounds a single sample. Zero means no bound.
	Timeout time.Duration
	// Warmup samples are executed sequentially and discarded before the
	// measured sampling begins.
	Warmup int
}

// RunResult bundles valid samples (statistics input) with every invalid
// sample (kept for auditing, never mixed into statistics).
type RunResult struct {
	Workload string
	Valid    []Sample
	Invalid  []Sample
}

// Durations returns the valid sample durations.
func (r RunResult) Durations() []time.Duration {
	out := make([]time.Duration, len(r.Valid))
	for i, s := range r.Valid {
		out[i] = s.Duration
	}
	return out
}

// Runner executes registered workloads.
type Runner struct {
	reg *Registry
}

// NewRunner creates a Runner over the given registry.
func NewRunner(reg *Registry) *Runner {
	return &Runner{reg: reg}
}

// Run samples the named workload. Samples are dispatched to Concurrency
// worker slots; each iteration index is assigned before dispatch, so every
// observation is attributable even under concurrent execution. Panics,
// timeouts, workload errors and invalid durations produce failed samples
// with distinct reasons and are excluded from Valid.
func (rn *Runner) Run(ctx context.Context, name string, opts RunOptions) (RunResult, error) {
	w, ok := rn.reg.Get(name)
	if !ok {
		return RunResult{Workload: name}, &RunError{Reason: ReasonNotFound, Err: name}
	}
	if opts.Iterations <= 0 {
		return RunResult{Workload: name}, &RunError{
			Reason: ReasonBadConfig,
			Err:    fmt.Sprintf("iterations must be > 0, got %d", opts.Iterations),
		}
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}

	// Warmup: sequential, discarded. Failures here fail fast rather than
	// contaminating the measured window.
	for i := 0; i < opts.Warmup; i++ {
		if err := ctx.Err(); err != nil {
			return RunResult{Workload: name}, &RunError{Reason: ReasonCanceled, Err: err.Error()}
		}
		if _, err := rn.sample(ctx, w, -1, -1, opts.Timeout); err != nil {
			return RunResult{Workload: name}, err
		}
	}

	result := RunResult{Workload: name, Valid: []Sample{}, Invalid: []Sample{}}
	var mu sync.Mutex
	var wg sync.WaitGroup

	jobs := make(chan int)
	for worker := 0; worker < opts.Concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iter := range jobs {
				if ctx.Err() != nil {
					mu.Lock()
					result.Invalid = append(result.Invalid, Sample{
						Workload: name, Iter: iter, Worker: worker,
						StartedAt: time.Now(), OK: false, Reason: ReasonCanceled,
						Err: ctx.Err().Error(),
					})
					mu.Unlock()
					return
				}
				s, _ := rn.sample(ctx, w, iter, worker, opts.Timeout)
				mu.Lock()
				if s.OK {
					result.Valid = append(result.Valid, s)
				} else {
					result.Invalid = append(result.Invalid, s)
				}
				mu.Unlock()
			}
		}(worker)
	}
	for iter := 0; iter < opts.Iterations; iter++ {
		jobs <- iter
	}
	close(jobs)
	wg.Wait()

	if len(result.Valid) == 0 {
		return result, &RunError{
			Reason: ReasonNoSamples,
			Err:    fmt.Sprintf("workload %q produced %d samples, none valid", name, opts.Iterations),
		}
	}
	return result, nil
}

// sample executes one workload invocation, translating every failure mode
// into an attributed, non-valid Sample.
func (rn *Runner) sample(parent context.Context, w Workload, iter, worker int, timeout time.Duration) (Sample, error) {
	s := Sample{Workload: w.Name, Iter: iter, Worker: worker, StartedAt: time.Now()}

	ctx := parent
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, timeout)
	}
	defer func() {
		if cancel != nil {
			cancel()
		}
	}()

	type outcome struct {
		ns  int64
		err error
	}
	ch := make(chan outcome, 1)

	go func() {
		var (
			ns  int64
			err error
		)
		defer func() {
			if rec := recover(); rec != nil {
				ch <- outcome{ns: 0, err: panicError{rec: rec}}
			}
		}()
		ns, err = w.Run(ctx)
		ch <- outcome{ns: ns, err: err}
	}()

	select {
	case o := <-ch:
		switch e := o.err.(type) {
		case nil:
			if o.ns <= 0 {
				s.OK = false
				s.Reason = ReasonInvalid
				s.Err = fmt.Sprintf("duration must be positive, got %d ns", o.ns)
				return s, nil
			}
			s.OK = true
			s.Duration = time.Duration(o.ns)
			return s, nil
		case panicError:
			s.OK = false
			s.Reason = ReasonPanic
			s.Err = fmt.Sprintf("panic: %v", e.rec)
			return s, nil
		default:
			s.OK = false
			switch {
			case parent.Err() != nil:
				s.Reason = ReasonCanceled
				s.Err = parent.Err().Error()
			case ctx.Err() == context.DeadlineExceeded:
				s.Reason = ReasonTimeout
				s.Err = fmt.Sprintf("sample exceeded %s: %s", timeout, o.err.Error())
			default:
				s.Reason = ReasonError
				s.Err = o.err.Error()
			}
			return s, nil
		}
	case <-ctx.Done():
		s.OK = false
		switch {
		case parent.Err() != nil:
			s.Reason = ReasonCanceled
			s.Err = parent.Err().Error()
		default:
			s.Reason = ReasonTimeout
			s.Err = fmt.Sprintf("sample exceeded %s", timeout)
		}
		return s, nil
	}
}

type panicError struct{ rec any }

func (e panicError) Error() string { return fmt.Sprintf("panic: %v", e.rec) }
