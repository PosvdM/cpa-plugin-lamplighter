package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

type senderFunc func(context.Context, Message) error

func (f senderFunc) Send(ctx context.Context, msg Message) error { return f(ctx, msg) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFeishuDoesNotFallbackOnDeliveryErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"signature", `{"code":19021,"msg":"sign match fail"}`, 200},
		{"rate limit", `{"code":11232,"msg":"rate limit"}`, 200},
		{"keyword", `{"code":19024,"msg":"Key Words Not Found"}`, 200},
		{"http 429", `{"code":9499}`, 429},
		{"http 502", `{"code":9499}`, 502},
		{"invalid json", `<html>gateway</html>`, 200},
		{"missing code", `{}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			f := &Feishu{Webhook: srv.URL + "/hook/token", Client: srv.Client()}
			if err := f.Send(context.Background(), Message{Title: "test"}); err == nil {
				t.Fatal("expected delivery error")
			}
			if calls.Load() != 1 {
				t.Fatalf("ambiguous/non-format failure retried %d times", calls.Load())
			}
		})
	}
}

func TestFeishuNetworkErrorRedactsSecrets(t *testing.T) {
	webhook := "https://open.feishu.cn/open-apis/bot/v2/hook/private-token"
	for _, cause := range []error{context.DeadlineExceeded, errors.New("proxy failed for " + webhook + " private-token signing-secret")} {
		var calls atomic.Int32
		f := &Feishu{Webhook: webhook, Secret: "signing-secret", Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, &url.Error{Op: "Post", URL: webhook, Err: cause}
		})}}
		err := f.Send(context.Background(), Message{})
		if err == nil {
			t.Fatal("expected error")
		}
		for _, secret := range []string{webhook, "private-token", "signing-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("secret leaked: %v", err)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("network failure retried %d times", calls.Load())
		}
	}
}

func TestFeishuPlainTextAndJumpURL(t *testing.T) {
	attack := "*bold* _italic_ ~~strike~~ [x](https://example.com) <at id=all></at>"
	f := &Feishu{Secret: "secret", Now: func() time.Time { return time.Unix(1700000000, 0) }}
	msg := Message{Title: "test", Body: "error: " + attack + "\n5h：" + attack, JumpURL: "https://didcodexreset.com/history/1.html"}
	card := f.cardPayload(msg, f.now())["card"].(map[string]any)
	elements := card["elements"].([]any)
	prose := elements[0].(map[string]any)["text"].(map[string]any)
	if prose["tag"] != "plain_text" || prose["content"] != "error: "+attack {
		t.Fatalf("prose interpreted: %#v", prose)
	}
	fields := elements[1].(map[string]any)["fields"].([]any)
	field := fields[0].(map[string]any)["text"].(map[string]any)
	if field["tag"] != "plain_text" || field["content"] != "5h\n"+attack {
		t.Fatalf("field interpreted: %#v", field)
	}
	button := elements[2].(map[string]any)["actions"].([]any)[0].(map[string]any)
	if button["url"] != msg.JumpURL {
		t.Fatalf("missing jump button: %#v", button)
	}
	text := f.textPayload(msg, f.now())["content"].(map[string]any)["text"].(string)
	if strings.Contains(text, "<at") {
		t.Fatal("fallback contains an active mention tag")
	}
	if !strings.HasSuffix(text, msg.JumpURL) {
		t.Fatal("fallback lost jump URL")
	}
	if feishuJumpURL("javascript:alert(1)") != "" {
		t.Fatal("unsafe jump URL accepted")
	}
}

func TestFeishuUsesOneTimestampPerRequest(t *testing.T) {
	var calls int
	f := &Feishu{Secret: "secret", Now: func() time.Time { calls++; return time.Unix(int64(calls*60), 0) }}
	payload := f.cardPayload(Message{}, f.now())
	if calls != 1 || payload["timestamp"] != "60" {
		t.Fatalf("clock sampled more than once: %d %#v", calls, payload)
	}
}

func TestMultiConcurrentAndPartialSuccess(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	makeSender := func(err error) Sender {
		return senderFunc(func(ctx context.Context, _ Message) error {
			started <- struct{}{}
			select {
			case <-release:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	var partial error
	m := Multi{Senders: []Sender{makeSender(nil), makeSender(errors.New("feishu failed"))}, OnPartial: func(_ Message, err error) { partial = err }}
	done := make(chan error, 1)
	go func() { done <- m.Send(ctx, Message{}) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("channels were not started concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("partial success failed: %v", err)
	}
	if partial == nil || !strings.Contains(partial.Error(), "feishu failed") {
		t.Fatalf("missing partial report: %v", partial)
	}
	if err := m.SendAll(ctx, Message{}); err == nil {
		t.Fatal("test endpoint must report failing channel")
	}
}

func TestMultiNilAndAllFailed(t *testing.T) {
	for _, m := range []Multi{{}, {Senders: []Sender{nil}}, {Senders: []Sender{senderFunc(func(context.Context, Message) error { return errors.New("failed") })}}, {ConfigError: errors.New("invalid webhook")}, {Senders: []Sender{senderFunc(func(context.Context, Message) error { panic("private-token") })}}} {
		if err := m.Send(context.Background(), Message{}); err == nil {
			t.Fatal("undelivered alert was accepted")
		}
		if err := m.SendAll(context.Background(), Message{}); err == nil {
			t.Fatal("undelivered test was accepted")
		}
	}
}

func TestPartialDeliveryDoesNotRepeatQuotaAlert(t *testing.T) {
	healthy := &fakeSender{}
	broken := &fakeSender{fail: true}
	cfg := config.Default()
	st := store.NewState()
	a := &Alerts{Cfg: cfg, Sender: Multi{Senders: []Sender{healthy, broken}}}
	// Start above the threshold so a real pending alert is generated.
	now := time.Now()
	g := Group{Key: "g", Label: "test", Windows: []quota.Window{{ID: "five-hour", Label: "5 小时", Remaining: 80, Reset: now.Add(time.Hour)}}}
	a.ProcessGroup(context.Background(), st, g, now)
	g.Windows[0].Remaining = 10
	a.ProcessGroup(context.Background(), st, g, now.Add(time.Minute))
	a.ProcessGroup(context.Background(), st, g, now.Add(2*time.Minute))
	if len(healthy.sent) != 1 {
		t.Fatalf("healthy channel received %d alerts", len(healthy.sent))
	}
}
