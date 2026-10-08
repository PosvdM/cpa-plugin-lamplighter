package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFeishuSign pins the signature against vectors produced by an
// independent implementation (Python hmac/hashlib) so a refactor cannot
// silently change the algorithm. Feishu uses timestamp+"\n"+secret as the
// HMAC key over an empty message.
func TestFeishuSign(t *testing.T) {
	cases := []struct {
		secret    string
		timestamp string
		want      string
	}{
		{"test-secret", "1599360473", "wSds2BzzFIIGf/WrhUO+NI1q/9j+FRJd3JNHKAq0NZY="},
		{"demo", "1700000000", "8oT2n3SMKFfEnDoiwer8BUM/SjKLwe9SqoEIHlhDTKo="},
	}
	for _, c := range cases {
		if got := feishuSign(c.secret, c.timestamp); got != c.want {
			t.Errorf("feishuSign(%q, %q) = %q, want %q", c.secret, c.timestamp, got, c.want)
		}
	}
}

func TestFeishuSignKeyIsNotTheSecret(t *testing.T) {
	// The common mistake is to use the secret as the HMAC key. Guard against
	// reintroducing it: that variant produces a different signature.
	// hmac.New(sha256.New, secret).Write(timestamp+"\n"+secret) would give
	// this value, which must not equal what we send.
	wrong := feishuSign("test-secret", "1599360473")
	// A signature is 44 base64 characters ending in '='.
	if len(wrong) != 44 || !strings.HasSuffix(wrong, "=") {
		t.Fatalf("unexpected signature shape: %q", wrong)
	}
}

