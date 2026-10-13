package providers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseCodexUsageClassifiesWindowsByLength(t *testing.T) {
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	payload := decode(t, `{"rate_limit":{
		"primary_window":{"used_percent":93,"limit_window_seconds":604800,"reset_at":1791139696},
		"secondary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`)
	groups, err := ParseCodexUsage(payload, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Key != "codex:main" || groups[0].SourceLabel != "ChatGPT" {
		t.Fatalf("unexpected groups %+v", groups)
	}
	w := groups[0].Windows
	if w[0].ID != quota.WindowFiveHour || w[0].Remaining != 100 || !w[0].Reset.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("five-hour window %+v", w[0])
	}
	if w[1].ID != quota.WindowSevenDay || w[1].Remaining != 7 || w[1].Reset.Unix() != 1791139696 {
		t.Fatalf("seven-day window %+v", w[1])
	}
}

func TestParseClaudeUsageSplitsFableIntoOwnGroup(t *testing.T) {
	payload := decode(t, `{
		"five_hour":{"utilization":28.0,"resets_at":"2026-10-03T20:09:59.976598+00:00"},
		"seven_day":{"utilization":20.0,"resets_at":"2026-10-08T05:59:59+00:00"},
		"iguana_necktie":{"utilization":50.0,"resets_at":"2026-11-01T00:00:00+00:00"},
		"limits":[{"kind":"weekly_scoped","percent":3,"resets_at":"2026-11-05T07:59:00+00:00","is_active":true,
			"scope":{"model":{"display_name":"Fable 5"}}}]}`)
	groups, err := ParseClaudeUsage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("want main and Fable groups, got %+v", groups)
	}
	if groups[0].Key != "claude:main" || len(groups[0].Windows) != 2 || groups[0].Windows[0].Remaining != 72 {
		t.Fatalf("main group %+v", groups[0])
	}
	fable := groups[1]
	if fable.Key != "claude:seven-day-fable" || fable.SourceLabel != "Fable" || fable.Windows[0].Remaining != 97 {
		t.Fatalf("fable group %+v", fable)
	}
	if quota.ShortLabel(fable.Windows[0].Label) != "7d" {
		t.Fatalf("fable short label %q", quota.ShortLabel(fable.Windows[0].Label))
	}
}

func TestParseAntigravityQuotaKeepsPrecisionAndOrder(t *testing.T) {
	payload := decode(t, `{"groups":[
		{"displayName":"Gemini Models","buckets":[
			{"window":"weekly","remainingFraction":0.960011,"resetTime":"2026-10-08T09:12:01Z"},
			{"window":"5h","remainingFraction":0.9999985,"resetTime":"2026-10-03T18:01:02Z","bucketId":"gemini-5h"}]},
		{"displayName":"Claude and GPT Models","buckets":[{"window":"5h","remainingFraction":"100%"}]}]}`)
	groups, err := ParseAntigravityQuota(payload)
	if err != nil {
		t.Fatal(err)
	}
	if groups[0].Key != "antigravity:gemini-models" || groups[0].SourceLabel != "Gemini" {
		t.Fatalf("gemini group %+v", groups[0])
	}
	if groups[0].Windows[0].ID != "gemini-5h" || groups[0].Windows[0].Label != quota.LabelFiveHour {
		t.Fatalf("5h bucket should sort first: %+v", groups[0].Windows)
	}
	if got := groups[0].Windows[0].Remaining; got < 99.9998 || got > 99.9999 {
		t.Fatalf("precision lost: %v", got)
	}
	if groups[1].SourceLabel != "Claude / GPT" || groups[1].Windows[0].Remaining != 100 {
		t.Fatalf("claude/gpt group %+v", groups[1])
	}
}

