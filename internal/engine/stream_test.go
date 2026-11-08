package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/ignite"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// streamHost blocks reads until a chunk arrives or Close cancels the upstream.
// This models the host bridge rather than timing out only the caller's wait.
type streamHost struct {
	*fakeHost
	chunks    chan pluginapi.HostModelStreamReadResponse
	closed    chan struct{}
	reading   chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
	calls     int
	opened    pluginapi.HostModelStreamResponse
	openErr   error
	open      func()
	messages  []string
}

func newStreamHost() *streamHost {
	return &streamHost{
		fakeHost: &fakeHost{}, chunks: make(chan pluginapi.HostModelStreamReadResponse, 10),
		closed: make(chan struct{}), reading: make(chan struct{}, 10),
		opened: pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "stream-1", Headers: http.Header{"X-Test": {"quota"}}},
	}
}

func (h *streamHost) ModelExecuteStream(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelStreamResponse, error) {
	h.calls++
	if h.open != nil {
		h.open()
	}
	return h.opened, h.openErr
}

func (h *streamHost) ModelStreamRead(string) (pluginapi.HostModelStreamReadResponse, error) {
	select {
	case h.reading <- struct{}{}:
	default:
	}
	select {
	case chunk := <-h.chunks:
		return chunk, nil
	case <-h.closed:
		return pluginapi.HostModelStreamReadResponse{}, errors.New("upstream canceled SECRET")
	}
}

func (h *streamHost) ModelStreamClose(string) error {
	h.mu.Lock()
	h.closes++
	h.mu.Unlock()
	h.closeOnce.Do(func() { close(h.closed) })
	return nil
}

func (h *streamHost) Log(_ string, message string, _ map[string]any) {
	h.messages = append(h.messages, message)
}

func (h *streamHost) event(s string) {
	h.chunks <- pluginapi.HostModelStreamReadResponse{Payload: []byte("data: " + s + "\n\n")}
}

func streamTarget() target {
	return target{group: GroupView{Provider: "codex", AuthIndex: "42"}}
}

func streamRequest() pluginapi.HostModelExecutionRequest {
	return pluginapi.HostModelExecutionRequest{Model: "test-model", Stream: true, ExitProtocol: "openai-response", Body: []byte("SECRET request"), AuthID: "SECRET credential"}
}

func streamRecord(t *testing.T, h *streamHost) map[string]any {
	t.Helper()
	if len(h.messages) != 1 {
		t.Fatalf("want one per-call log, got %v", h.messages)
	}
	message := h.messages[0]
	if strings.Contains(message, "SECRET") || strings.Contains(message, "\n") {
		t.Fatalf("unsafe log message: %s", message)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "Lamplighter ignition ")), &record); err != nil {
		t.Fatal(err)
	}
	if record["provider"] != "codex" || record["model"] != "test-model" || record["auth_index"] != "42" || record["correlation_id"] == "" {
		t.Fatal(record)
	}
	return record
}

