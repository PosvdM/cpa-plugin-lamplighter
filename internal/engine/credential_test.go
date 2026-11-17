package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/failure"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/providers"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// tosError is the error CPA returned for the ignition of a suspended
// Antigravity account on 2026-11-16.
var tosError = &host.Error{Code: "host_call_failed", Status: 403, Message: `{
  "error": {
    "code": 403,
    "message": "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "appeal_url": "https://forms.gle/hGzM9MEUv2azZsrb9",
          "appeal_url_link_text": "Submit Appeal",
          "uiMessage": "true"
        }
      }
    ]
  }
}`}

func eventsOf(e *Engine, kind string) []Event {
	var out []Event
	for _, ev := range e.events {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func messagesOf(sender *fakeSender, kind string) []notify.Message {
	var out []notify.Message
	for _, msg := range sender.sent {
		if msg.Kind == kind {
			out = append(out, msg)
		}
	}
	return out
}

func claudeTarget(t *testing.T, e *Engine) target {
	t.Helper()
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "claude" {
			return candidate
		}
	}
	t.Fatal("missing Claude target")
	return target{}
}

func TestSuspendedAccountPausesIgnitionUntilTheCredentialChanges(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.files["3"] = `{"access_token":"t3","refresh_token":"r3"}`
	fail := true
	success := h.execute
	h.execute = func(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		if fail {
			return pluginapi.HostModelExecutionResponse{}, tosError
		}
		return success(req)
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 1 {
		t.Fatalf("want one ignition, got %d", len(h.executed))
	}
	suspended := messagesOf(sender, notify.KindSuspended)
	if len(sender.sent) != 1 || len(suspended) != 1 {
		t.Fatalf("want one suspension notification, got %+v", sender.sent)
	}
	msg := suspended[0]
	if msg.Title != "⛔ Claude 账号已停用" || !strings.HasPrefix(msg.Body, "403 · TOS_VIOLATION · 账号已被停用，需要申诉\n") ||
		!strings.Contains(msg.Body, "申诉：https://forms.gle/hGzM9MEUv2azZsrb9") || strings.Contains(msg.Body, "{") {
		t.Fatalf("message %+v", msg)
	}
	invalid := eventsOf(e, "credential_invalid")
	if len(invalid) != 1 {
		t.Fatalf("credential events %+v", invalid)
	}
	p := invalid[0].Params
	if p["reason"] != "suspended" || p["status"] != "403" || p["code"] != "TOS_VIOLATION" || p["source"] != "ignite" ||
		p["appeal_url"] != "https://forms.gle/hGzM9MEUv2azZsrb9" || p["until"] != "" || invalid[0].Level != "error" ||
		p["summary"] != "This service has been disabled in this account for violation of Terms of Service." {
		t.Fatalf("credential event %+v", invalid[0])
	}
	paused := eventsOf(e, "ignite_paused")
	if len(paused) != 1 || paused[0].Params["until"] != "" || paused[0].Params["reason"] != "suspended" || paused[0].Params["summary"] == "" || paused[0].Params["error"] == "" {
		t.Fatalf("paused events %+v", paused)
	}
	notices := eventsOf(e, "notify")
	// The page attaches the notification to the event of the same group.
	if len(notices) != 1 || notices[0].Params["notify_kind"] != "suspended" || notices[0].Group != invalid[0].Group || invalid[0].Group == "" {
		t.Fatalf("notify events %+v", notices)
	}

	// CPA keeps refreshing the access token, which is no new login.
	for day := 1; day <= 2; day++ {
		c.t = time.Date(2026, 11, 16+day, 7, 0, 0, 0, shanghai)
		h.files["3"] = `{"access_token":"t3-` + c.t.Format("0102") + `","refresh_token":"r3","tos_appeal_url":"x"}`
		e.poll(ctx, cfg, c.t, "", false)
		e.performDue(ctx, cfg)
	}
	if len(h.executed) != 1 || len(sender.sent) != 1 {
		t.Fatalf("a suspended account must not be retried: %d ignitions, %+v", len(h.executed), sender.sent)
	}
	e.refreshTargets(cfg)
	for _, view := range e.Status().Targets {
		if view.Provider == "claude" && !view.NextDue.IsZero() {
			t.Fatalf("a blocked target has no next ignition: %+v", view)
		}
	}

	// A new login lets ignition try again; the same problem is not
	// notified twice.
	h.files["3"] = `{"access_token":"t3-new","refresh_token":"r3-new"}`
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 2 || len(sender.sent) != 1 || len(eventsOf(e, "credential_invalid")) != 1 {
		t.Fatalf("retry after a new login: %d ignitions, %+v", len(h.executed), sender.sent)
	}
	e.performDue(ctx, cfg)
	if len(h.executed) != 2 {
		t.Fatal("the same failure blocks the new credential again")
	}

	// After the appeal, a manual ignition succeeds and ends the pause.
	fail = false
	if err := e.igniteTarget(ctx, cfg, claudeTarget(t, e), true); err != nil {
		t.Fatal(err)
	}
	restored := messagesOf(sender, notify.KindCredentialOK)
	if len(restored) != 1 || restored[0].Title != "✅ Claude 凭证已恢复" || e.credentialBlocked("3") {
		t.Fatalf("restored %+v", sender.sent)
	}
	if ev := eventsOf(e, "credential_restored"); len(ev) != 1 || ev[0].Params["source"] != "ignite" || ev[0].Level != "info" {
		t.Fatalf("restored events %+v", ev)
	}
	if _, ok := e.state.Credentials["3"]; ok {
		t.Fatalf("state kept after recovery: %+v", e.state.Credentials["3"])
	}
}

func TestRejectedQueryNotifiesOnceAfterItLasts(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := standardHost(c)
	rejected := true
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL && rejected {
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired."}}`)}
		}
		return base(req)
	}
	dir := t.TempDir()
	e, sender := newTestEngineIn(t, h, c, dir)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	// An expired access token is refreshed by CPA within minutes.
	for i := 0; i < 7; i++ {
		e.poll(ctx, cfg, c.t, "", true)
		if len(sender.sent) != 0 && c.t.Before(time.Date(2026, 11, 16, 7, 30, 0, 0, shanghai)) {
			t.Fatalf("notified before the rejection lasted: %+v", sender.sent)
		}
		c.add(5 * time.Minute)
	}
	credential := messagesOf(sender, notify.KindCredential)
	if len(sender.sent) != 1 || len(credential) != 1 || credential[0].Title != "🔑 Claude 凭证失效" ||
		!strings.Contains(credential[0].Body, "来源：额度查询") || !strings.Contains(credential[0].Body, "401") {
		t.Fatalf("want one credential notification, got %+v", sender.sent)
	}
	if ev := eventsOf(e, "credential_invalid"); len(ev) != 1 || ev[0].Params["reason"] != "auth" || ev[0].Params["source"] != "query" {
		t.Fatalf("events %+v", ev)
	}
	e.saveState()

	// A restart neither forgets nor repeats the notification.
	e2, sender2 := newTestEngineIn(t, h, c, dir)
	e2.Configure(cfg, nil)
	e2.poll(ctx, cfg, c.t, "", true)
	if len(sender2.sent) != 0 {
		t.Fatalf("notified again after a restart: %+v", sender2.sent)
	}
	rejected = false
	c.add(5 * time.Minute)
	e2.poll(ctx, cfg, c.t, "", true)
	restored := messagesOf(sender2, notify.KindCredentialOK)
	if len(sender2.sent) != 1 || len(restored) != 1 || restored[0].Body != "额度查询恢复正常。" {
		t.Fatalf("want one restored notification, got %+v", sender2.sent)
	}
	if ev := eventsOf(e2, "credential_restored"); len(ev) != 1 || ev[0].Params["source"] != "query" {
		t.Fatalf("events %+v", ev)
	}
}

func TestShortQueryRejectionIsNoProblem(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := standardHost(c)
	calls := 0
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL {
			calls++
			if calls <= 2 {
				return pluginapi.HTTPResponse{StatusCode: 401}
			}
		}
		return base(req)
	}
	e, sender := newTestEngine(t, h, c)
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	for i := 0; i < 4; i++ {
		e.poll(context.Background(), cfg, c.t, "", true)
		c.add(20 * time.Minute)
	}
	if len(sender.sent) != 0 || len(eventsOf(e, "credential_invalid")) != 0 || len(e.state.Credentials) != 0 {
		t.Fatalf("a short rejection must leave no trace: %+v %+v", sender.sent, e.state.Credentials)
	}
}

