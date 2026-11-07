package engine

import (
	"context"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAbsentWindowSurvivesRestartAndFailedActiveQuery(t *testing.T) {
	for _, emptyGroup := range []bool{false, true} {
		name := "weekly-remains"
		if emptyGroup {
			name = "empty-group"
		}
		t.Run(name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
			h := standardHost(c)
			e, _ := newTestEngine(t, h, c)
			cfg := e.config()
			cfg.RecoveryNotify = config.WindowModes{FiveHour: config.RecoveryAll, SevenDay: config.RecoveryAll}
			cfg.ResetReminder = config.WindowModes{FiveHour: config.ReminderAll, SevenDay: config.ReminderAll}
			e.Configure(cfg, nil)
			e.alerts.Cfg = cfg
			if _, err := e.loadCreds(cfg); err != nil {
				t.Fatal(err)
			}
			key := "codex:1:gpt"
			windows := []quota.Window{{ID: quota.WindowFiveHour, Label: "5h", Remaining: 0, Reset: c.t.Add(time.Hour)}, {ID: quota.WindowSevenDay, Label: "7d", Remaining: 90, Reset: c.t.Add(6 * 24 * time.Hour)}}
			apply := func(e *Engine, source quota.Source, windows []quota.Window) {
				e.applyGroups(context.Background(), cfg, e.creds["1"], []quota.RawGroup{{Key: "gpt", SourceLabel: "GPT", Windows: windows}}, source, c.t, c.t, true)
			}
			apply(e, quota.SourceActive, windows)
			c.add(time.Minute)
			kept := windows[1:]
			if emptyGroup {
				kept = nil
			}
			apply(e, quota.SourceActive, kept)
			e.saveState()
			restarted, sent := newTestEngineIn(t, h, c, e.dataDir)
			restarted.Configure(cfg, nil)
			restarted.alerts.Cfg = cfg
			h.http = func(pluginapi.HTTPRequest) pluginapi.HTTPResponse { return pluginapi.HTTPResponse{StatusCode: 503} }
			restarted.poll(context.Background(), cfg, c.t, "1", true)
			if restarted.pollErrs["1"] == "" {
				t.Fatal("active query must fail")
			}
			returning := windows[0]
			returning.Remaining, returning.Reset = 80, c.t.Add(30*time.Minute)
			c.add(time.Minute)
			apply(restarted, quota.SourcePassive, []quota.Window{returning})
			ws := restarted.state.Group(key).Windows[quota.WindowFiveHour]
			if !ws.Rebase || ws.Remaining != 0 || len(restarted.groups[key].Windows) != 0 {
				t.Fatalf("passive headers resurrected missing window: state=%+v view=%+v", ws, restarted.groups[key])
			}
			for _, target := range restarted.collectTargets(cfg) {
				if target.group.Key == key {
					t.Fatal("absent 5h window must not ignite")
				}
			}
			if len(sent.sent) != 0 {
				t.Fatalf("passive data sent notifications: %+v", sent.sent)
			}
			c.add(time.Minute)
			apply(restarted, quota.SourceActive, append(kept, returning))
			ws = restarted.state.Group(key).Windows[quota.WindowFiveHour]
			if ws.Rebase || ws.Remaining != 80 || ws.PendingRecovery != nil || len(sent.sent) != 0 {
				t.Fatalf("active return must establish baseline without reminders: %+v sent=%+v", ws, sent.sent)
			}
			found := false
			for _, target := range restarted.collectTargets(cfg) {
				if target.group.Key == key {
					found = true
				}
			}
			if !found {
				t.Fatal("active return did not restore ignition")
			}
		})
	}
}

func TestPlanRebaseDoesNotMeanWindowAbsent(t *testing.T) {
	c := &clock{t: time.Now()}
	e, _ := newTestEngine(t, standardHost(c), c)
	if _, err := e.loadCreds(e.config()); err != nil {
		t.Fatal(err)
	}
	e.state.Group("codex:1:gpt").Windows[quota.WindowFiveHour] = &store.WindowState{Rebase: true}
	e.applyGroups(context.Background(), e.config(), e.creds["1"], []quota.RawGroup{{Key: "gpt", Windows: []quota.Window{{ID: quota.WindowFiveHour, Remaining: 80}}}}, quota.SourcePassive, c.t, c.t, true)
	if len(e.groups["codex:1:gpt"].Windows) != 1 || e.state.Group("codex:1:gpt").Windows[quota.WindowFiveHour].Rebase {
		t.Fatal("plan rebase must allow a passive first reading when active presence is unknown")
	}
}
