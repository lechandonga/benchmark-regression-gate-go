package main

import (
	"context"
	"crypto/sha256"
	"math/rand"
	"sort"
	"time"

	"github.com/lechandonga/benchmark-regression-gate-go/bench"
)

// registerDemoWorkloads registers deterministic, CPU-bound demo workloads.
// Each Run measures its own operation and returns the duration in ns.
func registerDemoWorkloads(reg *bench.Registry) {
	must := func(w bench.Workload) {
		if err := reg.Register(w); err != nil {
			panic(err)
		}
	}

	must(bench.Workload{
		Name:   "sort-4k",
		Config: map[string]any{"n": 4096, "seed": 42},
		Run: func(ctx context.Context) (int64, error) {
			rng := rand.New(rand.NewSource(42))
			data := make([]int, 4096)
			for i := range data {
				data[i] = rng.Int()
			}
			start := time.Now()
			sort.Ints(data)
			return time.Since(start).Nanoseconds(), nil
		},
	})

	must(bench.Workload{
		Name:   "sha256-64k",
		Config: map[string]any{"bytes": 65536, "rounds": 8},
		Run: func(ctx context.Context) (int64, error) {
			buf := make([]byte, 65536)
			for i := range buf {
				buf[i] = byte(i)
			}
			start := time.Now()
			sum := buf
			for r := 0; r < 8; r++ {
				h := sha256.Sum256(sum)
				sum = h[:]
			}
			return time.Since(start).Nanoseconds(), nil
		},
	})

	must(bench.Workload{
		Name:   "map-insert-2k",
		Config: map[string]any{"n": 2048},
		Run: func(ctx context.Context) (int64, error) {
			start := time.Now()
			m := make(map[int]int, 2048)
			for i := 0; i < 2048; i++ {
				m[i] = i * 7
			}
			return time.Since(start).Nanoseconds(), nil
		},
	})
}
