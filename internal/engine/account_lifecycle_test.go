package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

func TestStatusAccountOrderUsesCredentialFileNames(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	// The credential ID and auth index must not act as account_order keys.
	e.creds["1"].ID = "different-id"
	for _, tc := range []struct {
		name  string
		order []string
		want  []string
	}{
		{"default", nil, []string{"codex-b.json", "codex-a.json", "claude.json", "ag.json"}},
		{"cross provider", []string{"ag.json", "codex-a.json"}, []string{"ag.json", "codex-a.json", "codex-b.json", "claude.json"}},
		{"unknown and duplicate", []string{"missing.json", "claude.json", "codex-a.json", "claude.json", "codex-b.json"}, []string{"claude.json", "codex-a.json", "codex-b.json", "ag.json"}},
		{"not ID or index", []string{"different-id", "1"}, []string{"codex-b.json", "codex-a.json", "claude.json", "ag.json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := e.config()
			cfg.AccountOrder = tc.order
			e.Configure(cfg, nil)
			var got []string
			for _, account := range e.Status().Accounts {
				got = append(got, account.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("accounts %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStatusHidesMCPKeyWithoutChangingRuntimeConfig(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	e, _ := newTestEngine(t, standardHost(c), c)
	for _, key := range []string{"", "private-mcp-token"} {
		cfg := e.config()
		cfg.MCPKey = key
		cfg.AccountOrder = []string{"a.json"}
		e.Configure(cfg, nil)
		status := e.Status()
		if status.Config.MCPKey != "" || status.Config.MCPKeySet != (key != "") {
			t.Fatalf("MCP key snapshot: %+v", status.Config)
		}
		raw, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"mcp_key":`) || key != "" && strings.Contains(string(raw), key) {
			t.Fatalf("MCP key exposed in status: %s", raw)
		}
		status.Config.AccountOrder[0] = "changed.json"
		if runtime := e.config(); runtime.MCPKey != key || runtime.MCPKeySet || runtime.AccountOrder[0] != "a.json" {
			t.Fatalf("status changed runtime configuration: %+v", runtime)
		}
	}
}

func TestActiveWindowDisappearanceAndReturn(t *testing.T) {
	for _, missingID := range []string{quota.WindowFiveHour, quota.WindowSevenDay} {
		for _, resetView := range []bool{false, true} {
			name := missingID
			if resetView {
				name += "/without_live_view"
			}
			t.Run(name, func(t *testing.T) {
				c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
				e, sender := newTestEngine(t, standardHost(c), c)
				cfg := e.config()
				cfg.RecoveryNotify = config.WindowModes{FiveHour: config.RecoveryAll, SevenDay: config.RecoveryAll}
				cfg.ResetReminder = config.WindowModes{FiveHour: config.ReminderAll, SevenDay: config.ReminderAll}
				e.alerts.Cfg = cfg
				if _, err := e.loadCreds(cfg); err != nil {
					t.Fatal(err)
				}
				cred := e.creds["1"]
				key := "codex:1:gpt"
				windows := []quota.Window{
					{ID: quota.WindowFiveHour, Label: "5h", Remaining: 80, Reset: c.t.Add(3 * time.Hour)},
					{ID: quota.WindowSevenDay, Label: "7d", Remaining: 80, Reset: c.t.Add(6 * 24 * time.Hour)},
				}
				for i := range windows {
					if windows[i].ID == missingID {
						windows[i].Remaining = 0
					}
				}
				apply := func(source quota.Source, windows []quota.Window, alerts bool) {
					e.applyGroups(context.Background(), cfg, cred, []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: windows}}, source, c.t, c.t, alerts)
				}
				apply(quota.SourceActive, windows, true)
				if len(sender.sent) != 0 || len(e.collectTargets(cfg)) != 1 {
					t.Fatal("initial sample must establish a baseline and a 5h target")
				}
				// A partial passive update must not mark an omitted window absent.
				c.add(time.Minute)
				var kept []quota.Window
				for _, w := range windows {
					if w.ID != missingID {
						kept = append(kept, w)
					}
				}
				apply(quota.SourcePassive, kept, true)
				if e.state.Group(key).Windows[missingID].Rebase {
					t.Fatal("partial passive updates do not remove windows")
				}
				if resetView {
					e.groups = map[string]*GroupView{}
				}
				c.add(time.Minute)
				e.dirty = false
				// A failed plan confirmation may suppress alerts, but must not
				// prevent recording the missing window for the next query.
				apply(quota.SourceActive, kept, false)
				if !e.state.Group(key).Windows[missingID].Rebase || !e.dirty {
					t.Fatal("missing window must be marked for a persisted baseline")
				}
				wantTargets := 1
				if missingID == quota.WindowFiveHour {
					wantTargets = 0
				}
				if got := len(e.collectTargets(cfg)); got != wantTargets {
					t.Fatalf("targets after disappearance=%d, want %d", got, wantTargets)
				}
				e.saveState()
				file := &store.StateFile{Path: filepath.Join(e.dataDir, "state.json")}
				restored, fresh, err := file.Load()
				if err != nil || fresh || !restored.Group(key).Windows[missingID].Rebase {
					t.Fatalf("missing marker must survive restart: fresh=%v, err=%v", fresh, err)
				}
				e.state = restored
				// Ignore passive headers for a window removed by the active API.
				var returning quota.Window
				for _, w := range windows {
					if w.ID == missingID {
						returning = w
					}
				}
				returning.Remaining, returning.Reset = 80, c.t.Add(30*time.Minute)
				apply(quota.SourcePassive, []quota.Window{returning}, true)
				if !e.state.Group(key).Windows[missingID].Rebase {
					t.Fatal("passive headers must not restore a removed window")
				}
				c.add(time.Minute)
				apply(quota.SourceActive, append(kept, returning), true)
				ws := e.state.Group(key).Windows[missingID]
				if ws.Rebase || ws.Remaining != 80 || ws.PendingRecovery != nil {
					t.Fatalf("returned window must establish a fresh baseline: %+v", ws)
				}
				if len(sender.sent) != 0 {
					t.Fatalf("disappearance/return must not send recovery or reset reminders: %+v", sender.sent)
				}
				if got := len(e.collectTargets(cfg)); got != 1 {
					t.Fatalf("5h target must be collected automatically after return: %d", got)
				}
			})
		}
	}
}
