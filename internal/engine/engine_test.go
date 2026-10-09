package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeHost struct {
	mu       sync.Mutex
	auths    []pluginapi.HostAuthFileEntry
	files    map[string]string
	http     func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse
	execute  func(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	requests []pluginapi.HTTPRequest
	executed []pluginapi.HostModelExecutionRequest
}

func (f *fakeHost) AuthList() ([]pluginapi.HostAuthFileEntry, error) { return f.auths, nil }

func (f *fakeHost) AuthGet(authIndex string) (json.RawMessage, error) {
	raw, ok := f.files[authIndex]
	if !ok {
		return nil, errors.New("auth not found")
	}
	return json.RawMessage(raw), nil
}

func (f *fakeHost) ModelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	f.mu.Lock()
	f.executed = append(f.executed, req)
	f.mu.Unlock()
	return f.execute(req)
}

func (f *fakeHost) HTTPDo(_ context.Context, req pluginapi.HTTPRequest, _ time.Duration) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return f.http(req), nil
}

func (f *fakeHost) Log(string, string, map[string]any) {}

func (f *fakeHost) requestsTo(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, req := range f.requests {
		if strings.HasPrefix(req.URL, url) {
			n++
		}
	}
	return n
}

type fakeSender struct{ sent []notify.Message }

func (f *fakeSender) Send(_ context.Context, msg notify.Message) error {
	f.sent = append(f.sent, msg)
	return nil
}

var shanghai = time.FixedZone("UTC+8", 8*3600)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestEngine(t *testing.T, h *fakeHost, c *clock) (*Engine, *fakeSender) {
	t.Helper()
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"gpt-6-luna"},{"id":"claude-haiku-4-5-20251001"},{"id":"claude-3-5-haiku-20241022"},{"id":"claude-sonnet-4-6"},{"id":"gemini-3.8-flash-high"}]}`))
	}))
	t.Cleanup(models.Close)
	cfg := config.Default()
	cfg.CPABaseURL = models.URL
	cfg.ModelsAPIKey = "k"
	cfg.DataDir = t.TempDir()
	cfg.Timezone = "Asia/Shanghai"
	e := New(Options{Host: h, Version: "test", Now: c.now})
	e.Configure(cfg, nil)
	if err := e.openStores(cfg); err != nil {
		t.Fatal(err)
	}
	e.applyRuntimeConfig(cfg)
	sender := &fakeSender{}
	e.alerts.Sender = sender
	return e, sender
}

func standardHost(c *clock) *fakeHost {
	return &fakeHost{
		auths: []pluginapi.HostAuthFileEntry{
			{ID: "codex-a.json", AuthIndex: "1", Name: "codex-a.json", Provider: "codex", Email: "alice.work@example.com"},
			{ID: "codex-b.json", AuthIndex: "2", Name: "codex-b.json", Provider: "codex", Email: "bob.team@example.net"},
			{ID: "codex-c.json", AuthIndex: "5", Name: "codex-c.json", Provider: "codex", Disabled: true},
			{ID: "claude.json", AuthIndex: "3", Name: "claude.json", Provider: "claude", Email: "me@example.com"},
			{ID: "ag.json", AuthIndex: "4", Name: "ag.json", Provider: "antigravity"},
		},
		files: map[string]string{
			"1": `{"access_token":"t1","account_id":"acc1"}`,
			"2": `{"access_token":"t2"}`,
			"3": `{"access_token":"t3"}`,
			"4": `{"access_token":"t4","project_id":"p4"}`,
		},
		http: func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
			reset := c.t.Add(3 * time.Hour)
			switch {
			case req.URL == quota.CodexUsageURL:
				body := `{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_at":` +
					strconv.FormatInt(reset.Unix(), 10) + `},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}
			case req.URL == quota.ClaudeUsageURL:
				body := `{"five_hour":{"utilization":30,"resets_at":"` + reset.UTC().Format(time.RFC3339) + `"},"seven_day":{"utilization":20,"resets_at":"` +
					reset.Add(48*time.Hour).UTC().Format(time.RFC3339) + `"}}`
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}
			case strings.Contains(req.URL, "retrieveUserQuotaSummary"):
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"window":"5h","remainingFraction":0.5,"resetTime":"` +
					reset.UTC().Format(time.RFC3339) + `"}]}]}`)}
			}
			return pluginapi.HTTPResponse{StatusCode: 404}
		},
	}
}

