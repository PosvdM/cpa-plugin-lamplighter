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

// resetNow is before the pending schedule in records.
var resetNow = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

func TestResetFeedFirstRunOnlyNotifiesPending(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	sent := ProcessResetRecords(context.Background(), st, records(), true, sender, time.UTC, LangZH, resetNow)
	if sent != 1 || sender.sent[0].Title != "📅 Codex 全局重置已排期" {
		t.Fatalf("got %d %+v", sent, sender.sent)
	}
	if sender.sent[0].JumpURL != "https://didcodexreset.com/zh/history/1791028800000.html" {
		t.Fatalf("jump url %q", sender.sent[0].JumpURL)
	}
	if again := ProcessResetRecords(context.Background(), st, records(), true, sender, time.UTC, LangZH, resetNow); again != 0 {
		t.Fatal("seen records must not be sent again")
	}
}

func TestResetFeedNewRecordAndManualIDRotation(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	manual := ResetRecord{"id": "manual:a", "kind": "reset_completed", "resetType": "banked", "announcedAt": "2026-09-01T00:00:00Z"}
	ProcessResetRecords(context.Background(), st, append(records(), manual), false, sender, time.UTC, LangZH, resetNow)
	if len(sender.sent) != 0 {
		t.Fatal("first run without pending notification sends nothing")
	}
	rotated := ResetRecord{"id": "manual:b", "kind": "reset_completed", "resetType": "banked", "announcedAt": "2026-09-01T00:00:00Z"}
	fresh := ResetRecord{"id": "r4", "kind": "reset_completed", "resetType": "global", "effectiveAt": "2026-10-06T00:00:00Z"}
	next := append([]ResetRecord{fresh}, append(records(), rotated)...)
	if sent := ProcessResetRecords(context.Background(), st, next, false, sender, time.UTC, LangZH, resetNow); sent != 1 {
		t.Fatalf("only the new record is sent, got %d: %+v", sent, sender.sent)
	}
	if sender.sent[0].Title != "✅ Codex 全局重置已完成" {
		t.Fatalf("title %q", sender.sent[0].Title)
	}
}

func bankedSchedule(id, state, announced, start, end string, confidence float64) ResetRecord {
	return ResetRecord{
		"id": id, "kind": "reset_scheduled", "resetType": "banked", "scheduleState": state,
		"announcedAt": announced, "effectiveAt": start, "schedulePrecision": "date", "confidence": confidence,
		"scheduleWindow": map[string]any{"startAt": start, "endAt": end},
		"scope":          map[string]any{"plans": []any{"all"}},
	}
}

// Of the posts about one schedule, only the latest is sent, as on Did Codex
// Reset; a later post that changes the schedule is sent as an update.
// Schedules that are no longer pending and old completed resets are not sent.
func TestResetFeedSendsLatestPostPerSchedule(t *testing.T) {
	const start, end = "2026-10-07T07:00:00Z", "2026-10-08T07:00:00Z"
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Date(2026, 10, 7, 19, 20, 0, 0, time.UTC)
	ProcessResetRecords(context.Background(), st, nil, false, sender, time.UTC, LangZH, now)
	shanghai := time.FixedZone("CST", 8*3600)
	next := []ResetRecord{
		bankedSchedule("post", "pending", "2026-10-07T19:19:17Z", start, end, 0.93),
		bankedSchedule("reply", "pending", "2026-10-07T19:19:33Z", start, end, 0.82),
		bankedSchedule("elapsed", "elapsed", "2026-09-26T00:07:13Z", "2026-09-25T07:00:00Z", "2026-09-26T07:00:00Z", 0.93),
		bankedSchedule("fulfilled", "fulfilled", "2026-10-03T04:01:28Z", "2026-10-02T07:00:00Z", "2026-10-03T07:00:00Z", 0.9),
		{"id": "old", "kind": "reset_completed", "resetType": "global", "announcedAt": "2026-10-03T04:26:28Z"},
	}
	if sent := ProcessResetRecords(context.Background(), st, next, false, sender, shanghai, LangZH, now); sent != 1 {
		t.Fatalf("only the latest post is sent, got %d: %+v", sent, sender.sent)
	}
	if want := "预计：10/07 15:00～10/08 15:00\n置信度：82%"; !strings.HasPrefix(sender.sent[0].Body, want) {
		t.Fatalf("body %q, want prefix %q", sender.sent[0].Body, want)
	}
	late := bankedSchedule("late", "pending", "2026-10-07T19:00:00Z", start, end, 0.9)
	update := bankedSchedule("update", "pending", "2026-10-07T20:00:00Z", start, end, 0.95)
	again := append([]ResetRecord{late, update}, next...)
	if sent := ProcessResetRecords(context.Background(), st, again, false, sender, shanghai, LangZH, now.Add(time.Hour)); sent != 1 ||
		sender.sent[1].Title != "📅 Codex 重置卡排期更新" || !strings.Contains(sender.sent[1].Body, "95%") {
		t.Fatalf("a later post is sent as an update and an earlier one is not, got %+v", sender.sent)
	}
}

