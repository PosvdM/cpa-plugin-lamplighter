package store

import (
	"context"
	"sort"
	"time"
)

// HistoryWindow 只保留范围内记录，以及每个系列在范围前的最后一个样本。
// KnownReset 标记系列在保留历史中是否出现过非零重置时间。
type HistoryWindow struct {
	Records    []Record
	Before     map[string]Record
	KnownReset map[string]bool
}

// QueryWindow 单遍扫描保留历史，内存为 O(系列数 + 范围内记录数)。
// 前驱按时间取最大值，同时间取最后补写的记录；仅对范围内记录稳定排序。
func (h *History) QueryWindow(ctx context.Context, from, to time.Time) (HistoryWindow, error) {
	out := HistoryWindow{Before: map[string]Record{}, KnownReset: map[string]bool{}}
	err := h.Scan(ctx, time.Time{}, to, func(record Record) error {
		if record.Kind != "e" {
			key := record.Group + "|" + record.Window
			if record.Kind == "s" && record.Reset != 0 {
				out.KnownReset[key] = true
			}
			if record.Time < from.Unix() {
				previous, exists := out.Before[key]
				if !exists || record.Time >= previous.Time {
					out.Before[key] = record
				}
				return nil
			}
		} else if record.Time < from.Unix() {
			return nil
		}
		out.Records = append(out.Records, record)
		return nil
	})
	if err != nil {
		return HistoryWindow{}, err
	}
	sort.SliceStable(out.Records, func(i, j int) bool { return out.Records[i].Time < out.Records[j].Time })
	if err := ctx.Err(); err != nil {
		return HistoryWindow{}, err
	}
	return out, nil
}
