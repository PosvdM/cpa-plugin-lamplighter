package engine

import (
	"context"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// geminiReadings are the active queries of the Antigravity Gemini group on
// 2026-11-14 around the weekly reset at 07:00 UTC+8, in Unix seconds: the
// query after the ignition at 07:00:12 reports the ended cycle again.
var geminiReadings = []struct {
	at                int64
	weekly            float64
	weeklyReset       int64
	fiveHour          float64
	fiveHourResetUnix int64
}{
	{1794610500, 94.763625, 1794614702, 100, 1794628500},
	{1794610800, 100, 1795215600, 100, 1794628800},
	{1794610812, 94.761723, 1794614702, 99.993336, 1794628805},
	{1794611100, 100, 1795215900, 100, 1794629100},
	{1794611400, 100, 1795216200, 100, 1794629400},
	{1794611412, 99.99998, 1795216201, 99.99994, 1794629401},
	{1794611701, 99.99809, 1795216201, 99.993336, 1794629401},
}

func TestWeeklyBounceRecordsOneResetAndOneNotification(t *testing.T) {
	c := &clock{t: time.Unix(geminiReadings[0].at, 0).In(shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cred := e.creds["4"]
	key := "antigravity:4:antigravity:gemini-models"
	for _, r := range geminiReadings {
		c.t = time.Unix(r.at, 0).In(shanghai)
		windows := []quota.Window{
			{ID: "gemini-5h", Label: quota.LabelFiveHour, Remaining: r.fiveHour, Reset: time.Unix(r.fiveHourResetUnix, 0)},
			{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: r.weekly, Reset: time.Unix(r.weeklyReset, 0)},
		}
		e.applyGroups(context.Background(), cfg, cred, []quota.RawGroup{{Key: "antigravity:gemini-models", SourceLabel: "Gemini", Windows: windows}}, quota.SourceActive, c.t, c.t, true)
	}
	var resets, notices []Event
	for _, event := range e.events {
		switch event.Kind {
		case "reset", "exhausted":
			resets = append(resets, event)
		case "notify", "notify_failed":
			notices = append(notices, event)
		}
	}
	if len(resets) != 1 || resets[0].Time.Unix() != 1794610800 {
		t.Fatalf("want one reset at 07:00, got %+v", resets)
	}
	p := resets[0].Params
	if resets[0].Group != key || p["window"] != "gemini-weekly" || p["kind"] != "7d" || p["label"] != "7d" || p["reset"] != "2026-11-20T23:00:00Z" {
		t.Fatalf("reset event %+v", resets[0])
	}
	if len(sender.sent) != 1 || len(notices) != 1 {
		t.Fatalf("want one recovery notification, got %+v / %+v", sender.sent, notices)
	}
	n := notices[0]
	if n.Group != key || n.Params["notify_kind"] != "recovery" || n.Params["windows"] != "gemini-weekly" {
		t.Fatalf("notify event %+v", n)
	}
	// History keeps every reading, the bounced one included.
	records, err := e.history.Query(time.Unix(1794610000, 0), time.Unix(1794612000, 0))
	if err != nil {
		t.Fatal(err)
	}
	samples := 0
	for _, record := range records {
		if record.Kind == "s" && record.Window == "gemini-weekly" {
			samples++
		}
	}
	if samples != len(geminiReadings) {
		t.Fatalf("want %d weekly samples, got %d", len(geminiReadings), samples)
	}
}
