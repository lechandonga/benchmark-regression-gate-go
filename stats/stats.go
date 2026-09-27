package stats

import (
	"math"
	"sort"
	"time"
)

// Summary is the statistical summary of a set of valid samples. The median
// is the primary statistic for regression decisions because it is robust
// against outliers; the coefficient of variation (CV) measures run noise.
type Summary struct {
	N      int
	Mean   time.Duration
	Median time.Duration
	StdDev time.Duration
	// CV is the coefficient of variation: stddev / mean (0 when mean == 0).
	CV  float64
	Min time.Duration
	Max time.Duration
}

// Summarize computes a Summary from valid durations. An empty input yields
// a zero Summary with N == 0.
func Summarize(durations []time.Duration) Summary {
	n := len(durations)
	if n == 0 {
		return Summary{}
	}
	sorted := make([]time.Duration, n)
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum float64
	for _, d := range sorted {
		sum += float64(d)
	}
	mean := sum / float64(n)

	var median time.Duration
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}

	var variance float64
	if n > 1 {
		for _, d := range sorted {
			delta := float64(d) - mean
			variance += delta * delta
		}
		variance /= float64(n - 1) // sample variance
	}
	stddev := math.Sqrt(variance)

	cv := 0.0
	if mean > 0 {
		cv = stddev / mean
	}

	return Summary{
		N:      n,
		Mean:   time.Duration(mean),
		Median: median,
		StdDev: time.Duration(stddev),
		CV:     cv,
		Min:    sorted[0],
		Max:    sorted[n-1],
	}
}