func TestRateLimitedIgnitionNotifiesWithPauseEnd(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "host_call_failed", Status: 429,
			Message: `{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit."}}`}
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	e.performDue(ctx, cfg)
	limited := messagesOf(sender, notify.KindRateLimited)
	if len(h.executed) != 1 || len(sender.sent) != 1 || len(limited) != 1 || limited[0].Title != "⏳ Claude 被限流" ||
		!strings.Contains(limited[0].Body, "11/17 07:00") || !strings.Contains(limited[0].Body, "Number of requests has exceeded your rate limit.") {
		t.Fatalf("rate limit notification %+v", sender.sent)
	}
	ev := eventsOf(e, "credential_invalid")
	if len(ev) != 1 || ev[0].Params["reason"] != "rate_limited" || ev[0].Params["until"] != "2026-11-16T23:00:00Z" {
		t.Fatalf("events %+v", ev)
	}
	if e.credentialBlocked("3") {
		t.Fatal("a rate limit pauses until the next day, not until a new credential")
	}
	if n := eventsOf(e, "notify"); len(n) != 1 || n[0].Params["notify_kind"] != "rate_limited" {
		t.Fatalf("notify events %+v", n)
	}
}

func TestCircuitNotificationShowsOneSentence(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 10, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, &host.Error{Code: "host_call_failed", Status: 403,
			Message: "{\n  \"error\": {\n    \"code\": 403,\n    \"message\": \"The caller does not have permission\",\n    \"status\": \"PERMISSION_DENIED\"\n  }\n}"}
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	circuit := messagesOf(sender, notify.KindCircuit)
	if len(sender.sent) != 1 || len(circuit) != 1 || strings.Contains(circuit[0].Body, "{") ||
		!strings.HasSuffix(circuit[0].Body, "\nThe caller does not have permission") {
		t.Fatalf("circuit notification %+v", sender.sent)
	}
	if n := eventsOf(e, "notify"); len(n) != 1 || n[0].Params["notify_kind"] != "circuit" || n[0].Group != claudeTarget(t, e).group.Key {
		t.Fatalf("notify events %+v", n)
	}
}

