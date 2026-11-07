package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestHistoryScanCancellationDuringRead(t *testing.T) {
	now := time.Now().UTC()
	h := &History{Dir: t.TempDir()}
	records := make([]Record, 1000)
	for i := range records {
		records[i] = Record{Kind: "s", Time: now.Unix(), Group: "g", Window: "w", Remaining: float64(i)}
	}
	if err := h.Append(records...); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	err := h.Scan(ctx, now.Add(-time.Hour), now, func(Record) error {
		seen++
		if seen == 100 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 100 {
		t.Fatalf("seen=%d err=%v", seen, err)
	}
}

func TestHistoryQuerySampledKeepsLatestAndBoundsMemory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	h := &History{Dir: t.TempDir()}
	if err := h.Append(
		Record{Kind: "s", Time: now.Unix() + 20, Group: "g", Window: "w", Remaining: 20},
		Record{Kind: "s", Time: now.Unix() + 10, Group: "g", Window: "w", Remaining: 10},
		Record{Kind: "s", Time: now.Unix() + 80, Group: "g", Window: "w", Remaining: 80},
		Record{Kind: "e", Time: now.Unix() + 30, Group: "g", Message: "ignored event"},
	); err != nil {
		t.Fatal(err)
	}
	got, err := h.QuerySampled(context.Background(), now, now.Add(time.Hour), 60, nil)
	if err != nil || len(got) != 2 || got[0].Remaining != 20 || got[1].Remaining != 80 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	records := make([]Record, 20001)
	for i := range records {
		records[i] = Record{Kind: "s", Time: now.Unix(), Group: fmt.Sprint(i), Window: "w"}
	}
	if err := h.Append(records...); err != nil {
		t.Fatal(err)
	}
	if _, err := h.QuerySampled(context.Background(), now, now.Add(time.Hour), 60, nil); !errors.Is(err, ErrHistoryLimit) {
		t.Fatal("query must reject more than 20000 retained buckets")
	}
	filtered, err := h.QuerySampled(context.Background(), now, now.Add(time.Hour), 60, func(r Record) bool { return r.Group == "g" })
	if err != nil || len(filtered) != 2 {
		t.Fatalf("unselected series consumed the memory budget: %d %v", len(filtered), err)
	}
}
