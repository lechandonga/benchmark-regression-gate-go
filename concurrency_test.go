package bench

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentEvaluations 多个评估并发进行：
// 注册表读取、报告聚合、缓存写入与审计追加都必须安全；
// 审计日志最终行数必须完整、无损坏（可复现/可审计）。
func TestConcurrentEvaluations(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 6; i++ {
		id := string(rune('a'+i)) + "-workload"
		h.reg.Register(noisyWorkload(id, "h1", 10_000, 3))
	}
	svc := h.service(false, nil)
	ids := h.reg.List()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rep, err := svc.Evaluate(context.Background(), ids)
			if err != nil {
				t.Errorf("eval %d: %v", seed, err)
				return
			}
			if len(rep.Reports) != len(ids) {
				t.Errorf("eval %d: got %d reports want %d", seed, len(rep.Reports), len(ids))
			}
			// 每个样本都必须能归因到自身工作负载
			for id, rp := range rep.Reports {
				for _, s := range rp.AllSamples() {
					if s.WorkloadID != id {
						t.Errorf("attribution corruption: %s in %s report", s.WorkloadID, id)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	recs, err := h.audit.ReadAll(context.Background())
	if err != nil {
		t.Fatalf("audit log corrupted under concurrency: %v", err)
	}
	want := 8 * len(ids)
	if len(recs) != want {
		t.Fatalf("audit records=%d want %d (lost or interleaved writes)", len(recs), want)
	}
	t.Logf("8 concurrent evaluations x %d workloads -> %d intact audit records, all samples attributable",
		len(ids), len(recs))
}

// TestRegistryConcurrentAccess 注册表在并发注册/读取下安全。
func TestRegistryConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		i := i
		go func() {
			defer wg.Done()
			_ = reg.Register(Workload{
				ID:  "w" + string(rune('A'+i%26)) + string(rune('a'+i/26)),
				Run: func(context.Context) (int64, int64, int64, error) { return 1, 0, 0, nil },
			})
		}()
		go func() {
			defer wg.Done()
			_ = reg.List()
			_, _ = reg.Get("wAa")
		}()
	}
	wg.Wait()
}