func TestMCPReportsCredentialProblems(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	ctx := context.Background()
	cfg := e.config()
	e.poll(ctx, cfg, c.t, "", false)
	cred := e.creds["3"]
	e.noteCredentialProblem(ctx, cred, e.accountGroup("3"), failure.Diagnose(tosError), sourceIgnite, "fp")
	e.mu.Lock()
	e.running, e.lastPoll = true, c.t
	e.mu.Unlock()
	b := NewMCPBackend(e)
	result, err := b.CallTool(ctx, "get_quota_events", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	text := string(raw)
	if !strings.Contains(text, `"kind":"credential_invalid"`) || !strings.Contains(text, `"reason":"suspended"`) || !strings.Contains(text, `"source":"ignite"`) ||
		strings.Contains(text, "Terms of Service") || strings.Contains(text, "forms.gle") {
		t.Fatalf("events %s", text)
	}
	result, err = b.CallTool(ctx, "list_quotas", map[string]string{"provider": "claude"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result)
	if text := string(raw); !strings.Contains(text, `"account_suspended":true`) || !strings.Contains(text, `"credential_invalid":false`) {
		t.Fatalf("list %s", text)
	}
}

// tosBody is the quota query answer of a suspended Antigravity account.
const tosBody = `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service.","status":"PERMISSION_DENIED","details":[{"reason":"TOS_VIOLATION","metadata":{"appeal_url":"https://forms.gle/hGzM9MEUv2azZsrb9"}}]}}`

// mcpText calls an MCP tool of e and returns its JSON result.
func mcpText(t *testing.T, e *Engine, tool string, args map[string]string) string {
	t.Helper()
	e.mu.Lock()
	e.running, e.lastPoll = true, e.now()
	e.mu.Unlock()
	result, err := NewMCPBackend(e).CallTool(context.Background(), tool, args)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	return string(raw)
}

// A 401 that names a disabled account is a suspension from the first query,
// also for an account whose first query fails and that has no quota group;
// MCP still reports its event, also after a restart.
func TestDeactivatedAccountQueryWithoutGroups(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := standardHost(c)
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL {
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":{"message":"Your account has been deactivated.","type":"invalid_request_error","code":"account_deactivated"}}`)}
		}
		return base(req)
	}
	dir := t.TempDir()
	e, sender := newTestEngineIn(t, h, c, dir)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", true)
	suspended := messagesOf(sender, notify.KindSuspended)
	if len(sender.sent) != 1 || len(suspended) != 1 || !strings.HasPrefix(suspended[0].Body, "401 · account_deactivated · ") || !strings.Contains(suspended[0].Body, "来源：额度查询") {
		t.Fatalf("want one suspension notification, got %+v", sender.sent)
	}
	invalid := eventsOf(e, "credential_invalid")
	if len(invalid) != 1 || invalid[0].Params["reason"] != "suspended" || invalid[0].Params["source"] != "query" || invalid[0].Group != "" || invalid[0].Account != "3" {
		t.Fatalf("events %+v", invalid)
	}
	if n := eventsOf(e, "notify"); len(n) != 1 || n[0].Group != invalid[0].Group {
		t.Fatalf("notify events %+v", n)
	}
	var label string
	for i, a := range e.Status().Accounts {
		if a.AuthIndex == "3" {
			label = accountLabels(e.Status().Accounts)[i]
		}
	}
	want := `"account":"` + label + `"`
	if text := mcpText(t, e, "get_quota_events", nil); label == "" || !strings.Contains(text, `"kind":"credential_invalid"`) || !strings.Contains(text, want) ||
		!strings.Contains(text, `"reason":"suspended"`) || strings.Contains(text, "deactivated.") {
		t.Fatalf("events %s", text)
	}
	if text := mcpText(t, e, "list_quotas", map[string]string{"provider": "claude"}); !strings.Contains(text, `"account_suspended":true`) {
		t.Fatalf("list %s", text)
	}
	e.saveState()

	e2, _ := newTestEngineIn(t, h, c, dir)
	e2.Configure(cfg, nil)
	e2.poll(ctx, cfg, c.t, "", true)
	if n := len(eventsOf(e2, "credential_invalid")); n != 1 {
		t.Fatalf("%d credential events after a restart", n)
	}
	if text := mcpText(t, e2, "get_quota_events", nil); !strings.Contains(text, `"kind":"credential_invalid"`) || !strings.Contains(text, want) {
		t.Fatalf("events after a restart %s", text)
	}
}

// A problem recorded while the credential file could not be read has no
// fingerprint. The first readable file, also after a restart, lets ignition
// try again; the fingerprint is then taken before the request.
func TestUnknownFingerprintIsSampledAgain(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.files["3"] = `{"access_token":"t3","refresh_token":"r3"}`
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, tosError
	}
	dir := t.TempDir()
	e, sender := newTestEngineIn(t, h, c, dir)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	delete(h.files, "3")
	e.performDue(ctx, cfg)
	if st := e.state.Credentials["3"]; len(h.executed) != 1 || st == nil || st.Fingerprint != "" || !e.credentialBlocked("3") || len(sender.sent) != 1 {
		t.Fatalf("problem with an unknown fingerprint: %d ignitions, %+v", len(h.executed), st)
	}
	e.saveState()

	c.t = time.Date(2026, 11, 17, 7, 0, 0, 0, shanghai)
	newLogin := `{"access_token":"t3-new","refresh_token":"r3-new"}`
	h.files["3"] = newLogin
	e2, sender2 := newTestEngineIn(t, h, c, dir)
	e2.Configure(cfg, nil)
	e2.poll(ctx, cfg, c.t, "", false)
	if e2.credentialBlocked("3") {
		t.Fatal("a readable credential after an unknown one must let ignition try again")
	}
	// The file disappears while the request is out; the fingerprint read
	// before it is kept.
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		delete(h.files, "3")
		return pluginapi.HostModelExecutionResponse{}, tosError
	}
	e2.performDue(ctx, cfg)
	st := e2.state.Credentials["3"]
	if len(h.executed) != 2 || st == nil || st.Fingerprint != providers.Fingerprint(json.RawMessage(newLogin)) || !e2.credentialBlocked("3") || len(sender2.sent) != 0 {
		t.Fatalf("retry: %d ignitions, state %+v, sent %+v", len(h.executed), st, sender2.sent)
	}
	h.files["3"] = newLogin
	e2.poll(ctx, cfg, c.t, "", false)
	e2.performDue(ctx, cfg)
	if len(h.executed) != 2 {
		t.Fatal("the same credential must stay blocked")
	}
}

