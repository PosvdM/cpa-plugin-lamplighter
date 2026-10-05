package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

type fakeSender struct {
	sent []Message
	fail bool
}

func (f *fakeSender) Send(_ context.Context, msg Message) error {
	if f.fail {
		return errors.New("offline")
	}
	f.sent = append(f.sent, msg)
	return nil
}

func alerts(cfg config.Config, sender Sender) *Alerts {
	return &Alerts{Cfg: cfg, Sender: sender}
}

func group(remaining5h, remaining7d float64, now time.Time) Group {
	return Group{Key: "codex:1:codex:main", Label: "ChatGPT#eg", Windows: []quota.Window{
		{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: remaining5h, Reset: now.Add(5 * time.Hour)},
		{ID: quota.WindowSevenDay, Label: quota.LabelSevenDay, Remaining: remaining7d, Reset: now.Add(6 * 24 * time.Hour)},
	}}
}

func TestFirstSightOnlyRecordsBaseline(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Now()
	alerts(config.Default(), sender).ProcessGroup(context.Background(), st, group(5, 80, now), now)
	if len(sender.sent) != 0 {
		t.Fatalf("first sight must not notify: %+v", sender.sent)
	}
	if st.Groups["codex:1:codex:main"].Windows[quota.WindowFiveHour].NotifiedSeverity != SeverityCritical {
		t.Fatal("baseline must be the current severity")
	}
}

func TestThresholdCrossingNotifiesWithResetHint(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(26, 80, now), now)
	if len(sender.sent) != 1 {
		t.Fatalf("want one notification, got %+v", sender.sent)
	}
	if got := sender.sent[0].Title; got != "🟡 ChatGPT#eg · 5h 26% | 05h" {
		t.Fatalf("title %q", got)
	}
	if !strings.HasPrefix(sender.sent[0].Body, "5h：26% | 05h | ") {
		t.Fatalf("body %q", sender.sent[0].Body)
	}
	a.ProcessGroup(context.Background(), st, group(25, 80, now), now)
	if len(sender.sent) != 1 {
		t.Fatal("the same level must not notify twice")
	}
}

func TestCriticalIsTimeSensitive(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Now()
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(8, 80, now), now)
	if sender.sent[0].Level != LevelTimeSensitive || !strings.HasPrefix(sender.sent[0].Title, "🔴") {
		t.Fatalf("got %+v", sender.sent[0])
	}
}

func TestLowIsRedButNotTimeSensitive(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Now()
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(18, 80, now), now)
	if sender.sent[0].Level != LevelActive || !strings.HasPrefix(sender.sent[0].Title, "🔴") {
		t.Fatalf("got %+v", sender.sent[0])
	}
}

func TestQuietRecoveryResetsBaseline(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Now()
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(0, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(100, 80, now), now)
	if len(sender.sent) != 1 {
		t.Fatalf("recovery notifications are off by default: %+v", sender.sent)
	}
	a.ProcessGroup(context.Background(), st, group(40, 80, now), now)
	if len(sender.sent) != 2 {
		t.Fatal("after a quiet recovery the next drop must notify again")
	}
}

func TestFailedDeliveryRetriesNextTime(t *testing.T) {
	sender := &fakeSender{fail: true}
	st := store.NewState()
	now := time.Now()
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	sender.fail = false
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	if len(sender.sent) != 1 {
		t.Fatalf("undelivered alert must be retried: %+v", sender.sent)
	}
}

func TestResetReminders(t *testing.T) {
	now := time.Now()
	soon := Group{Key: "g", Label: "Claude", Windows: []quota.Window{
		{ID: quota.WindowFiveHour, Label: quota.LabelFiveHour, Remaining: 90, Reset: now.Add(30 * time.Minute)},
	}}
	cfg := config.Default()
	sender := &fakeSender{}
	st := store.NewState()
	alerts(cfg, sender).ProcessGroup(context.Background(), st, soon, now)
	alerts(cfg, sender).ProcessGroup(context.Background(), st, soon, now)
	if len(sender.sent) != 0 {
		t.Fatal("reminders are off by default")
	}
	cfg.NotifyResetReminders = true
	alerts(cfg, sender).ProcessGroup(context.Background(), st, soon, now)
	alerts(cfg, sender).ProcessGroup(context.Background(), st, soon, now)
	if len(sender.sent) != 1 || sender.sent[0].Title != "⏰ Claude · 5h 重置提醒" {
		t.Fatalf("want one reminder, got %+v", sender.sent)
	}
}

func TestCompactDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                            "可刷新",
		30 * time.Minute:             "30m",
		4*time.Hour + 40*time.Minute: "05h",
		3 * 24 * time.Hour:           "03d",
	}
	for d, want := range cases {
		if got := CompactDuration(d, LangZH); got != want {
			t.Errorf("%v: got %q want %q", d, got, want)
		}
	}
}