// A reply can enter the latest 10 in a later poll. It is sent only when it
// changes the schedule.
func TestResetFeedReplyInLaterPoll(t *testing.T) {
	const start, end = "2026-10-07T07:00:00Z", "2026-10-08T07:00:00Z"
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Date(2026, 10, 7, 19, 20, 0, 0, time.UTC)
	ProcessResetRecords(context.Background(), st, nil, false, sender, time.UTC, LangZH, now)
	post := bankedSchedule("post", "pending", "2026-10-07T19:19:17Z", start, end, 0.93)
	repeat := bankedSchedule("repeat", "pending", "2026-10-07T19:19:33Z", start, end, 0.93)
	reply := bankedSchedule("reply", "pending", "2026-10-07T19:19:50Z", start, end, 0.82)
	ProcessResetRecords(context.Background(), st, []ResetRecord{post}, false, sender, time.UTC, LangZH, now)
	ProcessResetRecords(context.Background(), st, []ResetRecord{repeat, post}, false, sender, time.UTC, LangZH, now.Add(10*time.Minute))
	if len(sender.sent) != 1 {
		t.Fatalf("a reply with the same content must not be sent, got %+v", sender.sent)
	}
	ProcessResetRecords(context.Background(), st, []ResetRecord{reply, repeat, post}, false, sender, time.UTC, LangZH, now.Add(20*time.Minute))
	if len(sender.sent) != 2 || !strings.Contains(sender.sent[1].Body, "82%") {
		t.Fatalf("a reply with a new confidence is sent as an update, got %+v", sender.sent)
	}
}

func TestResetFeedSupersedeRules(t *testing.T) {
	const start, end = "2026-10-07T07:00:00Z", "2026-10-08T07:00:00Z"
	now := time.Date(2026, 10, 7, 19, 20, 0, 0, time.UTC)
	run := func(records ...ResetRecord) []Message {
		sender := &fakeSender{}
		st := store.NewState()
		ProcessResetRecords(context.Background(), st, nil, false, sender, time.UTC, LangZH, now)
		ProcessResetRecords(context.Background(), st, records, false, sender, time.UTC, LangZH, now)
		return sender.sent
	}
	pending := bankedSchedule("a", "pending", "2026-10-07T19:00:00Z", start, end, 0.9)
	unknown := bankedSchedule("b", "unknown", "2026-10-07T19:10:00Z", start, end, 0.9)
	if got := run(unknown, pending); len(got) != 1 {
		t.Fatalf("a later post that is not sent must not suppress a pending one, got %+v", got)
	}
	tie := bankedSchedule("c", "pending", "2026-10-07T19:00:00Z", start, end, 0.9)
	if got := run(tie, pending); len(got) != 1 {
		t.Fatalf("posts with the same announcedAt are sent once, got %+v", got)
	}
	pro := bankedSchedule("d", "pending", "2026-10-07T19:10:00Z", start, end, 0.9)
	pro["scope"] = map[string]any{"plans": []any{"pro"}}
	if got := run(pro, pending); len(got) != 2 {
		t.Fatalf("schedules for different plans are sent separately, got %+v", got)
	}
}

