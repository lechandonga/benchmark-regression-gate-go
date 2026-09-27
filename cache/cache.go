package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Key identifies a cache entry: workload name plus a fingerprint of its
// config, dependencies and runtime environment. Any change to those inputs
// yields a different key, so stale results are never reused.
type Key struct {
	Workload    string
	Fingerprint string
}

// CachedResult stores one measured run for reuse on incremental reruns.
type CachedResult struct {
	Durations []time.Duration `json:"durations"`
	CreatedAt time.Time       `json:"created_at"`
	// RunEnv records the environment the samples were taken in; it is
	// already folded into the fingerprint but kept for auditability.
	RunEnv map[string]string `json:"run_env"`
}

// Cache is a file-backed map from keys to measured results. It is safe for
// concurrent use.
type Cache struct {
	mu      sync.Mutex
	path    string
	entries map[string]CachedResult
	loaded  bool
}

// New creates a file-backed cache at path.
func New(path string) *Cache {
	return &Cache{path: path, entries: map[string]CachedResult{}}
}

func (c *Cache) mapKey(k Key) string { return k.Workload + "@" + k.Fingerprint }

// Load reads the cache file; a missing file yields an empty cache.
func (c *Cache) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		c.entries = map[string]CachedResult{}
		c.loaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("cache: read %s: %w", c.path, err)
	}
	entries := map[string]CachedResult{}
	if err := json.Unmarshal(data, &entries); err != nil {
		return fmt.Errorf("cache: parse %s: %w", c.path, err)
	}
	c.entries = entries
	c.loaded = true
	return nil
}

// Save persists the cache atomically.
func (c *Cache) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("cache: encode: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("cache: mkdir: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("cache: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("cache: rename: %w", err)
	}
	return nil
}

// Get returns a cached result when present.
func (c *Cache) Get(k Key) (CachedResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[c.mapKey(k)]
	return r, ok
}

// Put stores a result.
func (c *Cache) Put(k Key, r CachedResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[c.mapKey(k)] = r
}

// RuntimeEnv captures the environment dimensions that can change benchmark
// results. It is folded into every fingerprint so a machine or toolchain
// change invalidates cached results.
func RuntimeEnv() map[string]string {
	return map[string]string{
		"go_version": runtime.Version(),
		"goos":       runtime.GOOS,
		"goarch":     runtime.GOARCH,
		"cpus":       strconv.Itoa(runtime.NumCPU()),
	}
}

// Fingerprint builds a stable fingerprint from the workload config, its
// dependency descriptors and runtime environment values. The same inputs
// always produce the same key; any change produces a different one.
func Fingerprint(workload string, configJSON []byte, deps map[string]string, env map[string]string) Key {
	h := sha256.New()
	h.Write([]byte("workload\x00" + workload + "\x00"))
	h.Write([]byte("config\x00"))
	h.Write(configJSON)
	h.Write([]byte("\x00"))
	writeMap := func(label string, m map[string]string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(label + "\x00" + k + "\x00" + m[k] + "\x00"))
		}
	}
	writeMap("dep", deps)
	writeMap("env", env)
	return Key{Workload: workload, Fingerprint: hex.EncodeToString(h.Sum(nil))}
}
