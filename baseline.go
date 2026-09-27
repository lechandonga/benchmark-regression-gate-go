package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Baseline 是某个工作负载在既定配置与环境下的性能基线。
type Baseline struct {
	WorkloadID     string    `json:"workload_id"`
	Stats          Stats     `json:"stats"`
	Samples        []Sample  `json:"samples,omitempty"`
	ConfigHash     string    `json:"config_hash"`
	DepsHash       string    `json:"deps_hash"`
	EnvFingerprint string    `json:"env_fingerprint"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Note           string    `json:"note,omitempty"`
}

// BaselineStore 持久化基线。
type BaselineStore interface {
	Load(ctx context.Context, id string) (*Baseline, error)
	Save(ctx context.Context, b Baseline) error
	List(ctx context.Context) ([]string, error)
}

// Baseline 错误的可区分种类。
const (
	BaselineErrNotFound            = "not_found"
	BaselineErrAlreadyExists       = "already_exists"
	BaselineErrRegressionRejected  = "regression_rejected"
	BaselineErrFingerprintMismatch = "fingerprint_mismatch"
	BaselineErrForceReasonRequired = "force_reason_required"
)

// BaselineError 标记基线操作的可区分错误。
type BaselineError struct {
	Kind    string
	Message string
}

func (e *BaselineError) Error() string { return e.Message + " (" + e.Kind + ")" }

// ErrBaselineNotFound 在没有基线时返回。
type ErrBaselineNotFound struct{ WorkloadID string }

func (e *ErrBaselineNotFound) Error() string { return "baseline not found: " + e.WorkloadID }

var unsafeIDChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFileName(id string) string {
	return unsafeIDChars.ReplaceAllString(id, "_")
}

// BaselineManager 在存储之上提供比较与受保护的更新语义。
type BaselineManager struct {
	store BaselineStore
	now   func() time.Time
}

// NewBaselineManager 创建基线管理器。
func NewBaselineManager(store BaselineStore) *BaselineManager {
	return &BaselineManager{store: store, now: time.Now}
}

// fingerprintsMatch 判断新基线数据与已存基线是否在同一配置/依赖/环境下，
// 任何一项变化都会使旧基线失去比较意义。
func fingerprintsMatch(existing, candidate Baseline) (string, bool) {
	switch {
	case existing.ConfigHash != candidate.ConfigHash:
		return "run configuration changed (config hash mismatch)", false
	case existing.DepsHash != candidate.DepsHash:
		return "workload dependencies changed (deps hash mismatch)", false
	case existing.EnvFingerprint != candidate.EnvFingerprint:
		return fmt.Sprintf("runtime environment changed (%q -> %q)",
			existing.EnvFingerprint, candidate.EnvFingerprint), false
	}
	return "", true
}

// Establish 为尚无基线的工作负载建立基线。已存在基线时必须走 Update。
func (m *BaselineManager) Establish(ctx context.Context, b Baseline) error {
	if _, err := m.store.Load(ctx, b.WorkloadID); err == nil {
		return &BaselineError{
			Kind:    BaselineErrAlreadyExists,
			Message: fmt.Sprintf("baseline for %q already exists; use Update", b.WorkloadID),
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		var nf *ErrBaselineNotFound
		if !errors.As(err, &nf) {
			return err
		}
	}
	now := m.now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now
	if b.Note == "" {
		b.Note = "initial baseline"
	}
	return m.store.Save(ctx, b)
}

// Update 在确认不是性能回退后更新基线；回退必须被明确拒绝而不是静默覆盖。
//
//   - 配置/依赖/环境指纹不一致：拒绝（Kind=fingerprint_mismatch），调用方应重新 Establish；
//   - 判定为 regression 且 force=false：拒绝（Kind=regression_rejected），旧基线保持不变；
//   - 判定为 regression 且 force=true：必须提供非空 reason，理由写入 Note 并保留审计；
//   - improvement / stable：允许更新。
func (m *BaselineManager) Update(ctx context.Context, b Baseline, verdict Verdict, force bool, reason string) error {
	existing, err := m.store.Load(ctx, b.WorkloadID)
	if err != nil {
		return &BaselineError{
			Kind:    BaselineErrNotFound,
			Message: fmt.Sprintf("no baseline for %q; establish one first", b.WorkloadID),
		}
	}

	if msg, ok := fingerprintsMatch(*existing, b); !ok {
		return &BaselineError{
			Kind:    BaselineErrFingerprintMismatch,
			Message: fmt.Sprintf("baseline update for %q rejected: %s", b.WorkloadID, msg),
		}
	}

	if verdict == VerdictRegression {
		if !force {
			return &BaselineError{
				Kind: BaselineErrRegressionRejected,
				Message: fmt.Sprintf(
					"baseline update for %q rejected: regression detected (median %.0f ns -> %.0f ns); "+
						"old baseline retained. Re-run to confirm or force-update with an explicit reason.",
					b.WorkloadID, existing.Stats.Median, b.Stats.Median),
			}
		}
		if strings.TrimSpace(reason) == "" {
			return &BaselineError{
				Kind:    BaselineErrForceReasonRequired,
				Message: "force-accepting a regression requires a non-empty reason for the audit trail",
			}
		}
		b.Note = "FORCED regression acceptance: " + reason
	}

	// 保留创建时间，仅推进更新时间；改善/正常波动的更新会带上说明。
	b.CreatedAt = existing.CreatedAt
	b.UpdatedAt = m.now().UTC()
	if b.Note == "" {
		b.Note = "baseline refreshed (" + string(verdict) + ")"
	}
	return m.store.Save(ctx, b)
}

// Load 读取基线（便于服务层做比较）。
func (m *BaselineManager) Load(ctx context.Context, id string) (*Baseline, error) {
	return m.store.Load(ctx, id)
}

// FileBaselineStore 是基于 JSON 文件目录的基线存储。
type FileBaselineStore struct {
	dir string
}

// NewFileBaselineStore 创建文件基线存储（目录不存在时自动创建）。
func NewFileBaselineStore(dir string) (*FileBaselineStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create baseline dir %q: %w", dir, err)
	}
	return &FileBaselineStore{dir: dir}, nil
}

func (s *FileBaselineStore) path(id string) string {
	return filepath.Join(s.dir, safeFileName(id)+".json")
}

func (s *FileBaselineStore) Load(ctx context.Context, id string) (*Baseline, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &ErrBaselineNotFound{WorkloadID: id}
		}
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("decode baseline %q: %w", id, err)
	}
	return &b, nil
}

func (s *FileBaselineStore) Save(ctx context.Context, b Baseline) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(b.WorkloadID), data)
}

func (s *FileBaselineStore) List(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}
