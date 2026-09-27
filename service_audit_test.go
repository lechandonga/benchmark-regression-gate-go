package bench

import (
	"context"
	"fmt"
	"os"
	"testing"
)

// noisyWorkload 返回围绕 base、抖动 ±jitterPct 的纳秒耗时。
func noisyWorkload(id, hash string, base int64, jitterPct float64) Workload {
	// 确定性伪噪声：用迭代序号驱动，避免引入全局随机导致结论不可复现。
	i := int64(0)
	return Workload{
		ID: id, SourceHash: hash, Deps: []string{"dep@1"},
		Run: func(context.Context) (int64, int64, int64, error) {
			i++
			phase := float64((i*37)%11-5) / 5 // [-1,1] 确定性序列
			return base + int64(float64(base)*jitterPct*phase/100), 0, 0, nil
		},
	}
}

type harness struct {
	dir   string
	reg   *Registry
	bm    *BaselineManager
	cache ResultCache
	audit *JSONLAuditLog
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := NewFileBaselineStore(dir + "/baselines")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewFileResultCache(dir + "/cache")
	if err != nil {
		t.Fatal(err)
	}
	audit, err := NewJSONLAuditLog(dir + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		dir: dir, reg: NewRegistry(), bm: NewBaselineManager(store),
		cache: cache, audit: audit,
	}
}

func (h *harness) service(update bool, extra func(*ServiceConfig)) *Service {
	cfg := ServiceConfig{
		Run:             RunConfig{Runs: 3, Iterations: 8, Warmup: 1, Concurrency: 2},
		Gate:            GateConfig{RegressionThreshold: 0.10, ImprovementThreshold: 0.10, MinZScore: 2.0},
		UseCache:        true,
		UpdateBaselines: update,
	}
	if extra != nil {
		extra(&cfg)
	}
	return NewService(h.reg, h.bm, h.cache, h.audit, cfg)
}

func logDecision(t *testing.T, tag string, rp *ServiceReport) {
	for _, id := range sortedKeys(rp.Decisions) {
		d := rp.Decisions[id]
		rep := rp.Reports[id]
		t.Logf("[%s] workload=%s verdict=%-11s ratio=%.4f z=%6.2f samples=%d failures=%d",
			tag, id, d.Verdict, d.Ratio, d.ZScore, rep.Stats.N, len(rep.Failures))
		for _, r := range d.Reasons {
			t.Logf("           basis: %s", r)
		}
	}
}

func sortedKeys(m map[string]Decision) []string {
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	return ids
}

// TestServiceNoiseIsNotRegression 是核心场景：同一份工作负载反复跑，
// 噪声波动绝不能被误报为回退，结论在重复运行下稳定可复现。
func TestServiceNoiseIsNotRegression(t *testing.T) {
	h := newHarness(t)
	// ±4% 的确定性抖动，基线/当前相同分布
	h.reg.Register(noisyWorkload("latency", "h1", 1_000_000, 4))

	svc := h.service(true, nil)
	first, err := svc.Evaluate(context.Background(), []string{"latency"})
	if err != nil {
		t.Fatal(err)
	}
	logDecision(t, "run1 establish", first)
	if first.Decisions["latency"].Verdict != VerdictNoBaseline {
		t.Fatal("first run must be no_baseline")
	}

	// 接下来 5 次重复评估必须全部 stable（噪声 <=4% < 10% 阈值，且 z 不显著）。
	for i := 2; i <= 6; i++ {
		rep, err := svc.Evaluate(context.Background(), []string{"latency"})
		if err != nil {
			t.Fatal(err)
		}
		logDecision(t, fmt.Sprintf("run%d repeat", i), rep)
		if v := rep.Decisions["latency"].Verdict; v != VerdictStable {
			t.Fatalf("run %d: noise must be stable, got %s (reasons=%v)", i, v, rep.Decisions["latency"].Reasons)
		}
		if failed := rep.Failed(); len(failed) != 0 {
			t.Fatalf("run %d: false-positive regression: %v", i, failed)
		}
	}

	recs, _ := h.audit.ReadAll(context.Background())
	if len(recs) != 6 {
		t.Fatalf("audit records=%d want 6", len(recs))
	}
	if recs[0].BaselineOp != "establish" {
		t.Fatalf("first audit op=%q want establish", recs[0].BaselineOp)
	}
	t.Logf("audit: %d records; ops run1=%q run2=%q ... all repeatable, no false positives",
		len(recs), recs[0].BaselineOp, recs[1].BaselineOp)
}