func TestBarkRequestFormat(t *testing.T) {
	var gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"code":200}`))
	}))
	defer server.Close()
	bark := &Bark{URL: server.URL + "/key/", Group: "CPA"}
	err := bark.Send(context.Background(), Message{Title: "⚠️ A · 5h 26%", Body: "5h：26% | 05h", JumpURL: "https://x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotPath, "/key/%E2%9A%A0%EF%B8%8F%20A%20%C2%B7%205h%2026%25/") {
		t.Fatalf("path %q", gotPath)
	}
	if !strings.Contains(gotQuery, "group=CPA") || !strings.Contains(gotQuery, "url=https%3A%2F%2Fx%2Fy") {
		t.Fatalf("query %q", gotQuery)
	}
}

func TestBarkRejectsErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":400,"message":"bad key"}`))
	}))
	defer server.Close()
	if err := (&Bark{URL: server.URL}).Send(context.Background(), Message{Title: "t", Body: "b"}); err == nil {
		t.Fatal("code 400 must be an error")
	}
}

func records() []ResetRecord {
	return []ResetRecord{
		{"id": "r3", "kind": "reset_scheduled", "scheduleState": "pending", "resetType": "global", "effectiveAt": "2026-10-05T00:00:00Z", "announcedAt": "2026-10-03T12:00:00Z", "confidence": 0.9},
		{"id": "r2", "kind": "reset_completed", "resetType": "banked", "effectiveAt": "2026-10-01T00:00:00Z"},
		{"id": "r1", "kind": "reset_completed", "resetType": "global", "effectiveAt": "2026-09-20T00:00:00Z"},
	}
}

func TestResetFeedFirstRunOnlyNotifiesPending(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	sent := ProcessResetRecords(context.Background(), st, records(), true, sender, time.UTC, LangZH, time.Now())
	if sent != 1 || sender.sent[0].Title != "📅 Codex 全局重置已排期" {
		t.Fatalf("got %d %+v", sent, sender.sent)
	}
	if sender.sent[0].JumpURL != "https://didcodexreset.com/zh/history/1791028800000.html" {
		t.Fatalf("jump url %q", sender.sent[0].JumpURL)
	}
	if again := ProcessResetRecords(context.Background(), st, records(), true, sender, time.UTC, LangZH, time.Now()); again != 0 {
		t.Fatal("seen records must not be sent again")
	}
}

func TestResetFeedNewRecordAndManualIDRotation(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	manual := ResetRecord{"id": "manual:a", "kind": "reset_completed", "resetType": "banked", "announcedAt": "2026-09-01T00:00:00Z"}
	ProcessResetRecords(context.Background(), st, append(records(), manual), false, sender, time.UTC, LangZH, time.Now())
	if len(sender.sent) != 0 {
		t.Fatal("first run without pending notification sends nothing")
	}
	rotated := ResetRecord{"id": "manual:b", "kind": "reset_completed", "resetType": "banked", "announcedAt": "2026-09-01T00:00:00Z"}
	fresh := ResetRecord{"id": "r4", "kind": "reset_completed", "resetType": "global", "effectiveAt": "2026-10-06T00:00:00Z"}
	next := append([]ResetRecord{fresh}, append(records(), rotated)...)
	if sent := ProcessResetRecords(context.Background(), st, next, false, sender, time.UTC, LangZH, time.Now()); sent != 1 {
		t.Fatalf("only the new record is sent, got %d: %+v", sent, sender.sent)
	}
	if sender.sent[0].Title != "✅ Codex 全局重置已完成" {
		t.Fatalf("title %q", sender.sent[0].Title)
	}
}

func TestEnglishNotifications(t *testing.T) {
	for in, want := range map[string]string{"": LangZH, "zh-CN": LangZH, "zh-TW": LangZH, "en": LangEN, "ru": LangEN} {
		if got := NormalizeLang(in); got != want {
			t.Fatalf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.NotifyRecovery = true
	a := &Alerts{Cfg: cfg, Sender: sender, Lang: LangEN}
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(26, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(90, 80, now), now)
	if len(sender.sent) != 2 || !strings.HasPrefix(sender.sent[0].Body, "5h: 26% | 05h | ") || sender.sent[1].Title != "✅ ChatGPT#eg · 5h recovered" {
		t.Fatalf("got %+v", sender.sent)
	}
	if got := CircuitMessage(LangEN, "Claude", now, "boom", time.UTC).Title; got != "⚠️ Claude ignition paused" {
		t.Fatalf("circuit title %q", got)
	}
	msg := ResetMessage(records()[0], time.UTC, LangEN)
	if msg.Title != "📅 Codex global reset scheduled" || msg.JumpURL != "https://didcodexreset.com/history/1791028800000.html" {
		t.Fatalf("reset message %+v", msg)
	}
}