func TestResetFeedFirstRunIgnoresKindCase(t *testing.T) {
	r := bankedSchedule("a", "pending", "2026-10-07T19:00:00Z", "2026-10-07T07:00:00Z", "2026-10-08T07:00:00Z", 0.9)
	r["kind"] = "RESET_SCHEDULED"
	sender := &fakeSender{}
	now := time.Date(2026, 10, 7, 19, 20, 0, 0, time.UTC)
	if sent := ProcessResetRecords(context.Background(), store.NewState(), []ResetRecord{r}, true, sender, time.UTC, LangZH, now); sent != 1 {
		t.Fatalf("got %d", sent)
	}
}

func TestScheduleText(t *testing.T) {
	exact := bankedSchedule("a", "pending", "", "2026-10-09T07:00:00Z", "2026-10-09T07:00:00Z", 0.9)
	deadline := bankedSchedule("b", "pending", "", "2026-10-07T19:00:00Z", "2026-10-08T07:00:00Z", 0.9)
	deadline["scheduleConstraint"] = "deadline"
	noWindow := ResetRecord{"kind": "reset_scheduled", "effectiveAt": "2026-10-09T07:00:00Z"}
	for _, c := range []struct {
		r          ResetRecord
		lang, want string
	}{
		{exact, LangZH, "10/09 07:00"},
		{deadline, LangZH, "10/08 07:00 前"},
		{deadline, LangEN, "by 10/08 07:00"},
		{bankedSchedule("c", "pending", "", "2026-10-07T07:00:00Z", "2026-10-08T07:00:00Z", 0.9), LangEN, "10/07 07:00–10/08 07:00"},
		{noWindow, LangZH, "10/09 07:00"},
	} {
		if got := scheduleText(c.r, time.UTC, c.lang); got != c.want {
			t.Fatalf("scheduleText(%v) = %q, want %q", c.r["id"], got, c.want)
		}
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

// Usage endpoints and response headers can differ by a point, so a reading
// bounces around a threshold within one window.
func TestThresholdBounceNotifiesOnce(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		sender := &fakeSender{}
		st := store.NewState()
		now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
		cfg := config.Default()
		cfg.NotifyRecovery = recovery
		a := alerts(cfg, sender)
		for _, v := range []float64{60, 50, 51, 50, 49, 21, 20, 21, 19.99, 20} {
			a.ProcessGroup(context.Background(), st, group(v, 80, now), now)
		}
		if len(sender.sent) != 2 || !strings.HasPrefix(sender.sent[0].Title, "🟡") || !strings.HasPrefix(sender.sent[1].Title, "🔴") {
			t.Fatalf("recovery=%v: want one yellow and one red alert, got %+v", recovery, sender.sent)
		}
	}
}

func TestRecoveryNeedsNewWindowOrJump(t *testing.T) {
	sender := &fakeSender{}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	a := alerts(config.Default(), sender)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(48, 80, now), now)
	// A small rise into a new window counts; the reset moved by 5 hours.
	later := now.Add(5 * time.Hour)
	a.ProcessGroup(context.Background(), st, group(52, 80, later), later)
	a.ProcessGroup(context.Background(), st, group(49, 80, later), later)
	// A reset time one second off is the same window.
	g := group(51, 80, later)
	g.Windows[0].Reset = g.Windows[0].Reset.Add(-time.Second)
	a.ProcessGroup(context.Background(), st, g, later)
	a.ProcessGroup(context.Background(), st, group(50, 80, later), later)
	// A jump of 5 points counts without a reset change.
	a.ProcessGroup(context.Background(), st, group(55, 80, later), later)
	a.ProcessGroup(context.Background(), st, group(50, 80, later), later)
	if len(sender.sent) != 3 {
		t.Fatalf("want three alerts, got %+v", sender.sent)
	}
}
