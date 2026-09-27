package bench

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RunConfig 控制采样行为。
type RunConfig struct {
	// Runs 为重复运行次数（每次运行独立收集 Iterations 个样本）。
	Runs int
	// Iterations 为每次运行中的迭代采样数（不含预热）。
	Iterations int
	// Concurrency 控制同时执行的工作负载数量（并发运行）。
	Concurrency int
	// IterationTimeout 为单次迭代超时；0 表示不额外限时。
	IterationTimeout time.Duration
	// Warmup 为每次运行开始前丢弃的预热迭代数。
	Warmup int
}

func (c RunConfig) normalized() RunConfig {
	if c.Runs < 1 {
		c.Runs = 1
	}
	if c.Iterations < 1 {
		c.Iterations = 1
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.Warmup < 0 {
		c.Warmup = 0
	}
	return c
}

// RunResult 是单个工作负载在一次重复运行中的采样结果。
type RunResult struct {
	WorkloadID string       `json:"workload_id"`
	RunID      int          `json:"run_id"`
	Samples    []Sample     `json:"samples"`
	Failures   []RunFailure `json:"failures"`
}

// ValidSamples 返回本次运行中的有效样本（无效样本从不写入 Samples）。
func (rr *RunResult) ValidSamples() []Sample {
	return rr.Samples
}

// RunReport 是一个工作负载的完整采样报告。
type RunReport struct {
	WorkloadID string       `json:"workload_id"`
	Runs       []RunResult  `json:"runs"`
	Failures   []RunFailure `json:"failures"`
	Stats      Stats        `json:"stats"`
	// Aborted 表示该工作负载因致命原因（如未注册）未能执行。
	Aborted bool `json:"aborted"`
}

// AllSamples 汇总该工作负载所有运行的有效样本。
func (rp *RunReport) AllSamples() []Sample {
	var out []Sample
	for i := range rp.Runs {
		out = append(out, rp.Runs[i].Samples...)
	}
	return out
}

// FailureOfKind 统计指定类型的失败数量。
func (rp *RunReport) FailureOfKind(kind FailureKind) int {
	n := 0
	for _, f := range rp.Failures {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// Runner 负责对工作负载进行并发、可重复、可归因的采样。
type Runner struct {
	registry *Registry
	cfg      RunConfig
	now      func() time.Time
}

// NewRunner 创建采样器。
func NewRunner(reg *Registry, cfg RunConfig) *Runner {
	return &Runner{
		registry: reg,
		cfg:      cfg.normalized(),
		now:      time.Now,
	}
}

// runOneIteration 执行单次迭代，panic、超时、无效值与工作负载错误
// 分别归为可区分的失败原因，且都不会产生样本。
// 返回 (sample, ok, failure)；ok=false 时 failure 非空。
func (rn *Runner) runOneIteration(ctx context.Context, w Workload, runID, iteration int) (Sample, bool, RunFailure) {
	started := rn.now()
	res := make(chan iterResult, 1)

	iterCtx := ctx
	var cancel context.CancelFunc
	if rn.cfg.IterationTimeout > 0 {
		iterCtx, cancel = context.WithTimeout(ctx, rn.cfg.IterationTimeout)
	} else {
		cancel = func() {}
	}
	defer cancel()

	go func() {
		res <- executeIter(w, iterCtx)
	}()

	select {
	case r := <-res:
		ended := rn.now()
		if r.panicMsg != "" {
			return Sample{}, false, RunFailure{
				WorkloadID: w.ID, RunID: runID, Iteration: iteration,
				Kind: FailurePanic, Message: r.panicMsg,
				StartedAt: started, EndedAt: ended,
			}
		}
		if r.err != nil {
			kind := FailureWorkloadError
			return Sample{}, false, RunFailure{
				WorkloadID: w.ID, RunID: runID, Iteration: iteration,
				Kind: kind, Message: r.err.Error(),
				StartedAt: started, EndedAt: ended,
			}
		}
		if r.nanos <= 0 {
			return Sample{}, false, RunFailure{
				WorkloadID: w.ID, RunID: runID, Iteration: iteration,
				Kind:      FailureInvalid,
				Message:   fmt.Sprintf("non-positive duration: %d ns", r.nanos),
				StartedAt: started, EndedAt: ended,
			}
		}
		return Sample{
			WorkloadID: w.ID,
			RunID:      runID,
			Iteration:  iteration,
			Nanos:      r.nanos,
			Bytes:      r.bytes,
			Ops:        r.ops,
			RecordedAt: ended,
		}, true, RunFailure{}
	case <-iterCtx.Done():
		// 仅在确实配置了超时时归类为超时；否则属于父上下文取消。
		kind := FailureTimeout
		if rn.cfg.IterationTimeout <= 0 {
			kind = FailureWorkloadError
		}
		return Sample{}, false, RunFailure{
			WorkloadID: w.ID, RunID: runID, Iteration: iteration,
			Kind: kind, Message: contextErr(iterCtx, rn.cfg.IterationTimeout),
			StartedAt: started, EndedAt: rn.now(),
		}
	}
}

func contextErr(ctx context.Context, timeout time.Duration) string {
	if timeout > 0 && ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("iteration timed out after %s", timeout)
	}
	return "iteration canceled: " + ctx.Err().Error()
}

type iterResult struct {
	nanos    int64
	bytes    int64
	ops      int64
	err      error
	panicMsg string
}

func executeIter(w Workload, ctx context.Context) (r iterResult) {
	defer func() {
		if rec := recover(); rec != nil {
			r = iterResult{panicMsg: fmt.Sprintf("%v", rec)}
		}
	}()
	r.nanos, r.bytes, r.ops, r.err = w.Run(ctx)
	return r
}

// runOneWorkload 顺序完成一个工作负载的全部重复运行（每个样本可归因到 run/iteration）。
func (rn *Runner) runOneWorkload(ctx context.Context, id string) *RunReport {
	report := &RunReport{WorkloadID: id}
	w, ok := rn.registry.Get(id)
	if !ok {
		report.Aborted = true
		report.Failures = append(report.Failures, RunFailure{
			WorkloadID: id, Kind: FailureMissing,
			Message:   fmt.Sprintf("workload %q is not registered", id),
			StartedAt: rn.now(), EndedAt: rn.now(),
		})
		return report
	}

	allSamples := make([]Sample, 0, rn.cfg.Runs*rn.cfg.Iterations)
	for runID := 0; runID < rn.cfg.Runs; runID++ {
		rr := RunResult{WorkloadID: id, RunID: runID}
		for seq := 0; seq < rn.cfg.Warmup+rn.cfg.Iterations; seq++ {
			if err := ctx.Err(); err != nil {
				rr.Failures = append(rr.Failures, RunFailure{
					WorkloadID: id, RunID: runID, Iteration: seq,
					Kind: FailureWorkloadError, Message: "run canceled: " + err.Error(),
					StartedAt: rn.now(), EndedAt: rn.now(),
				})
				break
			}
			sample, valid, failure := rn.runOneIteration(ctx, w, runID, seq)
			if !valid {
				rr.Failures = append(rr.Failures, failure)
				continue
			}
			// 预热样本直接丢弃，不计入任何统计。
			if seq < rn.cfg.Warmup {
				continue
			}
			rr.Samples = append(rr.Samples, sample)
			allSamples = append(allSamples, sample)
		}
		report.Runs = append(report.Runs, rr)
		report.Failures = append(report.Failures, rr.Failures...)
	}
	report.Stats = ComputeStats(allSamples)
	return report
}

// RunOne 对单个工作负载执行配置中的重复运行。
func (rn *Runner) RunOne(ctx context.Context, id string) (*RunReport, error) {
	return rn.runOneWorkload(ctx, id), nil
}

// RunMany 并发执行多个工作负载，返回按 ID 索引的报告。
// 同一工作负载内部顺序采样（保证计时不被自身并行扭曲），
// 不同工作负载之间受 Concurrency 信号量约束；
// 每个样本都带 WorkloadID/RunID/Iteration，结果在并发下仍可归因。
func (rn *Runner) RunMany(ctx context.Context, ids []string) (map[string]*RunReport, error) {
	reports := make(map[string]*RunReport, len(ids))
	var mu sync.Mutex
	sem := make(chan struct{}, rn.cfg.Concurrency)
	var wg sync.WaitGroup

	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			rp := rn.runOneWorkload(ctx, id)
			mu.Lock()
			reports[id] = rp
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return reports, ctx.Err()
}
