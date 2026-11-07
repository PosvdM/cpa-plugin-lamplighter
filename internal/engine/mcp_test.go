package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

func TestMCPDTOsAndTools(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.MCPKey = "secret-mcp-token"
	cfg.ModelsAPIKey = "secret-models-token"
	cfg.AccountWeights = map[string]float64{"codex-a.json": 5}
	e.Configure(cfg, nil)
	e.creds["1"].Note = "Primary\nprivate note"
	e.creds["2"].Note = "Secondary"
	apply := func(index string, remaining float64, windows bool) {
		raw := []quota.Window{{ID: quota.WindowSevenDay, Label: "7d", Remaining: remaining, Reset: c.t.Add(3 * 24 * time.Hour)}}
		if windows {
			raw = append(raw, quota.Window{ID: quota.WindowFiveHour, Label: "5h", Remaining: remaining, Reset: c.t.Add(2 * time.Hour)})
		}
		e.applyGroups(context.Background(), cfg, e.creds[index], []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: raw}}, quota.SourceActive, c.t, c.t, true)
	}
	apply("1", 50, true)
	apply("2", 20, false)
	b := NewMCPBackend(e)
	result, err := b.CallTool(context.Background(), "list_quotas", map[string]string{"provider": "codex", "model": "alias-with-unknown-route"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	for _, secret := range []string{"secret-mcp-token", "secret-models-token", "codex-a.json", "private note", e.dataDir, "auth_index"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("model DTO exposed %q: %s", secret, raw)
		}
	}
	resultMap := result.(map[string]any)
	if resultMap["certain"] != false {
		t.Fatal("model routing must be explicitly uncertain")
	}
	groups := resultMap["groups"].([]*quotaGroupDTO)
	if len(groups) != 1 || len(groups[0].Accounts) != 2 {
		t.Fatalf("groups: %s", raw)
	}
	for _, total := range groups[0].Totals {
		if total.Window == quota.WindowSevenDay && math.Abs(total.Remaining-45) > 0.001 {
			t.Fatalf("weighted remaining %g, want 45", total.Remaining)
		}
		if total.Window == quota.WindowFiveHour && total.Remaining != 50 {
			t.Fatalf("retired window included in total: %g", total.Remaining)
		}
	}
	history, err := b.CallTool(context.Background(), "get_quota_history", map[string]string{"account": "Primary", "range": "8d"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(history)
	if !strings.Contains(string(raw), "Primary") || strings.Contains(string(raw), "codex:1") || strings.Contains(string(raw), "Secondary") {
		t.Fatalf("history: %s", raw)
	}
	for _, name := range []string{"get_quota_history", "get_quota_events"} {
		args := map[string]string{"range": "invalid"}
		if name == "get_quota_events" {
			args = map[string]string{"since": "invalid"}
		}
		if _, err := b.CallTool(context.Background(), name, args); err == nil {
			t.Fatal("invalid args accepted", name)
		}
	}
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); cmd := <-e.cmdCh; cmd.reply <- commandReply{} }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.CallTool(ctx, "refresh_quotas", map[string]string{"account": "Primary"}); err != nil {
		t.Fatal(err)
	}
	<-done
	throttled, err := b.CallTool(ctx, "refresh_quotas", nil)
	if err != nil || throttled.(map[string]any)["retry_after_seconds"] != 60 {
		t.Fatalf("rate limit %+v %v", throttled, err)
	}
	c.add(time.Minute)
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := b.CallTool(canceled, "refresh_quotas", nil); err == nil {
		t.Fatal("canceled refresh did not return")
	}
	if _, err := b.CallTool(ctx, "refresh_quotas", map[string]string{"account": "does not exist"}); err == nil {
		t.Fatal("unknown account accepted")
	}
}

func TestForecastRatesPaceAndFreshness(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	weekly := WindowStatus{ID: quota.WindowSevenDay, Kind: "7d", Remaining: 60, Reset: now.Add(3 * 24 * time.Hour), ObservedAt: now, Source: "active"}
	dto := forecastWindow(weekly, TargetView{}, nil, now, 5*time.Minute)
	if dto.Started == nil || !*dto.Started || dto.Rate == nil || math.Abs(*dto.Rate-40.0/96) > 0.0001 {
		t.Fatalf("weekly rate %+v", dto)
	}
	pace := dto.Pace.(map[string]float64)
	if math.Abs(pace["expected_remaining_percent"]-100.0*3/7) > 0.0001 || pace["difference_percent"] <= 0 || dto.BeforeReset == nil || *dto.BeforeReset {
		t.Fatalf("weekly pace %+v", dto)
	}
	weekly.Remaining = 5
	dto = forecastWindow(weekly, TargetView{}, nil, now, 5*time.Minute)
	if dto.BeforeReset == nil || !*dto.BeforeReset || dto.Exhaustion == nil {
		t.Fatalf("weekly risk %+v", dto)
	}
	five := WindowStatus{ID: quota.WindowFiveHour, Kind: "5h", Remaining: 40, Reset: now.Add(2 * time.Hour), ObservedAt: now, Source: "passive"}
	points := [][3]float64{{float64(now.Add(-time.Hour).Unix()), 60, 1}, {float64(now.Add(-30 * time.Minute).Unix()), 50, 1}, {float64(now.Unix()), 40, 1}}
	dto = forecastWindow(five, TargetView{}, points, now, 5*time.Minute)
	if dto.Rate == nil || math.Abs(*dto.Rate-20) > 0.0001 || dto.Exhaustion == nil || dto.BeforeReset == nil || *dto.BeforeReset {
		t.Fatalf("five-hour forecast %+v", dto)
	}
	dto = forecastWindow(five, TargetView{Rolling: true}, points, now, 5*time.Minute)
	if dto.Started == nil || *dto.Started || dto.Exhaustion != nil {
		t.Fatalf("unstarted window forecast %+v", dto)
	}
	five.ObservedAt = now.Add(-time.Hour)
	dto = forecastWindow(five, TargetView{}, points, now, 5*time.Minute)
	if !dto.Stale || dto.Rate != nil || dto.Exhaustion != nil {
		t.Fatalf("stale forecast %+v", dto)
	}
	five.Reset = time.Time{}
	dto = forecastWindow(five, TargetView{}, points, now, 5*time.Minute)
	if dto.Started != nil {
		t.Fatal("unknown reset must mean unknown start")
	}
}

