package engine

import (
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

func claudeGroup(now time.Time, fiveHour, sevenDay float64, sevenDayReset time.Time) GroupView {
	return GroupView{Key: "claude:1:claude:main", Provider: "claude", AuthIndex: "1", Windows: []WindowView{
		{Window: quota.Window{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: fiveHour, Reset: now.Add(3 * time.Hour)}, Source: quota.SourceActive, ObservedAt: now},
		{Window: quota.Window{ID: "seven-day", Label: quota.LabelSevenDay, Remaining: sevenDay, Reset: sevenDayReset}, Source: quota.SourceActive, ObservedAt: now},
	}}
}

func TestExhaustedSevenDayBlocksIgnition(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	reset := now.Add(72 * time.Hour)
	w, blocked := exhaustedWindow(claudeGroup(now, 100, 0, reset), now)
	if !blocked || !w.Reset.Equal(reset) {
		t.Fatalf("a used-up 7-day window must block until its reset, got %v %v", blocked, w)
	}
	if _, blocked := exhaustedWindow(claudeGroup(now, 0, 40, reset), now); blocked {
		t.Fatal("a used-up 5-hour window is what ignition waits for, not a block")
	}
	if _, blocked := exhaustedWindow(claudeGroup(now, 100, 0, now.Add(-time.Minute)), now); blocked {
		t.Fatal("a 7-day window past its reset must not block")
	}
}

func TestStaleCooldown(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	cred := &Cred{AuthIndex: "1", Provider: "claude", Unavailable: true, CooldownUntil: now.Add(72 * time.Hour)}
	recovered := claudeGroup(now, 100, 100, now.Add(7*24*time.Hour))
	if !staleCooldown(cred, []*GroupView{&recovered}, now) {
		t.Fatal("a long cooldown with a recovered quota is stale")
	}
	exhausted := claudeGroup(now, 100, 0, now.Add(72*time.Hour))
	if staleCooldown(cred, []*GroupView{&exhausted}, now) {
		t.Fatal("a cooldown for a used-up quota is not stale")
	}
	short := &Cred{AuthIndex: "1", Provider: "claude", Unavailable: true, CooldownUntil: now.Add(20 * time.Second)}
	if staleCooldown(short, []*GroupView{&recovered}, now) {
		t.Fatal("a cooldown ending around a 5-hour reset is not stale")
	}
	old := claudeGroup(now.Add(-time.Hour), 100, 100, now.Add(7*24*time.Hour))
	if staleCooldown(cred, []*GroupView{&old}, now) {
		t.Fatal("an old quota does not prove that the quota is back")
	}
}

func TestRangeSpan(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, loc)
	cases := map[string]time.Duration{
		"26h": 26 * time.Hour,
		"15d": 15 * 24 * time.Hour,
		"1mo": 30 * 24 * time.Hour,
		"":    24 * time.Hour,
		"35d": 24 * time.Hour,
	}
	for name, want := range cases {
		if got := RangeSpan(name, now, loc); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
	// March 31 reaches back to the last day of February.
	march := time.Date(2026, 3, 31, 12, 0, 0, 0, loc)
	if got := RangeSpan("1mo", march, loc); got != 31*24*time.Hour {
		t.Errorf("1mo on March 31: got %v", got)
	}
}