func TestIgnitionStreamWatchdogClosesBlockedRead(t *testing.T) {
	h := newStreamHost()
	h.event(`{"type":"response.output_text.delta","delta":"OK"}`)
	e := New(Options{Host: h})
	started := time.Now()
	resp, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	elapsed := time.Since(started)
	if err != nil || elapsed < 1900*time.Millisecond || elapsed > 5*time.Second || h.closes != 1 {
		t.Fatalf("err=%v elapsed=%s closes=%d", err, elapsed, h.closes)
	}
	if resp.Headers.Get("X-Test") != "quota" {
		t.Fatal("lost headers for quota confirmation")
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "output_timeout" || r["reply"] != "OK" || r["usage_completeness"] != "missing" || r["first_output_wait_ms"] == nil {
		t.Fatal(r)
	}
}

func TestIgnitionStreamNaturalEndAndLog(t *testing.T) {
	h := newStreamHost()
	h.event(`{"type":"response.output_item.added","item":{"encrypted_content":"SECRET"}}`)
	h.event(`{"type":"response.reasoning_summary_text.delta","delta":"brief\n"}`)
	h.event(`{"type":"response.function_call_arguments.delta","delta":"{}"}`)
	text, _ := json.Marshal(strings.Repeat("好🙂", 2500))
	h.event(`{"type":"response.output_text.delta","delta":` + string(text) + `}`)
	h.event(`{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":19,"output_tokens_details":{"reasoning_tokens":7,"encrypted_content":"SECRET"}}}}`)
	e := New(Options{Host: h})
	started := time.Now()
	_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
	if err != nil || time.Since(started) > time.Second || h.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "completed" || r["usage_completeness"] != "complete" || r["reply_truncated"] != true || len([]rune(str(r["reply"]))) != replyLimit || !strings.HasPrefix(str(r["reply"]), "brief\n{}") {
		t.Fatal(r)
	}
	if object(r["usage"])["output_tokens"] != float64(19) {
		t.Fatal("usage must be reported, not estimated")
	}
}

func TestIgnitionHostChatJSONChunks(t *testing.T) {
	for _, cutoff := range []bool{false, true} {
		h := newStreamHost()
		h.chunks <- pluginapi.HostModelStreamReadResponse{Payload: []byte(`{"choices":[{"delta":{"content":"OK"},"finish_reason":null}]}`)}
		if !cutoff {
			h.chunks <- pluginapi.HostModelStreamReadResponse{Payload: []byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)}
			h.chunks <- pluginapi.HostModelStreamReadResponse{Payload: []byte(`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":1}}`)}
			h.chunks <- pluginapi.HostModelStreamReadResponse{Done: true}
		}
		e := New(Options{Host: h})
		req := streamRequest()
		req.EntryProtocol, req.ExitProtocol = "openai", "openai"
		_, err := e.executeStream(context.Background(), streamTarget(), req, 20*time.Millisecond)
		if err != nil || h.closes != 1 {
			t.Fatalf("cutoff=%v err=%v closes=%d", cutoff, err, h.closes)
		}
		r := streamRecord(t, h)
		if r["reply"] != "OK" || r["first_output_wait_ms"] == nil {
			t.Fatal(r)
		}
		if cutoff {
			if r["end_reason"] != "output_timeout" || r["usage_completeness"] != "missing" {
				t.Fatal(r)
			}
		} else if r["end_reason"] != "completed" || r["usage_completeness"] != "partial" {
			t.Fatal(r)
		}
	}
}

func TestIgnitionStreamUsageProvenance(t *testing.T) {
	for _, tc := range []struct {
		provider, protocol, completeness string
		chunks                           []string
	}{
		{"antigravity", "openai", "partial", []string{
			`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":0}}`,
		}},
		{"antigravity", "openai", "partial", []string{
			`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}}`,
		}},
		{"claude", "claude", "complete", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`,
			`{"type":"message_delta","usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		}},
		{"codex", "openai-response", "complete", []string{
			`{"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":9}}}`,
		}},
	} {
		t.Run(tc.provider+"/"+tc.chunks[0], func(t *testing.T) {
			h := newStreamHost()
			for _, chunk := range tc.chunks {
				if tc.protocol == "openai" {
					h.chunks <- pluginapi.HostModelStreamReadResponse{Payload: []byte(chunk)}
				} else {
					h.event(chunk)
				}
			}
			h.chunks <- pluginapi.HostModelStreamReadResponse{Done: true}
			e := New(Options{Host: h})
			target, req := streamTarget(), streamRequest()
			target.group.Provider, req.ExitProtocol = tc.provider, tc.protocol
			_, err := e.executeStream(context.Background(), target, req, ignitionOutputWindow)
			if err != nil || h.closes != 1 || len(h.messages) != 1 {
				t.Fatalf("err=%v closes=%d logs=%v", err, h.closes, h.messages)
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(h.messages[0], "Lamplighter ignition ")), &record); err != nil {
				t.Fatal(err)
			}
			if record["usage_completeness"] != tc.completeness || record["usage_source"] != "cpa_host" || record["end_reason"] != "completed" {
				t.Fatal(record)
			}
		})
	}
}

func TestIgnitionStreamLifecycleDoesNotStartTimer(t *testing.T) {
	h := newStreamHost()
	h.event(`{"type":"response.created"}`)
	h.event(`{"choices":[{"delta":{"role":"assistant","content":""}}]}`)
	h.event(`{"type":"ping"}`)
	e := New(Options{Host: h})
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	_, err := e.executeStream(ctx, streamTarget(), streamRequest(), 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) || h.closes != 1 {
		t.Fatalf("err=%v closes=%d", err, h.closes)
	}
	r := streamRecord(t, h)
	if r["end_reason"] != "canceled" || r["first_output_wait_ms"] != nil || r["output_received"] != false {
		t.Fatal(r)
	}
}

func TestIgnitionStreamStopAndStartupCancellation(t *testing.T) {
	for _, phase := range []string{"before call", "during startup", "blocked read"} {
		t.Run(phase, func(t *testing.T) {
			h := newStreamHost()
			e := New(Options{Host: h})
			var stopperDone chan struct{}
			switch phase {
			case "before call":
				close(e.stopCh)
			case "during startup":
				// Cancellation cannot reach CPA until the startup call returns.
				h.open = func() { close(e.stopCh) }
			case "blocked read":
				stopperDone = make(chan struct{})
				go func() {
					defer close(stopperDone)
					<-h.reading
					close(e.stopCh)
				}()
			}
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), ignitionOutputWindow)
			if stopperDone != nil {
				<-stopperDone
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if phase == "before call" {
				if h.calls != 0 || h.closes != 0 {
					t.Fatal("started request after stop")
				}
			} else if h.closes != 1 {
				t.Fatalf("leaked stream: closes=%d", h.closes)
			}
			if streamRecord(t, h)["end_reason"] != "canceled" {
				t.Fatal(h.messages)
			}
		})
	}
}

