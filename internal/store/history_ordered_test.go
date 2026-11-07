package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestHistoryOrderedScanBoundsOutsideRangeBuffer(t *testing.T) {
	var previous orderedScanStats
	for _, count := range []int{4096, 32768} {
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		h := &History{Dir: t.TempDir()}
		batch := make([]Record, 0, 256)
		for i := 0; i < count; i++ {
			// 全部位于一小时查询范围之外，并故意乱序及重复时间。
			batch = append(batch, Record{Kind: "s", Time: now.Add(-48*time.Hour).Unix() + int64(999-i%1000), Group: "g", Window: "five-hour", Remaining: float64(i), Reset: now.Unix()})
			if len(batch) == cap(batch) {
				if err := h.Append(batch...); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
		stats := orderedScanStats{}
		seen := 0
		var last Record
		err := h.scanOrdered(context.Background(), time.Time{}, now, nil, func(record Record) error {
			if seen > 0 && (record.Time < last.Time || record.Time == last.Time && record.Remaining < last.Remaining) {
				t.Fatalf("unstable ordering: previous=%+v next=%+v", last, record)
			}
			seen++
			last = record
			return nil
		}, &stats)
		if err != nil || seen != count {
			t.Fatalf("seen=%d err=%v", seen, err)
		}
		// 断言的是同时保留的记录数及字节数，累计分配量会随扫描量增长。
		if stats.maxRecords > orderedChunkRecords || stats.maxBytes > orderedChunkBytes || stats.maxMergeInputs > 2 {
			t.Fatalf("unbounded working set: %+v", stats)
		}
		if previous.maxRecords != 0 && (stats.maxRecords != previous.maxRecords || stats.maxMergeInputs != previous.maxMergeInputs) {
			t.Fatalf("outside-range growth increased the record working set: small=%+v large=%+v", previous, stats)
		}
		t.Logf("scanned=%d buffered_records=%d buffered_bytes=%d merge_inputs=%d", count, stats.maxRecords, stats.maxBytes, stats.maxMergeInputs)
		previous = stats
	}
}

func TestHistoryOrderedScanCancellationAndScratchCleanup(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	t.Setenv("TMPDIR", scratch)
	h := &History{Dir: t.TempDir()}
	now := time.Now().UTC()
	records := make([]Record, 3000)
	for i := range records {
		records[i] = Record{Kind: "s", Time: now.Unix() - int64(i), Group: "g", Window: "w"}
	}
	if err := h.Append(records...); err != nil {
		t.Fatal(err)
	}
	for _, duringScan := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		count := 0
		err := h.ScanOrdered(ctx, time.Time{}, now, func(Record) bool {
			if duringScan {
				count++
				if count == 2000 {
					cancel()
				}
			}
			return true
		}, func(Record) error {
			if !duringScan {
				cancel()
			}
			return nil
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel during scan=%v err=%v", duringScan, err)
		}
		entries, err := os.ReadDir(scratch)
		if err != nil || len(entries) != 0 {
			t.Fatalf("scratch files leaked: %v %v", entries, err)
		}
	}
}