// TestServiceClearRegressionDetectedAndRejected 验证真实回退被检出，
// 且基线更新被拒绝；换一台“修复后”的工作负载则恢复 stable。
func TestServiceClearRegressionDetectedAndRejected(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(noisyWorkload("algo", "v1", 1_000_000, 1))

	svc := h.service(true, nil)
	r1, _ := svc.Evaluate(context.Background(), []string{"algo"}) // establish
	logDecision(t, "v1 establish", r1)

	before, _ := h.bm.Load(context.Background(), "algo")

	// 回退版本：变慢 25%（源码 hash 同时变化以模拟新版本）。
	// 注意：缓存键含 SourceHash，新版本不会命中旧缓存；
	// 但基线指纹要求 SourceHash 不参与基线匹配（基线存的是 cfg/deps/env），
	// 因此新旧版本可以比较。
	h.reg = NewRegistry()
	h.reg.Register(noisyWorkload("algo", "v2", 1_250_000, 1))
	svc = h.service(true, nil)
	svc.registry = h.reg
	svc.runner = NewRunner(h.reg, svc.cfg.Run)

	r2, _ := svc.Evaluate(context.Background(), []string{"algo"})
	logDecision(t, "v2 regressed", r2)
	d := r2.Decisions["algo"]
	if d.Verdict != VerdictRegression {
		t.Fatalf("25%% slowdown must be regression, got %s", d.Verdict)
	}
	if failed := r2.Failed(); len(failed) != 1 || failed[0] != "algo" {
		t.Fatalf("Failed()=%v want [algo]", failed)
	}
	after, _ := h.bm.Load(context.Background(), "algo")
	if after.UpdatedAt != before.UpdatedAt {
		t.Fatal("baseline must NOT be silently overwritten on regression")
	}
	recs, _ := h.audit.ReadAll(context.Background())
	last := recs[len(recs)-1]
	if last.BaselineOp != "rejected" {
		t.Fatalf("audit op=%q want rejected; note=%q", last.BaselineOp, last.Note)
	}
	t.Logf("baseline protected: median stays %.0f, audit note=%q", after.Stats.Median, last.Note)

	// 恢复版本 -> improvement 或 stable，但绝不再是 regression；基线允许刷新
	h.reg = NewRegistry()
	h.reg.Register(noisyWorkload("algo", "v3", 990_000, 1))
	svc.registry = h.reg
	svc.runner = NewRunner(h.reg, svc.cfg.Run)
	r3, _ := svc.Evaluate(context.Background(), []string{"algo"})
	logDecision(t, "v3 recovered", r3)
	if r3.Decisions["algo"].Verdict == VerdictRegression {
		t.Fatal("recovered workload must not be regression")
	}
}

// TestServiceIncrementalRerunUsesCache 未变化时第二次评估必须命中缓存，
// 不重复计算；配置变化后必须失效重算。
func TestServiceIncrementalRerunUsesCache(t *testing.T) {
	h := newHarness(t)
	var calls int64
	h.reg.Register(Workload{ID: "c", SourceHash: "h1", Deps: []string{"d@1"},
		Run: func(context.Context) (int64, int64, int64, error) {
			calls++
			return 5000, 0, 0, nil
		}})

	svc := h.service(false, nil)
	r1, _ := svc.Evaluate(context.Background(), []string{"c"})
	firstCalls := calls
	r2, _ := svc.Evaluate(context.Background(), []string{"c"})
	if r2.RunSeq != 2 {
		t.Fatalf("run_seq=%d want 2", r2.RunSeq)
	}
	if calls != firstCalls {
		t.Fatalf("unchanged workload recomputed: calls %d -> %d", firstCalls, calls)
	}
	recs, _ := h.audit.ReadAll(context.Background())
	if !recs[len(recs)-1].CacheHit {
		t.Fatal("second evaluation should be recorded as cache_hit=true")
	}
	t.Logf("incremental rerun: calls stayed at %d (cache_hit=true), verdict run1=%s run2=%s",
		calls, r1.Decisions["c"].Verdict, r2.Decisions["c"].Verdict)

	// 配置变化（迭代数变了）-> 键变化 -> 必须重算
	h2 := newHarness(t)
	var calls2 int64
	h2.reg.Register(Workload{ID: "c", SourceHash: "h1",
		Run: func(context.Context) (int64, int64, int64, error) {
			calls2++
			return 5000, 0, 0, nil
		}})
	sv1 := NewService(h2.reg, h2.bm, h2.cache, h2.audit, ServiceConfig{
		Run: RunConfig{Runs: 1, Iterations: 3}, Gate: DefaultGateConfig(), UseCache: true})
	sv1.Evaluate(context.Background(), nil)
	n1 := calls2
	sv2 := NewService(h2.reg, h2.bm, h2.cache, h2.audit, ServiceConfig{
		Run: RunConfig{Runs: 1, Iterations: 6}, Gate: DefaultGateConfig(), UseCache: true})
	sv2.Evaluate(context.Background(), nil)
	if calls2 != n1+6 {
		t.Fatalf("config change must invalidate cache: calls %d -> %d (want %d)", n1, calls2, n1+6)
	}
	t.Logf("config change (iterations 3->6) forced recompute: calls %d -> %d", n1, calls2)
}