func TestParsePassiveClaudeHeaders(t *testing.T) {
	header := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.2"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {"1791058200"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.19"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {"1791439200"},
	}
	groups := claude.Headers(header, time.Now())
	if len(groups) != 1 || groups[0].Key != "claude:main" || len(groups[0].Windows) != 2 {
		t.Fatalf("unexpected %+v", groups)
	}
	group := groups[0]
	if group.Windows[0].Remaining != 80 || group.Windows[0].Reset.Unix() != 1791058200 {
		t.Fatalf("5h %+v", group.Windows[0])
	}
	if diff := group.Windows[1].Remaining - 81; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("7d %+v", group.Windows[1])
	}
}

func TestParsePassiveClaudeFableHeaders(t *testing.T) {
	header := http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization":    {"0.2"},
		"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.35"},
		"Anthropic-Ratelimit-Unified-7d_oi-Reset":       {"1793865540"},
	}
	groups := claude.Headers(header, time.Now())
	if len(groups) != 2 || groups[1].Key != FableGroupKey || groups[1].SourceLabel != "Fable" {
		t.Fatalf("unexpected %+v", groups)
	}
	w := groups[1].Windows[0]
	if w.ID != quota.WindowFable || w.Label != quota.LabelFable || w.Remaining != 65 || w.Reset.Unix() != 1793865540 {
		t.Fatalf("fable %+v", w)
	}
	// The Fable group key matches the one from the usage API, so passive
	// and active data land in the same group.
	active, err := ParseClaudeUsage(map[string]any{"limits": []any{map[string]any{"kind": "weekly_scoped", "percent": 10.0, "scope": map[string]any{"model": map[string]any{"display_name": "Fable"}}}}})
	if err != nil || len(active) != 1 || active[0].Key != FableGroupKey {
		t.Fatalf("active %+v %v", active, err)
	}
}

