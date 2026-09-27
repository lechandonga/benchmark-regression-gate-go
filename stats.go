package bench

import (
	"math"
	"sort"
)

// madScale 将 MAD 换算为与标准差可比的尺度（正态分布一致性因子）。
const madScale = 1.4826

// Stats 是一组有效样本的汇总统计。
type Stats struct {
	N           int     `json:"n"`
	Median      float64 `json:"median_nanos"`
	MAD         float64 `json:"mad_nanos"`
	Mean        float64 `json:"mean_nanos"`
	TrimmedMean float64 `json:"trimmed_mean_nanos"`
	StdDev      float64 `json:"stddev_nanos"`
	Min         float64 `json:"min_nanos"`
	Max         float64 `json:"max_nanos"`
}

// medianOf 返回已排序切片的中位数。
func medianOf(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// ComputeStats 对样本耗时计算稳健统计量（中位数、MAD 等）。
// 中位数与 MAD 对单次抖动不敏感，是抗噪声的判定依据；
// 均值/截尾均值/标准差同时保留，便于解释与审计。
func ComputeStats(samples []Sample) Stats {
	if len(samples) == 0 {
		return Stats{}
	}
	values := make([]float64, len(samples))
	var sum float64
	for i, s := range samples {
		values[i] = float64(s.Nanos)
		sum += values[i]
	}
	sort.Float64s(values)

	n := len(values)
	median := medianOf(values)

	absDev := make([]float64, n)
	for i, v := range values {
		absDev[i] = math.Abs(v - median)
	}
	sort.Float64s(absDev)
	mad := medianOf(absDev)

	// 20% 双侧截尾均值，进一步抑制极端样本的影响。
	trimCut := n / 5
	trimmed := values[trimCut : n-trimCut]
	var trimSum float64
	for _, v := range trimmed {
		trimSum += v
	}
	trimmedMean := trimSum / float64(len(trimmed))

	mean := sum / float64(n)
	var sq float64
	for _, v := range values {
		d := v - mean
		sq += d * d
	}
	stddev := 0.0
	if n > 1 {
		stddev = math.Sqrt(sq / float64(n-1))
	}

	return Stats{
		N:           n,
		Median:      median,
		MAD:         mad,
		Mean:        mean,
		TrimmedMean: trimmedMean,
		StdDev:      stddev,
		Min:         values[0],
		Max:         values[n-1],
	}
}

// robustSigma 合并当前与基线的波动尺度；
// 当 MAD 退化为 0（离散度过低）时，回退到相对中位数的最小尺度，
// 避免 z 分数被零除放大。
func robustSigma(median float64, mad float64) float64 {
	sigma := mad * madScale
	floor := math.Max(median*1e-4, 1) // 中位数的万分之一，至少 1ns
	if sigma < floor {
		sigma = floor
	}
	return sigma
}

// CompareStats 依据当前统计与基线统计计算比值与稳健 z 分数。
// ratio = current / baseline（>1 表示变慢）。
// z 分数把相对变化与“基线 + 当前”双方的波动尺度比较，
// 只有超过噪声尺度的变化才可能被判定为显著。
func CompareStats(current, baseline Stats) (ratio, zscore float64) {
	if baseline.Median <= 0 {
		return 0, 0
	}
	ratio = current.Median / baseline.Median
	sigBase := robustSigma(baseline.Median, baseline.MAD)
	sigCur := robustSigma(current.Median, current.MAD)
	pooled := math.Sqrt(sigBase*sigBase + sigCur*sigCur)
	if pooled == 0 {
		return ratio, 0
	}
	zscore = (current.Median - baseline.Median) / pooled
	return ratio, zscore
}
