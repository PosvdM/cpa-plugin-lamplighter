package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// webhookFromYAML builds a webhook the way the plugin does, from the YAML
// subtree, so values such as success_json have the types YAML decodes to.
func webhookFromYAML(t *testing.T, yaml string) (*Webhook, error) {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return NewWebhook(cfg.Webhook)
}

type captured struct {
	method, path, query, body string
	header                    http.Header
}

func captureServer(t *testing.T, status int, response string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = captured{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(raw), header: r.Header}
		w.WriteHeader(status)
		w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server, got
}

var testMessage = Message{
	Kind:    KindQuota,
	Title:   `🔴 ChatGPT#eg · 5h 8% "low"`,
	Body:    "5h：8% | 03h\n7d：60%",
	Level:   LevelTimeSensitive,
	JumpURL: "https://example.com/a?b=c",
	Label:   "ChatGPT#eg",
}

func TestWebhookEscapesPlaceholdersWhereTheyAppear(t *testing.T) {
	server, got := captureServer(t, 200, "ok")
	w, err := webhookFromYAML(t, `webhook:
  url: "`+server.URL+`/hook/{{kind}}?label={{label}}"
  headers:
    X-Title: "{{text}}"
  body: |
    {"title":"{{title}}","body":"{{ body }}","priority":"{{priority}}","url":"{{url}}"}
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/hook/quota" || got.query != "label=ChatGPT%23eg" {
		t.Fatalf("request %s %s?%s", got.method, got.path, got.query)
	}
	if ct := got.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	if h := got.header.Get("X-Title"); strings.ContainsAny(h, "\r\n") || !strings.Contains(h, "7d：60%") {
		t.Fatalf("header %q", h)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, got.body)
	}
	if body["title"] != testMessage.Title || body["body"] != testMessage.Body || body["priority"] != "high" || body["url"] != testMessage.JumpURL {
		t.Fatalf("body %v", body)
	}
}

func TestWebhookDefaultBodyAndPlainText(t *testing.T) {
	server, got := captureServer(t, 204, "")
	w, err := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	w.Now = func() time.Time { return now }
	w.Loc = time.FixedZone("UTC+8", 8*3600)
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "lamplighter" || body["kind"] != KindQuota || body["text"] != testMessage.Title+"\n"+testMessage.Body ||
		body["time"] != "2026-10-10T16:00:00+08:00" || body["label"] != testMessage.Label {
		t.Fatalf("default body %v", body)
	}

	w, err = webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  method: put\n  body: '{{text}}'\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if got.method != "PUT" || got.body != testMessage.Title+"\n"+testMessage.Body || !strings.HasPrefix(got.header.Get("Content-Type"), "text/plain") {
		t.Fatalf("plain text request %s %q %q", got.method, got.body, got.header.Get("Content-Type"))
	}
}

func TestWebhookSuccessJSON(t *testing.T) {
	cases := []struct {
		response string
		ok       bool
	}{
		{`{"StatusCode":0,"code":0,"msg":"success"}`, true},
		{`{"code": 0.0}`, true},
		{`{"code":19024,"msg":"Key Words Not Found"}`, false},
		{`{"msg":"no code"}`, false},
		{`ok`, false},
	}
	for _, c := range cases {
		server, _ := captureServer(t, 200, c.response)
		w, err := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  success_json:\n    code: 0\n")
		if err != nil {
			t.Fatal(err)
		}
		response, err := w.Deliver(context.Background(), testMessage)
		if (err == nil) != c.ok {
			t.Fatalf("%s: err %v", c.response, err)
		}
		if response != c.response {
			t.Fatalf("response %q", response)
		}
		if !w.Checked() {
			t.Fatal("success_json must count as checked")
		}
	}
}

func TestWebhookRejectsNon2xx(t *testing.T) {
	server, _ := captureServer(t, 400, `{"ok":false,"description":"chat not found"}`)
	w, _ := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n")
	err := w.Send(context.Background(), testMessage)
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err %v", err)
	}
	if w.Checked() {
		t.Fatal("a webhook without success_json only checks the status")
	}
}

func TestWebhookErrorsHideTokens(t *testing.T) {
	const token = "tok3n-abcdef123456"
	server, _ := captureServer(t, 500, "invalid token "+token+" for Bearer hdr-secret-999")
	w, _ := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"/hook/"+token+"?key=query-secret-1\n  headers:\n    Authorization: Bearer hdr-secret-999\n")
	err := w.Send(context.Background(), testMessage)
	if err == nil {
		t.Fatal("HTTP 500 must fail")
	}
	server.Close()
	_, connErr := w.Deliver(context.Background(), testMessage)
	for _, err := range []error{err, connErr} {
		if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "query-secret-1") || strings.Contains(err.Error(), "hdr-secret-999") {
			t.Fatalf("error leaks a secret: %v", err)
		}
	}
}

func TestNewWebhookChecksSettings(t *testing.T) {
	if w, err := webhookFromYAML(t, "bark_url: https://api.day.app/k\n"); w != nil || err != nil {
		t.Fatalf("empty url must disable the webhook: %v %v", w, err)
	}
	for _, yaml := range []string{
		"webhook:\n  url: ftp://example.com/\n",
		"webhook:\n  url: example.com/hook\n",
		"webhook:\n  url: https://example.com/\n  method: DELETE\n",
		"webhook:\n  url: https://example.com/\n  method: GET\n  body: x\n",
		"webhook:\n  url: https://example.com/\n  body: '{{message}}'\n",
		"webhook:\n  url: https://example.com/{{nope}}\n",
		"webhook:\n  url: https://example.com/\n  headers:\n    'Bad Name': x\n",
	} {
		if _, err := webhookFromYAML(t, yaml); err == nil {
			t.Fatalf("settings must be rejected:\n%s", yaml)
		}
	}
}

func TestBarkErrorsHideDeviceKey(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	b := &Bark{URL: server.URL + "/device-key-123"}
	err := b.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || strings.Contains(err.Error(), "device-key-123") {
		t.Fatalf("err %v", err)
	}
}

type channelSender struct {
	err  error
	sent int
}

func (c *channelSender) Send(context.Context, Message) error {
	c.sent++
	return c.err
}

func TestFanoutDeliversWhenOneChannelWorks(t *testing.T) {
	good, bad := &channelSender{}, &channelSender{err: errors.New("offline")}
	var failed []string
	f := &Fanout{
		Channels:       []Channel{{Name: "Bark", Sender: good}, {Name: "Webhook", Sender: bad}},
		OnChannelError: func(_ Message, err error) { failed = append(failed, err.Error()) },
	}
	if err := f.Send(context.Background(), testMessage); err != nil {
		t.Fatalf("one working channel delivers the message: %v", err)
	}
	if good.sent != 1 || bad.sent != 1 || len(failed) != 1 || !strings.HasPrefix(failed[0], "Webhook") {
		t.Fatalf("sent %d/%d, failed %v", good.sent, bad.sent, failed)
	}

	good.err = errors.New("down")
	failed = nil
	err := f.Send(context.Background(), testMessage)
	if err == nil || !strings.Contains(err.Error(), "Bark") || !strings.Contains(err.Error(), "Webhook") || len(failed) != 0 {
		t.Fatalf("all channels failed: %v, %v", err, failed)
	}

	if err := (&Fanout{}).Send(context.Background(), testMessage); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no channel: %v", err)
	}
}

func TestWebhookContentTypeDetection(t *testing.T) {
	server, got := captureServer(t, 200, "")
	w, err := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  body: '[Lamplighter] {{text}}'\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.header.Get("Content-Type"), "text/plain") || got.body != "[Lamplighter] "+testMessage.Title+"\n"+testMessage.Body {
		t.Fatalf("plain text with a bracket: %q %q", got.header.Get("Content-Type"), got.body)
	}

	w, err = webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  headers:\n    content-type: application/x-www-form-urlencoded\n  body: 'title={{title}}&label={{label}}'\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	form, err := url.ParseQuery(got.body)
	if err != nil || form.Get("title") != testMessage.Title || form.Get("label") != testMessage.Label {
		t.Fatalf("form body %q", got.body)
	}
}

func TestWebhookDefaultBodyOrder(t *testing.T) {
	server, got := captureServer(t, 200, "")
	w, _ := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n")
	if err := w.Send(context.Background(), testMessage); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.body, `{"source":"lamplighter","kind":"quota","title":`) {
		t.Fatalf("default body order %s", got.body)
	}
}

func TestWebhookHeaderValues(t *testing.T) {
	server, got := captureServer(t, 200, "")
	// A block scalar ends with a line break, which is trimmed.
	w, err := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  headers:\n    Authorization: |\n      Bearer x\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Send(context.Background(), testMessage); err != nil || got.header.Get("Authorization") != "Bearer x" {
		t.Fatalf("header %q, err %v", got.header.Get("Authorization"), err)
	}
	for _, yaml := range []string{
		"webhook:\n  url: https://example.com/\n  headers:\n    X-A: " + `"a\nb"` + "\n",
		"webhook:\n  url: https://example.com/\n  headers:\n    X-A: a\n    x-a: b\n",
	} {
		if _, err := webhookFromYAML(t, yaml); err == nil {
			t.Fatalf("header must be rejected:\n%s", yaml)
		}
	}
}

func TestWebhookHidesBodyStrings(t *testing.T) {
	server, _ := captureServer(t, 200, `{"id":"x","topic":"my-private-topic","message":"t"}`)
	w, _ := webhookFromYAML(t, "webhook:\n  url: "+server.URL+"\n  body: '{\"topic\":\"my-private-topic\",\"message\":\"{{body}}\"}'\n")
	response, err := w.Deliver(context.Background(), testMessage)
	if err != nil || strings.Contains(response, "my-private-topic") {
		t.Fatalf("response %q, err %v", response, err)
	}
}

func TestBarkRequestErrorHidesDeviceKey(t *testing.T) {
	b := &Bark{URL: "https://api .day.app/device-key-123"}
	err := b.Send(context.Background(), Message{Title: "t", Body: "b"})
	if err == nil || strings.Contains(err.Error(), "device-key-123") {
		t.Fatalf("err %v", err)
	}
}

type panicSender struct{}

func (panicSender) Send(context.Context, Message) error { panic("boom") }

func TestFanoutRecoversFromPanics(t *testing.T) {
	good := &channelSender{}
	var failed []string
	f := &Fanout{
		Channels:       []Channel{{Name: "Bark", Sender: good}, {Name: "Webhook", Sender: panicSender{}}},
		OnChannelError: func(_ Message, err error) { failed = append(failed, err.Error()) },
	}
	if err := f.Send(context.Background(), testMessage); err != nil || len(failed) != 1 || !strings.Contains(failed[0], "boom") {
		t.Fatalf("err %v, failed %v", err, failed)
	}
}

func TestAlertsWithOneWorkingChannelDoNotRepeat(t *testing.T) {
	good, bad := &channelSender{}, &channelSender{err: errors.New("offline")}
	failures := 0
	f := &Fanout{
		Channels:       []Channel{{Name: "Bark", Sender: good}, {Name: "Webhook", Sender: bad}},
		OnChannelError: func(Message, error) { failures++ },
	}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	a := alerts(config.Default(), f)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	if good.sent != 1 || failures != 1 {
		t.Fatalf("working channel got %d messages, %d failures logged", good.sent, failures)
	}
}

func TestNoChannelTurnsNotificationsOff(t *testing.T) {
	failures := 0
	f := &Fanout{}
	a := &Alerts{Cfg: config.Default(), Sender: f, OnError: func(Message, error) { failures++ }}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now.Add(5*time.Minute))
	if failures != 0 {
		t.Fatalf("notifications are off, but %d failures were logged", failures)
	}
	// A channel set up later does not receive the alert dropped earlier.
	good := &channelSender{}
	f.Channels = []Channel{{Name: "Bark", Sender: good}}
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now.Add(10*time.Minute))
	if good.sent != 0 {
		t.Fatalf("old alert sent after a channel was set up")
	}
}

func TestInvalidWebhookAloneFailsAndRetries(t *testing.T) {
	failures := 0
	f := &Fanout{Invalid: errors.New("Webhook 设置无效")}
	a := &Alerts{Cfg: config.Default(), Sender: f, OnError: func(Message, error) { failures++ }}
	st := store.NewState()
	now := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	a.ProcessGroup(context.Background(), st, group(80, 80, now), now)
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now)
	good := &channelSender{}
	f.Invalid, f.Channels = nil, []Channel{{Name: "Webhook", Sender: good}}
	a.ProcessGroup(context.Background(), st, group(30, 80, now), now.Add(5*time.Minute))
	if failures != 1 || good.sent != 1 {
		t.Fatalf("failures %d, sent after the fix %d", failures, good.sent)
	}
}

func TestNotificationsOffClearRecoveriesAndReminders(t *testing.T) {
	cfg := config.Default()
	cfg.RecoveryNotify.FiveHour = config.RecoveryAll
	cfg.ResetReminder.FiveHour = config.ReminderAll
	failures := 0
	f := &Fanout{}
	start := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	reset := start.Add(time.Hour)
	after := reset.Add(time.Minute)
	// The same readings with a working channel send a reminder and a recovery.
	run := func(a *Alerts, st *store.State) {
		a.ProcessGroup(context.Background(), st, fiveHour(80, reset), start)
		a.ProcessGroup(context.Background(), st, fiveHour(63, reset), reset.Add(-30*time.Minute))
		a.ProcessGroup(context.Background(), st, fiveHour(100, after.Add(5*time.Hour)), after)
	}
	control := &fakeSender{}
	run(alerts(cfg, control), store.NewState())
	if len(control.sent) != 2 {
		t.Fatalf("control run sent %+v", control.sent)
	}

	a := &Alerts{Cfg: cfg, Sender: f, OnError: func(Message, error) { failures++ }}
	st := store.NewState()
	run(a, st)
	ws := st.Group("g").Windows[quota.WindowFiveHour]
	if failures != 0 || ws.PendingRecovery != nil || ws.ResetNotice1hFor == "" {
		t.Fatalf("failures %d, pending %+v, reminder for %q", failures, ws.PendingRecovery, ws.ResetNotice1hFor)
	}
	good := &channelSender{}
	f.Channels = []Channel{{Name: "Bark", Sender: good}}
	a.ProcessGroup(context.Background(), st, fiveHour(100, after.Add(5*time.Hour)), after.Add(5*time.Minute))
	if good.sent != 0 {
		t.Fatal("a recovery from while notifications were off was sent later")
	}
}

func TestResetFeedKeepsRecordsWhileNotificationsAreOff(t *testing.T) {
	st := store.NewState()
	if sent := ProcessResetRecords(context.Background(), st, records(), true, &Fanout{}, time.UTC, LangZH, resetNow); sent != 0 {
		t.Fatalf("sent %d with notifications off", sent)
	}
	sender := &fakeSender{}
	if sent := ProcessResetRecords(context.Background(), st, records(), true, sender, time.UTC, LangZH, resetNow); sent != 1 {
		t.Fatalf("the pending schedule must go out once a channel is set up, sent %d", sent)
	}
}
