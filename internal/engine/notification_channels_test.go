package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
)

func channelEngine(t *testing.T, yaml string) *Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e := New(Options{Host: &fakeHost{}, Version: "test", Now: c.now})
	e.Configure(cfg, nil)
	e.applyRuntimeConfig(cfg)
	return e
}

func TestTestNotifyReportsEachChannel(t *testing.T) {
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":200}`))
	}))
	defer bark.Close()
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":19024,"msg":"Key Words Not Found"}`))
	}))
	defer hook.Close()

	e := channelEngine(t, "bark_url: "+bark.URL+"/key\nwebhook:\n  url: "+hook.URL+"\n")
	results, err := e.testNotify(context.Background())
	if err != nil || len(results) != 2 {
		t.Fatalf("results %+v, err %v", results, err)
	}
	if !results[0].OK || results[0].Channel != "Bark" {
		t.Fatalf("bark %+v", results[0])
	}
	// Without success_json a 200 counts, and the page is told so.
	if w := results[1]; !w.OK || !w.Unchecked || !strings.Contains(w.Response, "19024") {
		t.Fatalf("webhook %+v", w)
	}

	e = channelEngine(t, "bark_url: "+bark.URL+"/key\nwebhook:\n  url: "+hook.URL+"\n  success_json:\n    code: 0\n")
	results, _ = e.testNotify(context.Background())
	if w := results[1]; w.OK || w.Unchecked || !strings.Contains(w.Error, "Key Words Not Found") {
		t.Fatalf("webhook with success_json %+v", w)
	}
}

func TestInvalidWebhookIsReported(t *testing.T) {
	e := channelEngine(t, "webhook:\n  url: https://example.com/\n  body: '{{nope}}'\n")
	if status := e.Status(); !strings.Contains(status.NotifyError, "{{nope}}") {
		t.Fatalf("notify_error %q", status.NotifyError)
	}
	results, err := e.testNotify(context.Background())
	if err != nil || len(results) != 1 || results[0].OK || results[0].Channel != "Webhook" {
		t.Fatalf("results %+v, err %v", results, err)
	}

	fixed, _ := config.Parse([]byte("webhook:\n  url: https://example.com/\n  body: '{{text}}'\n"))
	if sameRuntimeConfig(fixed, e.config()) {
		t.Fatal("a webhook change must rebuild the channels")
	}
	e.Configure(fixed, nil)
	e.applyRuntimeConfig(fixed)
	if status := e.Status(); status.NotifyError != "" {
		t.Fatalf("notify_error stays after the fix: %q", status.NotifyError)
	}
}

func TestTestNotifyWithoutChannels(t *testing.T) {
	e := channelEngine(t, "")
	if _, err := e.testNotify(context.Background()); !errors.Is(err, notify.ErrNotConfigured) {
		t.Fatalf("err %v", err)
	}
}