func TestPollBuildsAccountsWithSuffixes(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)

	status := e.Status()
	if len(status.Accounts) != 4 {
		t.Fatalf("want 4 monitored accounts (disabled skipped), got %d", len(status.Accounts))
	}
	titles := []string{}
	for _, account := range status.Accounts {
		titles = append(titles, account.Title)
	}
	if strings.Join(titles, ",") != "ChatGPT#am,ChatGPT#rk,Claude,Antigravity" &&
		strings.Join(titles, ",") != "ChatGPT#rk,ChatGPT#am,Claude,Antigravity" {
		t.Fatalf("titles %v", titles)
	}
	for _, req := range h.requests {
		if req.URL == quota.CodexUsageURL && req.Headers.Get("Authorization") == "Bearer t1" && req.Headers.Get("Chatgpt-Account-Id") != "acc1" {
			t.Fatal("Codex request must carry the account ID")
		}
	}
	if status.Accounts[3].Groups[0].SourceLabel != "Gemini" {
		t.Fatalf("antigravity group %+v", status.Accounts[3].Groups)
	}
}

func TestStatusOrdersGroupsLikeThePage(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if !strings.Contains(req.URL, "retrieveUserQuotaSummary") {
			return base(req)
		}
		reset := c.t.Add(3 * time.Hour).UTC().Format(time.RFC3339)
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"groups":[{"displayName":"Claude and GPT Models","buckets":[{"window":"5h","remainingFraction":1,"resetTime":"` + reset + `"}]},{"displayName":"Gemini Models","buckets":[{"window":"5h","remainingFraction":0.5,"resetTime":"` + reset + `"}]}]}`)}
	}
	e, _ := newTestEngine(t, h, c)
	e.poll(context.Background(), e.config(), c.t, "", false)

	var labels []string
	for _, group := range e.Status().Accounts[3].Groups {
		labels = append(labels, group.SourceLabel)
	}
	if strings.Join(labels, ",") != "Gemini,Claude / GPT" {
		t.Fatalf("antigravity groups %v", labels)
	}
}

func TestPassiveDataSkipsActiveQueryNearTheSlot(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	before := h.requestsTo(quota.ClaudeUsageURL)

	// A Claude request 30 seconds before the next slot.
	c.add(4*time.Minute + 30*time.Second)
	e.applyUsage(ctx, usageEvent{provider: "claude", authIndex: "3", header: http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.35"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(3*time.Hour).Unix(), 10)},
	}})
	c.add(30 * time.Second)
	e.poll(ctx, e.config(), c.t, "", false)
	if got := h.requestsTo(quota.ClaudeUsageURL); got != before {
		t.Fatalf("Claude query should be skipped, got %d requests", got-before)
	}
	if got := e.Status().Accounts[2].Groups[0].Windows[0]; got.Source != "passive" || got.Remaining != 65 {
		t.Fatalf("passive window %+v", got)
	}

	// Passive data two minutes before the slot is too old.
	c.add(3 * time.Minute)
	e.passiveAt["3"] = c.t
	c.add(2 * time.Minute)
	e.poll(ctx, e.config(), c.t, "", false)
	if got := h.requestsTo(quota.ClaudeUsageURL); got != before+1 {
		t.Fatalf("Claude query should run, got %d", got-before)
	}

	// After 30 minutes without an active query, skipping stops.
	e.activeAt["3"] = c.t.Add(-31 * time.Minute)
	e.passiveAt["3"] = c.t.Add(-10 * time.Second)
	if e.shouldSkipActive(e.config(), e.creds["3"], c.t, c.t) {
		t.Fatal("skipping must stop after passive_skip_max_minutes")
	}
}

func claudeIgnitionHost(c *clock, failFirst error) *fakeHost {
	h := standardHost(c)
	expired := c.t.Add(-time.Minute)
	// The current Claude window expired a minute ago.
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == quota.ClaudeUsageURL {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"five_hour":{"utilization":0,"resets_at":"` + expired.UTC().Format(time.RFC3339) + `"}}`)}
		}
		return base(req)
	}
	calls := 0
	h.execute = func(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		calls++
		if calls == 1 && failFirst != nil {
			return pluginapi.HostModelExecutionResponse{}, failFirst
		}
		return pluginapi.HostModelExecutionResponse{StatusCode: 200, Headers: http.Header{
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
		}}, nil
	}
	return h
}

