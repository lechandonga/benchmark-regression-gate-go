package bench

import (
	"context"
	"errors"
	"sync"
)

// ServiceConfig 汇总顶层服务的配置。
type ServiceConfig struct {
	Run  RunConfig
	Gate GateConfig
	// UpdateBaselines 为 true 时，非回退结论会自动更新基线。
	UpdateBaselines bool
	// ForceAcceptRegression 显式接受回退并更新基线（必须给出理由，留痕审计）。
	ForceAcceptRegression bool
	AcceptReason          string
	// UseCache 启用结果缓存与增量重跑。
	UseCache bool
}

// ServiceReport 是一次门禁运行中每个工作负载的结论集合。
type ServiceReport struct {
	// RunSeq 是本进程内第几次 Evaluate（从 1 开始），用于审计定位。
	RunSeq    int                     `json:"run_seq"`
	Decisions map[string]Decision     `json:"decisions"`
	Reports   map[string]*RunReport   `json:"reports"`
	Failures  map[string][]RunFailure `json:"failures"`
}

// Failed 返回存在明确回退或无效运行的工作负载 ID（按字典序，便于稳定展示）。
func (sr *ServiceReport) Failed() []string {
	var ids []string
	for id, d := range sr.Decisions {
		if d.IsFailure() {
			ids = append(ids, id)
		}
	}
	sortStrings(ids)
	return ids
}

// Service 编排注册、采样、缓存、判定、基线管理与审计。
type Service struct {
	registry  *Registry
	runner    *Runner
	gate      *Gate
	baselines *BaselineManager
	cache     ResultCache
	audit     AuditLog
	cfg       ServiceConfig
	env       string
	mu        sync.Mutex
	runSeq    int64
}

// NewService 组装服务。cache 或 audit 为 nil 表示不启用对应能力。
func NewService(reg *Registry, bm *BaselineManager, cache ResultCache, audit AuditLog, cfg ServiceConfig) *Service {
	cfg.Run = cfg.Run.normalized()
	return &Service{
		registry:  reg,
		runner:    NewRunner(reg, cfg.Run),
		gate:      NewGate(cfg.Gate),
		baselines: bm,
		cache:     cache,
		audit:     audit,
		cfg:       cfg,
		env:       EnvFingerprint(),
	}
}

// keyFor 根据工作负载实现、配置、依赖与环境构造缓存/一致性键。
func (s *Service) keyFor(w Workload) CacheKey {
	return CacheKey{
		WorkloadID:     w.ID,
		SourceHash:     w.SourceHash,
		ConfigHash:     HashConfig(s.cfg.Run),
		DepsHash:       HashDeps(w.Deps),
		EnvFingerprint: s.env,
	}
}

// Evaluate 对给定工作负载执行“采样（可走缓存）→ 判定 → 受保护基线更新 → 审计”。
//
// 增量重跑语义：键完全一致（实现/配置/依赖/环境均未变化）时直接复用缓存样本，
// 不重复计算；任一字段变化即缓存未命中并重新采样。
// 判定与基线更新在每次 Evaluate 中都会重新执行（基线可能已被人工调整）。
func (s *Service) Evaluate(ctx context.Context, ids []string) (*ServiceReport, error) {
	s.mu.Lock()
	s.runSeq++
	runSeq := int(s.runSeq)
	s.mu.Unlock()

	if len(ids) == 0 {
		ids = s.registry.List()
	}

	// 先解析键并区分缓存命中，未命中的才进入并发采样。
	type plan struct {
		id  string
		key CacheKey
	}
	var toRun []plan
	reports := make(map[string]*RunReport, len(ids))
	hitFlags := make(map[string]bool, len(ids))
	keys := make(map[string]CacheKey, len(ids))

	for _, id := range ids {
		w, ok := s.registry.Get(id)
		if !ok {
			// 未注册工作负载直接产出 aborted 报告，判定为 invalid_run。
			rp := s.runner.runOneWorkload(ctx, id)
			reports[id] = rp
			keys[id] = CacheKey{WorkloadID: id, EnvFingerprint: s.env}
			continue
		}
		key := s.keyFor(w)
		keys[id] = key
		if s.cfg.UseCache && s.cache != nil {
			if entry, hit, _ := s.cache.Get(ctx, key); hit && entry.Report != nil {
				reports[id] = entry.Report
				hitFlags[id] = true
				continue
			}
		}
		toRun = append(toRun, plan{id: id, key: key})
	}

	runIDs := make([]string, len(toRun))
	for i, p := range toRun {
		runIDs[i] = p.id
	}
	if len(runIDs) > 0 {
		fresh, err := s.runner.RunMany(ctx, runIDs)
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, err
		}
		for id, rp := range fresh {
			reports[id] = rp
			// 仅缓存“至少有有效样本”的报告；全失败的运行下次应重试。
			if s.cfg.UseCache && s.cache != nil && len(rp.AllSamples()) > 0 && !rp.Aborted {
				key := keys[id]
				_ = s.cache.Put(ctx, CacheEntry{Key: key, Report: rp})
			}
		}
	}

	out := &ServiceReport{
		RunSeq:    runSeq,
		Decisions: make(map[string]Decision, len(ids)),
		Reports:   reports,
		Failures:  make(map[string][]RunFailure, len(ids)),
	}

	for _, id := range ids {
		rp := reports[id]
		out.Failures[id] = rp.Failures

		var existing *Baseline
		if s.baselines != nil {
			if b, err := s.baselines.Load(ctx, id); err == nil {
				existing = b
			}
		}

		var decision Decision
		if rp.Aborted {
			decision = Decision{
				WorkloadID: id,
				Verdict:    VerdictInvalidRun,
				Current:    rp.Stats,
				Reasons:    []string{"workload missing or aborted before sampling"},
			}
		} else {
			decision = s.gate.Decide(id, rp.Stats, existing)
		}
		out.Decisions[id] = decision

		op, note := s.applyBaselinePolicy(ctx, id, rp, existing, decision)
		s.writeAudit(ctx, AuditInput{
			id: id, rp: rp, decision: decision, baseline: existing,
			key: keys[id], cacheHit: hitFlags[id], op: op, note: note, runSeq: runSeq,
		})
	}

	return out, ctx.Err()
}

