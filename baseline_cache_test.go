package bench

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func newBaseline(id string, median float64, cfg, deps, env string) Baseline {
	return Baseline{
		WorkloadID: id,
		Stats:      Stats{N: 20, Median: median, MAD: 5},
		ConfigHash: cfg, DepsHash: deps, EnvFingerprint: env,
	}
}

func TestBaselineEstablishAndRejectRegression(t *testing.T) {
	ctx := context.Background()
	store, _ := NewFileBaselineStore(t.TempDir())
	bm := NewBaselineManager(store)
	const cfg, deps, env = "cfg1", "deps1", "env1"

	// 1) 首次建立
	if err := bm.Establish(ctx, newBaseline("w", 1000, cfg, deps, env)); err != nil {
		t.Fatal(err)
	}
	t.Log("establish: baseline 1000ns created")

	// 重复建立必须报错
	err := bm.Establish(ctx, newBaseline("w", 1000, cfg, deps, env))
	var be *BaselineError
	if !errors.As(err, &be) || be.Kind != BaselineErrAlreadyExists {
		t.Fatalf("re-establish should fail with already_exists, got %v", err)
	}
	t.Logf("re-establish rejected: %v", err)

	// 2) 正常波动 -> 允许更新
	if err := bm.Update(ctx, newBaseline("w", 1010, cfg, deps, env), VerdictStable, false, ""); err != nil {
		t.Fatalf("stable update failed: %v", err)
	}
	t.Log("update(1010ns, stable): accepted")

	// 3) 改善 -> 允许更新
	if err := bm.Update(ctx, newBaseline("w", 850, cfg, deps, env), VerdictImprovement, false, ""); err != nil {
		t.Fatalf("improvement update failed: %v", err)
	}
	t.Log("update(850ns, improvement): accepted, baseline now 850ns")

	// 4) 回退且不强制 -> 必须明确拒绝，旧基线保留
	err = bm.Update(ctx, newBaseline("w", 1200, cfg, deps, env), VerdictRegression, false, "")
	if !errors.As(err, &be) || be.Kind != BaselineErrRegressionRejected {
		t.Fatalf("regression must be rejected, got %v", err)
	}
	got, _ := bm.Load(ctx, "w")
	if got.Stats.Median != 850 {
		t.Fatalf("old baseline must be retained after rejection, got median=%v", got.Stats.Median)
	}
	t.Logf("update(1200ns, regression) REJECTED, old median=%.0f retained: %v", got.Stats.Median, be.Message)

	// 5) 回退但强制且无理由 -> 拒绝
	err = bm.Update(ctx, newBaseline("w", 1200, cfg, deps, env), VerdictRegression, true, "  ")
	if !errors.As(err, &be) || be.Kind != BaselineErrForceReasonRequired {
		t.Fatalf("force without reason must fail, got %v", err)
	}
	t.Logf("force-update without reason rejected: %v", be.Message)

	// 6) 回退 + 强制 + 理由 -> 接受且理由写入 Note 留痕
	if err := bm.Update(ctx, newBaseline("w", 1200, cfg, deps, env),
		VerdictRegression, true, "new data structure intentionally trades latency"); err != nil {
		t.Fatalf("forced update with reason failed: %v", err)
	}
	got, _ = bm.Load(ctx, "w")
	if got.Stats.Median != 1200 {
		t.Fatalf("forced baseline median=%v want 1200", got.Stats.Median)
	}
	t.Logf("force-update accepted: median=%.0f note=%q created_at_preserved=%v",
		got.Stats.Median, got.Note, !got.CreatedAt.IsZero())
}

func TestBaselineFingerprintMismatch(t *testing.T) {
	ctx := context.Background()
	bm := NewBaselineManager(mustStore(t))
	if err := bm.Establish(ctx, newBaseline("w", 1000, "cfg1", "deps1", "env1")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		cfg, deps, env string
	}{
		{"config changed", "cfg2", "deps1", "env1"},
		{"deps changed", "cfg1", "deps2", "env1"},
		{"env changed", "cfg1", "deps1", "env2"},
	}
	for _, c := range cases {
		err := bm.Update(ctx, newBaseline("w", 1000, c.cfg, c.deps, c.env), VerdictStable, false, "")
		var be *BaselineError
		if !errors.As(err, &be) || be.Kind != BaselineErrFingerprintMismatch {
			t.Fatalf("%s: expected fingerprint_mismatch, got %v", c.name, err)
		}
		t.Logf("%s -> update rejected: %s", c.name, be.Message)
	}
}

