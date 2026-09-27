package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// AuditRecord 是一条不可变的历史判定记录，使结论可审计、可复现。
type AuditRecord struct {
	Timestamp   time.Time    `json:"timestamp"`
	WorkloadID  string       `json:"workload_id"`
	RunSeq      int          `json:"run_seq"`
	Verdict     Verdict      `json:"verdict"`
	Ratio       float64      `json:"ratio"`
	ZScore      float64      `json:"zscore"`
	Current     Stats        `json:"current"`
	Baseline    *Stats       `json:"baseline,omitempty"`
	CacheHit    bool         `json:"cache_hit"`
	Key         CacheKey     `json:"cache_key"`
	Reasons     []string     `json:"reasons"`
	Failures    []RunFailure `json:"failures,omitempty"`
	SampleCount int          `json:"sample_count"`
	BaselineOp  string       `json:"baseline_op,omitempty"` // "establish" | "update" | "rejected"
	Note        string       `json:"note,omitempty"`
}

// AuditLog 追加式审计日志。
type AuditLog interface {
	Append(ctx context.Context, rec AuditRecord) error
	ReadAll(ctx context.Context) ([]AuditRecord, error)
}

// JSONLAuditLog 将记录以 JSON Lines 追加落盘（每行一条，历史不可变）。
type JSONLAuditLog struct {
	path string
}

// NewJSONLAuditLog 创建/打开 JSONL 审计日志（父目录不存在时自动创建）。
func NewJSONLAuditLog(path string) (*JSONLAuditLog, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &JSONLAuditLog{path: path}, nil
}

// Append 以 O_APPEND 写入一行 JSON。
func (l *JSONLAuditLog) Append(ctx context.Context, rec AuditRecord) error {
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	line, err := MarshalJSONLine(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// ReadAll 读取全部历史记录（损坏行返回错误而非静默丢弃，保证审计完整）。
func (l *JSONLAuditLog) ReadAll(ctx context.Context) ([]AuditRecord, error) {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var records []AuditRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec AuditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, &AuditCorruptError{Path: l.path, Line: lineNo, Err: err}
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// AuditCorruptError 标记审计日志中无法解析的行。
type AuditCorruptError struct {
	Path string
	Line int
	Err  error
}

func (e *AuditCorruptError) Error() string {
	return "corrupt audit log " + e.Path + " at line " + strconv.Itoa(e.Line) + ": " + e.Err.Error()
}

// MarshalJSONLine 将一条审计记录编码为单行 JSON。
func MarshalJSONLine(rec AuditRecord) ([]byte, error) {
	return json.Marshal(rec)
}
