package notify

import (
	"context"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// bounceGroupKey is the Antigravity Gemini group whose readings these tests
// replay.
const bounceGroupKey = "antigravity:6a7862c969a025af:antigravity:gemini-models"

// geminiReading is one active query of the Gemini group; times are Unix
// seconds.
type geminiReading struct {
	at                int64
	weekly            float64
	weeklyReset       int64
	fiveHour          int64
	fiveHourRemaining float64
}

func (r geminiReading) group() Group {
	return Group{Key: bounceGroupKey, Label: "Gemini", Windows: []quota.Window{
		{ID: "gemini-5h", Label: quota.LabelFiveHour, Remaining: r.fiveHourRemaining, Reset: time.Unix(r.fiveHour, 0)},
		{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: r.weekly, Reset: time.Unix(r.weeklyReset, 0)},
	}}
}

// geminiBounce are the readings of 2026-11-14 around the weekly reset at
// 07:00 UTC+8: the reading after the ignition at 07:00:12 reports the ended
// cycle again, the next ones the new cycle.
var geminiBounce = []geminiReading{
	{at: 1794610500, weekly: 94.763625, weeklyReset: 1794614702, fiveHourRemaining: 100, fiveHour: 1794628500}, // 06:55:00
	{at: 1794610800, weekly: 100, weeklyReset: 1795215600, fiveHourRemaining: 100, fiveHour: 1794628800},       // 07:00:00
	{at: 1794610812, weekly: 94.761723, weeklyReset: 1794614702, fiveHourRemaining: 99.993336, fiveHour: 1794628805},
	{at: 1794611100, weekly: 100, weeklyReset: 1795215900, fiveHourRemaining: 100, fiveHour: 1794629100}, // 07:05:00
	{at: 1794611400, weekly: 100, weeklyReset: 1795216200, fiveHourRemaining: 100, fiveHour: 1794629400}, // 07:10:00
	{at: 1794611412, weekly: 99.99998, weeklyReset: 1795216201, fiveHourRemaining: 99.99994, fiveHour: 1794629401},
	{at: 1794611701, weekly: 99.99809, weeklyReset: 1795216201, fiveHourRemaining: 99.993336, fiveHour: 1794629401}, // 07:15:01
}

func bounceConfig() config.Config {
	cfg := config.Default()
	cfg.RecoveryNotify = config.WindowModes{FiveHour: config.RecoveryAll, SevenDay: config.RecoveryAll}
	return cfg
}

func TestWeeklyBounceAfterResetNotifiesOnce(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := alerts(bounceConfig(), sender)
	for _, r := range geminiBounce {
		a.ProcessGroup(context.Background(), st, r.group(), time.Unix(r.at, 0))
	}
	if len(sender.sent) != 1 {
		t.Fatalf("want one recovery, got %+v", sender.sent)
	}
	msg := sender.sent[0]
	if msg.Kind != KindRecovery || msg.Title != "✅ Gemini · 7d 已恢复" || msg.Group != bounceGroupKey ||
		len(msg.Windows) != 1 || msg.Windows[0] != "gemini-weekly" {
		t.Fatalf("recovery %+v", msg)
	}
	ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]
	if ws.Reset != "2026-11-20T23:10:01Z" || ws.EndedReset != "2026-11-14T00:05:02Z" || ws.BounceReadings != 0 {
		t.Fatalf("baseline %+v", ws)
	}
}

func TestBounceKeepsBaselineAndReminders(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	cfg := bounceConfig()
	cfg.ResetReminder = config.WindowModes{FiveHour: config.ReminderAll, SevenDay: config.ReminderAll}
	a := alerts(cfg, sender)
	for _, r := range geminiBounce[:3] {
		a.ProcessGroup(context.Background(), st, r.group(), time.Unix(r.at, 0))
	}
	ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]
	// The bounced reading resets in about an hour; it must not send a
	// reminder or replace the baseline of the new cycle.
	if ws.Reset != "2026-11-20T23:00:00Z" || ws.Remaining != 100 || ws.BounceReadings != 1 {
		t.Fatalf("baseline %+v", ws)
	}
	for _, msg := range sender.sent {
		if msg.Kind == KindReminder {
			t.Fatalf("bounced reading sent a reminder: %+v", msg)
		}
	}
}