// A problem a query found and an ignition confirmed after a new login ends
// only with a successful ignition, not with the next working query.
func TestIgnitionConfirmsAQueryProblem(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.files["3"] = `{"access_token":"t3","refresh_token":"r3"}`
	rejected := false
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL && rejected {
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(tosBody)}
		}
		return base(req)
	}
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, tosError
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	rejected = true
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 0 || len(messagesOf(sender, notify.KindSuspended)) != 1 || e.state.Credentials["3"].Source != sourceQuery {
		t.Fatalf("query problem: %d ignitions, %+v", len(h.executed), sender.sent)
	}
	h.files["3"] = `{"access_token":"t3-new","refresh_token":"r3-new"}`
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if st := e.state.Credentials["3"]; len(h.executed) != 1 || st.Source != sourceQuery || !st.Ignited || !e.credentialBlocked("3") || len(sender.sent) != 1 {
		t.Fatalf("ignition after a new login: %d ignitions, state %+v, sent %+v", len(h.executed), st, sender.sent)
	}
	rejected = false
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if len(h.executed) != 1 || !e.credentialBlocked("3") || len(eventsOf(e, "credential_restored")) != 0 || len(sender.sent) != 1 {
		t.Fatalf("a working query ended a problem ignition confirmed: %d ignitions, %+v", len(h.executed), sender.sent)
	}
}

