package engine

import (
	"context"
	"testing"
	"time"
)

func assertMCPNotReady(t *testing.T, value any, err error, state string) {
	t.Helper()
	if err != nil {
		t.Fatalf("readiness is a successful tool result: %v", err)
	}
	result, ok := value.(map[string]any)
	if !ok || result["ready"] != false || result["certain"] != false || result["state"] != state || result["note"] == "" {
		t.Fatalf("invalid %s result: %#v", state, value)
	}
	wantFields := 4
	if state == "starting" {
		wantFields++
		if result["retry_after_seconds"] != 30 {
			t.Fatalf("startup retry: %#v", result)
		}
	}
	if len(result) != wantFields {
		t.Fatalf("not-ready result must not include data: %#v", result)
	}
}

func TestMCPReadiness(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name          string
		running       bool
		polled        bool
		listFailed    bool
		accountFailed bool
		disabled      bool
		stopped       bool
		state         string
	}{
		{name: "not running", state: "starting"},
		{name: "not running with old poll", polled: true, state: "starting"},
		{name: "first poll pending", running: true, state: "starting"},
		{name: "only account query failed", running: true, accountFailed: true, state: "starting"},
		{name: "credential listing failed", running: true, listFailed: true},
		{name: "first full poll complete", running: true, polled: true},
		{name: "disabled before running", disabled: true, state: "disabled"},
		{name: "disabled with old data", running: true, polled: true, disabled: true, state: "disabled"},
		{name: "shutdown in progress", running: true, polled: true, stopped: true, state: "starting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(Options{})
			e.dataDir = t.TempDir()
			e.enabled, e.running, e.stopped = !tc.disabled, tc.running, tc.stopped
			if tc.polled {
				e.lastPoll = now
			}
			if tc.listFailed {
				e.pollErrs[""] = "credential list failed: private@example.com"
			}
			if tc.accountFailed {
				e.pollErrs["1"] = "account query failed"
			}
			b := NewMCPBackend(e)
			for _, tool := range []string{"list_quotas", "get_quota_history", "get_quota_events"} {
				value, err := b.CallTool(context.Background(), tool, nil)
				if tc.state != "" {
					assertMCPNotReady(t, value, err, tc.state)
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", tool, err)
				}
				result := value.(map[string]any)
				if _, blocked := result["ready"]; blocked {
					t.Fatalf("%s should be ready: %#v", tool, value)
				}
			}
			if _, err := b.CallTool(context.Background(), "unknown", nil); err == nil {
				t.Fatal("unknown tools must fail in every readiness state")
			}
		})
	}
}

func TestMCPRefreshStartupDoesNotConsumeLimit(t *testing.T) {
	now := time.Now().UTC()
	e := New(Options{Now: func() time.Time { return now }})
	b := NewMCPBackend(e)
	for _, state := range []string{"starting", "disabled"} {
		e.enabled = state != "disabled"
		for range 2 {
			value, err := b.CallTool(context.Background(), "refresh_quotas", map[string]string{"account": "not loaded yet"})
			assertMCPNotReady(t, value, err, state)
			if !b.nextRefresh.IsZero() {
				t.Fatalf("%s consumed refresh limit: %v", state, b.nextRefresh)
			}
		}
	}
	e.enabled, e.running = true, true
	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd := <-e.cmdCh
		cmd.reply <- commandReply{}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, err := b.CallTool(ctx, "refresh_quotas", nil)
	if err != nil || value.(map[string]any)["ok"] != true {
		t.Fatalf("running engine must accept refresh before first poll: %#v, %v", value, err)
	}
	<-done
	if !b.nextRefresh.Equal(now.Add(time.Minute)) {
		t.Fatalf("refresh reservation: %v", b.nextRefresh)
	}
	value, err = b.CallTool(ctx, "refresh_quotas", nil)
	if err != nil || value.(map[string]any)["retry_after_seconds"] != 60 {
		t.Fatalf("running engine must enforce refresh limit: %#v, %v", value, err)
	}
	e.running = false
	reserved := b.nextRefresh
	value, err = b.CallTool(ctx, "refresh_quotas", nil)
	assertMCPNotReady(t, value, err, "starting")
	if !b.nextRefresh.Equal(reserved) {
		t.Fatal("not-running request changed the existing reservation")
	}
}

func TestMCPRefreshShutdownRaceDoesNotConsumeLimit(t *testing.T) {
	for _, state := range []string{"starting", "disabled"} {
		t.Run(state, func(t *testing.T) {
			now := time.Now().UTC()
			snapshot := make(chan struct{}, 1)
			e := New(Options{Now: func() time.Time {
				select {
				case snapshot <- struct{}{}:
				default:
				}
				return now
			}})
			e.running = true
			b := NewMCPBackend(e)
			// Hold the reservation lock while refresh takes a running snapshot.
			// Stop/disable then wins before the final running check and reservation.
			b.mu.Lock()
			locked := true
			defer func() {
				if locked {
					b.mu.Unlock()
				}
			}()
			type reply struct {
				value any
				err   error
			}
			done := make(chan reply, 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			go func() {
				value, err := b.CallTool(ctx, "refresh_quotas", nil)
				done <- reply{value, err}
			}()
			select {
			case <-snapshot:
			case <-ctx.Done():
				t.Fatal("refresh did not take a status snapshot")
			}
			if state == "starting" {
				e.Stop(0)
			} else {
				e.mu.Lock()
				e.enabled = false
				e.mu.Unlock()
			}
			b.mu.Unlock()
			locked = false
			select {
			case result := <-done:
				assertMCPNotReady(t, result.value, result.err, state)
			case <-ctx.Done():
				t.Fatal("refresh did not return after shutdown")
			}
			if !b.nextRefresh.IsZero() {
				t.Fatalf("shutdown race consumed limit: %v", b.nextRefresh)
			}
		})
	}
}