// applyBaselinePolicy 按策略建立/更新基线，并返回审计用的操作描述。
func (s *Service) applyBaselinePolicy(ctx context.Context, id string, rp *RunReport, existing *Baseline, d Decision) (op, note string) {
	if s.baselines == nil || rp.Aborted {
		return "", ""
	}
	w, ok := s.registry.Get(id)
	if !ok {
		return "", ""
	}
	candidate := Baseline{
		WorkloadID:     id,
		Stats:          rp.Stats,
		Samples:        rp.AllSamples(),
		ConfigHash:     HashConfig(s.cfg.Run),
		DepsHash:       HashDeps(w.Deps),
		EnvFingerprint: s.env,
	}

	if existing == nil {
		// 无基线：首次建立（不要求判定通过，但至少需要有效样本）。
		if rp.Stats.N >= minDecisionSamples {
			if err := s.baselines.Establish(ctx, candidate); err == nil {
				return "establish", "initial baseline established"
			} else {
				return "rejected", "establish failed: " + err.Error()
			}
		}
		return "rejected", "not enough samples to establish baseline"
	}

	if !s.cfg.UpdateBaselines {
		return "", ""
	}

	err := s.baselines.Update(ctx, candidate, d.Verdict, s.cfg.ForceAcceptRegression, s.cfg.AcceptReason)
	if err == nil {
		return "update", "baseline updated (" + string(d.Verdict) + ")"
	}
	var be *BaselineError
	if errors.As(err, &be) && be.Kind == BaselineErrRegressionRejected {
		return "rejected", be.Message
	}
	return "rejected", err.Error()
}

// AuditInput 聚合写审计记录所需的字段。
type AuditInput struct {
	id       string
	rp       *RunReport
	decision Decision
	baseline *Baseline
	key      CacheKey
	cacheHit bool
	op       string
	note     string
	runSeq   int
}

func (s *Service) writeAudit(ctx context.Context, in AuditInput) {
	if s.audit == nil {
		return
	}
	rec := AuditRecord{
		RunSeq:      in.runSeq,
		WorkloadID:  in.id,
		Verdict:     in.decision.Verdict,
		Ratio:       in.decision.Ratio,
		ZScore:      in.decision.ZScore,
		Current:     in.decision.Current,
		CacheHit:    in.cacheHit,
		Key:         in.key,
		Reasons:     in.decision.Reasons,
		Failures:    in.rp.Failures,
		SampleCount: in.rp.Stats.N,
		BaselineOp:  in.op,
		Note:        in.note,
	}
	if in.baseline != nil {
		bstats := in.baseline.Stats
		rec.Baseline = &bstats
	}
	_ = s.audit.Append(ctx, rec)
}

func sortStrings(x []string) {
	// 避免在本文件再引入 sort，仅做简单插入排序（ID 数量通常很小）。
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j-1] > x[j]; j-- {
			x[j-1], x[j] = x[j], x[j-1]
		}
	}
}