// The confirmation queries of a manual ignition leave the credential state
// alone, like its failure.
func TestManualConfirmationLeavesCredentialState(t *testing.T) {
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	rejected := false
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL && rejected {
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(tosBody)}
		}
		return base(req)
	}
	// The response has no quota headers, so a query has to confirm it.
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{StatusCode: 200}, nil
	}
	e, sender := newTestEngine(t, h, c)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	rejected = true
	err := e.igniteTarget(ctx, cfg, claudeTarget(t, e), true)
	if err == nil || len(h.executed) != 1 {
		t.Fatalf("manual ignition: err=%v ignitions=%d", err, len(h.executed))
	}
	if _, ok := e.state.Credentials["3"]; ok || e.credentialBlocked("3") || len(eventsOf(e, "credential_invalid")) != 0 || len(sender.sent) != 0 {
		t.Fatalf("manual confirmation changed the credential state: %+v %+v", e.state.Credentials["3"], sender.sent)
	}
	if failed := eventsOf(e, "ignite_failed"); len(failed) != 1 || failed[0].Params["manual"] != "1" {
		t.Fatalf("ignite_failed %+v", failed)
	}
	// The next poll finds the suspension.
	e.poll(ctx, cfg, c.t, "", false)
	if !e.credentialBlocked("3") || len(messagesOf(sender, notify.KindSuspended)) != 1 {
		t.Fatalf("poll after the manual ignition: %+v", sender.sent)
	}
}

