package stats

import (
	"testing"
	"time"
)

func summaryOf(median time.Duration, cv float64) Summary {
	return Summary{
		N:      30,
		Mean:   median,
		Median: median,
		StdDev: time.Duration(float64(median) * cv),
		CV:     cv,
		Min:    median / 2,
		Max:    median * 2,
	}
}

var defaultOpts = CompareOptions{NoiseBand: 0.05, MinRegression: 0.10}

func TestCompareRegressionOnlyBeyondThreshold(t *testing.T) {
	base := summaryOf(1000, 0.03)

	cases := []struct {
		name    string
		current time.Duration
		cv      float64
		want    Verdict
	}{
		{"equal", 1000, 0.03, VerdictStable},
		{"within_noise_band", 1040, 0.03, VerdictStable},
		{"above_band_below_min_regression", 1080, 0.03, VerdictStable},
		{"clear_regression", 1200, 0.03, VerdictRegression},
		{"huge_regression", 3000, 0.03, VerdictRegression},
		{"improvement", 800, 0.03, VerdictImproved},
		{"tiny_improvement_is_stable", 980, 0.03, VerdictStable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := summaryOf(tc.current, tc.cv)
			d := Compare(cur, base, defaultOpts)
			if d.Verdict != tc.want {
				t.Errorf("verdict=%s, want %s (ratio=%.3f threshold=%.3f)",
					d.Verdict, tc.want, d.Ratio, d.Threshold)
			}
			if tc.want == VerdictRegression && d.Ratio-1 <= d.Threshold {
				t.Errorf("regression declared below threshold: ratio=%.3f threshold=%.3f", d.Ratio, d.Threshold)
			}
			t.Logf("baseline median=%s current median=%s ratio=%.3f noise_floor=%.1f%% threshold=%.1f%% -> %s (%s)",
				base.Median, cur.Median, d.Ratio, d.NoiseFloor*100, d.Threshold*100, d.Verdict, d.Reason)
		})
	}
}

func TestCompareNoiseWidensTolerance(t *testing.T) {
	base := summaryOf(1000, 0.03)
	// Current run is noisy (CV 25%): a 15% slowdown must NOT fail the gate,
	// it is indistinguishable from noise.
	noisy := summaryOf(1150, 0.25)
	d := Compare(noisy, base, defaultOpts)
	if d.Verdict == VerdictRegression {
		t.Errorf("noisy run must not be flagged as regression: %+v", d)
	}
	t.Logf("noisy current (cv=%.2f) ratio=%.3f noise_floor=%.1f%% -> %s",
		noisy.CV, d.Ratio, d.NoiseFloor*100, d.Verdict)

	// But a slowdown far beyond the noise floor still fails.
	noisySlow := summaryOf(1400, 0.25)
	d = Compare(noisySlow, base, defaultOpts)
	if d.Verdict != VerdictRegression {
		t.Errorf("slowdown beyond noise floor must regress, got %s", d.Verdict)
	}
	t.Logf("noisy+slow ratio=%.3f threshold=%.1f%% -> %s", d.Ratio, d.Threshold*100, d.Verdict)
}

func TestCompareTooNoisyIsInconclusiveNotFailure(t *testing.T) {
	base := summaryOf(1000, 0.03)
	wild := summaryOf(1600, 0.60) // CV above default cap
	d := Compare(wild, base, defaultOpts)
	if d.Verdict != VerdictNoisy {
		t.Errorf("verdict=%s, want %s; wild runs must be inconclusive, never a failure", d.Verdict, VerdictNoisy)
	}
	t.Logf("wild run cv=%.2f -> %s (%s)", wild.CV, d.Verdict, d.Reason)
}

func TestCompareMissingData(t *testing.T) {
	base := summaryOf(1000, 0.03)
	if d := Compare(Summary{}, base, defaultOpts); d.Verdict != VerdictUnknown {
		t.Errorf("empty current: verdict=%s, want unknown", d.Verdict)
	}
	if d := Compare(base, Summary{}, defaultOpts); d.Verdict != VerdictNoBaseline {
		t.Errorf("empty baseline: verdict=%s, want no_baseline", d.Verdict)
	}
}

func TestCompareImprovementNeverFails(t *testing.T) {
	base := summaryOf(1000, 0.02)
	for _, cur := range []time.Duration{999, 950, 900, 500, 100} {
		d := Compare(summaryOf(cur, 0.02), base, defaultOpts)
		if d.Verdict == VerdictRegression {
			t.Errorf("improvement to %s misreported as regression", cur)
		}
	}
}