func TestIgnitionIsConfirmedFromResponseHeaders(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)

	if len(h.executed) != 1 {
		t.Fatalf("want one ignition, got %d", len(h.executed))
	}
	req := h.executed[0]
	if req.AuthID != "claude.json" || req.ForcedProvider != "claude" || req.Model != "claude-haiku-4-5-20251001" || req.EntryProtocol != "claude" {
		t.Fatalf("ignition request %+v", req)
	}
	ts := e.state.Target("claude:3:claude:main")
	if ts.LastModel != "claude-haiku-4-5-20251001" || ts.LastSuccessEpoch == 0 || ts.ConsecutiveFailures != 0 {
		t.Fatalf("target state %+v", ts)
	}
	var ignited *Event
	for i, ev := range e.Status().Events {
		if ev.Kind == "ignite" {
			ignited = &e.Status().Events[i]
		}
	}
	if ignited == nil || ignited.Params["model"] != "claude-haiku-4-5-20251001" || ignited.Params["reset"] == "" {
		t.Fatalf("ignite event %+v", ignited)
	}
	e.refreshTargets(cfg)
	for _, target := range e.Status().Targets {
		if target.Key == "claude:3:claude:main" && !target.NextDue.Equal(c.t.Add(5*time.Hour+3*time.Second).UTC()) {
			t.Fatalf("next due %v", target.NextDue)
		}
	}
}

func TestIgnitionTriesNextModelWhenCredentialRejectsOne(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, &host.Error{Code: "host_call_failed", Message: "auth_not_found: no auth available", Status: 503})
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 2 || h.executed[1].Model != "claude-3-5-haiku-20241022" {
		t.Fatalf("executed %+v", h.executed)
	}
	if ts := e.state.Target("claude:3:claude:main"); ts.LastModel != "claude-3-5-haiku-20241022" {
		t.Fatalf("target state %+v", ts)
	}
}

func TestIgnitionUsesNewestModelOverLastSuccessfulOne(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.state.Target("claude:3:claude:main").LastModel = "claude-3-5-haiku-20241022"
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 1 || h.executed[0].Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("executed %+v", h.executed)
	}
	if ts := e.state.Target("claude:3:claude:main"); ts.LastModel != "claude-haiku-4-5-20251001" {
		t.Fatalf("target state %+v", ts)
	}
}

