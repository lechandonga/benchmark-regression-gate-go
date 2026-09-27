package bench

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Workload is a named, reproducible unit of work that the harness can sample.
type Workload struct {
	// Name uniquely identifies the workload.
	Name string
	// Config is an opaque, JSON-serializable configuration object. Its
	// serialized form participates in cache/baseline fingerprinting.
	Config any
	// Run executes one sample and returns its observed duration in
	// nanoseconds. A non-nil error marks the sample as failed.
	Run func(ctx context.Context) (int64, error)
}

// Registry holds workloads available for sampling. It is safe for
// concurrent registration and lookup.
type Registry struct {
	mu        sync.RWMutex
	workloads map[string]Workload
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{workloads: make(map[string]Workload)}
}

// Register adds a workload. It returns an error on duplicate names, empty
// names or a missing Run function.
func (r *Registry) Register(w Workload) error {
	if w.Name == "" {
		return fmt.Errorf("bench: workload name must not be empty")
	}
	if w.Run == nil {
		return fmt.Errorf("bench: workload %q has no Run function", w.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.workloads[w.Name]; exists {
		return fmt.Errorf("bench: workload %q already registered", w.Name)
	}
	r.workloads[w.Name] = w
	return nil
}

// Get looks up a workload by name.
func (r *Registry) Get(name string) (Workload, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.workloads[name]
	return w, ok
}

// Names returns all registered workload names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.workloads))
	for n := range r.workloads {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
