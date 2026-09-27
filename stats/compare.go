package stats

import "fmt"

// Verdict is the regression decision for one workload.
type Verdict string

const (
	VerdictUnknown    Verdict = "unknown"
	VerdictNoBaseline Verdict = "no_baseline"
	VerdictStable     Verdict = "stable"
	VerdictImproved   Verdict = "improved"
	VerdictRegression Verdict = "regression"
	VerdictNoisy      Verdict = "inconclusive_noise"
)

// Decision carries the verdict together with the evidence behind it, so a
// conclusion is always explainable from the logged numbers.
type Decision struct {
	Verdict Verdict
	// Ratio = current median / baseline median; 1.0 means equal.
	Ratio float64
	// NoiseFloor is the effective relative tolerance derived from the
	// configured band and the observed CV of both sides.
	NoiseFloor float64
	// Threshold is the relative slowdown above which a regression is
	// declared: max(NoiseFloor, MinRegression).
	Threshold float64
	Current   Summary
	Baseline  Summary
	Reason    string
}

// CompareOptions configures the regression rule.
type CompareOptions struct {
	// NoiseBand is the minimum relative tolerance, e.g. 0.05. The effective
	// noise floor is max(NoiseBand, baseline CV, current CV).
	NoiseBand float64
	// MinRegression is the minimum relative slowdown that can ever be
	// declared a regression, e.g. 0.10. Guards against failing on tiny
	// absolute differences of very fast workloads.
	MinRegression float64
	// ImprovementFraction labels ratios below (1 - fraction) as an
	// improvement. When <= 0 the noise floor is used.
	ImprovementFraction float64
	// NoiseCap marks the comparison inconclusive when either side's CV
	// exceeds it (default 0.5 when <= 0). A noisy run never fails a gate.
	NoiseCap float64
}

// Compare decides whether current samples regress against a baseline.
// Only a slowdown exceeding both the noise floor and MinRegression is a
// regression; improvements and normal fluctuation never fail.
func Compare(current, baseline Summary, opts CompareOptions) Decision {
	d := Decision{Current: current, Baseline: baseline, Verdict: VerdictUnknown}

	if baseline.N == 0 || baseline.Median <= 0 {
		d.Verdict = VerdictNoBaseline
		d.Reason = "no usable baseline; establish one before gating"
		return d
	}
	if current.N == 0 || current.Median <= 0 {
		d.Verdict = VerdictUnknown
		d.Reason = "no valid current samples"
		return d
	}

	noiseCap := opts.NoiseCap
	if noiseCap <= 0 {
		noiseCap = 0.5
	}
	if current.CV > noiseCap || baseline.CV > noiseCap {
		d.Verdict = VerdictNoisy
		d.Ratio = float64(current.Median) / float64(baseline.Median)
		d.Reason = fmt.Sprintf("run too noisy to decide: current CV %.3f, baseline CV %.3f, cap %.3f",
			current.CV, baseline.CV, noiseCap)
		return d
	}

	noiseFloor := opts.NoiseBand
	if baseline.CV > noiseFloor {
		noiseFloor = baseline.CV
	}
	if current.CV > noiseFloor {
		noiseFloor = current.CV
	}
	threshold := noiseFloor
	if opts.MinRegression > threshold {
		threshold = opts.MinRegression
	}

	d.NoiseFloor = noiseFloor
	d.Threshold = threshold
	d.Ratio = float64(current.Median) / float64(baseline.Median)

	improvementFraction := opts.ImprovementFraction
	if improvementFraction <= 0 {
		improvementFraction = noiseFloor
	}

	switch {
	case d.Ratio-1 > threshold:
		d.Verdict = VerdictRegression
		d.Reason = fmt.Sprintf("median slowed by %.1f%% (%.3fx), exceeding threshold %.1f%%",
			(d.Ratio-1)*100, d.Ratio, threshold*100)
	case d.Ratio < 1-improvementFraction:
		d.Verdict = VerdictImproved
		d.Reason = fmt.Sprintf("median improved by %.1f%% (%.3fx)", (1-d.Ratio)*100, d.Ratio)
	default:
		d.Verdict = VerdictStable
		d.Reason = fmt.Sprintf("median ratio %.3fx within noise floor %.1f%%", d.Ratio, noiseFloor*100)
	}
	return d
}