func TestIgnitionStreamErrorsAndPartialUsage(t *testing.T) {
	for _, mode := range []string{"startup", "http", "error", "read terminal error", "eof", "invalid", "partial cutoff"} {
		t.Run(mode, func(t *testing.T) {
			h := newStreamHost()
			switch mode {
			case "startup":
				h.opened = pluginapi.HostModelStreamResponse{}
				h.openErr = &host.Error{Code: "auth_not_found", Message: "SECRET", Status: 503}
			case "http":
				h.opened.StatusCode = 400
			case "error":
				h.event(`{"error":{"message":"SECRET"}}`)
			case "read terminal error":
				h.chunks <- pluginapi.HostModelStreamReadResponse{Error: "SECRET", Done: true}
			case "eof":
				h.chunks <- pluginapi.HostModelStreamReadResponse{Done: true}
			case "invalid":
				h.event(`SECRET invalid json`)
			case "partial cutoff":
				h.event(`{"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}`)
				h.event(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"visible"}}`)
			}
			e := New(Options{Host: h})
			_, err := e.executeStream(context.Background(), streamTarget(), streamRequest(), 10*time.Millisecond)
			if (err == nil) != (mode == "partial cutoff") {
				t.Fatal(err)
			}
			r := streamRecord(t, h)
			if mode == "partial cutoff" && (r["usage_completeness"] != "partial" || r["reply"] != "visible") {
				t.Fatal(r)
			}
		})
	}
}

// Hold the watchdog inside timer creation until the reader owns a result.
// Both done and deadline are then ready at the same select, without sleeps.
func TestStreamEndReaderBeatsReadyDeadline(t *testing.T) {
	parseErr := (&streamCapture{}).event([]byte(`{"error":{"message":"SECRET"}}`))
	readErr := errors.New("ignition stream read failed")
	for _, tc := range []struct {
		reason string
		err    error
	}{
		{"completed", nil}, {"stream_error", parseErr}, {"read_error", readErr},
		{"unexpected_eof", errors.New("ignition stream ended without terminal event")},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			end := &streamEnd{done: make(chan struct{})}
			first := make(chan time.Time, 1)
			first <- time.Now()
			deadline := make(chan time.Time, 1)
			creating, release, joined := make(chan struct{}), make(chan struct{}), make(chan struct{})
			closes, stops := 0, 0
			go func() {
				defer close(joined)
				watchStream(context.Background(), nil, first, ignitionOutputWindow, end, func() { closes++ }, func(time.Duration) streamTimer {
					close(creating)
					<-release
					return streamTimer{deadline: deadline, stop: func() { stops++ }}
				})
			}()
			<-creating
			if !end.claim(tc.reason, tc.err) {
				t.Fatal("reader could not claim active stream")
			}
			deadline <- time.Now()
			close(release)
			<-joined
			// Exercise the cutoff arbitration directly as well, independent of
			// which of the two ready select cases the runtime chooses.
			if end.claim("output_timeout", nil) {
				t.Fatal("late deadline replaced the reader")
			}
			reason, err := end.result()
			if reason != tc.reason || err != tc.err || closes != 0 || stops != 1 {
				t.Fatalf("reason=%s err=%v closes=%d stops=%d", reason, err, closes, stops)
			}
		})
	}
}

func TestStreamWatchdogOwnershipAndPanic(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "stop", "panic", "late panic", "late cancel", "late stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := make(chan struct{})
			first := make(chan time.Time, 1)
			first <- time.Now()
			deadline := make(chan time.Time, 1)
			end := &streamEnd{done: make(chan struct{})}
			want := "output_timeout"
			closes := 0
			watchStream(ctx, stop, first, ignitionOutputWindow, end, func() { closes++ }, func(time.Duration) streamTimer {
				switch mode {
				case "cancel", "late cancel":
					cancel()
					want = "canceled"
				case "stop", "late stop":
					close(stop)
					want = "canceled"
				case "panic":
					panic("SECRET")
				}
				if strings.HasPrefix(mode, "late ") {
					end.claim("completed", nil)
					want = "completed"
					if mode == "late panic" {
						panic("SECRET")
					}
				}
				deadline <- time.Now()
				return streamTimer{deadline: deadline, stop: func() {}}
			})
			if mode == "panic" {
				want = "watchdog_error"
			}
			reason, err := end.result()
			wantCloses := 1
			if want == "completed" {
				wantCloses = 0
			}
			if reason != want || closes != wantCloses || ((err == nil) != (want == "completed" || want == "output_timeout")) {
				t.Fatalf("reason=%s err=%v closes=%d want=%s", reason, err, closes, want)
			}
			if end.claim("read_error", errors.New("closed reader")) {
				t.Fatal("read error replaced watchdog owner")
			}
		})
	}
}

type cleanupStreamHost struct {
	*streamHost
	read    func() (pluginapi.HostModelStreamReadResponse, error)
	cleanup func() error
}

func (h *cleanupStreamHost) ModelStreamRead(id string) (pluginapi.HostModelStreamReadResponse, error) {
	if h.read != nil {
		return h.read()
	}
	return h.streamHost.ModelStreamRead(id)
}

func (h *cleanupStreamHost) ModelStreamClose(id string) error {
	_ = h.streamHost.ModelStreamClose(id)
	return h.cleanup()
}

func TestIgnitionStreamCleanupPriority(t *testing.T) {
	for _, owner := range []string{"completed", "read_error", "canceled", "output_timeout"} {
		for _, cleanup := range []string{"ok", "error", "panic"} {
			t.Run(owner+"/"+cleanup, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h := &cleanupStreamHost{streamHost: newStreamHost()}
				switch owner {
				case "completed":
					h.event(`{"type":"response.completed"}`)
				case "output_timeout":
					h.event(`{"type":"response.output_text.delta","delta":"OK"}`)
				case "read_error", "canceled":
					h.read = func() (pluginapi.HostModelStreamReadResponse, error) {
						if owner == "canceled" {
							cancel()
							// Block until the watchdog closes upstream, ensuring
							// cancellation owns the terminal result first.
							<-h.closed
						}
						return pluginapi.HostModelStreamReadResponse{}, errors.New("SECRET")
					}
				}
				h.cleanup = func() error {
					cancel() // Cancellation during cleanup cannot replace an owner.
					switch cleanup {
					case "panic":
						panic("SECRET")
					case "error":
						return errors.New("SECRET")
					}
					return nil
				}
				e := New(Options{Host: h})
				_, err := e.executeStream(ctx, streamTarget(), streamRequest(), 0)
				wantReason := owner
				if cleanup != "ok" {
					wantReason = "close_error"
				}
				if streamRecord(t, h.streamHost)["end_reason"] != wantReason || h.closes != 1 {
					t.Fatalf("reason=%v closes=%d", h.messages, h.closes)
				}
				wantError := cleanup != "ok" || owner == "read_error" || owner == "canceled"
				if (err != nil) != wantError || (owner == "canceled" && !errors.Is(err, context.Canceled)) || (err != nil && strings.Contains(err.Error(), "SECRET")) {
					t.Fatalf("err=%v wantError=%v", err, wantError)
				}
			})
		}
	}
}

func TestIgnitionStreamErrorsKeepFailureProtection(t *testing.T) {
	for _, channel := range []string{"host", "error", "response.failed"} {
		for _, failure := range []string{"rate_limit_exceeded", "authentication_error", "model_not_found", "normalized_authentication", "explicit_authentication_status", "usage_limit_reached"} {
			t.Run(channel+"/"+failure, func(t *testing.T) {
				c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
				base := claudeIgnitionHost(c, nil)
				e, _ := newTestEngine(t, base, c)
				e.poll(context.Background(), e.config(), c.t, "", false)
				h := newStreamHost()
				h.fakeHost = base
				h.opened.Headers = http.Header{
					"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"},
					"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
				}
				h.open = func() {
					if h.calls > 1 {
						h.closed = make(chan struct{})
						h.closeOnce = sync.Once{}
						h.event(`{"type":"message_stop"}`)
						return
					}
					// The HTTP request has opened and returned a lifecycle event,
					// but this does not establish successful ignition.
					h.event(`{"type":"message_start"}`)
					detail := `{"code":"` + failure + `","type":"` + failure + `","message":"SECRET"}`
					switch failure {
					case "normalized_authentication":
						detail = `{"code":"auth_unavailable","type":"authentication_error","message":"SECRET"}`
					case "explicit_authentication_status":
						detail = `{"status":401,"code":"auth_unavailable","message":"SECRET"}`
					case "usage_limit_reached":
						detail = `{"type":"usage_limit_reached","message":"SECRET"}`
					}
					switch channel {
					case "host":
						h.chunks <- pluginapi.HostModelStreamReadResponse{Error: `{"error":` + detail + `}`, Done: true}
					case "error":
						h.event(`{"type":"error","error":` + detail + `}`)
					case "response.failed":
						h.event(`{"type":"response.failed","response":{"error":` + detail + `}}`)
					}
				}
				e.host = h
				var chosen target
				for _, candidate := range e.collectTargets(e.config()) {
					if candidate.group.Provider == "claude" {
						chosen = candidate
					}
				}
				if chosen.cred == nil {
					t.Fatal("missing Claude target")
				}
				err := e.igniteTarget(context.Background(), e.config(), chosen, true)
				ts := e.state.Target(chosen.group.Key)
				if failure == "model_not_found" {
					if err != nil || h.calls != 2 || h.closes != 2 || ts.LastSuccessEpoch == 0 || len(e.refused) != 1 {
						t.Fatalf("model fallback: err=%v calls=%d closes=%d state=%+v refused=%v", err, h.calls, h.closes, ts, e.refused)
					}
				} else if err == nil || ignite.Classify(err) != ignite.Hard || h.calls != 1 || h.closes != 1 || ts.CircuitOpenUntilEpoch == 0 || ts.ConsecutiveFailures != 1 || ts.LastSuccessEpoch != 0 {
					t.Fatalf("hard failure protection: err=%v calls=%d closes=%d state=%+v", err, h.calls, h.closes, ts)
				}
				if strings.Contains(ts.LastError+strings.Join(h.messages, ""), "SECRET") {
					t.Fatal("raw provider message escaped into state or logs")
				}
			})
		}
	}
}

func TestIgnitionCutoffConfirmsQuotaWindow(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	h := newStreamHost()
	h.fakeHost = base
	h.opened.Headers = http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.01"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(c.t.Add(5*time.Hour).Unix(), 10)},
	}
	h.event(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`)
	e.host = h
	var chosen target
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "claude" {
			chosen = candidate
		}
	}
	if chosen.cred == nil {
		t.Fatal("missing Claude target")
	}
	if err := e.igniteTarget(context.Background(), e.config(), chosen, true); err != nil || h.calls != 1 || h.closes != 1 {
		t.Fatalf("err=%v calls=%d closes=%d", err, h.calls, h.closes)
	}
	ts := e.state.Target(chosen.group.Key)
	if ts.LastSuccessEpoch == 0 || ts.ConsecutiveFailures != 0 || ts.CircuitOpenUntilEpoch != 0 || !e.confirmed(chosen.group.Key, c.t, c.t) {
		t.Fatalf("quota window not confirmed: %+v", ts)
	}
	var record map[string]any
	for _, message := range h.messages {
		if strings.HasPrefix(message, "Lamplighter ignition ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(message, "Lamplighter ignition ")), &record); err != nil {
				t.Fatal(err)
			}
		}
	}
	if record["end_reason"] != "output_timeout" || record["reply"] != "OK" {
		t.Fatal(record)
	}
}

func TestIgnitionCutoffDoesNotFallbackOrConfirmFromBody(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 4, 10, 0, 0, 0, shanghai)}
	base := claudeIgnitionHost(c, nil)
	e, _ := newTestEngine(t, base, c)
	e.poll(context.Background(), e.config(), c.t, "", false)
	h := newStreamHost()
	h.fakeHost = base
	h.event(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`)
	e.host = h
	old := confirmDelays
	confirmDelays = []time.Duration{time.Millisecond}
	defer func() { confirmDelays = old }()
	var chosen target
	for _, candidate := range e.collectTargets(e.config()) {
		if candidate.group.Provider == "claude" {
			chosen = candidate
		}
	}
	if chosen.cred == nil {
		t.Fatal("missing Claude target")
	}
	err := e.igniteTarget(context.Background(), e.config(), chosen, true)
	if !errors.Is(err, ignite.ErrNotConfirmed) || h.calls != 1 || h.closes != 1 {
		t.Fatalf("err=%v calls=%d closes=%d", err, h.calls, h.closes)
	}
	if e.state.Target(chosen.group.Key).LastSuccessEpoch != 0 {
		t.Fatal("body incorrectly established ignition success")
	}
}
