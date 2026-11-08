package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func checkManualFailure(t *testing.T, e *Engine, sender *fakeSender, c *clock, chosen target, seeded bool) {
	t.Helper()
	ts := e.state.Target(chosen.group.Key)
	if seeded {
		ts.ConsecutiveFailures = 2
		ts.RetryAtEpoch = float64(c.t.Add(15 * time.Minute).Unix())
		ts.CircuitOpenUntilEpoch = float64(c.t.Add(24 * time.Hour).Unix())
		ts.CircuitNotifiedUntilEpoch = ts.CircuitOpenUntilEpoch
		ts.CircuitReason, ts.LastError = "existing circuit", "automatic failure"
		ts.LastFailureEpoch = float64(c.t.Add(-time.Minute).Unix())
	}
	before := *ts
	due := e.schedule(e.config()).DueAt(ts, chosen.reset, c.t)
	for i := 0; i < 3; i++ {
		count, notifications := len(e.events), len(sender.sent)
		err := e.igniteTarget(context.Background(), e.config(), chosen, true)
		if err == nil || len(e.events) != count+1 || len(sender.sent) != notifications {
			t.Fatalf("err=%v events=%d notifications=%d", err, len(e.events)-count, len(sender.sent)-notifications)
		}
		got := *ts
		got.LastAttemptEpoch = before.LastAttemptEpoch
		if !reflect.DeepEqual(got, before) || !e.schedule(e.config()).DueAt(ts, chosen.reset, c.t).Equal(due) {
			t.Fatalf("manual failure changed automatic state: before=%+v after=%+v", before, ts)
		}
		ev := e.events[len(e.events)-1]
		if ev.Kind != "ignite_failed" || ev.Params != nil || ev.Detail != err.Error() || strings.Contains(ev.Message+ev.Detail, "重试") {
			t.Fatalf("manual failure must display error without retry: %+v", ev)
		}
	}
}

func TestManualFailurePreservesAutomaticSchedule(t *testing.T) {
	failures := map[string]error{
		"transient": errors.New("network down"),
		"hard":      &host.Error{Status: 429, Message: "rate limit"},
		"cooldown":  errors.New(`model_cooldown {"reset_seconds":20}`),
	}
	for name, failure := range failures {
		for _, seeded := range []bool{false, true} {
			t.Run(name+"/seeded="+map[bool]string{false: "false", true: "true"}[seeded], func(t *testing.T) {
				c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
				h := claudeIgnitionHost(c, nil)
				h.execute = func(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
					return pluginapi.HostModelExecutionResponse{}, failure
				}
				e, sender := newTestEngine(t, h, c)
				e.poll(context.Background(), e.config(), c.t, "", false)
				var chosen target
				for _, candidate := range e.collectTargets(e.config()) {
					if candidate.group.Provider == "claude" {
						chosen = candidate
					}
				}
				if chosen.cred == nil {
					t.Fatal("missing Claude target")
				}
				checkManualFailure(t, e, sender, c, chosen, seeded)
			})
		}
	}
}
