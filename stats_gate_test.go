package bench

import (
	"testing"
)

// makeSamples 按给定纳秒值构造可归因样本。
func makeSamples(id string, runID int, nanos ...int64) []Sample {
	out := make([]Sample, len(nanos))
	for i, n := range nanos {
		out[i] = Sample{WorkloadID: id, RunID: runID, Iteration: i, Nanos: n}
	}
	return out
}

func TestComputeStats(t *testing.T) {
	samples := makeSamples("w", 0, 100, 102, 98, 101, 99, 500, 95)
	st := ComputeStats(samples)
	if st.N != 7 {
		t.Fatalf("N=%d want 7", st.N)
	}
	if st.Median != 100 {
		t.Fatalf("median=%v want 100", st.Median)
	}
	if st.Min != 95 || st.Max != 500 {
		t.Fatalf("min/max=%v/%v want 95/500", st.Min, st.Max)
	}
	// 截尾均值不应被离群点 500 拉高到接近普通均值。
	if st.TrimmedMean >= st.Mean {
		t.Fatalf("trimmed mean %v should be below raw mean %v (outlier 500)", st.TrimmedMean, st.Mean)
	}
	t.Logf("samples=%v", []int64{100, 102, 98, 101, 99, 500, 95})
	t.Logf("stats: n=%d median=%.1f MAD=%.2f mean=%.2f trimmed=%.2f stddev=%.2f",
		st.N, st.Median, st.MAD, st.Mean, st.TrimmedMean, st.StdDev)

	if got := ComputeStats(nil); got.N != 0 {
		t.Fatalf("empty stats N=%d want 0", got.N)
	}
}

func baselineWith(median, mad float64, n int) *Baseline {
	return &Baseline{
		WorkloadID: "w",
		Stats:      Stats{N: n, Median: median, MAD: mad},
	}
}

// TestGateVerdicts 用完全确定的统计量验证判定边界，结论必须稳定可复现。
func TestGateVerdicts(t *testing.T) {
	g := NewGate(DefaultGateConfig())

	// 基线：1000ns，MAD=5ns（sigma≈7.4ns）
	base := baselineWith(1000, 5, 20)

	cases := []struct {
		name string
		med  float64
		mad  float64
		want Verdict
	}{
		{"clear regression", 1200, 5, VerdictRegression},
		{"clear improvement", 800, 5, VerdictImprovement},
		// 15% 的慢化但当前样本极不稳定（MAD=200），变化落在噪声尺度内 => stable
		{"slowdown but noisy", 1150, 200, VerdictStable},
		// 变化幅度低于 10% 阈值，即使很稳定也只是 normal fluctuation => stable
		{"small stable drift", 1050, 1, VerdictStable},
		{"normal fluctuation", 1010, 8, VerdictStable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := Stats{N: 20, Median: tc.med, MAD: tc.mad}
			d := g.Decide("w", cur, base)
			ratio, z := CompareStats(cur, base.Stats)
			t.Logf("case=%s current={median=%.0f mad=%.0f n=%d} baseline={median=%.0f mad=%.0f}",
				tc.name, tc.med, tc.mad, cur.N, base.Stats.Median, base.Stats.MAD)
			t.Logf("statistical basis: ratio=%.4f zscore=%.3f", ratio, z)
			t.Logf("verdict=%s reasons=%v", d.Verdict, d.Reasons)
			if d.Verdict != tc.want {
				t.Fatalf("verdict=%s want %s", d.Verdict, tc.want)
			}
		})
	}
}

func TestGateSpecialCases(t *testing.T) {
	g := NewGate(DefaultGateConfig())

	// 样本不足 => invalid_run，且属于门禁失败
	d := g.Decide("w", Stats{N: 2, Median: 5000}, baselineWith(1000, 5, 20))
	if d.Verdict != VerdictInvalidRun || !d.IsFailure() {
		t.Fatalf("got %s failure=%v, want invalid_run/failure", d.Verdict, d.IsFailure())
	}
	t.Logf("insufficient samples -> verdict=%s reasons=%v (IsFailure=%v)",
		d.Verdict, d.Reasons, d.IsFailure())

	// 无基线 => no_baseline，不算失败
	d = g.Decide("w", Stats{N: 20, Median: 1000}, nil)
	if d.Verdict != VerdictNoBaseline || d.IsFailure() {
		t.Fatalf("got %s failure=%v, want no_baseline/non-failure", d.Verdict, d.IsFailure())
	}
	t.Logf("no baseline -> verdict=%s reasons=%v (IsFailure=%v)",
		d.Verdict, d.Reasons, d.IsFailure())

	// 改善不算失败
	d = g.Decide("w", Stats{N: 20, Median: 800, MAD: 5}, baselineWith(1000, 5, 20))
	if d.Verdict != VerdictImprovement || d.IsFailure() {
		t.Fatalf("improvement must not be a gate failure, got %s", d.Verdict)
	}
	t.Logf("improvement -> verdict=%s (IsFailure=%v)", d.Verdict, d.IsFailure())
}