// A credential problem without a readable message shows its status and code
// once, without the fixed summary that repeats them.
func TestCredentialProblemWithoutMessage(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	e, sender := newTestEngine(t, standardHost(c), c)
	ctx := context.Background()
	e.poll(ctx, e.config(), c.t, "", false)
	sender.sent = nil
	d := failure.Diagnose(&quota.HTTPError{Status: 403, Body: `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"reason":"TOS_VIOLATION"}]}}`})
	if d.Reason != failure.ReasonSuspended || !d.SummaryFallback || d.Summary != "HTTP 403 · TOS_VIOLATION" {
		t.Fatalf("%+v", d)
	}
	e.noteCredentialProblem(ctx, e.creds["3"], e.accountGroup("3"), d, sourceQuery, "fp")
	ev := eventsOf(e, "credential_invalid")
	if len(ev) != 1 || ev[0].Params["summary"] != "" || ev[0].Detail != "403 · TOS_VIOLATION · 账号已停用" {
		t.Fatalf("events %+v", ev)
	}
	if len(sender.sent) != 1 || strings.Contains(sender.sent[0].Body, "HTTP 403") || !strings.HasPrefix(sender.sent[0].Body, "403 · TOS_VIOLATION · 账号已被停用，需要申诉\n来源：额度查询") {
		t.Fatalf("notification %+v", sender.sent)
	}
}

// failingSender records every notification and fails while fail is set.
type failingSender struct {
	sent []notify.Message
	fail bool
}

func (f *failingSender) Send(_ context.Context, msg notify.Message) error {
	f.sent = append(f.sent, msg)
	if f.fail {
		return errors.New("bark unreachable")
	}
	return nil
}

// A failed credential notification is tried once per poll: the poll that
// finds the problem does not retry it at its end, the next polls do.
func TestFailedCredentialNotificationIsTriedOncePerPoll(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := standardHost(c)
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL {
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(tosBody)}
		}
		return base(req)
	}
	e, _ := newTestEngine(t, h, c)
	sender := &failingSender{fail: true}
	e.alerts.Sender = sender
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", true)
	if len(sender.sent) != 1 || len(eventsOf(e, "notify_failed")) != 1 {
		t.Fatalf("first poll: %d sends, %d notify_failed", len(sender.sent), len(eventsOf(e, "notify_failed")))
	}
	c.add(5 * time.Minute)
	e.poll(ctx, cfg, c.t, "", true)
	if len(sender.sent) != 2 || len(eventsOf(e, "notify_failed")) != 2 {
		t.Fatalf("second poll: %d sends, %d notify_failed", len(sender.sent), len(eventsOf(e, "notify_failed")))
	}
	sender.fail = false
	c.add(5 * time.Minute)
	e.poll(ctx, cfg, c.t, "", true)
	e.poll(ctx, cfg, c.t, "", true)
	if len(sender.sent) != 3 || len(eventsOf(e, "notify")) != 1 || !e.state.Credentials["3"].Notified {
		t.Fatalf("delivered: %d sends, %+v", len(sender.sent), e.state.Credentials["3"])
	}
}

