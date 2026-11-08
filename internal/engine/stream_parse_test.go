package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/ignite"
)

func TestStreamCaptureProtocols(t *testing.T) {
	tests := []struct {
		name, events, reply          string
		output, terminal, usageFinal bool
	}{
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"你好🙂\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", "你好🙂", true, true, false},
		{"responses", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n", "OK", true, true, true},
		{"claude", "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":6,\"output_tokens\":0}}}\r\n\r\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "OK", true, true, true},
		{"empty lifecycle", "data: {\"type\":\"response.created\"}\n\n: ping\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n", "", false, false, false},
		{"encrypted reasoning", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"SECRET\"}}\n\n", "", false, false, false},
		{"chat reasoning", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n", "thinking", true, false, false},
		{"responses reasoning", "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n", "thinking", true, false, false},
		{"claude reasoning", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thinking\"}}\n\n", "thinking", true, false, false},
		{"chat tool args", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\"}}]}}]}\n\n", "{", true, false, false},
		{"responses tool args", "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\"}\n\n", "{", true, false, false},
		{"claude tool args", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\n", "{", true, false, false},
		{"multiline", "data: {\"choices\":\n" + "data: [{\"delta\":{\"content\":\"OK\"}}]}\n\n", "OK", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, fragment := range []bool{false, true} {
				var s streamCapture
				if fragment {
					for _, b := range []byte(tt.events) {
						if err := s.feed([]byte{b}); err != nil {
							t.Fatal(err)
						}
					}
				} else if err := s.feed([]byte(tt.events)); err != nil {
					t.Fatal(err)
				}
				if err := s.finish(); err != nil {
					t.Fatal(err)
				}
				if s.reply.String() != tt.reply || s.output != tt.output || s.terminal != tt.terminal || s.usageFinal != tt.usageFinal {
					t.Fatalf("reply=%q output=%v terminal=%v usageFinal=%v", s.reply.String(), s.output, s.terminal, s.usageFinal)
				}
			}
		})
	}
}

func TestStreamCaptureFirstOutputHook(t *testing.T) {
	calls := 0
	s := streamCapture{onOutput: func() { calls++ }}
	for _, event := range []string{
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"type":"ping"}`,
		`{"type":"response.output_item.added","item":{"encrypted_content":"SECRET"}}`,
	} {
		if err := s.event([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("lifecycle events started output timer")
	}
	for _, event := range []string{
		`{"type":"response.function_call_arguments.delta","delta":"{"}`,
		`{"type":"response.reasoning_text.delta","delta":"thought"}`,
		`{"type":"response.output_text.delta","delta":"OK"}`,
	} {
		if err := s.event([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("first-output hook calls=%d", calls)
	}
}

func TestStreamCaptureUsageAndUnicodeBounds(t *testing.T) {
	var s streamCapture
	s.text(strings.Repeat("你好🙂", 2000))
	if !utf8.ValidString(s.reply.String()) || utf8.RuneCountInString(s.reply.String()) != replyLimit || s.replyChars != 6000 {
		t.Fatalf("reply chars=%d total=%d", utf8.RuneCountInString(s.reply.String()), s.replyChars)
	}
	if s.usage != nil || s.usageFinal {
		t.Fatal("missing usage must remain absent")
	}
	s.recordUsage(map[string]any{"input_tokens": float64(3), "output_tokens": float64(0), "secret": "DO NOT LOG"}, false)
	if len(s.usage) != 2 || s.usageFinal {
		t.Fatalf("usage=%v final=%v", s.usage, s.usageFinal)
	}
	s.recordUsage(map[string]any{"secret": "must not mark usage final"}, true)
	if s.usageFinal {
		t.Fatal("unknown usage field marked complete")
	}
	s.recordUsage(map[string]any{"output_tokens": float64(2)}, true)
	if s.usage["input_tokens"] != float64(3) || s.usage["output_tokens"] != float64(2) || !s.usageFinal {
		t.Fatal(s.usage)
	}
	s.recordUsage(map[string]any{"output_tokens_details": map[string]any{"reasoning_tokens": float64(1), "encrypted_content": "SECRET"}}, true)
	details := object(s.usage["output_tokens_details"])
	if len(details) != 1 || details["reasoning_tokens"] != float64(1) {
		t.Fatal(details)
	}
}

func TestStreamCaptureHostChatAndUsageCompleteness(t *testing.T) {
	var s streamCapture
	for _, chunk := range []string{
		`{"choices":[{"delta":{"content":"OK"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":4}}`,
	} {
		if err := s.feedChunk("openai", []byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if s.terminal || s.completeUsage("antigravity", "openai") || s.reply.String() != "OK" || !s.output {
		t.Fatalf("terminal=%v usageFinal=%v reply=%q", s.terminal, s.usageFinal, s.reply.String())
	}
	if err := s.feedChunk("openai", []byte(`{"choices":[],"usage":{"completion_tokens":1}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(); err != nil || !s.terminal || s.completeUsage("antigravity", "openai") {
		t.Fatalf("terminal=%v usageFinal=%v err=%v", s.terminal, s.usageFinal, err)
	}
	for _, chunk := range []string{"SECRET invalid JSON", strings.Repeat("x", streamFrameLimit+1)} {
		if err := s.feedChunk("openai", []byte(chunk)); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestStreamCaptureTranslatedUsageRemainsPartial(t *testing.T) {
	// Host payload fixtures retain translator defaults. Missing upstream metadata
	// cannot be reconstructed from these fields, even on a natural Chat EOF.
	for _, tc := range []struct {
		name, usage string
	}{
		{"missing metadata defaults", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`},
		{"prompt only defaults", `{"prompt_tokens":12,"completion_tokens":0,"total_tokens":0}`},
		{"prompt only sparse", `{"prompt_tokens":12,"completion_tokens":0}`},
		{"thoughts included without breakdown", `{"prompt_tokens":12,"completion_tokens":9,"total_tokens":21}`},
		{"explicit reasoning breakdown", `{"prompt_tokens":12,"completion_tokens":3,"total_tokens":21,"completion_tokens_details":{"reasoning_tokens":6}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s streamCapture
			payload := `{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`
			if err := s.feedChunk("openai", []byte(payload)); err != nil {
				t.Fatal(err)
			}
			if err := s.finish(); err != nil || !s.terminal || s.usageFinal || s.completeUsage("antigravity", "openai") {
				t.Fatalf("terminal=%v final=%v err=%v", s.terminal, s.usageFinal, err)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.usage), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.usage, want) {
				t.Fatalf("host usage changed: got=%v want=%v", s.usage, want)
			}
		})
	}
}

func TestStreamCaptureNativeUsageCompleteness(t *testing.T) {
	for _, tc := range []struct {
		provider, protocol string
		events             []string
	}{
		{"claude", "claude", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":6,"output_tokens":0,"cache_read_input_tokens":2}}}`,
			`{"type":"message_delta","usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		}},
		{"codex", "openai-response", []string{
			`{"type":"response.completed","response":{"usage":{"input_tokens":6,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":2}}}}`,
		}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var s streamCapture
			for _, event := range tc.events {
				if err := s.feedChunk(tc.protocol, []byte("data: "+event+"\n\n")); err != nil {
					t.Fatal(err)
				}
			}
			if !s.terminal || !s.completeUsage(tc.provider, tc.protocol) || s.usage["input_tokens"] != float64(6) || s.usage["output_tokens"] != float64(3) {
				t.Fatalf("terminal=%v final=%v usage=%v", s.terminal, s.usageFinal, s.usage)
			}
			for _, route := range [][2]string{{"antigravity", tc.protocol}, {tc.provider, "openai"}, {"unknown", tc.protocol}} {
				if s.completeUsage(route[0], route[1]) {
					t.Fatalf("translated/unknown route marked complete: %v", route)
				}
			}
		})
	}
}

func TestStreamErrorMetadataAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, raw, code string
		status          int
		kind            ignite.Kind
		modelRejected   bool
	}{
		{"host status", "HTTP 429: SECRET", "provider_stream_error", 429, ignite.Hard, false},
		{"host status fallback", "status 401", "provider_stream_error", 401, ignite.Hard, false},
		{"numeric protocol code", `{"error":{"code":429,"message":"SECRET"}}`, "provider_stream_error", 429, ignite.Hard, false},
		{"string protocol status", `{"error":{"status":"401","message":"SECRET"}}`, "provider_stream_error", 401, ignite.Hard, false},
		{"host JSON", `{"error":{"code":"model_not_found","type":"invalid_request_error","message":"SECRET"}}`, "model_not_found/invalid_request_error", 404, ignite.Hard, true},
		{"explicit status", `{"status_code":403,"error":{"code":"invalid_request_error","message":"SECRET"}}`, "invalid_request_error", 403, ignite.Hard, false},
		{"Claude error", `{"type":"error","error":{"type":"authentication_error","message":"SECRET"}}`, "authentication_error", 401, ignite.Hard, false},
		{"Responses failure", `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"SECRET"}}}`, "rate_limit_exceeded", 429, ignite.Hard, false},
		{"normalized authentication", `{"error":{"code":"auth_unavailable","type":"authentication_error","message":"SECRET"}}`, "authentication_error", 401, ignite.Hard, false},
		{"explicit authentication status", `{"status":401,"error":{"code":"auth_unavailable","message":"SECRET"}}`, "provider_stream_error", 401, ignite.Hard, false},
		{"local cooldown", `{"error":{"code":"auth_unavailable","message":"SECRET"}}`, "auth_unavailable", 0, ignite.Transient, false},
		{"Codex quota exhausted", `{"error":{"type":"usage_limit_reached","message":"SECRET"}}`, "usage_limit_reached", 429, ignite.Hard, false},
		{"server error", `{"error":{"type":"server_error","message":"SECRET"}}`, "server_error", 500, ignite.Transient, false},
		{"unknown metadata", `{"status":499,"error":{"code":"SECRET","type":"SECRET","message":"HTTP 401 model_not_found SECRET"}}`, "provider_stream_error", 0, ignite.Transient, false},
		{"fractional status", `{"status":401.5,"error":{"message":"SECRET"}}`, "provider_stream_error", 0, ignite.Transient, false},
		{"message only", `{"error":{"message":"HTTP 429 SECRET"}}`, "provider_stream_error", 0, ignite.Transient, false},
		{"unstructured message", "SECRET HTTP 401 invalid model", "provider_stream_error", 0, ignite.Transient, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := streamErrorText(tc.raw)
			var typed *host.Error
			if !errors.As(err, &typed) || typed.Code != tc.code || host.StatusOf(err) != tc.status || ignite.Classify(err) != tc.kind || ignite.ModelRejected(err) != tc.modelRejected || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("err=%v status=%d kind=%v rejected=%v", err, host.StatusOf(err), ignite.Classify(err), ignite.ModelRejected(err))
			}
		})
	}
}

func TestStreamCaptureErrors(t *testing.T) {
	for _, input := range []string{"data: invalid\n\n", "data: {\"type\":\"error\",\"error\":{\"message\":\"SECRET\"}}\n\n", "data: {\"type\":\"response.failed\"}\n\n", "data: {\"type\":\"response.incomplete\"}\n\n", strings.Repeat("x", streamFrameLimit+1)} {
		var s streamCapture
		err := s.feed([]byte(input))
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("error=%v", err)
		}
	}
	var s streamCapture
	if err := s.feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}")); err != nil {
		t.Fatal(err)
	}
	if s.output {
		t.Fatal("incomplete frame dispatched early")
	}
	if err := s.finish(); err != nil || s.reply.String() != "OK" {
		t.Fatalf("reply=%q err=%v", s.reply.String(), err)
	}
}
