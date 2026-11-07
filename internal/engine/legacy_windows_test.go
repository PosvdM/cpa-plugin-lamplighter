package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestLegacyAbsentWindowMigrationSurvivesFailedActiveQuery(t *testing.T) {
	for _, tc := range []struct {
		name, windows    string
		migrated, rebase bool
	}{
		{"active-weekly-proof", "codex:main|seven-day", true, true},
		{"active-group-absence-proof", "other|seven-day", true, true},
		{"no-active-proof", "", false, true},
		{"invalid-active-proof", "codex:main|seven-day\nbroken", false, true},
		{"active-proof-without-old-rebase", "codex:main|seven-day", true, false},
		{"unknown-without-old-rebase", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, shanghai)}
			dir := t.TempDir()
			// 使用实际旧格式：只有通知基线和可选的主动窗口签名，没有 active_windows。
			raw, err := json.Marshal(map[string]any{
				"version": 1, "groups": map[string]any{"codex:1:codex:main": map[string]any{"windows": map[string]any{
					"five-hour": map[string]any{"remaining": 0, "rebase": tc.rebase, "reset": c.t.Add(-time.Hour).Format(time.RFC3339)},
					"seven-day": map[string]any{"remaining": 10, "rebase": true},
				}}}, "plans": map[string]any{"1": map[string]any{"windows": tc.windows}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			h := standardHost(c)
			e, sent := newTestEngineIn(t, h, c, dir)
			cfg := e.config()
			cfg.RecoveryNotify = config.WindowModes{FiveHour: config.RecoveryAll, SevenDay: config.RecoveryAll}
			cfg.ResetReminder = config.WindowModes{FiveHour: config.ReminderAll, SevenDay: config.ReminderAll}
			e.Configure(cfg, nil)
			e.alerts.Cfg = cfg
			stored := e.state.Group("codex:1:codex:main")
			if tc.migrated != (stored.ActiveWindows != nil) || stored.LegacyUnconfirmed == tc.migrated {
				t.Fatalf("migration: %+v", stored)
			}
			beforeRebase := stored.Windows[quota.WindowFiveHour].Rebase
			h.http = func(pluginapi.HTTPRequest) pluginapi.HTTPResponse { return pluginapi.HTTPResponse{StatusCode: 503} }
			e.poll(context.Background(), cfg, c.t, "1", true)
			if e.pollErrs["1"] == "" {
				t.Fatal("active query must fail")
			}
			apply := func(key string, windows []quota.Window) {
				e.applyGroups(context.Background(), cfg, e.creds["1"], []quota.RawGroup{{Key: key, SourceLabel: "GPT", Windows: windows}}, quota.SourcePassive, c.t, c.t, true)
			}
			five := quota.Window{ID: quota.WindowFiveHour, Label: "5h", Remaining: 80, Reset: c.t.Add(30 * time.Minute)}
			apply("codex:main", []quota.Window{five})
			c.add(time.Minute)
			five.Remaining = 0
			apply("codex:main", []quota.Window{five})
			ws := stored.Windows[quota.WindowFiveHour]
			if ws.Rebase != beforeRebase || ws.Remaining != 0 || len(e.groups["codex:1:codex:main"].Windows) != 0 || len(sent.sent) != 0 {
				t.Fatalf("legacy window resurrected: baseline=%+v view=%+v sent=%+v", ws, e.groups["codex:1:codex:main"], sent.sent)
			}
			for _, target := range e.collectTargets(cfg) {
				if target.group.Key == "codex:1:codex:main" {
					t.Fatal("legacy 5h target resurrected")
				}
			}
			if tc.windows == "codex:main|seven-day" {
				apply("codex:main", []quota.Window{{ID: quota.WindowSevenDay, Remaining: 80, Reset: c.t.Add(6 * 24 * time.Hour)}})
				if len(e.groups["codex:1:codex:main"].Windows) != 1 || stored.Windows[quota.WindowSevenDay].Rebase {
					t.Fatal("a migrated present window must accept passive updates even with Rebase")
				}
			}
			// 状态文件中没有的新组仍可以先接收被动读数。
			five.Remaining = 80
			apply("new-pool", []quota.Window{five})
			if len(e.groups["codex:1:new-pool"].Windows) != 1 || len(sent.sent) != 0 {
				t.Fatal("new group must allow a passive first baseline")
			}
			c.add(time.Minute)
			h.http = func(pluginapi.HTTPRequest) pluginapi.HTTPResponse {
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":%d}}}`, c.t.Add(30*time.Minute).Unix(), c.t.Add(6*24*time.Hour).Unix()))}
			}
			e.poll(context.Background(), cfg, c.t, "1", true)
			if err := e.pollErrs["1"]; err != "" {
				t.Fatal(err)
			}
			ws = stored.Windows[quota.WindowFiveHour]
			if ws.Rebase || ws.Remaining != 80 || ws.PendingRecovery != nil || stored.LegacyUnconfirmed || !slices.Contains(stored.ActiveWindows, quota.WindowFiveHour) || len(sent.sent) != 0 {
				t.Fatalf("active return must establish a fresh baseline: %+v group=%+v sent=%+v", ws, stored, sent.sent)
			}
		})
	}
}
