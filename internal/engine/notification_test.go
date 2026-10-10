package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/notify"
)

func TestInvalidNotificationConfigVisibleAndTestFails(t *testing.T) {
	for _, webhook := range []string{"not-a-url", "https:///hook/private-token", "https://open.feishu.cn/hook/", "https://user:private-token@example.com/hook/token", "https://example.com/hook/private-token%xx"} {
		t.Run(webhook, func(t *testing.T) {
			e, _ := newTestEngine(t, &fakeHost{}, &clock{t: time.Now()})
			cfg := config.Default()
			cfg.FeishuWebhook = webhook
			e.Configure(cfg, nil)
			e.applyRuntimeConfig(cfg)
			status := e.Status()
			if status.NotificationError == "" {
				t.Fatal("invalid webhook hidden in status")
			}
			raw, _ := json.Marshal(status)
			if strings.Contains(string(raw), "private-token") {
				t.Fatalf("config error leaked token: %s", raw)
			}
			reply := make(chan error, 1)
			e.runCommand(context.Background(), command{kind: "test_bark", reply: reply})
			if err := <-reply; err == nil || strings.Contains(err.Error(), "未配置通知渠道") {
				t.Fatalf("test must report actual config error: %v", err)
			}
			cfg.FeishuWebhook = "https://example.com/hook/token"
			e.Configure(cfg, nil)
			e.applyRuntimeConfig(cfg)
			if e.Status().NotificationError != "" {
				t.Fatal("corrected config retains error")
			}
		})
	}
}

func TestPartialNotificationHasSeparateEvent(t *testing.T) {
	e, _ := newTestEngine(t, &fakeHost{}, &clock{t: time.Now()})
	e.notifyPartial(notify.Message{Title: "test"}, notify.ErrNoChannel)
	events := e.Status().Events
	if len(events) != 1 || events[0].Kind != "notify_partial" {
		t.Fatalf("wrong partial event: %+v", events)
	}
}