func TestIgnitionFallsBackWhenProviderRefusesNewestModel(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	refused := &host.Error{Code: "model_execution_failed", Message: `{"type":"error","error":{"type":"not_found_error","message":"model: claude-haiku-4-5-20251001"}}`, Status: 404}
	h := claudeIgnitionHost(c, refused)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 2 || h.executed[1].Model != "claude-3-5-haiku-20241022" {
		t.Fatalf("executed %+v", h.executed)
	}
	if ts := e.state.Target("claude:3:claude:main"); ts.LastModel != "claude-3-5-haiku-20241022" || ts.ConsecutiveFailures != 0 {
		t.Fatalf("target state %+v", ts)
	}
	var noted *Event
	for i, ev := range e.Status().Events {
		if ev.Kind == "model_refused" {
			noted = &e.Status().Events[i]
		}
	}
	if noted == nil || noted.Params["model"] != "claude-haiku-4-5-20251001" || noted.Params["fallback"] != "claude-3-5-haiku-20241022" || noted.Params["hours"] != "24" {
		t.Fatalf("refusal event %+v", noted)
	}

	var target target
	for _, candidate := range e.collectTargets(cfg) {
		if candidate.group.Key == "claude:3:claude:main" {
			target = candidate
		}
	}
	c.add(23 * time.Hour)
	if model, _, err := e.execute(ctx, cfg, target); err != nil || model != "claude-3-5-haiku-20241022" || len(h.executed) != 3 {
		t.Fatalf("a refused model is skipped for a day: %s %v %+v", model, err, h.executed)
	}
	c.add(time.Hour)
	if model, _, err := e.execute(ctx, cfg, target); err != nil || model != "claude-haiku-4-5-20251001" {
		t.Fatalf("a refused model is tried again after a day: %s %v", model, err)
	}
}

func TestIgnitionFallsBackOnlyOnceForRefusedModels(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 400, Body: []byte(`{"type":"error","error":{"type":"invalid_request_error"}}`)}, nil
	}
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 2 {
		t.Fatalf("want two attempts, got %+v", h.executed)
	}
	ts := e.state.Target("claude:3:claude:main")
	if ts.CircuitOpenUntilEpoch == 0 {
		t.Fatalf("a second refusal must pause the target: %+v", ts)
	}
	if !strings.Contains(ts.LastError, "此前 claude-haiku-4-5-20251001 被上游拒绝（状态码 400）") {
		t.Fatalf("the error must name the first refused model: %s", ts.LastError)
	}
	for _, ev := range e.Status().Events {
		if ev.Kind == "model_refused" {
			t.Fatalf("a refusal without a working fallback is not remembered: %+v", ev)
		}
	}
}

func TestStatusShowsNextAutomaticModels(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	if got := e.Status().NextModels; len(got) != 0 {
		t.Fatalf("the model list is only read for an ignition, got %v", got)
	}
	e.performDue(ctx, cfg)
	got := e.Status().NextModels
	want := map[string]string{"codex": "gpt-6-luna", "claude": "claude-haiku-4-5-20251001", "antigravity": "gemini-3.8-flash-high"}
	if !maps.Equal(got, want) {
		t.Fatalf("next models %v", got)
	}
}

func TestModelListIsReadAfterStartAndKeyChange(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reads == 1 || r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-haiku-5-5"},{"id":"claude-haiku-4-5-20251001"}]}`))
	}))
	defer server.Close()
	ctx := context.Background()
	cfg := e.config()
	cfg.CPABaseURL = server.URL

	e.maybeReadModels(ctx, cfg, false)
	if reads != 1 || e.Status().ModelsError == "" {
		t.Fatalf("reads %d, status %+v", reads, e.Status())
	}
	e.maybeReadModels(ctx, cfg, false)
	if reads != 1 {
		t.Fatalf("a failed read waits for the next poll, got %d reads", reads)
	}
	e.maybeReadModels(ctx, cfg, true)
	if got := e.Status().NextModels["claude"]; reads != 2 || got != "claude-haiku-5-5" {
		t.Fatalf("reads %d, next model %q", reads, got)
	}
	e.maybeReadModels(ctx, cfg, true)
	if reads != 2 {
		t.Fatalf("a read list is not read again until the next ignition, got %d reads", reads)
	}
	cfg.ModelsAPIKey = "k2"
	e.maybeReadModels(ctx, cfg, false)
	if reads != 3 {
		t.Fatalf("a new models key is read at once, got %d reads", reads)
	}

	cfg.ModelsAPIKey = "bad"
	e.maybeReadModels(ctx, cfg, false)
	if status := e.Status(); reads != 4 || status.ModelsError == "" || len(status.NextModels) != 0 {
		t.Fatalf("a failing key shows no models: reads %d, %+v", reads, status)
	}
	cfg.ModelsAPIKey = "k2"
	e.maybeReadModels(ctx, cfg, false)
	if status := e.Status(); status.ModelsError != "" || status.NextModels["claude"] != "claude-haiku-5-5" {
		t.Fatalf("restoring a working key reads it again: %+v", status)
	}
	cfg.ModelsAPIKey = ""
	e.maybeReadModels(ctx, cfg, true)
	if status := e.Status(); status.ModelsError != "" || len(status.NextModels) != 0 {
		t.Fatalf("removing the key clears the models: %+v", status)
	}
}

