package bench

import "fmt"

// Verdict 表示回归判定结论。
type Verdict string

const (
	// VerdictRegression 明确的性能回退。
	VerdictRegression Verdict = "regression"
	// VerdictImprovement 明确的性能改善。
	VerdictImprovement Verdict = "improvement"
	// VerdictStable 处于正常噪声/波动范围内。
	VerdictStable Verdict = "stable"
	// VerdictNoBaseline 尚无基线，无法判定。
	VerdictNoBaseline Verdict = "no_baseline"
	// VerdictInvalidRun 采样无效（样本不足等），不给出性能结论。
	VerdictInvalidRun Verdict = "invalid_run"
)

// minDecisionSamples 是给出性能结论所需的最少有效样本数。
const minDecisionSamples = 3

// GateConfig 控制回归判定规则。
type GateConfig struct {
	// RegressionThreshold 判定回退的最小相对变化（如 0.10 表示变慢 10%）。
	RegressionThreshold float64
	// ImprovementThreshold 判定改善的最小相对变化（正数，如 0.10 表示变快 10%）。
	ImprovementThreshold float64
	// MinZScore 判定显著性的最小稳健 z 分数。
	MinZScore float64
}

// DefaultGateConfig 给出兼顾灵敏度与抗噪的默认规则：
// 中位数变慢/变快超过 10%，且变化幅度超过双方合并波动尺度的 2 倍，
// 才会判定为回退/改善；其余一律视为正常波动（stable）。
func DefaultGateConfig() GateConfig {
	return GateConfig{
		RegressionThreshold:  0.10,
		ImprovementThreshold: 0.10,
		MinZScore:            2.0,
	}
}

// Decision 是一次判定的完整、可解释结果。
type Decision struct {
	WorkloadID string   `json:"workload_id"`
	Verdict    Verdict  `json:"verdict"`
	Ratio      float64  `json:"ratio"`
	ZScore     float64  `json:"zscore"`
	Current    Stats    `json:"current"`
	Baseline   Stats    `json:"baseline"`
	Reasons    []string `json:"reasons"`
}

// IsFailure 只有明确回退或无效运行时，门禁才失败；
// 改善与正常波动均不算失败。
func (d Decision) IsFailure() bool {
	return d.Verdict == VerdictRegression || d.Verdict == VerdictInvalidRun
}

// Gate 依据统计与规则给出可解释的回归判定。
type Gate struct {
	cfg GateConfig
}

// NewGate 创建判定器；零值字段会回退到默认规则。
func NewGate(cfg GateConfig) *Gate {
	d := DefaultGateConfig()
	if cfg.RegressionThreshold > 0 {
		d.RegressionThreshold = cfg.RegressionThreshold
	}
	if cfg.ImprovementThreshold > 0 {
		d.ImprovementThreshold = cfg.ImprovementThreshold
	}
	if cfg.MinZScore > 0 {
		d.MinZScore = cfg.MinZScore
	}
	return &Gate{cfg: d}
}

// Decide 对当前统计与基线进行判定。baseline 为 nil 时返回 no_baseline。
//
// 判定规则（方向以“耗时越小越好”为准）：
//   - invalid_run：有效样本不足，拒绝给出性能结论；
//   - regression：ratio >= 1+RegressionThreshold 且 z >= MinZScore；
//   - improvement：ratio <= 1-ImprovementThreshold 且 z <= -MinZScore；
//   - stable：变化幅度或显著性未同时达标（视为噪声/正常波动）。
//
// “幅度 + 显著性”双条件保证：大噪声下的偶然偏移不会被误报，
// 小而稳定的变化也不会因样本离散被夸大。
func (g *Gate) Decide(workloadID string, current Stats, baseline *Baseline) Decision {
	d := Decision{WorkloadID: workloadID, Current: current}

	if current.N < minDecisionSamples {
		d.Verdict = VerdictInvalidRun
		d.Reasons = []string{
			fmt.Sprintf("only %d valid samples, at least %d required", current.N, minDecisionSamples),
		}
		return d
	}

	if baseline == nil {
		d.Verdict = VerdictNoBaseline
		d.Reasons = []string{"no baseline available; establish one first"}
		return d
	}
	d.Baseline = baseline.Stats

	ratio, z := CompareStats(current, baseline.Stats)
	d.Ratio = ratio
	d.ZScore = z

	switch {
	case ratio >= 1+g.cfg.RegressionThreshold && z >= g.cfg.MinZScore:
		d.Verdict = VerdictRegression
		d.Reasons = []string{
			fmt.Sprintf("median slowdown %.2f%% >= threshold %.2f%%",
				(ratio-1)*100, g.cfg.RegressionThreshold*100),
			fmt.Sprintf("robust z=%.2f >= %.2f (change exceeds pooled noise scale)",
				z, g.cfg.MinZScore),
			fmt.Sprintf("current median=%.0f ns vs baseline median=%.0f ns",
				current.Median, baseline.Stats.Median),
		}
	case ratio <= 1-g.cfg.ImprovementThreshold && z <= -g.cfg.MinZScore:
		d.Verdict = VerdictImprovement
		d.Reasons = []string{
			fmt.Sprintf("median speedup %.2f%% >= threshold %.2f%%",
				(1-ratio)*100, g.cfg.ImprovementThreshold*100),
			fmt.Sprintf("robust z=%.2f <= -%.2f (change exceeds pooled noise scale)",
				z, g.cfg.MinZScore),
			fmt.Sprintf("current median=%.0f ns vs baseline median=%.0f ns",
				current.Median, baseline.Stats.Median),
		}
	default:
		d.Verdict = VerdictStable
		delta := (ratio - 1) * 100
		d.Reasons = []string{
			fmt.Sprintf("median change %.2f%% within normal fluctuation", delta),
			fmt.Sprintf("thresholds: regression >= %.2f%%, improvement <= %.2f%%; require |z| >= %.2f",
				g.cfg.RegressionThreshold*100, g.cfg.ImprovementThreshold*100, g.cfg.MinZScore),
			fmt.Sprintf("observed ratio=%.4f, robust z=%.2f", ratio, z),
		}
	}
	return d
}
