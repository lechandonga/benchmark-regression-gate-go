# benchmark-regression-gate-go

一套可复现的性能基准与回归判定库：注册工作负载 → 并发/重复采样 → 稳健统计
→ 噪声感知的回归判定 → 受保护的基线管理 → 结果缓存与增量重跑 → 追加式审计日志。

核心目标：**只有明确的性能回退才判失败**；正常波动与改善不会误报；基线永远不会
被一次回退静默覆盖；历史结论可审计、可复现。

---

## 1. 基准模型

### 1.1 工作负载（`Workload`）

```go
bench.Workload{
    ID:          "encode.1k",                 // 唯一标识
    SourceHash:  "v1.3.0",                    // 实现指纹（实现变了就必须变）
    Deps:        []string{"codec@2.3.1"},     // 外部依赖指纹（数据/模型/库版本）
    Description: "encode a 1 KiB payload",
    Run: func(ctx context.Context) (nanos, bytes, ops int64, err error) { ... },
}
```

- `Registry` 并发安全地注册工作负载；重复 ID、空 ID、缺少 `Run` 会被拒绝。
- 每次迭代返回**纳秒耗时**（指标方向：越小越好），可选返回处理字节数/操作数。
- 采样维度为 `Runs × Iterations`，另有 `Warmup` 个预热迭代被**丢弃**、不计入统计。

### 1.2 采样与可归因性（`Runner`）

- **重复运行**：`Runs` 次独立运行，每次收集 `Iterations` 个有效样本。
- **并发执行**：`Concurrency` 控制**同时执行的工作负载数量**；同一工作负载内部
  顺序采样，避免自身并行导致计时扭曲。
- 每个样本都带 `(WorkloadID, RunID, Iteration, RecordedAt)`，
  在并发与重复运行下结果都能精确归因。
- `IterationTimeout` 为单次迭代设上限。

### 1.3 失败样本被隔离，绝不混入统计

失败原因是可区分的枚举（`FailureKind`），失败迭代只产生 `RunFailure` 记录，
不产生样本：

| 原因 | 触发条件 |
| --- | --- |
| `panic` | 工作负载 panic（被 recover 捕获） |
| `timeout` | 单次迭代超过 `IterationTimeout` |
| `invalid_sample` | 返回非正耗时等无效值 |
| `workload_error` | 工作负载返回 error / 被父上下文取消 |
| `missing_workload` | 引用了未注册的工作负载（报告 `Aborted=true`） |
| `insufficient_samples` | 有效样本不足，无法给出统计结论 |

### 1.4 稳健统计量（`ComputeStats`）

为抵抗单次抖动，判定依据以**抗扰统计量**为主：

- **中位数**（而非均值）作为中心趋势；
- **MAD（中位数绝对偏差）× 1.4826** 作为离散尺度，对离群点不敏感；
- 同时保留均值、20% 双侧截尾均值、标准差、min/max 供解释与审计。

MAD 退化为 0（离散度极低）时，使用“中位数万分之一、至少 1ns”的尺度下限，
避免 z 分数被零除放大。

---

## 2. 回归判定规则（`Gate`）

方向约定：**耗时越小越好**。`ratio = 当前中位数 / 基线中位数`。
稳健 z 分数把变化量与“基线 + 当前”双方的合并波动尺度比较：

```
z = (median_current - median_baseline) / sqrt(sigma_base² + sigma_current²)
sigma = max(MAD × 1.4826, median × 1e-4, 1ns)
```

默认规则（`DefaultGateConfig`）采用**幅度 + 显著性双条件**：

| 结论 | 条件 | 是否门禁失败 |
| --- | --- | --- |
| `regression` | `ratio ≥ 1.10` **且** `z ≥ 2.0` | ✅ 失败 |
| `improvement` | `ratio ≤ 0.90` **且** `z ≤ -2.0` | ❌ 不算失败 |
| `stable` | 幅度或显著性未同时达标（正常波动） | ❌ 不算失败 |
| `no_baseline` | 尚无基线 | ❌（首次自动建立基线） |
| `invalid_run` | 有效样本 < 3，或工作负载未能执行 | ✅ 失败 |

设计含义：

- **大噪声下的偶然偏移不会被误报**——例如慢了 15% 但样本本身高度离散
  （z 只有 0.5），判 `stable`；
- **小而稳定的漂移也不会被夸大**——稳定地慢 5%（z 很大）但低于 10% 幅度阈值，
  仍判 `stable`；
- 每个 `Decision` 都带人类可读的 `Reasons`，打印比值、z 分数、阈值与双方中位数，
  结论可解释。阈值可通过 `GateConfig` 调整。

---

## 3. 基线管理（`BaselineManager` + `FileBaselineStore`）

基线以 JSON 文件存储，内容包含统计量、原始样本，以及建立时的
**配置哈希、依赖哈希、运行环境指纹**。

更新规则（`Update`）：

1. **指纹一致性检查**：配置 / 依赖 / 环境任一变化 → 拒绝更新
   （`fingerprint_mismatch`）。旧环境下的基线与新数据不可比，
   应当重新 `Establish`，而不是覆盖。
2. **回退保护**：判定为 `regression` 且未显式强制 → **明确拒绝**
   （`regression_rejected`），旧基线原样保留并返回原因；
   系统不会静默用更差的数字覆盖基线。
3. **强制接受需留痕**：`force=true` 时必须提供非空 `reason`
   （`force_reason_required`），理由写入基线 `Note` 与审计记录。