func TestParsePassiveCodexHeaders(t *testing.T) {
	now := time.Unix(1791046436, 0)
	header := http.Header{
		"X-Codex-Primary-Used-Percent":          {"0"},
		"X-Codex-Primary-Reset-At":              {"1791064435"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Secondary-Used-Percent":        {"93"},
		"X-Codex-Secondary-Reset-After-Seconds": {"93262"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
	}
	groups := codex.Headers(header, now)
	if len(groups) != 1 || len(groups[0].Windows) != 2 {
		t.Fatalf("unexpected %+v", groups)
	}
	group := groups[0]
	if group.Windows[0].ID != quota.WindowFiveHour || group.Windows[0].Reset.Unix() != 1791064435 {
		t.Fatalf("5h %+v", group.Windows[0])
	}
	if group.Windows[1].ID != quota.WindowSevenDay || group.Windows[1].Remaining != 7 || group.Windows[1].Reset.Unix() != now.Unix()+93262 {
		t.Fatalf("7d %+v", group.Windows[1])
	}
}

func TestParsePassiveIgnoresUnrelatedHeaders(t *testing.T) {
	if groups := claude.Headers(http.Header{"Content-Type": {"application/json"}}, time.Now()); len(groups) > 0 {
		t.Fatal("headers without quota fields must be ignored")
	}
	if antigravity.Headers != nil {
		t.Fatal("antigravity has no passive quota")
	}
}

func TestParseCredential(t *testing.T) {
	secret := ParseCredential(json.RawMessage(`{"access_token":"tok","account_id":"acc","proxy_url":"socks5://p:1","metadata":{"project_id":"proj"}}`))
	if secret.AccessToken != "tok" || secret.AccountID != "acc" || secret.ProxyURL != "socks5://p:1" || secret.ProjectID != "proj" {
		t.Fatalf("unexpected %+v", secret)
	}
}

func TestClampPercentDropsFloatNoise(t *testing.T) {
	for in, want := range map[float64]float64{100 - 0.8*100: 20, 100 - 0.78*100: 22, 99.99985: 99.99985, -1: 0, 101: 100} {
		if got := clampPercent(in); got != want {
			t.Fatalf("clampPercent(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestTriggerPromptIsStrict(t *testing.T) {
	if !strings.Contains(TriggerPrompt, "Reply with exactly OK and nothing else.") {
		t.Fatal("trigger prompt must ask for exactly OK")
	}
}

func TestRequestUsesMinimalBodies(t *testing.T) {
	for _, p := range All() {
		if p.Ignition == nil {
			continue
		}
		entry, raw, err := Request(p, "m")
		if err != nil || entry != p.Ignition.Protocol {
			t.Fatalf("%s: %s %v", p.ID, entry, err)
		}
		var body map[string]any
		json.Unmarshal(raw, &body)
		if body["model"] != "m" || !strings.Contains(string(raw), "exactly OK") {
			t.Fatalf("%s body %s", p.ID, raw)
		}
		if strings.Contains(string(raw), `"tools":[{`) {
			t.Fatalf("%s must not declare tools", p.ID)
		}
	}
	for id, entry := range map[string]string{"codex": "openai-response", "claude": "claude", "antigravity": "openai"} {
		if got := Lookup(id).Ignition.Protocol; got != entry {
			t.Fatalf("%s protocol %s, want %s", id, got, entry)
		}
	}
}

func TestParseCodexUsageKeepsAWeeklyOnlyPlanWithoutFiveHours(t *testing.T) {
	// A ChatGPT Pro response from the Management Center tests: one weekly
	// window as the primary window.
	payload := decode(t, `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":1,"limit_window_seconds":604800,"reset_after_seconds":601888,"reset_at":1785902974},
		"secondary_window":null}}`)
	groups, err := ParseCodexUsage(payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := groups[0].Windows
	if len(w) != 1 || w[0].ID != quota.WindowSevenDay || w[0].Remaining != 99 || quota.Kind(w[0]) != quota.KindSevenDay {
		t.Fatalf("windows %+v", w)
	}
}

func TestParseCodexUsageKeepsOtherLengthsApart(t *testing.T) {
	payload := decode(t, `{"rate_limit":{
		"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1791139696},
		"secondary_window":{"used_percent":40,"limit_window_seconds":2592000,"reset_at":1793139696}}}`)
	groups, err := ParseCodexUsage(payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := groups[0].Windows
	if len(w) != 2 || w[0].ID != quota.WindowFiveHour || w[1].ID != "window-30d" || w[1].Label != "30d" || quota.Kind(w[1]) != "" {
		t.Fatalf("windows %+v", w)
	}
	header := http.Header{
		"X-Codex-Primary-Used-Percent":     {"10"},
		"X-Codex-Primary-Window-Minutes":   {"43200"},
		"X-Codex-Secondary-Used-Percent":   {"5"},
		"X-Codex-Secondary-Window-Minutes": {"300"},
	}
	passive := codex.Headers(header, time.Now())
	if len(passive) != 1 || len(passive[0].Windows) != 2 || passive[0].Windows[0].ID != "window-30d" || passive[0].Windows[1].ID != quota.WindowFiveHour {
		t.Fatalf("passive %+v", passive)
	}
}

func TestPeriodsAreRecorded(t *testing.T) {
	groups, _ := ParseClaudeUsage(map[string]any{"five_hour": map[string]any{"utilization": 1.0}, "seven_day": map[string]any{"utilization": 2.0}})
	if groups[0].Windows[0].Period != quota.FiveHours || groups[0].Windows[1].Period != quota.SevenDays {
		t.Fatalf("claude %+v", groups[0].Windows)
	}
	ag, _ := ParseAntigravityQuota(map[string]any{"groups": []any{map[string]any{"displayName": "Gemini Models", "buckets": []any{
		map[string]any{"window": "5h", "remainingFraction": 1.0}, map[string]any{"window": "weekly", "remainingFraction": 1.0}}}}})
	if ag[0].Windows[0].Period != quota.FiveHours || ag[0].Windows[1].Period != quota.SevenDays {
		t.Fatalf("antigravity %+v", ag[0].Windows)
	}
}
