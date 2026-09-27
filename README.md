# benchmark-regression-gate-go

一套可复现的性能基准与回归判定工具：工作负载注册与并发采样、鲁棒统计、
噪声感知的回归门禁、结果缓存与增量重跑、带防回退保护的基线管理。

## 基准模型

```
bench     工作负载注册表 + 并发采样执行器（样本可归因、失败可区分）
stats     统计汇总（中位数/均值/标准差/CV）与回归判定规则
baseline  基线存储（JSON 文件，原子写入，回退拒绝）
cache     结果缓存（指纹键控，配置/依赖/环境变化即失效）
gate      编排层：缓存查找 → 采样 → 统计 → 判定 → 审计报告
cmd/benchgate  CLI 入口
```

**工作负载**通过 `bench.Registry` 注册，包含名称、可序列化配置（参与指纹计算）
和 `Run func(ctx) (int64, error)`（返回单次操作耗时，单位纳秒）。

**采样模型**：`Runner` 将 `Iterations` 个迭代分发到 `Concurrency` 个 worker，
每个样本携带 `Workload / Iter / Worker / StartedAt`，并发与重复运行下均可归因。
预热样本（`Warmup`）顺序执行并丢弃。所有失败模式被分类且**绝不混入统计**：

| 原因 | 含义 |
|---|---|
| `panic` | 工作负载 panic（已 recover） |
| `timeout` | 单样本超过 `Timeout` |
| `execution_error` | 工作负载返回 error |
| `invalid_sample` | 返回非正耗时 |
| `canceled` | 父 context 取消 |
| `workload_not_found` / `no_valid_samples` / `bad_options` | 运行级失败 |

## 回归判定规则

判定基于**中位数比值**（`ratio = 当前中位数 / 基线中位数`），中位数对离群值鲁棒：

- **噪声下限** `noise_floor = max(NoiseBand, 基线CV, 当前CV)`：运行越 noisy，容忍越宽；
- **回归阈值** `threshold = max(noise_floor, MinRegression)`：只有明确超过阈值的
  变慢才判定为 `regression`（门禁失败）；
- `ratio < 1 - ImprovementFraction` 判定为 `improved`；其余为 `stable`；
  改善与正常波动**永远不会**被判为失败；
- 任一侧 CV 超过 `NoiseCap`（默认 0.5）时判定为 `inconclusive_noise`——
  过于噪声的运行不可信，但**不构成失败**；
- 无基线或无有效样本分别给出 `no_baseline` / `unknown`，与真实回退可区分。

每个 `Decision` 都携带 ratio、噪声下限、阈值与文字理由，结论可解释、可复现。

## 缓存与基线管理

**指纹**：`sha256(工作负载名 + 配置JSON + 依赖表 + 运行环境)`，其中运行环境包含
Go 版本、GOOS/GOARCH、CPU 数。任何一项变化都会产生新指纹：

- **缓存**（默认 `.benchgate/cache.json`）：指纹命中则直接复用历史样本，
  未变化的工作负载绝不重复计算；配置、依赖或环境变化自动失效并重采样。
  缓存条目保留采样环境，历史判定可审计。
- **基线**（默认 `.benchgate/baseline.json`）：原子写入（临时文件 + rename）。
  更新规则：
  - 变快或持平 → 接受；
  - 变慢超过 `Tolerance` → **明确拒绝**（不会静默覆盖），必须显式
    `--allow-regression` 才会被记录，且理由可审计；
  - 指纹变化 → 视为新配置域，重置该工作负载的基线；
  - 指纹不匹配的基线不参与判定（判定为 `no_baseline`，提示重新建立基线）。

## 本地验证

```bash
# 运行全部测试（日志会打印样本、统计依据与最终判定）
go test ./... -v

# 含竞态检测
CGO_ENABLED=1 go test ./... -race -count=1

# CLI 冒烟：建立基线 → 门禁运行 → 再次运行（命中缓存）
go build -o benchgate ./cmd/benchgate
./benchgate baseline -iter 20 -conc 4
./benchgate run -iter 20 -conc 4          # 退出码 0=PASS, 1=回归失败
./benchgate run -iter 20 -conc 4          # 第二次命中缓存，结果一致
./benchgate run -no-cache                 # 强制重采样
./benchgate baseline --allow-regression   # 显式接受基线回退
```

常用参数：`-iter`（样本数）、`-conc`（并发 worker）、`-warmup`、`-timeout`
（单样本超时）、`-noise`（噪声容忍下限）、`-min-regression`（最小回归幅度）。
