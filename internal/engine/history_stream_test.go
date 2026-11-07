package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

func TestHistoryShortRangeStreamsResetPredecessorAndUnorderedSamples(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	e, _ := newTestEngine(t, standardHost(c), c)
	from := c.t.Add(-time.Hour).Unix()
	reset := c.t.Add(time.Hour).Unix()
	old := c.t.Add(-40 * 24 * time.Hour).Unix()
	// 大量范围外记录只保留每个系列的最后一个样本，不进入范围内排序。
	batch := make([]store.Record, 0, 128)
	for i := 0; i < 4096; i++ {
		batch = append(batch, store.Record{Kind: "s", Time: old + int64(4096-i), Group: "g", Window: "five-hour", Reset: reset})
		if len(batch) == cap(batch) {
			if err := e.history.Append(batch...); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	changed := from - 48*3600
	if err := e.history.Append(
		store.Record{Kind: "s", Time: from + 20, Group: "g", Window: "five-hour", Remaining: 70, Reset: reset - 1000},
		store.Record{Kind: "s", Time: from + 10, Group: "g", Window: "five-hour", Remaining: 80, Reset: reset + 10040},
		// 变化点及其抖动在较新的样本之后补写，仍需按时间判断相邻变化。
		store.Record{Kind: "s", Time: changed, Group: "g", Window: "five-hour", Reset: reset + 10000},
		store.Record{Kind: "s", Time: changed + 1, Group: "g", Window: "five-hour", Reset: reset + 10030},
		store.Record{Kind: "s", Time: from - 10, Group: "g", Window: "five-hour", Remaining: 90, Reset: reset + 10035},
		store.Record{Kind: "s", Time: old, Group: "retired", Window: "five-hour", Reset: reset},
		store.Record{Kind: "e", Time: from + 15, Group: "g", Event: "ignite", Message: "event"},
	); err != nil {
		t.Fatal(err)
	}
	resp, err := e.History(time.Hour, "")
	if err != nil || len(resp.Series) != 1 {
		t.Fatalf("history=%+v err=%v", resp, err)
	}
	series := resp.Series[0]
	want := [][2]int64{{from - 10, reset + 10035}, {from + 20, reset - 1000}}
	if !slices.Equal(series.Resets, want) {
		t.Fatalf("resets=%v want=%v", series.Resets, want)
	}
	if len(series.Points) != 3 || int64(series.Points[0][0]) != from-10 || int64(series.Points[1][0]) != from+10 || int64(series.Points[2][0]) != from+20 {
		t.Fatalf("range/lead point rules changed: %v", series.Points)
	}
	if len(resp.Events) != 1 || resp.Events[0].Kind != "ignite" {
		t.Fatalf("events=%+v", resp.Events)
	}
}