func TestHardIgnitionFailurePausesAndNotifiesOnce(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "host_call_failed", Message: "unauthorized", Status: 401}
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	e.performDue(ctx, cfg)
	if len(h.executed) != 1 {
		t.Fatalf("a paused target must not be retried today, got %d attempts", len(h.executed))
	}
	paused := 0
	for _, msg := range sender.sent {
		if strings.Contains(msg.Title, "点火已暂停") {
			paused++
		}
	}
	if paused != 1 {
		t.Fatalf("want one pause notification, got %+v", sender.sent)
	}
}

func TestHistoryRecordsActiveAndPassiveSamples(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	c.add(10 * time.Second)
	header := func(util string) http.Header {
		return http.Header{"Anthropic-Ratelimit-Unified-5h-Utilization": {util}, "Anthropic-Ratelimit-Unified-5h-Reset": {strconv.FormatInt(c.t.Add(3*time.Hour).Unix(), 10)}}
	}
	e.applyUsage(ctx, usageEvent{provider: "claude", authIndex: "3", header: header("0.40")})
	c.add(70 * time.Second)
	e.applyUsage(ctx, usageEvent{provider: "claude", authIndex: "3", header: header("0.45")})
	c.add(10 * time.Second)
	e.applyUsage(ctx, usageEvent{provider: "claude", authIndex: "3", header: header("0.50")})
	e.flushSamples(c.t, true)

	resp, err := e.History(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Series {
		if s.Group == "claude:3:claude:main" && s.Window == quota.WindowFiveHour {
			// Active 70%, passive 60% (within the minute of the active sample,
			// so held), passive 55% after a minute, passive 50% flushed.
			last := s.Points[len(s.Points)-1]
			if last[1] != 50 || last[2] != 1 {
				t.Fatalf("points %v", s.Points)
			}
			return
		}
	}
	t.Fatalf("claude series missing: %+v", resp.Series)
}

func TestLanguageReportedByThePageIsUsedForNotifications(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "host_call_failed", Message: "unauthorized", Status: 401}
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cmd := command{kind: "language", arg: "en-US", reply: make(chan error, 1)}
	e.runCommand(ctx, cmd)
	if err := <-cmd.reply; err != nil {
		t.Fatal(err)
	}
	if e.Status().Language != "en" || e.state.Language != "en" {
		t.Fatalf("language %q, state %q", e.Status().Language, e.state.Language)
	}
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{Monitor: true}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	var titles []string
	for _, msg := range sender.sent {
		titles = append(titles, msg.Title)
	}
	if strings.Join(titles, ",") != "⚠️ Claude ignition paused" {
		t.Fatalf("notifications %v", titles)
	}
	for _, ev := range e.Status().Events {
		if ev.Kind == "ignite_paused" && (ev.Params["until"] == "" || ev.Params["error"] == "") {
			t.Fatalf("paused event %+v", ev)
		}
	}
}