func mustStore(t *testing.T) BaselineStore {
	t.Helper()
	s, err := NewFileBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCacheHitsAndInvalidation(t *testing.T) {
	ctx := context.Background()
	cache, _ := NewFileResultCache(t.TempDir())

	base := CacheKey{WorkloadID: "w", SourceHash: "src1",
		ConfigHash: HashConfig(RunConfig{Runs: 1, Iterations: 5}),
		DepsHash:   HashDeps([]string{"dep@1"}), EnvFingerprint: EnvFingerprint()}
	entry := CacheEntry{Key: base, Report: &RunReport{WorkloadID: "w", Stats: Stats{N: 5, Median: 1000}}}

	if _, hit, _ := cache.Get(ctx, base); hit {
		t.Fatal("cache should be empty initially")
	}
	if err := cache.Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if got, hit, _ := cache.Get(ctx, base); !hit || got.Report.Stats.Median != 1000 {
		t.Fatal("cache miss after put")
	}
	t.Logf("identical key -> hit: %s", base.Hash())

	// 任一维度变化都必须未命中（配置/源码/依赖/环境）
	mutated := []struct {
		name string
		key  CacheKey
	}{
		{"source changed", func() CacheKey { k := base; k.SourceHash = "src2"; return k }()},
		{"config changed", func() CacheKey {
			k := base
			k.ConfigHash = HashConfig(RunConfig{Runs: 2, Iterations: 5})
			return k
		}()},
		{"deps changed", func() CacheKey { k := base; k.DepsHash = HashDeps([]string{"dep@2"}); return k }()},
		{"env changed", func() CacheKey { k := base; k.EnvFingerprint = "different-env"; return k }()},
		// 依赖声明顺序不应影响命中
	}
	for _, m := range mutated {
		if _, hit, _ := cache.Get(ctx, m.key); hit {
			t.Fatalf("%s: expected cache miss", m.name)
		}
		t.Logf("%s -> miss (recompute required)", m.name)
	}

	// 依赖顺序变化但内容相同 => 键不变（仍命中）
	reordered := CacheKey{WorkloadID: "w", SourceHash: "src1",
		ConfigHash: base.ConfigHash,
		DepsHash:   HashDeps([]string{"a@1", "dep@1"}), EnvFingerprint: base.EnvFingerprint}
	other := CacheKey{WorkloadID: "w", SourceHash: "src1",
		ConfigHash: base.ConfigHash,
		DepsHash:   HashDeps([]string{"dep@1", "a@1"}), EnvFingerprint: base.EnvFingerprint}
	if reordered.DepsHash != other.DepsHash {
		t.Fatal("deps hash must be order-independent")
	}

	// Invalidate 清除该工作负载全部条目
	if err := cache.Invalidate(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := cache.Get(ctx, base); hit {
		t.Fatal("cache should be empty after invalidation")
	}
	t.Log("invalidate(w) cleared all entries for workload")
}

func TestCacheKeyStable(t *testing.T) {
	k := CacheKey{WorkloadID: "w", SourceHash: "s", ConfigHash: "c", DepsHash: "d", EnvFingerprint: "e"}
	if k.Hash() != k.Hash() {
		t.Fatal("cache key hash not stable")
	}
	// 文件名安全：不允许路径分隔等字符
	name := fmt.Sprintf("%s-%s.json", k.WorkloadID, k.Hash())
	if name != safeFileName(k.WorkloadID)+"-"+k.Hash()+".json" {
		t.Fatal("unsafe cache filename")
	}
}

func TestEnvFingerprint(t *testing.T) {
	f1, f2 := EnvFingerprint(), EnvFingerprint()
	if f1 != f2 {
		t.Fatalf("env fingerprint unstable: %s vs %s", f1, f2)
	}
	t.Logf("env fingerprint: %s", f1)
}

// 确保临时目录被清理（示例直接使用 os 接口的边界检查）。
func TestFileStoreMissingBaseline(t *testing.T) {
	store, _ := NewFileBaselineStore(t.TempDir())
	_, err := store.Load(context.Background(), "nope")
	var nf *ErrBaselineNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("expected ErrBaselineNotFound, got %v", err)
	}
	t.Logf("missing baseline -> %v", err)
}