// TestServiceFailedWorkloadExcluded 工作负载在运行中失败：
// 失败原因可区分，样本不混入统计，整体判定 invalid_run 且门禁失败。
func TestServiceFailedWorkloadExcluded(t *testing.T) {
	h := newHarness(t)
	h.reg.Register(Workload{ID: "ok", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		return 1000, 0, 0, nil
	}})
	h.reg.Register(Workload{ID: "boom", SourceHash: "h", Run: func(context.Context) (int64, int64, int64, error) {
		panic("npe")
	}})
	svc := h.service(false, nil)
	rep, _ := svc.Evaluate(context.Background(), []string{"ok", "boom"})
	logDecision(t, "mixed", rep)
	if rep.Decisions["ok"].Verdict == VerdictInvalidRun {
		t.Fatal("healthy workload must not be invalid")
	}
	if rep.Decisions["boom"].Verdict != VerdictInvalidRun {
		t.Fatalf("panicking workload verdict=%s want invalid_run", rep.Decisions["boom"].Verdict)
	}
	if rep.Reports["boom"].FailureOfKind(FailurePanic) == 0 {
		t.Fatal("panic failure must be recorded")
	}
	if rep.Reports["boom"].Stats.N != 0 {
		t.Fatal("panic iterations must not contribute samples")
	}
	if failed := rep.Failed(); len(failed) != 1 || failed[0] != "boom" {
		t.Fatalf("Failed()=%v want [boom]", failed)
	}
	// 审计中失败原因可查
	recs, _ := h.audit.ReadAll(context.Background())
	for _, r := range recs {
		if r.WorkloadID == "boom" && (len(r.Failures) == 0 || r.Failures[0].Kind != FailurePanic) {
			t.Fatal("audit record must carry distinguishable failure")
		}
	}
	t.Log("panic workload excluded from stats; audit carries kind=panic; gate fails only on 'boom'")
}

// TestAuditLogRoundTrip 审计记录可完整回放、支持复现。
func TestAuditLogRoundTrip(t *testing.T) {
	path := t.TempDir() + "/sub/dir/audit.jsonl"
	log, err := NewJSONLAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := AuditRecord{
		WorkloadID: "w", Verdict: VerdictRegression, Ratio: 1.25, ZScore: 9.1,
		Current:     Stats{N: 20, Median: 1250},
		Key:         CacheKey{WorkloadID: "w", SourceHash: "v2"},
		Reasons:     []string{"slowdown 25%"},
		SampleCount: 20, BaselineOp: "rejected",
	}
	if err := log.Append(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := log.ReadAll(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("readback failed: %v %d", err, len(got))
	}
	if got[0].Verdict != VerdictRegression || got[0].Ratio != 1.25 ||
		got[0].Key.SourceHash != "v2" || got[0].BaselineOp != "rejected" {
		t.Fatalf("audit roundtrip mismatch: %+v", got[0])
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatal("audit file missing or empty")
	}
	t.Logf("audit roundtrip ok: %+v (one JSON line, append-only)", got[0])
}
