package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

func TestHistoryResetsPreserveRawChangesAndRangePredecessor(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	c := &clock{t: now}
	e, _ := newTestEngine(t, standardHost(c), c)
	from := now.Add(-8 * 24 * time.Hour)
	reset := from.Add(5 * time.Hour).Unix()
	times := []int64{from.Add(-48 * time.Hour).Unix(), from.Add(-time.Hour).Unix(), from.Unix() + 1, from.Unix() + 2, from.Unix() + 3, from.Unix() + 4, from.Unix() + 5, from.Unix() + 6}
	resets := []int64{reset, reset + 30, reset + 90, reset - 3600, reset - 3570, 0, 0, reset + 7200}
	records := []store.Record{}
	for i, at := range times {
		records = append(records, store.Record{Kind: "s", Time: at, Group: "codex:1:gpt", Window: "five-hour", Remaining: 80 - float64(i), Reset: resets[i]})
	}
	// 无重置时间的系列必须输出 []，避免把未知时间当作有效周期。
	records = append(records, store.Record{Kind: "s", Time: from.Unix() + 1, Group: "codex:2:gpt", Window: "five-hour", Remaining: 80})
	if err := e.history.Append(records...); err != nil {
		t.Fatal(err)
	}
	resp, err := e.History(8*24*time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Series) != 2 {
		t.Fatalf("series: %+v", resp.Series)
	}
	var got Series
	for _, series := range resp.Series {
		if series.Group == "codex:1:gpt" {
			got = series
		} else if series.Resets == nil || len(series.Resets) != 0 {
			t.Fatalf("unknown resets must be an empty array: %+v", series)
		}
	}
	want := [][2]int64{{times[0], reset}, {times[3], reset - 3600}, {times[5], 0}, {times[7], reset + 7200}}
	if !slices.Equal(got.Resets, want) {
		t.Fatalf("resets=%v want=%v", got.Resets, want)
	}
	if len(got.Points) != 2 || int64(got.Points[1][0]) != times[7] {
		t.Fatalf("point downsampling changed: %v", got.Points)
	}
	// 最早重置事件比 points 的一天前导范围更早，仍需保留其实际生效时间。
	if got.Resets[0][0] >= from.Add(-24*time.Hour).Unix() {
		t.Fatal("reset predecessor was replaced with point lead time")
	}
}

func TestHistoryResetsIncludeUnknownStateAtRangeStart(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	e, _ := newTestEngine(t, standardHost(c), c)
	from := c.t.Add(-time.Hour).Unix()
	e.history.Append(
		store.Record{Kind: "s", Time: from - 100, Group: "g", Window: "five-hour", Reset: from + 1000},
		store.Record{Kind: "s", Time: from - 50, Group: "g", Window: "five-hour"},
		store.Record{Kind: "s", Time: from + 50, Group: "g", Window: "five-hour"},
	)
	resp, err := e.History(time.Hour, "")
	if err != nil || len(resp.Series) != 1 {
		t.Fatalf("history=%+v err=%v", resp, err)
	}
	if want := [][2]int64{{from - 50, 0}}; !slices.Equal(resp.Series[0].Resets, want) {
		t.Fatalf("resets=%v want=%v", resp.Series[0].Resets, want)
	}
}