func TestQuotaEventsIndependentOfNotifications(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	cfg := e.config()
	if _, err := e.loadCreds(cfg); err != nil {
		t.Fatal(err)
	}
	e.creds["1"].Note = "Primary"
	cred := e.creds["1"]
	apply := func(remaining float64, reset time.Time, exists bool) {
		windows := []quota.Window{}
		if exists {
			windows = append(windows, quota.Window{ID: quota.WindowFiveHour, Label: "5h", Remaining: remaining, Reset: reset})
		}
		e.applyGroups(context.Background(), cfg, cred, []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: windows}}, quota.SourceActive, c.t, c.t, true)
	}
	apply(20, c.t.Add(time.Hour), true)
	c.add(time.Minute)
	apply(0, c.t.Add(time.Hour-time.Minute), true)
	c.add(time.Minute)
	apply(100, c.t.Add(5*time.Hour), true)
	b := NewMCPBackend(e)
	data, err := b.events(map[string]string{"limit": "2"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if !strings.Contains(string(raw), `"kind":"reset"`) || !strings.Contains(string(raw), `"kind":"exhausted"`) || !strings.Contains(string(raw), "Primary") {
		t.Fatalf("events: %s", raw)
	}
	e.events = nil
	c.add(time.Minute)
	apply(0, c.t.Add(time.Hour), false)
	c.add(time.Minute)
	apply(100, c.t.Add(time.Hour), true)
	if len(e.events) != 0 {
		t.Fatalf("disappearance/reappearance emitted events: %+v", e.events)
	}
}

func TestActivePoolDisappearanceAndPassiveTombstone(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	cfg := e.config()
	if _, err := e.loadCreds(cfg); err != nil {
		t.Fatal(err)
	}
	raw := []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: []quota.Window{{ID: quota.WindowFiveHour, Label: "5h", Remaining: 0, Reset: c.t.Add(time.Hour)}}}}
	apply := func(groups []quota.RawGroup, source quota.Source) {
		e.applyGroups(context.Background(), cfg, e.creds["1"], groups, source, c.t, c.t, true)
	}
	apply(raw, quota.SourceActive)
	c.add(time.Minute)
	apply(nil, quota.SourceActive)
	if len(e.groups["codex:1:gpt"].Windows) != 0 || !e.state.Group("codex:1:gpt").Windows[quota.WindowFiveHour].Rebase {
		t.Fatal("missing pool was not retired")
	}
	raw[0].Windows[0].Remaining = 100
	apply(raw, quota.SourcePassive)
	if len(e.groups["codex:1:gpt"].Windows) != 0 {
		t.Fatal("passive headers revived retired pool")
	}
	c.add(time.Minute)
	apply(raw, quota.SourceActive)
	if len(sender.sent) != 0 || len(e.events) != 0 || len(e.collectTargets(cfg)) != 1 {
		t.Fatal("return should silently restore the target")
	}
	// Rolling placeholder state remains available even when ignition is disabled.
	provider := cfg.Providers["codex"]
	provider.Ignite = false
	cfg.Providers["codex"] = provider
	e.Configure(cfg, nil)
	c.add(time.Minute)
	raw[0].Windows[0].Reset = c.t.Add(5 * time.Hour)
	apply(raw, quota.SourceActive)
	c.add(time.Minute)
	raw[0].Windows[0].Reset = c.t.Add(5 * time.Hour)
	apply(raw, quota.SourceActive)
	e.refreshTargets(cfg)
	if len(e.Status().Targets) != 0 {
		t.Fatal("disabled ignition produced targets")
	}
	result, err := NewMCPBackend(e).list(map[string]string{"provider": "codex"})
	if err != nil {
		t.Fatal(err)
	}
	groups := result.(map[string]any)["groups"].([]*quotaGroupDTO)
	if groups[0].Accounts[0].Windows[0].Started == nil || *groups[0].Accounts[0].Windows[0].Started {
		t.Fatal("rolling placeholder reported started with ignition disabled")
	}
}

func TestHistoryDownsampleBounds(t *testing.T) {
	points := make([][3]float64, 10000)
	for i := range points {
		points[i] = [3]float64{float64(i), 50, 0}
	}
	reduced := downsample(points, 200)
	if len(reduced) != 200 || reduced[0] != points[0] || reduced[199] != points[9999] {
		t.Fatal("downsampling endpoints or count")
	}
	if _, err := NewMCPBackend(New(Options{})).CallTool(context.Background(), "missing", nil); err == nil {
		t.Fatal("unknown tool")
	}
}

func TestMCPRefreshFailureIsSanitized(t *testing.T) {
	e := New(Options{})
	e.running = true
	b := NewMCPBackend(e)
	go func() {
		cmd := <-e.cmdCh
		cmd.reply <- commandReply{err: errors.New("secret token /private/auth.json")}
	}()
	_, err := b.refresh(context.Background(), nil)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/private") {
		t.Fatalf("refresh error %v", err)
	}
}
