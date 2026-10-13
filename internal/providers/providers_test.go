package providers

import (
	"context"
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

// xaiBilling is a billing?format=credits response of a SuperGrok account
// (JWT tier 1), read on 2026-10-13.
const xaiBilling = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-06T04:01:28.835241+00:00","end":"2026-10-13T04:01:28.835241+00:00"},"creditUsagePercent":23.0,"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"productUsage":[{"product":"GrokBuild","usagePercent":17.0},{"product":"GrokChat","usagePercent":6.0}],"isUnifiedBillingUser":true,"prepaidBalance":{"val":0},"topUpMethod":"TOP_UP_METHOD_SAVED_PAYMENT_METHOD","billingPeriodStart":"2026-10-06T04:01:28.835241+00:00","billingPeriodEnd":"2026-10-13T04:01:28.835241+00:00"}}`

func TestParseXAIBillingReadsTheWeeklyCredits(t *testing.T) {
	groups, err := ParseXAIBilling(decode(t, xaiBilling))
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Key != "xai:main" || groups[0].SourceLabel != "Grok" || len(groups[0].Windows) != 1 {
		t.Fatalf("groups %+v", groups)
	}
	w := groups[0].Windows[0]
	if w.ID != quota.WindowSevenDay || w.Label != quota.LabelSevenDay || w.Remaining != 77 || w.Period != quota.SevenDays {
		t.Fatalf("window %+v", w)
	}
	if want := time.Date(2026, 10, 13, 4, 1, 28, 835241000, time.UTC); !w.Reset.Equal(want) {
		t.Fatalf("reset %v, want %v", w.Reset, want)
	}
	if _, err := ParseXAIBilling(decode(t, `{"config":{"monthlyLimit":{"val":0}}}`)); err == nil {
		t.Fatal("the monthly spending response has no credit usage")
	}
	if xai.Ignition != nil || xai.Headers != nil {
		t.Fatal("Grok has no ignition and no quota headers")
	}
}

func TestFetchXAISendsTheGrokHeaders(t *testing.T) {
	doer := &recordingDoer{body: xaiBilling}
	cred := ParseCredential(json.RawMessage(`{"type":"xai","access_token":"tok","sub":"user-1"}`))
	if _, err := Fetch(context.Background(), xai, doer, cred, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := doer.req.Header
	if doer.req.URL != XAIBillingURL || h.Get("Authorization") != "Bearer tok" || h.Get("x-userid") != "user-1" ||
		h.Get("x-xai-token-auth") != "xai-grok-cli" || h.Get("x-grok-client-version") != "1.0.46" {
		t.Fatalf("request %+v", doer.req)
	}
}

type recordingDoer struct {
	body string
	req  quota.Request
}

func (d *recordingDoer) Do(_ context.Context, _ string, req quota.Request) (quota.Response, error) {
	d.req = req
	return quota.Response{Status: 200, Body: []byte(d.body)}, nil
}

func TestParseKimiUsageMatchesTheManagementCenter(t *testing.T) {
	now := time.Date(2099, 7, 31, 4, 0, 0, 0, time.UTC)
	// The payload of the Management Center's Kimi ordering test.
	groups, err := ParseKimiUsage(decode(t, `{
		"usage":{"used":"1","limit":"100","remaining":"99","resetTime":"2099-08-06T13:59:23.136523Z"},
		"limits":[{"detail":{"used":"2","limit":"100","remaining":"98","resetTime":"2099-07-31T06:59:23.136523Z"},
			"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"}}]}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Key != "kimi:main" || groups[0].SourceLabel != "Kimi" {
		t.Fatalf("groups %+v", groups)
	}
	w := groups[0].Windows
	if len(w) != 2 {
		t.Fatalf("windows %+v", w)
	}
	if w[0].ID != quota.WindowFiveHour || w[0].Label != quota.LabelFiveHour || w[0].Remaining != 98 || w[0].Period != quota.FiveHours ||
		!w[0].Reset.Equal(time.Date(2099, 7, 31, 6, 59, 23, 136523000, time.UTC)) {
		t.Fatalf("5-hour window %+v", w[0])
	}
	if w[1].ID != quota.WindowSevenDay || w[1].Remaining != 99 || quota.Kind(w[1]) != quota.KindSevenDay {
		t.Fatalf("weekly window %+v", w[1])
	}

	// A relative reset, a remaining count instead of used, and a window of
	// another length.
	groups, err = ParseKimiUsage(decode(t, `{"limits":[
		{"detail":{"limit":200,"remaining":150,"reset_in":3600},"window":{"duration":1,"timeUnit":"TIME_UNIT_DAY"}},
		{"detail":{"limit":0,"used":0}}]}`), now)
	if err != nil {
		t.Fatal(err)
	}
	w = groups[0].Windows
	if len(w) != 1 || w[0].ID != "limit-0" || w[0].Label != "1d" || w[0].Remaining != 75 || !w[0].Reset.Equal(now.Add(time.Hour)) || quota.Kind(w[0]) != "" {
		t.Fatalf("other window %+v", w)
	}
	if _, err := ParseKimiUsage(decode(t, `{"limits":[]}`), now); err == nil {
		t.Fatal("no windows must fail")
	}
}

func TestKimiUsageURLFollowsTheDomain(t *testing.T) {
	cases := []struct {
		provider, file, want string
	}{
		{"kimi", `{"access_token":"t"}`, KimiUsageURL},
		{"kimi-ai", `{"access_token":"t"}`, KimiAIUsageURL},
		{"kimi_ai", `{"access_token":"t"}`, KimiAIUsageURL},
		{"kimi", `{"access_token":"t","domain":"kimi.ai"}`, KimiAIUsageURL},
		{"kimi-ai", `{"access_token":"t","domain":"other"}`, KimiUsageURL},
		{"kimi", `{"access_token":"t","base_url":"https://api.kimi.ai/coding/v1"}`, KimiAIUsageURL},
		{"kimi", `{"access_token":"t","type":"kimi-ai"}`, KimiAIUsageURL},
	}
	for _, c := range cases {
		cred := ParseCredential(json.RawMessage(c.file))
		cred.Provider = c.provider
		if got := kimiUsageURL(cred); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.provider, c.file, got, c.want)
		}
	}
	for _, name := range []string{"kimi", "kimi-ai", "kimi_ai", "kimi.com", "Kimi.AI"} {
		if Lookup(name) != kimi {
			t.Errorf("%s is not Kimi", name)
		}
	}
}

func TestKimiCandidatesSkipThinkingAndHighSpeed(t *testing.T) {
	available := []string{"kimi-k2", "kimi-k2-thinking", "kimi-k2.5", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.8", "gpt-6-luna"}
	got, err := Candidates(kimi, "Kimi", "", available)
	if err != nil || strings.Join(got, ",") != "kimi-k2.8,kimi-k2.7-code,kimi-k2.5,kimi-k2" {
		t.Fatalf("got %v %v", got, err)
	}
	if kimi.Ignition.Protocol != "openai" || kimi.Headers != nil {
		t.Fatal("Kimi ignites over the OpenAI protocol and sends no quota headers")
	}
}
