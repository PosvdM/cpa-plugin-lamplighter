package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrHistoryLimit 表示查询超过历史扫描的内存预算。
var ErrHistoryLimit = errors.New("History query exceeds the 20000-bucket memory limit")

// Record is one line of a history file: a quota sample or an event.
type Record struct {
	Kind      string  `json:"k"`           // "s" for a sample, "e" for an event
	Time      int64   `json:"t"`           // Unix seconds
	Group     string  `json:"g,omitempty"` // quota group key
	Window    string  `json:"w,omitempty"` // window ID, samples only
	Remaining float64 `json:"r,omitempty"` // remaining percent, samples only
	Reset     int64   `json:"x,omitempty"` // reset time in Unix seconds, samples only
	Source    string  `json:"s,omitempty"` // "active" or "passive", samples only
	Event     string  `json:"e,omitempty"` // event type, events only
	Level     string  `json:"v,omitempty"` // "info", "warn" or "error", events only
	Message   string  `json:"m,omitempty"` // event text, events only
	Label     string  `json:"l,omitempty"` // quota group or source label, events only
	Detail    string  `json:"d,omitempty"` // event text without the label, events only
	// Params holds the values the page builds the event text from in its
	// own language, events only.
	Params map[string]string `json:"p,omitempty"`
}

// History stores records in one JSON Lines file per UTC day under Dir.
type History struct {
	Dir string
	mu  sync.Mutex
}

func (h *History) fileFor(t time.Time) string {
	return filepath.Join(h.Dir, t.UTC().Format("2006-01-02")+".jsonl")
}

// Append writes records to the file of their day.
func (h *History) Append(records ...Record) error {
	if len(records) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		return err
	}
	byFile := map[string][]Record{}
	for _, record := range records {
		path := h.fileFor(time.Unix(record.Time, 0))
		byFile[path] = append(byFile[path], record)
	}
	for path, items := range byFile {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		writer := bufio.NewWriter(file)
		for _, item := range items {
			raw, err := json.Marshal(item)
			if err != nil {
				continue
			}
			writer.Write(raw)
			writer.WriteByte('\n')
		}
		flushErr := writer.Flush()
		closeErr := file.Close()
		if flushErr != nil {
			return flushErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Query returns the records between from and to, oldest first.
func (h *History) Query(from, to time.Time) ([]Record, error) {
	return h.QueryContext(context.Background(), from, to)
}

func (h *History) QueryContext(ctx context.Context, from, to time.Time) ([]Record, error) {
	var out []Record
	err := h.Scan(ctx, from, to, func(record Record) error {
		out = append(out, record)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, ctx.Err()
}

// Scan 逐行读取历史；零值 from 表示读取所有保留文件。回调顺序为文件顺序，
// 同一文件中的补写记录可以乱序。扫描不持有写锁，取消无需等待写入结束。
func (h *History) Scan(ctx context.Context, from, to time.Time, visit func(Record) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(h.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(entry.Name(), ".jsonl"))
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || err != nil || day.After(to) || !from.IsZero() && day.Add(24*time.Hour).Before(from) {
			continue
		}
		err = func() error {
			file, err := os.Open(filepath.Join(h.Dir, entry.Name()))
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var record Record
				if json.Unmarshal(scanner.Bytes(), &record) != nil || !from.IsZero() && record.Time < from.Unix() || record.Time > to.Unix() {
					continue
				}
				if err := visit(record); err != nil {
					return err
				}
			}
			return scanner.Err()
		}()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// QuerySampled 在扫描时仅保留每桶的最后一个样本，内存最多容纳 20000 个桶。
// 文件大小和记录数量不影响此上限；超过上限时返回工具错误。
func (h *History) QuerySampled(ctx context.Context, from, to time.Time, bucket int64, accept func(Record) bool) ([]Record, error) {
	type key struct {
		group, window string
		bucket        int64
	}
	kept := map[key]Record{}
	err := h.Scan(ctx, from, to, func(record Record) error {
		if record.Kind != "s" || accept != nil && !accept(record) {
			return nil
		}
		k := key{record.Group, record.Window, record.Time / max(1, bucket)}
		old, exists := kept[k]
		if !exists && len(kept) >= 20000 {
			return ErrHistoryLimit
		}
		if !exists || record.Time >= old.Time {
			kept[k] = record
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(kept))
	for _, record := range kept {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Window < out[j].Window
	})
	return out, ctx.Err()
}

// Prune deletes day files older than retention.
func (h *History) Prune(now time.Time, retention time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	entries, err := os.ReadDir(h.Dir)
	if err != nil {
		return nil
	}
	cutoff := now.UTC().Add(-retention).Truncate(24 * time.Hour)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl"))
		if err != nil {
			continue
		}
		if day.Before(cutoff) {
			os.Remove(filepath.Join(h.Dir, name))
		}
	}
	return nil
}
