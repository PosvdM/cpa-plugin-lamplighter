package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryWindowBoundsOutsideRangeMemory(t *testing.T) {
	for _, count := range []int{4096, 32768} {
		now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
		from := now.Add(-time.Hour)
		h := &History{Dir: t.TempDir()}
		// 最新前驱先写入，后续的旧补写不得覆盖它；未知重置时间也需要保留。
		if err := h.Append(Record{Kind: "s", Time: from.Unix() - 1, Group: "g", Window: "w", Remaining: 123}); err != nil {
			t.Fatal(err)
		}
		batch := make([]Record, 0, 256)
		for i := 0; i < count; i++ {
			batch = append(batch, Record{Kind: "s", Time: now.Add(-2*time.Hour).Unix() + int64(999-i%1000), Group: "g", Window: "w", Remaining: float64(i), Reset: now.Unix()})
			if len(batch) == cap(batch) {
				if err := h.Append(batch...); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
		if err := h.Append(
			Record{Kind: "s", Time: now.Unix() - 10, Group: "g", Window: "w", Remaining: 2},
			Record{Kind: "s", Time: now.Unix() - 20, Group: "g", Window: "w", Remaining: 1},
			Record{Kind: "s", Time: now.Unix() - 20, Group: "g", Window: "w", Remaining: 3},
		); err != nil {
			t.Fatal(err)
		}
		result, err := h.QueryWindow(context.Background(), from, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Before) != 1 || len(result.Records) != 3 || cap(result.Records) > 4 || len(result.KnownReset) != 1 {
			t.Fatalf("outside=%d retained predecessors=%d records=%d capacity=%d known=%d", count, len(result.Before), len(result.Records), cap(result.Records), len(result.KnownReset))
		}
		predecessor := result.Before["g|w"]
		if predecessor.Time != from.Unix()-1 || predecessor.Remaining != 123 || predecessor.Reset != 0 || !result.KnownReset["g|w"] {
			t.Fatalf("wrong predecessor/reset summary: %+v known=%v", predecessor, result.KnownReset)
		}
		if result.Records[0].Remaining != 1 || result.Records[1].Remaining != 3 || result.Records[2].Remaining != 2 {
			t.Fatalf("range order is not stable: %v", result.Records)
		}
		t.Logf("outside=%d retained_predecessors=%d retained_in_range=%d range_capacity=%d", count, len(result.Before), len(result.Records), cap(result.Records))
	}
}

func TestHistoryWindowDoesNotNeedTemporaryDirectory(t *testing.T) {
	now := time.Now().UTC()
	h := &History{Dir: t.TempDir()}
	records := make([]Record, 2048)
	for i := range records {
		records[i] = Record{Kind: "s", Time: now.Add(-2*time.Hour).Unix() + int64(i), Group: "g", Window: "w", Reset: now.Unix()}
	}
	// 同时间的前驱也按最后补写的记录取值。
	records = append(records, Record{Kind: "s", Time: now.Add(-2*time.Hour).Unix() + 2047, Group: "g", Window: "w", Remaining: 88})
	if err := h.Append(records...); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", blocked)
	t.Setenv("TEMP", blocked)
	t.Setenv("TMPDIR", blocked)
	result, err := h.QueryWindow(context.Background(), now.Add(-time.Hour), now)
	if err != nil || result.Before["g|w"].Remaining != 88 {
		t.Fatalf("query depended on a temporary directory or lost same-time predecessor: %+v %v", result, err)
	}
}

type cancelWindowContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelWindowContext) Err() error {
	c.checks++
	if c.checks == 100 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestHistoryWindowCancellationDuringScan(t *testing.T) {
	now := time.Now().UTC()
	h := &History{Dir: t.TempDir()}
	records := make([]Record, 1000)
	for i := range records {
		records[i] = Record{Kind: "s", Time: now.Add(-2*time.Hour).Unix() + int64(i), Group: "g", Window: "w"}
	}
	if err := h.Append(records...); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watched := &cancelWindowContext{Context: ctx, cancel: cancel}
	result, err := h.QueryWindow(watched, now.Add(-time.Hour), now)
	if !errors.Is(err, context.Canceled) || watched.checks < 100 || len(result.Before) != 0 || len(result.Records) != 0 {
		t.Fatalf("cancel result=%+v err=%v checks=%d", result, err, watched.checks)
	}
	if _, err := h.QueryWindow(ctx, now.Add(-time.Hour), now); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-canceled query was accepted")
	}
}
