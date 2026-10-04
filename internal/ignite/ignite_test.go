package ignite

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

var shanghai = time.FixedZone("UTC+8", 8*3600)

func schedule() Schedule {
	return Schedule{Cfg: config.Default().Ignition, Loc: shanghai}
}

func local(hour, minute int) time.Time {
	return time.Date(2026, 10, 4, hour, minute, 0, 0, shanghai)
}

func TestTriggerPromptIsStrict(t *testing.T) {
	if !strings.Contains(TriggerPrompt, "Reply with exactly OK and nothing else.") {
		t.Fatal("prompt must ask for exactly OK")
	}
}

func TestWaitsForDailyStart(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, time.Time{}, local(5, 0))
	if !due.Equal(local(7, 0)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestUsesResetPlusGrace(t *testing.T) {
	reset := local(10, 0)
	due := schedule().DueAt(&store.TargetState{}, reset, local(9, 0))
	if !due.Equal(reset.Add(3 * time.Second)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestResetAfterCutoffWaitsForNextDay(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, local(23, 0), local(20, 0))
	if !due.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestAfterCutoffWaitsForNextDay(t *testing.T) {
	due := schedule().DueAt(&store.TargetState{}, time.Time{}, local(22, 31))
	if !due.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestExpiredResetIsDueNow(t *testing.T) {
	now := local(12, 0)
	due := schedule().DueAt(&store.TargetState{}, local(11, 59), now)
	if !due.Equal(now) {
		t.Fatalf("due %v", due.In(shanghai))
	}
}

func TestRollingResetIsDetectedFromTwoObservations(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	if Observe(ts, first.Add(5*time.Hour), first, false) {
		t.Fatal("one observation cannot be rolling")
	}
	second := first.Add(5 * time.Minute)
	if !Observe(ts, second.Add(5*time.Hour), second, false) {
		t.Fatal("reset moving with time must be rolling")
	}
	if due := schedule().DueAt(ts, second.Add(5*time.Hour), second); !due.Equal(second) {
		t.Fatalf("rolling window should be due now, got %v", due.In(shanghai))
	}
}

func TestFixedResetIsNotRolling(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	reset := first.Add(5 * time.Hour)
	Observe(ts, reset, first, false)
	if Observe(ts, reset, first.Add(5*time.Minute), false) {
		t.Fatal("a fixed reset is a started window")
	}
}

func TestPassiveObservationIsNeverRolling(t *testing.T) {
	ts := &store.TargetState{}
	first := local(9, 0)
	Observe(ts, first.Add(5*time.Hour), first, false)
	second := first.Add(5 * time.Minute)
	if Observe(ts, second.Add(5*time.Hour), second, true) {
		t.Fatal("passive data comes from a real request, which starts the window")
	}
}

func TestHardFailureOpensCircuitImmediately(t *testing.T) {
	ts := &store.TargetState{}
	now := local(10, 0)
	err := &host.Error{Code: "host_call_failed", Message: "upstream rejected", Status: 429}
	outcome := schedule().RecordFailure(ts, now, err)
	if !outcome.CircuitOpened || !outcome.NotifyCircuit || !outcome.Until.Equal(local(7, 0).AddDate(0, 0, 1)) {
		t.Fatalf("outcome %+v", outcome)
	}
	again := schedule().RecordFailure(ts, now.Add(time.Minute), err)
	if again.NotifyCircuit {
		t.Fatal("the pause must be notified only once")
	}
}

func TestTransientFailureBacksOffThenOpensCircuit(t *testing.T) {
	ts := &store.TargetState{}
	now := local(10, 0)
	err := errors.New("connection reset")
	if o := schedule().RecordFailure(ts, now, err); o.CircuitOpened || o.RetryIn != 5*time.Minute {
		t.Fatalf("first failure %+v", o)
	}
	if o := schedule().RecordFailure(ts, now, err); o.CircuitOpened || o.RetryIn != 15*time.Minute {
		t.Fatalf("second failure %+v", o)
	}
	if o := schedule().RecordFailure(ts, now, err); !o.CircuitOpened {
		t.Fatalf("third failure must pause, got %+v", o)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Kind
	}{
		{&host.Error{Code: "host_call_failed", Message: "auth_not_found: no auth available", Status: 503}, Unsupported},
		{&host.Error{Code: "host_call_failed", Message: "unknown provider for model x", Status: 400}, Unsupported},
		{&host.Error{Code: "host_call_failed", Message: "All credentials for model x are cooling down", Status: 429}, Transient},
		{&host.Error{Code: "host_call_failed", Message: "upstream error", Status: 401}, Hard},
		{&host.Error{Code: "host_call_failed", Message: "upstream error", Status: 502}, Transient},
		{ErrNotConfirmed, Hard},
		{errors.Join(ErrNoModel, errors.New("auth_not_found")), Hard},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

func TestBuildRequestUsesMinimalBodies(t *testing.T) {
	for provider, entry := range map[string]string{"codex": "openai-response", "claude": "claude", "antigravity": "openai"} {
		req, err := BuildRequest(provider, "m")
		if err != nil || req.EntryProtocol != entry {
			t.Fatalf("%s: %+v %v", provider, req, err)
		}
		var body map[string]any
		json.Unmarshal(req.Body, &body)
		if body["model"] != "m" || !strings.Contains(string(req.Body), "exactly OK") {
			t.Fatalf("%s body %s", provider, req.Body)
		}
		if strings.Contains(string(req.Body), `"tools":[{`) {
			t.Fatalf("%s must not declare tools", provider)
		}
	}
}

// cooldownError is the error CPA returned at 22:00:01 on 2026-10-04, when the
// credential was still cooling down 16 seconds after the 5-hour reset.
func cooldownError() error {
	return &host.Error{Code: "host_call_failed", Status: 429, Message: `{"error":{"code":"model_cooldown","last_upstream_error":"rate_limit_error: This request would exceed your account's rate limit. Please try again later.","message":"All credentials for model claude-sonnet-4-6 are cooling down via provider claude","model":"claude-sonnet-4-6","provider":"claude","reset_seconds":17,"reset_time":"16s"}}`}
}

func TestCooldownRetriesWhenItEnds(t *testing.T) {
	ts := &store.TargetState{}
	now := local(22, 0)
	o := schedule().RecordFailure(ts, now, cooldownError())
	if o.CircuitOpened || !o.Cooldown || o.RetryIn != 20*time.Second {
		t.Fatalf("outcome %+v", o)
	}
	if ts.ConsecutiveFailures != 0 {
		t.Fatalf("a cooldown must not count as a failure, got %d", ts.ConsecutiveFailures)
	}
	if held := schedule().HeldUntil(ts, now); !held.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("held until %v", held)
	}
}

func TestRepeatedCooldownCountsAsFailure(t *testing.T) {
	ts := &store.TargetState{}
	now := local(22, 0)
	schedule().RecordFailure(ts, now, cooldownError())
	o := schedule().RecordFailure(ts, now.Add(20*time.Second), cooldownError())
	if o.Cooldown || ts.ConsecutiveFailures != 1 || o.RetryIn != 5*time.Minute {
		t.Fatalf("second cooldown %+v, failures %d", o, ts.ConsecutiveFailures)
	}
}

func TestLongCooldownIsNotWaitedOut(t *testing.T) {
	if _, ok := CooldownWait(errors.New(`model_cooldown "reset_seconds":3600`)); ok {
		t.Fatal("an hour-long cooldown must use the normal retry")
	}
	if _, ok := CooldownWait(errors.New("connection reset")); ok {
		t.Fatal("other errors are not cooldowns")
	}
}
