package baseline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/lechandonga/benchmark-regression-gate-go/stats"
)

// Entry stores one workload's baseline.
type Entry struct {
	// Fingerprint binds the baseline to the workload config, dependencies
	// and environment it was measured under.
	Fingerprint string
	Median      time.Duration
	Summary     stats.Summary
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Note        string
}

// Baseline is the on-disk baseline set keyed by workload name.
type Baseline struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// Store loads/saves baselines from a file path.
type Store struct {
	path string
	now  func() time.Time
}

// NewStore creates a Store backed by path.
func NewStore(path string) *Store {
	return &Store{path: path, now: time.Now}
}

// Load reads the baseline file; a missing file yields an empty baseline.
func (s *Store) Load() (*Baseline, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Baseline{Version: 1, Entries: map[string]Entry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("baseline: read %s: %w", s.path, err)
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("baseline: parse %s: %w", s.path, err)
	}
	if b.Entries == nil {
		b.Entries = map[string]Entry{}
	}
	return &b, nil
}

// Save writes the baseline atomically (temp file + rename) so a crash never
// leaves a half-written baseline.
func (s *Store) Save(b *Baseline) error {
	if b.Entries == nil {
		b.Entries = map[string]Entry{}
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("baseline: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("baseline: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("baseline: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("baseline: rename: %w", err)
	}
	return nil
}

// Get returns a baseline entry.
func (s *Store) Get(name string) (Entry, bool) {
	b, err := s.Load()
	if err != nil {
		return Entry{}, false
	}
	e, ok := b.Entries[name]
	return e, ok
}

// UpdateOptions guards baseline updates.
type UpdateOptions struct {
	// AllowRegression must be explicitly true to overwrite a baseline with
	// a slower measurement. Without it a regression is rejected, never
	// silently absorbed into the baseline.
	AllowRegression bool
	// Tolerance is the relative slowdown still accepted as an update
	// without AllowRegression (default 0: any slowdown is rejected).
	Tolerance float64
}

// UpdateResult reports what an update did.
type UpdateResult struct {
	Workload string
	Updated  bool
	Rejected bool
	Reason   string
}

// Update writes a new baseline entry. A slower median than the stored
// baseline (beyond Tolerance) is rejected unless AllowRegression is set;
// an entry whose fingerprint changed is treated as a fresh baseline for
// the new configuration.
func (s *Store) Update(name string, e Entry, opts UpdateOptions) (UpdateResult, error) {
	res := UpdateResult{Workload: name}
	if e.Median <= 0 {
		res.Rejected = true
		res.Reason = "refusing to store a baseline with non-positive median"
		return res, nil
	}

	b, err := s.Load()
	if err != nil {
		return res, err
	}

	now := s.now()
	e.UpdatedAt = now

	old, exists := b.Entries[name]
	switch {
	case !exists:
		e.CreatedAt = now
		b.Entries[name] = e
		res.Updated = true
		res.Reason = "baseline created"
	case old.Fingerprint != e.Fingerprint:
		e.CreatedAt = now
		b.Entries[name] = e
		res.Updated = true
		res.Reason = fmt.Sprintf("fingerprint changed (%s -> %s); baseline reset for new configuration",
			old.Fingerprint, e.Fingerprint)
	default:
		ratio := float64(e.Median) / float64(old.Median)
		if ratio-1 > opts.Tolerance && !opts.AllowRegression {
			res.Rejected = true
			res.Reason = fmt.Sprintf("refusing to regress baseline: median %.1f%% slower than existing baseline "+
				"(old %s, new %s, tolerance %.1f%%); pass AllowRegression to override",
				(ratio-1)*100, old.Median, e.Median, opts.Tolerance*100)
			return res, nil
		}
		e.CreatedAt = old.CreatedAt
		b.Entries[name] = e
		res.Updated = true
		if ratio-1 > opts.Tolerance {
			res.Reason = fmt.Sprintf("baseline updated with explicit regression override (%.1f%% slower)", (ratio-1)*100)
		} else {
			res.Reason = fmt.Sprintf("baseline updated (ratio %.3fx)", ratio)
		}
	}

	if err := s.Save(b); err != nil {
		return res, err
	}
	return res, nil
}