func TestQueriesAroundResetRunOncePerReset(t *testing.T) {
	start := time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)
	c := &clock{t: start}
	// The host's reset times follow a separate clock, so they stay fixed.
	h := standardHost(&clock{t: start})
	reset := start.Add(3 * time.Hour)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	// The queries run whatever the notification settings are.
	cfg := e.config()
	cfg.RecoveryNotify = config.WindowModes{FiveHour: config.RecoveryOff, SevenDay: config.RecoveryOff}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	if due, next := e.probes(c.t); len(due) != 0 || !next.Equal(reset.Add(-probeOffset)) {
		t.Fatalf("next pre-reset query: %v %v", due, next)
	}
	before := h.requestsTo(quota.ClaudeUsageURL)
	c.t = reset.Add(-20 * time.Second)
	// Computing the next wake-up does not use up a due query.
	if due, _ := e.probes(c.t); len(due) == 0 {
		t.Fatal("pre-reset queries are due 30 seconds before the reset")
	}
	e.runProbes(ctx, cfg, c.t)
	e.runProbes(ctx, cfg, c.t)
	if got := h.requestsTo(quota.ClaudeUsageURL) - before; got != 1 {
		t.Fatalf("want one pre-reset Claude query, got %d", got)
	}

	// A reset time a few seconds off is the same reset and is not queried again.
	e.mu.Lock()
	for _, g := range e.groups {
		for i := range g.Windows {
			if g.Windows[i].Reset.Sub(reset).Abs() < time.Minute {
				g.Windows[i].Reset = g.Windows[i].Reset.Add(5 * time.Second)
				g.Windows[i].ObservedAt = c.t.Add(-time.Minute)
			}
		}
	}
	e.mu.Unlock()
	e.runProbes(ctx, cfg, c.t)
	if got := h.requestsTo(quota.ClaudeUsageURL) - before; got != 1 {
		t.Fatalf("a jittered reset time must not repeat the query, got %d", got)
	}

	// The query after the reset comes next.
	if due, next := e.probes(c.t); len(due) != 0 || next.Sub(reset.Add(probeOffset)).Abs() > probeTolerance {
		t.Fatalf("next query after the reset: %v %v", due, next)
	}
	c.t = reset.Add(40 * time.Second)
	e.runProbes(ctx, cfg, c.t)
	e.runProbes(ctx, cfg, c.t)
	if got := h.requestsTo(quota.ClaudeUsageURL) - before; got != 2 {
		t.Fatalf("want one Claude query after the reset, got %d", got-1)
	}
	if due, next := e.probes(c.t); len(due) != 0 || next.Before(reset.Add(time.Hour)) {
		t.Fatalf("both queries ran; the next one belongs to a 7-day window: %v %v", due, next)
	}

	// A reset older than probeLate is left to the next poll.
	c.t = reset.Add(probeLate + time.Minute)
	e.mu.Lock()
	for _, g := range e.groups {
		for i := range g.Windows {
			g.Windows[i].Reset = reset
			g.Windows[i].ObservedAt = reset.Add(-time.Hour)
		}
	}
	e.mu.Unlock()
	e.probed = map[string][]time.Time{}
	if due, _ := e.probes(c.t); len(due) != 0 {
		t.Fatalf("a long-passed reset is not queried: %v", due)
	}
}

func TestPassiveRelativeResetCountsFromTheRequest(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	h := standardHost(c)
	e, _ := newTestEngine(t, h, c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	requested := c.t
	c.add(3 * time.Minute)
	e.applyUsage(ctx, usageEvent{provider: "codex", authIndex: "1", at: requested, header: http.Header{
		"X-Codex-Primary-Used-Percent":        {"70"},
		"X-Codex-Primary-Reset-After-Seconds": {"600"},
		"X-Codex-Primary-Window-Minutes":      {"300"},
	}})
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, g := range e.groups {
		if g.AuthIndex != "1" {
			continue
		}
		if w, ok := g.fiveHour(); !ok || !w.Reset.Equal(requested.Add(10*time.Minute)) {
			t.Fatalf("reset %v, want %v", w.Reset, requested.Add(10*time.Minute))
		}
		return
	}
	t.Fatal("no Codex group")
}
