package cache

import (
	"path/filepath"
	"testing"
	"time"
)

func TestFingerprintStableAndSensitive(t *testing.T) {
	cfg := []byte(`{"n":4096}`)
	deps := map[string]string{"mod-a": "v1.2.3"}
	env := map[string]string{"go_version": "go1.26", "cpus": "8"}

	base := Fingerprint("w", cfg, deps, env)

	// Same inputs -> identical fingerprint (map iteration order must not matter).
	again := Fingerprint("w", cfg, map[string]string{"mod-a": "v1.2.3"}, map[string]string{"cpus": "8", "go_version": "go1.26"})
	if base != again {
		t.Errorf("fingerprint not stable: %v vs %v", base, again)
	}

	cases := []struct {
		name string
		key  Key
	}{
		{"config_change", Fingerprint("w", []byte(`{"n":8192}`), deps, env)},
		{"dep_change", Fingerprint("w", cfg, map[string]string{"mod-a": "v1.2.4"}, env)},
		{"dep_added", Fingerprint("w", cfg, map[string]string{"mod-a": "v1.2.3", "mod-b": "v0.1"}, env)},
		{"env_change", Fingerprint("w", cfg, deps, map[string]string{"go_version": "go1.27", "cpus": "8"})},
		{"workload_change", Fingerprint("other", cfg, deps, env)},
	}
	for _, tc := range cases {
		if tc.key.Fingerprint == base.Fingerprint {
			t.Errorf("%s did not change the fingerprint", tc.name)
		}
		t.Logf("%-16s -> fingerprint %.12s (base %.12s)", tc.name, tc.key.Fingerprint, base.Fingerprint)
	}
}

func TestCacheRoundTripAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	c := New(path)
	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	k1 := Fingerprint("w1", []byte(`{"a":1}`), nil, nil)
	k2 := Fingerprint("w1", []byte(`{"a":2}`), nil, nil)

	c.Put(k1, CachedResult{
		Durations: []time.Duration{100, 200, 300},
		CreatedAt: time.Now(),
		RunEnv:    map[string]string{"cpus": "8"},
	})
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload from disk: entry survives, distinct keys stay isolated.
	c2 := New(path)
	if err := c2.Load(); err != nil {
		t.Fatal(err)
	}
	got, ok := c2.Get(k1)
	if !ok || len(got.Durations) != 3 || got.Durations[1] != 200 {
		t.Errorf("round trip failed: %+v ok=%v", got, ok)
	}
	if _, ok := c2.Get(k2); ok {
		t.Errorf("different config must not hit the cache")
	}
	t.Logf("cached result: durations=%v env=%v", got.Durations, got.RunEnv)
}

func TestRuntimeEnvCaptured(t *testing.T) {
	env := RuntimeEnv()
	for _, k := range []string{"go_version", "goos", "goarch", "cpus"} {
		if env[k] == "" {
			t.Errorf("runtime env missing %q", k)
		}
	}
	t.Logf("runtime env folded into fingerprints: %v", env)
}
