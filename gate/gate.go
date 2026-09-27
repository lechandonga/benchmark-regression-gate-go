// Package gate orchestrates sampling, caching and regression decisions so
// that repeated and incremental runs produce stable, explainable verdicts.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lechandonga/benchmark-regression-gate-go/baseline"
	"github.com/lechandonga/benchmark-regression-gate-go/bench"
	"github.com/lechandonga/benchmark-regression-gate-go/cache"
	"github.com/lechandonga/benchmark-regression-gate-go/stats"
)

// Gate ties together the registry, result cache and baseline store.
type Gate struct {
	Registry  *bench.Registry
	Runner    *bench.Runner
	Cache     *cache.Cache
	Baselines *baseline.Store
	Compare   stats.CompareOptions
	// Deps describes dependency inputs (e.g. module versions) folded into
	// fingerprints. Env defaults to cache.RuntimeEnv() when nil.
	Deps map[string]string
	Env  map[string]string
	// DisableCache forces fresh sampling (results are still written back).
	DisableCache bool
}

// Report is the full, auditable record of one workload's evaluation.
type Report struct {
	Workload    string
	Fingerprint string
	FromCache   bool
	Summary     stats.Summary
	Decision    stats.Decision
	Valid       []bench.Sample
	Invalid     []bench.Sample
}

// Failed reports whether the report represents a gate failure.
func (r Report) Failed() bool { return r.Decision.Verdict == stats.VerdictRegression }

// fingerprintFor builds the cache/baseline key for a workload.
func (g *Gate) fingerprintFor(w bench.Workload) (cache.Key, error) {
	cfg, err := json.Marshal(w.Config)
	if err != nil {
		return cache.Key{}, fmt.Errorf("gate: encode config of %q: %w", w.Name, err)
	}
	env := g.Env
	if env == nil {
		env = cache.RuntimeEnv()
	}
	return cache.Fingerprint(w.Name, cfg, g.Deps, env), nil
}

// Run evaluates the named workloads (all registered names when empty) and
// returns one auditable report per workload. Workloads whose fingerprint is
// unchanged reuse cached samples instead of being re-executed.
func (g *Gate) Run(ctx context.Context, names []string, opts bench.RunOptions) ([]Report, error) {
	if len(names) == 0 {
		names = g.Registry.Names()
	}
	if err := g.Cache.Load(); err != nil {
		return nil, err
	}

	reports := make([]Report, 0, len(names))
	for _, name := range names {
		rep, err := g.runOne(ctx, name, opts)
		if err != nil {
			return reports, err
		}
		reports = append(reports, rep)
	}
	if err := g.Cache.Save(); err != nil {
		return reports, err
	}
	return reports, nil
}

func (g *Gate) runOne(ctx context.Context, name string, opts bench.RunOptions) (Report, error) {
	rep := Report{Workload: name}

	w, ok := g.Registry.Get(name)
	if !ok {
		return rep, &bench.RunError{Reason: bench.ReasonNotFound, Err: name}
	}
	key, err := g.fingerprintFor(w)
	if err != nil {
		return rep, err
	}
	rep.Fingerprint = key.Fingerprint

	var durations []time.Duration
	if !g.DisableCache {
		if cached, hit := g.Cache.Get(key); hit {
			rep.FromCache = true
			durations = cached.Durations
		}
	}
	if durations == nil {
		res, err := g.Runner.Run(ctx, name, opts)
		if err != nil {
			return rep, err
		}
		rep.Valid = res.Valid
		rep.Invalid = res.Invalid
		durations = res.Durations()
		g.Cache.Put(key, cache.CachedResult{
			Durations: durations,
			CreatedAt: time.Now(),
			RunEnv:    g.envOrDefault(),
		})
	}

	rep.Summary = stats.Summarize(durations)

	base, hasBase := g.Baselines.Get(name)
	if hasBase && base.Fingerprint == key.Fingerprint {
		rep.Decision = stats.Compare(rep.Summary, base.Summary, g.Compare)
	} else if hasBase {
		rep.Decision = stats.Decision{
			Verdict: stats.VerdictNoBaseline,
			Current: rep.Summary,
			Reason:  "baseline fingerprint does not match current configuration; re-baseline required",
		}
	} else {
		rep.Decision = stats.Compare(rep.Summary, stats.Summary{}, g.Compare)
	}
	return rep, nil
}

func (g *Gate) envOrDefault() map[string]string {
	if g.Env != nil {
		return g.Env
	}
	return cache.RuntimeEnv()
}

// UpdateBaseline records fresh measurements as the new baseline, subject to
// the store's regression guard.
func (g *Gate) UpdateBaseline(ctx context.Context, names []string, opts bench.RunOptions, uopts baseline.UpdateOptions) ([]baseline.UpdateResult, error) {
	if len(names) == 0 {
		names = g.Registry.Names()
	}
	results := make([]baseline.UpdateResult, 0, len(names))
	for _, name := range names {
		w, ok := g.Registry.Get(name)
		if !ok {
			return results, &bench.RunError{Reason: bench.ReasonNotFound, Err: name}
		}
		key, err := g.fingerprintFor(w)
		if err != nil {
			return results, err
		}
		res, err := g.Runner.Run(ctx, name, opts)
		if err != nil {
			return results, err
		}
		summary := stats.Summarize(res.Durations())
		ur, err := g.Baselines.Update(name, baseline.Entry{
			Fingerprint: key.Fingerprint,
			Median:      summary.Median,
			Summary:     summary,
		}, uopts)
		if err != nil {
			return results, err
		}
		results = append(results, ur)
	}
	return results, nil
}
