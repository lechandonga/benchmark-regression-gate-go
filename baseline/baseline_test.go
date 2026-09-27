package baseline

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lechandonga/benchmark-regression-gate-go/stats"
)

func entry(fp string, median time.Duration) Entry {
	return Entry{
		Fingerprint: fp,
		Median:      median,
		Summary:     stats.Summary{N: 30, Median: median, Mean: median},
	}
}

func TestBaselineLifecycle(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "baseline.json"))

	// 1. Create.
	res, err := store.Update("w", entry("fp-a", 1000), UpdateOptions{})
	if err != nil || !res.Updated || res.Rejected {
		t.Fatalf("create: %+v err=%v", res, err)
	}
	t.Logf("create: %s", res.Reason)

	// 2. Improvement is accepted silently (baseline only gets better).
	res, err = store.Update("w", entry("fp-a", 900), UpdateOptions{})
	if err != nil || !res.Updated {
		t.Fatalf("improvement update: %+v err=%v", res, err)
	}
	t.Logf("improvement: %s", res.Reason)

	// 3. Regression must be explicitly rejected, not silently overwritten.
	res, err = store.Update("w", entry("fp-a", 1200), UpdateOptions{})
	if err != nil {
		t.Fatalf("regression update err: %v", err)
	}
	if !res.Rejected || res.Updated {
		t.Fatalf("regression must be rejected: %+v", res)
	}
	t.Logf("regression rejected: %s", res.Reason)

	// The stored baseline must be untouched.
	got, ok := store.Get("w")
	if !ok || got.Median != 900 {
		t.Fatalf("baseline silently overwritten: %+v", got)
	}

	// 4. Explicit override is honored and auditable.
	res, err = store.Update("w", entry("fp-a", 1200), UpdateOptions{AllowRegression: true})
	if err != nil || !res.Updated || res.Rejected {
		t.Fatalf("override: %+v err=%v", res, err)
	}
	t.Logf("override: %s", res.Reason)

	// 5. Fingerprint change resets the baseline for the new configuration.
	res, err = store.Update("w", entry("fp-b", 5000), UpdateOptions{})
	if err != nil || !res.Updated || res.Rejected {
		t.Fatalf("fingerprint reset: %+v err=%v", res, err)
	}
	t.Logf("fingerprint change: %s", res.Reason)
	got, _ = store.Get("w")
	if got.Median != 5000 || got.Fingerprint != "fp-b" {
		t.Fatalf("baseline not reset for new fingerprint: %+v", got)
	}
}

func TestBaselineTolerance(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "baseline.json"))
	if _, err := store.Update("w", entry("fp", 1000), UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// 3% slower within 5% tolerance: accepted.
	res, _ := store.Update("w", entry("fp", 1030), UpdateOptions{Tolerance: 0.05})
	if !res.Updated {
		t.Errorf("within-tolerance update rejected: %s", res.Reason)
	}
	// 10% slower beyond 5% tolerance: rejected.
	res, _ = store.Update("w", entry("fp", 1133), UpdateOptions{Tolerance: 0.05})
	if !res.Rejected {
		t.Errorf("beyond-tolerance update must be rejected: %+v", res)
	}
	t.Logf("tolerance boundary: within=accepted, beyond=%s", res.Reason)
}

func TestBaselineRejectsInvalidEntry(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "baseline.json"))
	res, err := store.Update("w", entry("fp", 0), UpdateOptions{AllowRegression: true})
	if err != nil || !res.Rejected {
		t.Errorf("zero-median baseline must be rejected: %+v err=%v", res, err)
	}
	t.Logf("invalid entry: %s", res.Reason)
}

func TestBaselinePersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "baseline.json")
	s1 := NewStore(path)
	if _, err := s1.Update("w", entry("fp", 777), UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s2 := NewStore(path)
	got, ok := s2.Get("w")
	if !ok || got.Median != 777 || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("round trip failed: %+v ok=%v", got, ok)
	}
	t.Logf("persisted entry: median=%s created=%s updated=%s", got.Median, got.CreatedAt, got.UpdatedAt)
}
