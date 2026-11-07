package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

func TestHistoryResetPredecessorOlderThanPointLead(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
		e, _ := newTestEngine(t, standardHost(c), c)
		from := c.t.Add(-time.Hour).Unix()
		at := from - 48*3600
		reset := c.t.Add(time.Hour).Unix()
		latest, inRange := reset+30, reset+45
		if unknown {
			latest, inRange = 0, 0
		}
		if err := e.history.Append(
			store.Record{Kind: "s", Time: at - 24*3600, Group: "g", Window: "five-hour", Reset: reset},
			store.Record{Kind: "s", Time: from + 30, Group: "g", Window: "five-hour", Remaining: 80, Reset: inRange},
			store.Record{Kind: "s", Time: at, Group: "g", Window: "five-hour", Remaining: 90, Reset: latest},
		); err != nil {
			t.Fatal(err)
		}
		result, err := e.History(time.Hour, "")
		if err != nil || len(result.Series) != 1 {
			t.Fatalf("history=%+v err=%v", result, err)
		}
		series := result.Series[0]
		if want := [][2]int64{{at, latest}}; !slices.Equal(series.Resets, want) {
			t.Fatalf("unknown=%v resets=%v want=%v", unknown, series.Resets, want)
		}
		if len(series.Points) != 1 || int64(series.Points[0][0]) != from+30 {
			t.Fatalf("old reset predecessor entered the points lead: %v", series.Points)
		}
	}
}
