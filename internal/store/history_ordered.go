package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const orderedChunkRecords = 1024
const orderedChunkBytes = 1 << 20

type orderedLine struct {
	at  int64
	raw []byte
}

// orderedScanStats 记录归并工作集的上限，供内存边界测试使用。
type orderedScanStats struct {
	maxRecords     int
	maxBytes       int
	maxMergeInputs int
}

// ScanOrdered 按时间稳定地流式访问筛选后的记录。排序块最多 1024 条，
// 目标大小为 1 MiB，超过目标的单条记录独占一块。超出的块在私有临时目录中
// 两路归并，完成或取消时删除临时文件。
// 同一时间的记录保留文件内原始顺序，兼容延迟补写的被动样本。
func (h *History) ScanOrdered(ctx context.Context, from, to time.Time, accept func(Record) bool, visit func(Record) error) error {
	return h.scanOrdered(ctx, from, to, accept, visit, nil)
}

func (h *History) scanOrdered(ctx context.Context, from, to time.Time, accept func(Record) bool, visit func(Record) error, stats *orderedScanStats) error {
	chunk := make([]orderedLine, 0, orderedChunkRecords)
	size := 0
	var runs [64]string
	dir := ""
	defer func() {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}()
	serial := 0
	nextPath := func() (string, error) {
		if dir == "" {
			var err error
			dir, err = os.MkdirTemp("", "lamplighter-history-")
			if err != nil {
				return "", err
			}
		}
		serial++
		return filepath.Join(dir, fmt.Sprintf("%d.jsonl", serial)), nil
	}
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		sort.SliceStable(chunk, func(i, j int) bool { return chunk[i].at < chunk[j].at })
		path, err := nextPath()
		if err != nil {
			return err
		}
		err = writeOrderedRun(ctx, path, func(emit func([]byte) error) error {
			for _, line := range chunk {
				if err := emit(line.raw); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		clear(chunk)
		chunk = chunk[:0]
		size = 0
		for level := range runs {
			if runs[level] == "" {
				runs[level] = path
				return nil
			}
			merged, err := nextPath()
			if err != nil {
				return err
			}
			if stats != nil {
				stats.maxMergeInputs = 2
			}
			if err := mergeOrderedRuns(ctx, runs[level], path, merged); err != nil {
				return err
			}
			if err := os.Remove(runs[level]); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			runs[level] = ""
			path = merged
		}
		return fmt.Errorf("history exceeds ordered scan capacity")
	}
	err := h.Scan(ctx, from, to, func(record Record) error {
		if accept != nil && !accept(record) {
			return nil
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if len(chunk) > 0 && (len(chunk) == orderedChunkRecords || size+len(raw) > orderedChunkBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		chunk = append(chunk, orderedLine{record.Time, raw})
		size += len(raw)
		if stats != nil {
			stats.maxRecords = max(stats.maxRecords, len(chunk))
			stats.maxBytes = max(stats.maxBytes, size)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if dir == "" {
		sort.SliceStable(chunk, func(i, j int) bool { return chunk[i].at < chunk[j].at })
		for _, line := range chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			var record Record
			if err := json.Unmarshal(line.raw, &record); err != nil {
				return err
			}
			if err := visit(record); err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	if err := flush(); err != nil {
		return err
	}
	path := ""
	// 高层分段更早，合并时置于左侧，以保留相同时间的原始顺序。
	for level := len(runs) - 1; level >= 0; level-- {
		if runs[level] == "" {
			continue
		}
		if path == "" {
			path = runs[level]
			continue
		}
		merged, err := nextPath()
		if err != nil {
			return err
		}
		if stats != nil {
			stats.maxMergeInputs = 2
		}
		if err := mergeOrderedRuns(ctx, path, runs[level], merged); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Remove(runs[level]); err != nil {
			return err
		}
		path = merged
	}
	reader, err := openOrderedRun(path)
	if err != nil {
		return err
	}
	defer reader.file.Close()
	for reader.next(ctx) {
		var record Record
		if err := json.Unmarshal(reader.line.raw, &record); err != nil {
			return err
		}
		if err := visit(record); err != nil {
			return err
		}
	}
	return reader.err
}

type orderedRunReader struct {
	file    *os.File
	scanner *bufio.Scanner
	line    orderedLine
	err     error
}

func openOrderedRun(path string) (*orderedRunReader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	// 原始行最多 1 MiB；JSON 重新编码的 HTML 转义最多扩大六倍。
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &orderedRunReader{file: file, scanner: scanner}, nil
}
func (r *orderedRunReader) next(ctx context.Context) bool {
	if r.err = ctx.Err(); r.err != nil {
		return false
	}
	if !r.scanner.Scan() {
		r.err = r.scanner.Err()
		return false
	}
	var stamp struct {
		Time int64 `json:"t"`
	}
	r.err = json.Unmarshal(r.scanner.Bytes(), &stamp)
	if r.err != nil {
		return false
	}
	r.line = orderedLine{stamp.Time, r.scanner.Bytes()}
	return true
}
func writeOrderedRun(ctx context.Context, path string, produce func(func([]byte) error) error) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	err = produce(func(raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
		return writer.WriteByte('\n')
	})
	if err == nil {
		err = writer.Flush()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func mergeOrderedRuns(ctx context.Context, left, right, out string) error {
	a, err := openOrderedRun(left)
	if err != nil {
		return err
	}
	defer a.file.Close()
	b, err := openOrderedRun(right)
	if err != nil {
		return err
	}
	defer b.file.Close()
	return writeOrderedRun(ctx, out, func(emit func([]byte) error) error {
		haveA, haveB := a.next(ctx), b.next(ctx)
		for haveA || haveB {
			if haveA && (!haveB || a.line.at <= b.line.at) {
				if err := emit(a.line.raw); err != nil {
					return err
				}
				haveA = a.next(ctx)
			} else {
				if err := emit(b.line.raw); err != nil {
					return err
				}
				haveB = b.next(ctx)
			}
		}
		if a.err != nil {
			return a.err
		}
		return b.err
	})
}