4. `stable` / `improvement` 允许刷新基线；`CreatedAt` 保留、`UpdatedAt` 推进。
5. 首次评估（无基线且有效样本足够）自动 `Establish`。

文件写入采用“随机临时文件 + 原子 rename”，并发更新不会产生半写/互相覆盖。

---

## 4. 缓存与增量重跑（`FileResultCache`）

缓存键为五个维度的组合：

```
CacheKey = { WorkloadID, SourceHash, ConfigHash, DepsHash, EnvFingerprint }
```

- **未变化不重算**：键完全一致时直接复用上次采样结果（审计记录 `cache_hit=true`）。
- **变化必失效**：
  - 工作负载实现变化 → 更新 `SourceHash` → 未命中；
  - 运行配置变化（Runs/Iterations/Concurrency/Timeout/Warmup）→ `ConfigHash` 变；
  - 依赖指纹变化（顺序无关，哈希前排序）→ `DepsHash` 变；
  - 运行环境变化（GOOS/GOARCH/CPU 核数/GOMAXPROCS/Go 版本）→ 环境指纹变。
- 缓存条目按“工作负载 + 键哈希”命名，同一工作负载的不同配置/环境互不覆盖；
  `Invalidate(workloadID)` 清除该工作负载的全部条目。
- 全失败 / 中止的运行不写缓存，下次自动重试。

> 注意：缓存只跳过**采样**；判定与基线策略每次评估都会重新执行
> （基线可能被人工调整或强制更新过）。

---

## 5. 审计与可复现（`JSONLAuditLog`）

每次评估对每个工作负载追加一行 JSON（JSON Lines，不可变），记录：

时间戳、工作负载、结论、`ratio`、`zscore`、当前/基线统计、
样本数、失败明细、缓存键、`cache_hit`、判定依据 `reasons`、
基线操作（`establish` / `update` / `rejected`）及备注。

因此任意历史结论都可以回答：**用什么代码/配置/环境、采了哪些样本、
依据什么统计量、得到什么判定、基线是否被更新或拒绝**。
读取时遇到损坏行会直接报错，而不是静默丢失历史。

---

## 6. 本地验证

要求 Go 1.25+。

```bash
# 构建与静态检查
go build ./...
go vet ./...

# 全部单测（确定性测试，可反复运行）
go test -v ./...

# 多轮验证稳定性与可复现性
go test -count=5 ./...

# 如环境支持 cgo，可加竞态检测
CGO_ENABLED=1 go test -race ./...
```

### 可运行示例

```bash
# 第一次：建立基线（verdict=no_baseline），产物在 .benchstate/
go run ./example

# 第二次：命中缓存、与基线一致（verdict=stable，GATE OK）
go run ./example

# 人为放慢 25% 实现（SourceHash 变化触发重算）：
# encode.1k 判 regression，基线更新被拒绝，进程退出码为 1
go run ./example -slowdown 0.25

# 查看审计轨迹
cat .benchstate/audit.jsonl | jq .

# 从零开始
rm -rf .benchstate
```

### 测试覆盖

| 测试 | 覆盖点 |
| --- | --- |
| `TestComputeStats` | 中位数/MAD/截尾均值抗离群点 |
| `TestGateVerdicts` / `TestGateSpecialCases` | 回退/改善/噪声/样本不足/无基线边界 |
| `TestRunnerSamplesAttributable` | 并发采样的归因正确性 |
| `TestRunnerFailureKinds` | panic/超时/无效/错误/未注册可区分且被隔离 |
| `TestRunnerWarmupAndRepeatability` | 预热丢弃、重复运行一致 |
| `TestBaselineEstablishAndRejectRegression` | 建立/重复建立/回退拒绝/强制留痕 |
| `TestBaselineFingerprintMismatch` | 配置/依赖/环境变化拒绝覆盖 |
| `TestCacheHitsAndInvalidation` | 命中、各维度失效、顺序无关、清除 |
| `TestServiceNoiseIsNotRegression` | ±4% 噪声连续 6 轮零误报 |
| `TestServiceClearRegressionDetectedAndRejected` | 25% 回退检出且基线受保护 |
| `TestServiceIncrementalRerunUsesCache` | 增量重跑零重算、配置变化强制重算 |
| `TestServiceFailedWorkloadExcluded` | 失败负载不影响健康负载，门禁只在失败负载上失败 |
| `TestConcurrentEvaluations` | 8 路并发评估、审计行完整无损坏 |
| `TestAuditLogRoundTrip` | 审计记录可回放、可复现 |

单测日志（`go test -v`）会打印样本数、中位数/MAD、ratio、z 分数、
判定依据与最终结论。

---

## 7. 包结构

| 文件 | 职责 |
| --- | --- |
| `workload.go` | `Workload` / `Sample` / `RunFailure` / `Registry`、失败原因枚举 |
| `stats.go` | 稳健统计量与 ratio / z 分数 |
| `runner.go` | 并发、重复、带超时与预热的采样执行 |
| `gate.go` | 阈值 + 显著性的回归判定与可解释结论 |
| `baseline.go` | 基线 JSON 存储、指纹检查、受保护的更新 |
| `cache.go` | 多维缓存键、环境指纹、文件缓存与失效 |
| `audit.go` | JSONL 追加式审计日志 |
| `service.go` | 编排：采样（缓存）→ 判定 → 基线策略 → 审计 |
| `example/` | 可运行的端到端演示 |