func TestFeishuTemplate(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
		want string
	}{
		{"notice uses yellow", Message{Severity: SeverityNotice}, feishuYellow},
		{"low uses orange", Message{Severity: SeverityLow}, feishuOrange},
		{"critical uses red", Message{Severity: SeverityCritical}, feishuRed},
		{"exhausted uses red", Message{Severity: SeverityExhausted}, feishuRed},
		{"recovery uses green", Message{Title: "✅ Claude · 5h 已恢复"}, feishuGreen},
		{"quota notice emoji", Message{Title: "🟡 Claude · 7d 48%"}, feishuYellow},
		{"quota critical emoji", Message{Title: "🔴 Claude · 7d 8%"}, feishuRed},
		{"warning emoji", Message{Title: "⚠️ Claude 点火已暂停"}, feishuOrange},
		{"test message is neutral", Message{Title: "🕯️ Lamplighter 测试通知"}, feishuBlue},
	}
	for _, c := range cases {
		if got := feishuTemplate(c.msg); got != c.want {
			t.Errorf("%s: feishuTemplate() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSplitField(t *testing.T) {
	cases := []struct {
		line  string
		label string
		value string
		ok    bool
	}{
		{"5h：100% | 04h | 10/04 13:50", "5h", "100% | 04h | 10/04 13:50", true},
		{"7d：33% | 03d | 10/07 14:00", "7d", "33% | 03d | 10/07 14:00", true},
		{"上周期剩余：5h 3% / 7d 12%", "上周期剩余", "5h 3% / 7d 12%", true},
		{"Expected: 10/05 12:00", "Expected", "10/05 12:00", true},
		// A URL must survive intact rather than split at the scheme colon.
		{"https://didcodexreset.com/history/1.html", "", "", false},
		// Prose without a separator is not a field.
		{"CPA 把这个账号冷却到 10/05 12:00，期间的请求和点火都会被拒绝。", "", "", false},
		// A long label is prose, not a field.
		{"这是一个非常长的标签超过十六个字了：值", "", "", false},
		// Empty halves are rejected.
		{"：空标签", "", "", false},
		{"空值：", "", "", false},
	}
	for _, c := range cases {
		label, value, ok := splitField(c.line)
		if ok != c.ok || label != c.label || value != c.value {
			t.Errorf("splitField(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.line, label, value, ok, c.label, c.value, c.ok)
		}
	}
}

func TestFeishuElementsGroupFields(t *testing.T) {
	body := "5h：100% | 04h | 10/04 13:50\n7d：33% | 03d | 10/07 14:00"
	elements := feishuElements(body)
	if len(elements) != 1 {
		t.Fatalf("expected the two field lines to share one div, got %d elements", len(elements))
	}
	div, ok := elements[0].(map[string]any)
	if !ok || div["tag"] != "div" {
		t.Fatalf("expected a div element, got %#v", elements[0])
	}
	fields, ok := div["fields"].([]any)
	if !ok || len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %#v", div["fields"])
	}
	first, _ := fields[0].(map[string]any)
	if first["is_short"] != true {
		t.Errorf("expected is_short fields so they sit side by side")
	}
	text, _ := first["text"].(map[string]any)
	if text["tag"] != "lark_md" || text["content"] != "**5h**\n100% | 04h | 10/04 13:50" {
		t.Errorf("unexpected field content: %#v", text)
	}
}

func TestFeishuSendCard(t *testing.T) {
	var got map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("expected JSON content type, got %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("invalid JSON body: %v", err)
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	loc := time.FixedZone("CST", 8*3600)
	f := &Feishu{
		Webhook: srv.URL + "/open-apis/bot/v2/hook/token",
		Client:  srv.Client(),
		Loc:     loc,
		Now:     func() time.Time { return time.Date(2026, 10, 8, 20, 55, 0, 0, loc) },
	}
	msg := Message{
		Title:    "🟡 Claude · 7d 33%",
		Body:     "5h：100% | 04h | 10/04 13:50\n7d：33% | 03d | 10/07 14:00",
		Severity: SeverityNotice,
	}
	if err := f.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/open-apis/bot/v2/hook/token" {
		t.Errorf("unexpected path %q", gotPath)
	}
	if got["msg_type"] != "interactive" {
		t.Fatalf("expected an interactive card, got %#v", got["msg_type"])
	}
	// No secret configured means no signature fields.
	if _, present := got["sign"]; present {
		t.Errorf("sign must be omitted when no secret is configured")
	}
	card, _ := got["card"].(map[string]any)
	header, _ := card["header"].(map[string]any)
	if header["template"] != feishuYellow {
		t.Errorf("expected yellow header for a notice, got %#v", header["template"])
	}
	title, _ := header["title"].(map[string]any)
	if title["content"] != msg.Title {
		t.Errorf("unexpected title %#v", title["content"])
	}
	elements, _ := card["elements"].([]any)
	if len(elements) != 3 {
		t.Fatalf("expected fields div + hr + note, got %d elements", len(elements))
	}
	note, _ := elements[2].(map[string]any)
	if note["tag"] != "note" {
		t.Fatalf("expected a note footer, got %#v", note["tag"])
	}
	noteElements, _ := note["elements"].([]any)
	footer, _ := noteElements[0].(map[string]any)
	if footer["content"] != "Lamplighter · 10/08 20:55" {
		t.Errorf("unexpected footer %#v", footer["content"])
	}
}

func TestFeishuSendAddsSignatureWhenSecretSet(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	f := &Feishu{
		Webhook: srv.URL + "/hook/x",
		Secret:  "test-secret",
		Client:  srv.Client(),
		Now:     func() time.Time { return time.Unix(1599360473, 0) },
	}
	if err := f.Send(context.Background(), Message{Title: "test"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got["timestamp"] != "1599360473" {
		t.Errorf("expected the timestamp as a string, got %#v", got["timestamp"])
	}
	if got["sign"] != "wSds2BzzFIIGf/WrhUO+NI1q/9j+FRJd3JNHKAq0NZY=" {
		t.Errorf("unexpected sign %#v", got["sign"])
	}
}

// TestFeishuFallsBackToText covers the case where the bot refuses the card:
// the alert must still be delivered as plain text.
func TestFeishuFallsBackToText(t *testing.T) {
	var types []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		kind, _ := payload["msg_type"].(string)
		types = append(types, kind)
		if kind == "interactive" {
			_, _ = w.Write([]byte(`{"code":9499,"msg":"invalid card"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	f := &Feishu{Webhook: srv.URL + "/hook/x", Client: srv.Client()}
	if err := f.Send(context.Background(), Message{Title: "标题", Body: "正文"}); err != nil {
		t.Fatalf("Send should succeed via the text fallback: %v", err)
	}
	if len(types) != 2 || types[0] != "interactive" || types[1] != "text" {
		t.Fatalf("expected card then text, got %v", types)
	}
}

func TestFeishuSendReportsErrors(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		f := &Feishu{}
		if err := f.Send(context.Background(), Message{}); !errors.Is(err, ErrFeishuNotConfigured) {
			t.Fatalf("expected ErrFeishuNotConfigured, got %v", err)
		}
	})
	t.Run("api error survives the fallback", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
		}))
		defer srv.Close()
		f := &Feishu{Webhook: srv.URL + "/hook/x", Client: srv.Client()}
		err := f.Send(context.Background(), Message{Title: "t"})
		if err == nil || !strings.Contains(err.Error(), "19021") {
			t.Fatalf("expected the API error to surface, got %v", err)
		}
	})
	t.Run("http error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()
		f := &Feishu{Webhook: srv.URL + "/hook/x", Client: srv.Client()}
		if err := f.Send(context.Background(), Message{Title: "t"}); err == nil {
			t.Fatal("expected an error for HTTP 502")
		}
	})
}

func TestMultiSendFanout(t *testing.T) {
	a, b := &fakeSender{}, &fakeSender{}
	var senders Multi = Multi{a, b}
	if err := senders.Send(context.Background(), Message{Title: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(a.sent) != 1 || len(b.sent) != 1 {
		t.Fatalf("expected both senders to receive the message, got %d and %d", len(a.sent), len(b.sent))
	}
}

func TestMultiKeepsGoingAfterFailure(t *testing.T) {
	broken, healthy := &fakeSender{fail: true}, &fakeSender{}
	var senders Multi = Multi{broken, healthy}
	err := senders.Send(context.Background(), Message{Title: "x"})
	if err == nil {
		t.Fatal("expected the failing channel to be reported")
	}
	if len(healthy.sent) != 1 {
		t.Fatalf("a broken channel must not silence the healthy one, got %d", len(healthy.sent))
	}
}

// TestMultiEmptyReportsNoChannel is load-bearing: an undeliverable alert has
// to stay pending (error returned) instead of being marked as notified.
func TestMultiEmptyReportsNoChannel(t *testing.T) {
	var senders Multi = Multi{}
	if err := senders.Send(context.Background(), Message{}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("expected ErrNoChannel, got %v", err)
	}
	// A nil Multi (what Alerts holds when nothing was configured) behaves the
	// same way, so the caller sees an error rather than a silent success.
	var nilMulti Multi
	if err := nilMulti.Send(context.Background(), Message{}); !errors.Is(err, ErrNoChannel) {
		t.Fatalf("expected ErrNoChannel for a nil Multi, got %v", err)
	}
}