// TestLastingBounceAcceptsTheOldCycle replays 2026-10-22: the weekly window
// reported a new, unused cycle, then the old cycle again until its reset.
// The old readings are accepted silently once they last.
func TestLastingBounceAcceptsTheOldCycle(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	a := alerts(bounceConfig(), sender)
	oldReset := time.Date(2026, 10, 23, 23, 0, 6, 0, time.UTC)
	weekly := func(remaining float64, reset time.Time) Group {
		return Group{Key: bounceGroupKey, Label: "Gemini", Windows: []quota.Window{
			{ID: "gemini-weekly", Label: quota.LabelSevenDay, Remaining: remaining, Reset: reset},
		}}
	}
	start := time.Date(2026, 10, 22, 11, 55, 0, 0, time.UTC)
	a.ProcessGroup(context.Background(), st, weekly(95, oldReset), start)
	now := start.Add(5 * time.Minute)
	a.ProcessGroup(context.Background(), st, weekly(100, now.Add(7*24*time.Hour)), now)
	if len(sender.sent) != 1 || sender.sent[0].Kind != KindRecovery {
		t.Fatalf("the false reset notifies like a reset: %+v", sender.sent)
	}
	// The old cycle comes back and stays.
	var accepted time.Time
	for step := 1; step <= 12; step++ {
		now = now.Add(5 * time.Minute)
		a.ProcessGroup(context.Background(), st, weekly(94.86-float64(step)*0.1, oldReset), now)
		ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]
		if ws.EndedReset == "" && accepted.IsZero() {
			accepted = now
		}
	}
	if accepted.IsZero() {
		t.Fatal("the lasting old cycle was never accepted")
	}
	// The first bounced reading counts from 12:05; three readings and 30
	// minutes later the old cycle is accepted.
	if want := start.Add(5*time.Minute + 35*time.Minute); !accepted.Equal(want) {
		t.Fatalf("accepted at %s, want %s", accepted, want)
	}
	ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]
	if ws.Reset != "2026-10-23T23:00:06Z" || ws.Remaining > 95 || ws.BounceReadings != 0 || ws.PendingRecovery != nil {
		t.Fatalf("baseline %+v", ws)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("accepting the old cycle must not notify: %+v", sender.sent)
	}
	// The real reset of the old cycle notifies again.
	now = oldReset.Add(time.Minute)
	a.ProcessGroup(context.Background(), st, weekly(100, now.Add(7*24*time.Hour)), now)
	if len(sender.sent) != 2 || sender.sent[1].Kind != KindRecovery {
		t.Fatalf("real reset after an accepted bounce: %+v", sender.sent)
	}
}

func TestShortBounceIsForgottenByTheNewCycle(t *testing.T) {
	st := store.NewState()
	a := alerts(bounceConfig(), &fakeSender{})
	for _, r := range geminiBounce[:3] {
		a.ProcessGroup(context.Background(), st, r.group(), time.Unix(r.at, 0))
	}
	if ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]; ws.BounceReadings != 1 || ws.BounceSince != 1794610812 {
		t.Fatalf("bounce not counted: %+v", ws)
	}
	r := geminiBounce[3]
	a.ProcessGroup(context.Background(), st, r.group(), time.Unix(r.at, 0))
	if ws := st.Groups[bounceGroupKey].Windows["gemini-weekly"]; ws.BounceReadings != 0 || ws.BounceSince != 0 {
		t.Fatalf("a reading of the new cycle must reset the count: %+v", ws)
	}
}
