package bench

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// FailureKind 以可区分的方式描述采样失败原因。
type FailureKind string

const (
	// FailurePanic 工作负载执行期间发生 panic。
	FailurePanic FailureKind = "panic"
	// FailureTimeout 单次迭代超过配置的超时时间。
	FailureTimeout FailureKind = "timeout"
	// FailureInvalid 工作负载返回了无效样本（如非正耗时）。
	FailureInvalid FailureKind = "invalid_sample"
	// FailureWorkloadError 工作负载自身返回错误。
	FailureWorkloadError FailureKind = "workload_error"
	// FailureMissing 引用了未注册的工作负载。
	FailureMissing FailureKind = "missing_workload"
	// FailureInsufficient 有效样本数量不足，无法给出统计结论。
	FailureInsufficient FailureKind = "insufficient_samples"
)

// Sample 是一次可归因的采样记录。
type Sample struct {
	WorkloadID string    `json:"workload_id"`
	RunID      int       `json:"run_id"`
	Iteration  int       `json:"iteration"`
	Nanos      int64     `json:"nanos"`
	Bytes      int64     `json:"bytes,omitempty"`
	Ops        int64     `json:"ops,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// RunFailure 记录一次被排除出统计的失败采样。
type RunFailure struct {
	WorkloadID string      `json:"workload_id"`
	RunID      int         `json:"run_id"`
	Iteration  int         `json:"iteration"`
	Kind       FailureKind `json:"kind"`
	Message    string      `json:"message"`
	StartedAt  time.Time   `json:"started_at"`
	EndedAt    time.Time   `json:"ended_at"`
}

// Workload 描述一个可注册、可执行的基准工作负载。
type Workload struct {
	// ID 是工作负载的唯一标识。
	ID string
	// Description 是人类可读说明。
	Description string
	// Deps 声明外部依赖指纹（如输入文件哈希、依赖版本），
	// 依赖变化会使缓存与基线失效。
	Deps []string
	// SourceHash 是工作负载实现本身的指纹，由注册方提供。
	SourceHash string
	// Run 执行一次迭代，返回纳秒耗时及可选的吞吐数据。
	Run func(ctx context.Context) (nanos int64, bytes int64, ops int64, err error)
}

// Registry 保存已注册的工作负载。
type Registry struct {
	mu        sync.RWMutex
	workloads map[string]Workload
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{workloads: make(map[string]Workload)}
}

// Register 注册一个工作负载。重复 ID、空 ID 或缺少 Run 函数会被拒绝。
func (r *Registry) Register(w Workload) error {
	if w.ID == "" {
		return errors.New("workload id is required")
	}
	if w.Run == nil {
		return fmt.Errorf("workload %q: Run function is required", w.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.workloads[w.ID]; exists {
		return fmt.Errorf("workload %q already registered", w.ID)
	}
	r.workloads[w.ID] = w
	return nil
}

// Get 返回已注册的工作负载。
func (r *Registry) Get(id string) (Workload, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.workloads[id]
	return w, ok
}

// List 返回全部已注册工作负载 ID（按字典序，保证可复现）。
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.workloads))
	for id := range r.workloads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
