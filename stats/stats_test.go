package stats

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	d := []time.Duration{300, 100, 200, 400, 500}
	s := Summarize(d)
	if s.N != 5 || s.Median != 300 || s.Mean != 300 || s.Min != 100 || s.Max != 500 {
		t.Errorf("unexpected summary: %+v", s)
	}
	if s.StdDev <= 0 || s.CV <= 0 {
		t.Errorf("expected positive stddev/cv: %+v", s)
	}
	t.Logf("samples=%v -> median=%s mean=%s stddev=%s cv=%.4f", d, s.Median, s.Mean, s.StdDev, s.CV)

	even := Summarize([]time.Duration{100, 200, 300, 400})
	if even.Median != 250 {
		t.Errorf("even median=%s, want 250", even.Median)
	}
	if got := Summarize(nil); got.N != 0 {
		t.Errorf("empty summary N=%d, want 0", got.N)
	}
}

func TestSummarizeDeterministic(t *testing.T) {
	d := []time.Duration{11, 7, 42, 7, 19, 23, 7, 1000}
	a, b := Summarize(d), Summarize(d)
	if a != b {
		t.Errorf("summarize not deterministic: %+v vs %+v", a, b)
	}
}