// An ignition-confirmed problem replaced by a problem of another reason,
// found by queries, stays ignition-confirmed across a restart: a working
// query does not end it.
func TestReasonChangeKeepsIgnitionConfirmation(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.files["3"] = `{"access_token":"t3","refresh_token":"r3"}`
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		return pluginapi.HostModelExecutionResponse{}, tosError
	}
	rejected := false
	base := h.http
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL && rejected {
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid bearer token."}}`)}
		}
		return base(req)
	}
	dir := t.TempDir()
	e, sender := newTestEngineIn(t, h, c, dir)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	e.performDue(ctx, cfg)
	if st := e.state.Credentials["3"]; st == nil || st.Reason != "suspended" || !st.Ignited {
		t.Fatalf("ignition problem %+v", st)
	}
	// Queries are rejected long enough to report a rejected credential.
	rejected = true
	for i := 0; i < 7; i++ {
		e.poll(ctx, cfg, c.t, "", true)
		c.add(5 * time.Minute)
	}
	st := e.state.Credentials["3"]
	if st == nil || st.Reason != "auth" || st.Source != sourceQuery || !st.Ignited || !e.credentialBlocked("3") {
		t.Fatalf("replaced problem %+v", st)
	}
	if ev := eventsOf(e, "credential_invalid"); len(ev) != 2 || ev[1].Params["reason"] != "auth" || ev[1].Params["source"] != "query" ||
		len(messagesOf(sender, notify.KindCredential)) != 1 {
		t.Fatalf("events %+v, sent %+v", ev, sender.sent)
	}
	e.saveState()

	e2, sender2 := newTestEngineIn(t, h, c, dir)
	e2.Configure(cfg, nil)
	rejected = false
	e2.poll(ctx, cfg, c.t, "", true)
	e2.performDue(ctx, cfg)
	if !e2.credentialBlocked("3") || len(eventsOf(e2, "credential_restored")) != 0 || len(sender2.sent) != 0 || len(h.executed) != 1 {
		t.Fatalf("a working query ended an ignition-confirmed problem: %+v, sent %+v", e2.state.Credentials["3"], sender2.sent)
	}

	// A new credential drops the confirmation: a problem of another
	// reason that a query finds with it ends with a working query.
	h.files["3"] = `{"access_token":"t3-new","refresh_token":"r3-new"}`
	suspendedQuery := true
	h.http = func(req pluginapi.HTTPRequest) pluginapi.HTTPResponse {
		if req.URL == providers.ClaudeUsageURL && suspendedQuery {
			return pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(tosBody)}
		}
		return base(req)
	}
	c.add(5 * time.Minute)
	e2.poll(ctx, cfg, c.t, "", true)
	if st := e2.state.Credentials["3"]; st == nil || st.Reason != "suspended" || st.Ignited {
		t.Fatalf("problem of the new credential %+v", st)
	}
	suspendedQuery = false
	c.add(5 * time.Minute)
	e2.poll(ctx, cfg, c.t, "", true)
	if e2.credentialBlocked("3") || len(eventsOf(e2, "credential_restored")) != 1 {
		t.Fatalf("query problem of the new credential: %+v", e2.state.Credentials["3"])
	}
}

// When the credential cannot be read before the request, a rejection does
// not take the file read afterwards for the rejected credential: a login
// that replaced it while the request was out still gets its ignition.
func TestLoginDuringTheRequestIsNotBlocked(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 16, 7, 0, 0, 0, shanghai)}
	h := claudeIgnitionHost(c, nil)
	h.files["3"] = `{"access_token":"t3","refresh_token":"r3"}`
	newLogin := `{"access_token":"t3-new","refresh_token":"r3-new"}`
	h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
		h.files["3"] = newLogin
		return pluginapi.HostModelExecutionResponse{}, tosError
	}
	dir := t.TempDir()
	e, _ := newTestEngineIn(t, h, c, dir)
	ctx := context.Background()
	cfg := e.config()
	cfg.Providers["codex"] = config.Provider{}
	e.Configure(cfg, nil)
	e.poll(ctx, cfg, c.t, "", false)
	delete(h.files, "3")
	e.performDue(ctx, cfg)
	if st := e.state.Credentials["3"]; len(h.executed) != 1 || st == nil || st.Fingerprint != "" {
		t.Fatalf("the login read after the request was taken for the rejected one: %+v", st)
	}
	e.saveState()

	e2, _ := newTestEngineIn(t, h, c, dir)
	e2.Configure(cfg, nil)
	e2.poll(ctx, cfg, c.t, "", false)
	e2.performDue(ctx, cfg)
	if st := e2.state.Credentials["3"]; len(h.executed) != 2 || st == nil || st.Fingerprint != providers.Fingerprint(json.RawMessage(newLogin)) {
		t.Fatalf("the new login was not tried: %d ignitions, %+v", len(h.executed), st)
	}
}
