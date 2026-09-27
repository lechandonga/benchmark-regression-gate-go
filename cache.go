package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// CacheKey 唯一标识一组“工作负载实现 + 配置 + 依赖 + 运行环境”下的采样。
// 任一字段变化都会得到不同的键，即触发重算；未变化则直接命中缓存。
type CacheKey struct {
	WorkloadID     string `json:"workload_id"`
	SourceHash     string `json:"source_hash"`
	ConfigHash     string `json:"config_hash"`
	DepsHash       string `json:"deps_hash"`
	EnvFingerprint string `json:"env_fingerprint"`
}

// String 返回稳定的缓存键字符串（字段经 JSON 规范序列化，与机器无关）。
func (k CacheKey) String() string {
	b, _ := json.Marshal(k)
	return string(b)
}

// Hash 返回该键的短哈希，用作缓存文件名。
func (k CacheKey) Hash() string {
	sum := sha256.Sum256([]byte(k.String()))
	return hex.EncodeToString(sum[:16])
}

// CacheEntry 是缓存的采样结果。
type CacheEntry struct {
	Key       CacheKey   `json:"key"`
	Report    *RunReport `json:"report"`
	CreatedAt time.Time  `json:"created_at"`
	Hit       bool       `json:"-"`
}

// ResultCache 支持结果缓存与增量重跑。
type ResultCache interface {
	Get(ctx context.Context, key CacheKey) (*CacheEntry, bool, error)
	Put(ctx context.Context, entry CacheEntry) error
	Invalidate(ctx context.Context, workloadID string) error
}

// stableHash 对任意可 JSON 序列化的值计算稳定哈希。
func stableHash(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		// 退化为字符串表示，保证仍可计算。
		b = []byte(fmt.Sprintf("%#v", v))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashConfig 对运行配置做稳定序列化哈希。
func HashConfig(cfg RunConfig) string {
	return stableHash(struct {
		Runs             int   `json:"runs"`
		Iterations       int   `json:"iterations"`
		Concurrency      int   `json:"concurrency"`
		IterationTimeout int64 `json:"iteration_timeout_nanos"`
		Warmup           int   `json:"warmup"`
	}{
		Runs:             cfg.Runs,
		Iterations:       cfg.Iterations,
		Concurrency:      cfg.Concurrency,
		IterationTimeout: int64(cfg.IterationTimeout),
		Warmup:           cfg.Warmup,
	})
}

// HashDeps 对工作负载声明的依赖指纹做稳定哈希。
// 先对依赖列表排序，保证声明顺序不影响缓存命中；
// 空列表与 nil 视为等价（无外部依赖）。
func HashDeps(deps []string) string {
	cp := append([]string(nil), deps...)
	sort.Strings(cp)
	return stableHash(cp)
}

// EnvFingerprint 返回运行环境指纹：GOOS/GOARCH、CPU 核数、Go 版本。
// GOMAXPROCS 也纳入其中，因为它直接影响并发工作负载的计时行为。
func EnvFingerprint() string {
	parts := []string{
		"goos=" + runtime.GOOS,
		"goarch=" + runtime.GOARCH,
		"cpu=" + fmt.Sprintf("%d", runtime.NumCPU()),
		"gomaxprocs=" + fmt.Sprintf("%d", runtime.GOMAXPROCS(0)),
		"goversion=" + runtime.Version(),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:12])
}

// FileResultCache 是基于 JSON 文件目录的缓存实现。
// 文件名使用 CacheKey 的哈希，同一工作负载不同配置/环境的缓存互不覆盖。
type FileResultCache struct {
	dir string
}

// NewFileResultCache 创建文件缓存（目录不存在时自动创建）。
func NewFileResultCache(dir string) (*FileResultCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	return &FileResultCache{dir: dir}, nil
}

func (c *FileResultCache) path(key CacheKey) string {
	return filepath.Join(c.dir, safeFileName(key.WorkloadID)+"-"+key.Hash()+".json")
}

// globPrefix 返回某工作负载的全部缓存文件模式。
func (c *FileResultCache) globPrefix(workloadID string) string {
	return filepath.Join(c.dir, safeFileName(workloadID)+"-*.json")
}

func (c *FileResultCache) Get(ctx context.Context, key CacheKey) (*CacheEntry, bool, error) {
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false, fmt.Errorf("decode cache entry %s: %w", key.Hash(), err)
	}
	entry.Hit = true
	return &entry, true, nil
}

// writeFileAtomic 通过带随机后缀的临时文件 + rename 原子落盘，
// 避免并发写同一目标时在固定 .tmp 路径上互相覆盖。
func writeFileAtomic(target string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (c *FileResultCache) Put(ctx context.Context, entry CacheEntry) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path(entry.Key), data)
}

// Invalidate 删除某工作负载在所有配置/环境下的缓存条目。
func (c *FileResultCache) Invalidate(ctx context.Context, workloadID string) error {
	matches, err := filepath.Glob(c.globPrefix(workloadID))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
