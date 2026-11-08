package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const ignitionOutputWindow = 2 * time.Second

// streamEnd gives the reader and watchdog a single terminal owner. Closing done
// publishes the result; a late deadline, cancellation or panic cannot replace it.
type streamEnd struct {
	mu     sync.Mutex
	done   chan struct{}
	reason string
	err    error
}

func (s *streamEnd) claim(reason string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != "" {
		return false
	}
	s.reason, s.err = reason, err
	close(s.done)
	return true
}

func (s *streamEnd) result() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.err
}

type streamTimer struct {
	deadline <-chan time.Time
	stop     func()
}

func newStreamTimer(wait time.Duration) streamTimer {
	timer := time.NewTimer(wait)
	return streamTimer{deadline: timer.C, stop: func() { timer.Stop() }}
}

// watchStream only closes an active stream after claiming its terminal result.
// The timer factory allows tests to make completion and deadline ready together.
func watchStream(ctx context.Context, stop <-chan struct{}, first <-chan time.Time, window time.Duration, end *streamEnd, closeStream func(), newTimer func(time.Duration) streamTimer) {
	cutoff := func(reason string, err error) {
		if end.claim(reason, err) {
			closeStream()
		}
	}
	defer func() {
		if recover() != nil {
			cutoff("watchdog_error", errors.New("ignition stream watchdog failed"))
		}
	}()
	var timer streamTimer
	defer func() {
		if timer.stop != nil {
			timer.stop()
		}
	}()
	for {
		select {
		case <-end.done:
			return
		case at := <-first:
			timer = newTimer(time.Until(at.Add(window)))
		case <-timer.deadline:
			// A cancellation already visible at cutoff must not be reported as
			// deliberate success; cancellation after a terminal claim is late.
			select {
			case <-ctx.Done():
				cutoff("canceled", context.Canceled)
			case <-stop:
				cutoff("canceled", context.Canceled)
			default:
				cutoff("output_timeout", nil)
			}
			return
		case <-ctx.Done():
			cutoff("canceled", context.Canceled)
			return
		case <-stop:
			cutoff("canceled", context.Canceled)
			return
		}
	}
}

// executeStream retains headers for quota confirmation. A deliberate cutoff
// returns no execution error, so it cannot cause a model fallback. It does not
// establish ignition success: the caller must still confirm the quota window.
func (e *Engine) executeStream(ctx context.Context, t target, req pluginapi.HostModelExecutionRequest, outputWindow time.Duration) (resp pluginapi.HostModelExecutionResponse, err error) {
	started := time.Now()
	correlation := rand.Text()
	var firstOutput time.Time
	var capture streamCapture
	var streamID string
	reason := "start_error"
	defer func() {
		usageState := "missing"
		if len(capture.usage) > 0 {
			usageState = "partial"
			if capture.completeUsage(t.group.Provider, req.ExitProtocol) && capture.terminal && reason == "completed" {
				usageState = "complete"
			}
		}
		var firstWait any
		if !firstOutput.IsZero() {
			firstWait = firstOutput.Sub(started).Milliseconds()
		}
		fields := map[string]any{
			"event": "ignition_stream", "provider": t.group.Provider,
			"model": req.Model, "auth_index": t.group.AuthIndex,
			"correlation_id": correlation, "stream_id": streamID,
			"reply": capture.reply.String(), "reply_truncated": capture.replyChars > replyLimit,
			"output_received": capture.output, "usage": capture.usage, "usage_source": "cpa_host",
			"usage_completeness": usageState, "end_reason": reason,
			"first_output_wait_ms": firstWait, "total_ms": time.Since(started).Milliseconds(),
			"status_code": resp.StatusCode,
		}
		// CPA's default formatter discards arbitrary fields. JSON in Message
		// preserves the record and escapes newlines/control characters.
		raw, _ := json.Marshal(fields)
		level := "info"
		if err != nil {
			level = "warn"
		}
		defer func() { _ = recover() }()
		e.host.Log(level, "Lamplighter ignition "+string(raw), fields)
	}()
	if ctx.Err() != nil || e.stopping() {
		reason = "canceled"
		return resp, context.Canceled
	}

	// Do not detach this synchronous startup into a timeout goroutine: until
	// CPA returns an ID there is no public way to cancel its upstream HTTP.
	opened, err := e.host.ModelExecuteStream(req)
	resp.StatusCode, resp.Headers = opened.StatusCode, opened.Headers
	streamID = opened.StreamID
	if err != nil {
		if ctx.Err() != nil || e.stopping() {
			reason = "canceled"
			return resp, context.Canceled
		}
		return resp, err
	}
	if streamID == "" {
		return resp, errors.New("ignition stream ID missing")
	}

	var closeOnce sync.Once
	var closeErr error
	closeStream := func() {
		closeOnce.Do(func() {
			defer func() {
				if recover() != nil {
					closeErr = errors.New("ignition stream close panic")
				}
			}()
			closeErr = e.host.ModelStreamClose(streamID)
		})
	}
	first := make(chan time.Time, 1)
	end := &streamEnd{done: make(chan struct{})}
	finish := func(why string, failure error) (pluginapi.HostModelExecutionResponse, error) {
		end.claim(why, failure)
		return resp, failure
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchStream(ctx, e.stopCh, first, outputWindow, end, closeStream, newStreamTimer)
	}()
	defer func() {
		// Also release the watchdog if the reader unwinds through a panic.
		end.claim("read_error", errors.New("ignition stream reader failed"))
		<-watchDone
		closeStream()
		reason, err = end.result()
		if closeErr != nil {
			// Cleanup failure is never deliberate success. Preserve the owned
			// execution error as well, including context.Canceled for callers.
			reason, err = "close_error", errors.Join(err, errors.New("ignition stream close failed"))
		}
	}()
	capture.onOutput = func() {
		firstOutput = time.Now()
		first <- firstOutput
	}
	if opened.StatusCode < 200 || opened.StatusCode >= 300 {
		return finish("http_error", &host.Error{Method: "host.model.execute_stream", Code: "model_execution_failed", Message: "ignition model request failed", Status: opened.StatusCode})
	}
	for {
		if ctx.Err() != nil || e.stopping() {
			return finish("canceled", context.Canceled)
		}
		chunk, readErr := e.host.ModelStreamRead(streamID)
		if readErr != nil {
			return finish("read_error", errors.New("ignition stream read failed"))
		}
		if parseErr := capture.feedChunk(req.ExitProtocol, chunk.Payload); parseErr != nil {
			return finish("stream_error", parseErr)
		}
		if chunk.Error != "" {
			return finish("stream_error", streamErrorText(chunk.Error))
		}
		if chunk.Done {
			if parseErr := capture.finish(); parseErr != nil {
				return finish("stream_error", parseErr)
			}
		}
		if capture.terminal {
			return finish("completed", nil)
		}
		if chunk.Done {
			return finish("unexpected_eof", errors.New("ignition stream ended without terminal event"))
		}
	}
}
