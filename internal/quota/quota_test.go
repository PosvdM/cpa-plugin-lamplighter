package quota

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
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
	if w[0].ID != WindowFiveHour || w[0].Remaining != 100 || !w[0].Reset.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("five-hour window %+v", w[0])
	}
	if w[1].ID != WindowSevenDay || w[1].Remaining != 7 || w[1].Reset.Unix() != 1791139696 {
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
	if ShortLabel(fable.Windows[0].Label) != "7d" {
		t.Fatalf("fable short label %q", ShortLabel(fable.Windows[0].Label))
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
	if groups[0].Windows[0].ID != "gemini-5h" || groups[0].Windows[0].Label != LabelFiveHour {
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
	groups := ParsePassive("claude", header, time.Now())
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
	groups := ParsePassive("claude", header, time.Now())
	if len(groups) != 2 || groups[1].Key != FableGroupKey || groups[1].SourceLabel != "Fable" {
		t.Fatalf("unexpected %+v", groups)
	}
	w := groups[1].Windows[0]
	if w.ID != WindowFable || w.Label != LabelFable || w.Remaining != 65 || w.Reset.Unix() != 1793865540 {
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
	groups := ParsePassive("codex", header, now)
	if len(groups) != 1 || len(groups[0].Windows) != 2 {
		t.Fatalf("unexpected %+v", groups)
	}
	group := groups[0]
	if group.Windows[0].ID != WindowFiveHour || group.Windows[0].Reset.Unix() != 1791064435 {
		t.Fatalf("5h %+v", group.Windows[0])
	}
	if group.Windows[1].ID != WindowSevenDay || group.Windows[1].Remaining != 7 || group.Windows[1].Reset.Unix() != now.Unix()+93262 {
		t.Fatalf("7d %+v", group.Windows[1])
	}
}

func TestParsePassiveIgnoresUnrelatedHeaders(t *testing.T) {
	if groups := ParsePassive("claude", http.Header{"Content-Type": {"application/json"}}, time.Now()); len(groups) > 0 {
		t.Fatal("headers without quota fields must be ignored")
	}
	if groups := ParsePassive("antigravity", http.Header{"X-Codex-Primary-Used-Percent": {"1"}}, time.Now()); len(groups) > 0 {
		t.Fatal("antigravity has no passive quota")
	}
}

func TestParseSecret(t *testing.T) {
	secret := ParseSecret(json.RawMessage(`{"access_token":"tok","account_id":"acc","proxy_url":"socks5://p:1","metadata":{"project_id":"proj"}}`))
	if secret.AccessToken != "tok" || secret.AccountID != "acc" || secret.ProxyURL != "socks5://p:1" || secret.ProjectID != "proj" {
		t.Fatalf("unexpected %+v", secret)
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]any{
		"seconds":      float64(1791064435),
		"milliseconds": float64(1791064435000),
		"string":       "1791064435",
		"iso":          "2026-10-03T21:53:55Z",
	}
	for name, value := range cases {
		if got := ParseTime(value).Unix(); got != 1791064435 {
			t.Errorf("%s: got %d", name, got)
		}
	}
	if !ParseTime("").IsZero() || !ParseTime(nil).IsZero() {
		t.Error("empty values must give zero time")
	}
}
