package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
)

const replyLimit = 4096
const streamFrameLimit = 1 << 20

// Only protocol identifiers in this allowlist may reach persisted errors.
// Never classify or retain free-form provider messages: they can contain secrets.
var streamErrorCodes = map[string]int{
	"rate_limit_error": 429, "rate_limit_exceeded": 429, "insufficient_quota": 429, "quota_exceeded": 429,
	"usage_limit_reached":  429,
	"authentication_error": 401, "invalid_api_key": 401, "unauthorized": 401,
	"permission_error": 403, "permission_denied": 403, "forbidden": 403,
	"not_found_error": 404, "not_found": 404, "model_not_found": 404,
	"invalid_request_error": 400, "invalid_request": 400, "invalid_model": 400,
	"unsupported_model": 400, "model_not_supported": 400,
	"overloaded_error": 529, "server_error": 500, "api_error": 500, "internal_server_error": 500,
	"auth_not_found": 0, "auth_unavailable": 0,
}

var streamErrorStatusPrefix = regexp.MustCompile(`(?i)^(?:http(?: status)?|status(?: code)?)[: ]+([0-9]{3})\b`)

func allowedStreamStatus(v any) int {
	var status int
	switch value := v.(type) {
	case float64:
		if value != float64(int(value)) {
			return 0
		}
		status = int(value)
	case string:
		status, _ = strconv.Atoi(value)
	}
	switch status {
	case 400, 401, 403, 404, 408, 409, 422, 429, 500, 502, 503, 504, 529:
		return status
	}
	return 0
}

// streamErrorText handles the host bridge's string-only error channel. JSON
// envelopes and anchored HTTP/status prefixes supply metadata; all other text
// is discarded, including messages that merely mention a status or model.
func streamErrorText(raw string) error {
	if len(raw) > streamFrameLimit {
		return streamError(nil)
	}
	var v map[string]any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return streamError(v)
	}
	if match := streamErrorStatusPrefix.FindStringSubmatch(strings.TrimSpace(raw)); match != nil {
		return streamError(map[string]any{"status": match[1]})
	}
	return streamError(nil)
}

func streamError(v map[string]any) error {
	detail := object(v["error"])
	response := object(v["response"])
	if detail == nil {
		detail = object(response["error"])
	}
	if detail == nil {
		detail = v
	}
	status := 0
	for _, envelope := range []map[string]any{v, response, detail} {
		for _, key := range []string{"status_code", "status", "http_status", "code"} {
			if status == 0 {
				status = allowedStreamStatus(envelope[key])
			}
		}
	}
	for _, key := range []string{"code", "type"} {
		if status == 0 {
			status = streamErrorCodes[str(detail[key])]
		}
	}
	var identifiers []string
	for _, key := range []string{"code", "type"} {
		value := str(detail[key])
		// CPA also uses auth_unavailable for normalized upstream authentication
		// failures. Do not let its local-cooldown marker override that failure.
		if value == "auth_unavailable" && (status == 401 || str(detail["type"]) == "authentication_error") {
			continue
		}
		if _, ok := streamErrorCodes[value]; ok {
			identifiers = append(identifiers, value)
		}
	}
	code := "provider_stream_error"
	if len(identifiers) > 0 {
		code = strings.Join(identifiers, "/")
	}
	return &host.Error{Method: "host.model.stream_read", Code: code, Status: status, Message: "ignition provider stream error"}
}

// streamCapture parses CPA's translated chunks without retaining raw responses or
// opaque reasoning. Only visible text is included in the bounded log reply.
type streamCapture struct {
	pending      []byte
	byteStream   bool
	eventData    []byte
	reply        strings.Builder
	replyChars   int
	output       bool
	onOutput     func()
	terminal     bool
	chatFinished bool
	usage        map[string]any
	usageFinal   bool
}

func (s *streamCapture) text(v string) {
	s.markOutput(v)
	n := utf8.RuneCountInString(v)
	remaining := replyLimit - s.replyChars
	if remaining > 0 {
		r := []rune(v)
		if len(r) > remaining {
			r = r[:remaining]
		}
		s.reply.WriteString(string(r))
	}
	s.replyChars += n
}

func (s *streamCapture) markOutput(v string) {
	if v == "" || s.output {
		return
	}
	s.output = true
	if s.onOutput != nil {
		s.onOutput()
	}
}

// feedChunk follows the model callback wire format. OpenAI chat chunks are
// JSON objects; the public HTTP handler adds SSE framing and [DONE] later.
// Claude always carries SSE bytes. Responses carries complete scanner lines
// until a chunk with a newline identifies standard SSE bytes.
func (s *streamCapture) feedChunk(protocol string, payload []byte) error {
	if protocol == "openai-response" {
		if bytes.IndexByte(payload, '\n') >= 0 {
			s.byteStream = true
		}
		if !s.byteStream && len(s.pending) == 0 {
			return s.scannerLine(payload)
		}
	}
	if protocol != "openai" {
		return s.feed(payload)
	}
	if len(payload) > streamFrameLimit {
		return errors.New("ignition stream frame too large")
	}
	return s.event(bytes.TrimSpace(payload))
}

// scannerLine receives one whole line with its newline removed by CPA.
// Complete data events dispatch immediately because empty lines are omitted.
func (s *streamCapture) scannerLine(payload []byte) error {
	if len(payload) > streamFrameLimit {
		return errors.New("ignition stream frame too large")
	}
	if err := s.line(bytes.TrimSuffix(payload, []byte{'\r'})); err != nil {
		return err
	}
	data := bytes.TrimSpace(s.eventData)
	if json.Valid(data) || bytes.Equal(data, []byte("[DONE]")) {
		s.eventData = nil
		return s.event(data)
	}
	return nil
}

func (s *streamCapture) feed(payload []byte) error {
	// Standard SSE may split JSON, UTF-8, CRLF, or metadata at any byte.
	// Preserve the current line until its delimiter arrives.
	for len(payload) > 0 {
		i := bytes.IndexByte(payload, '\n')
		if i < 0 {
			if len(s.pending)+len(payload) > streamFrameLimit {
				return errors.New("ignition stream frame too large")
			}
			s.pending = append(s.pending, payload...)
			return nil
		}
		if len(s.pending)+i > streamFrameLimit {
			return errors.New("ignition stream frame too large")
		}
		s.pending = append(s.pending, payload[:i]...)
		line := bytes.TrimSuffix(s.pending, []byte{'\r'})
		if err := s.line(line); err != nil {
			return err
		}
		s.pending = s.pending[:0]
		payload = payload[i+1:]
	}
	return nil
}

func (s *streamCapture) line(line []byte) error {
	if len(line) == 0 {
		data := s.eventData
		s.eventData = nil
		return s.event(data)
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		data := bytes.TrimPrefix(line[5:], []byte{' '})
		if len(s.eventData)+len(data)+1 > streamFrameLimit {
			return errors.New("ignition stream frame too large")
		}
		if len(s.eventData) > 0 {
			s.eventData = append(s.eventData, '\n')
		}
		s.eventData = append(s.eventData, data...)
	}
	return nil
}

func (s *streamCapture) finish() error {
	if len(s.pending) > 0 {
		line := bytes.TrimSuffix(s.pending, []byte{'\r'})
		s.pending = nil
		if err := s.line(line); err != nil {
			return err
		}
	}
	if err := s.line(nil); err != nil {
		return err
	}
	// The host chat stream ends at EOF rather than an HTTP-layer [DONE].
	// Wait for EOF so a separate final usage chunk is not discarded.
	if s.chatFinished {
		s.terminal = true
	}
	return nil
}

func (s *streamCapture) event(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		s.terminal = true
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return errors.New("invalid ignition stream JSON")
	}
	if v["error"] != nil {
		return streamError(v)
	}
	typ, _ := v["type"].(string)
	switch typ {
	case "error", "response.failed":
		return streamError(v)
	case "response.completed":
		s.terminal = true
		s.recordUsage(object(v["response"])["usage"], true)
	case "response.incomplete":
		s.recordUsage(object(v["response"])["usage"], false)
		return errors.New("ignition provider stream incomplete")
	case "response.output_text.delta":
		s.text(str(v["delta"]))
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		s.text(str(v["delta"]))
	case "content_block_delta":
		d := object(v["delta"])
		switch str(d["type"]) {
		case "text_delta":
			s.text(str(d["text"]))
		case "thinking_delta":
			s.text(str(d["thinking"]))
		case "input_json_delta":
			s.text(str(d["partial_json"]))
		}
	case "message_start":
		s.recordUsage(object(v["message"])["usage"], false)
	case "message_delta":
		s.recordUsage(v["usage"], true)
	case "message_stop":
		s.terminal = true
	}
	// OpenAI chat completion chunks (also used by Antigravity).
	if choices, ok := v["choices"].([]any); ok {
		for _, c := range choices {
			if str(object(c)["finish_reason"]) != "" {
				s.chatFinished = true
			}
			d := object(object(c)["delta"])
			s.text(str(d["content"]))
			for _, key := range []string{"reasoning", "reasoning_content"} {
				s.text(str(d[key]))
			}
			calls, _ := d["tool_calls"].([]any)
			for _, call := range calls {
				s.text(str(object(object(call)["function"])["arguments"]))
			}
			s.text(str(object(d["function_call"])["arguments"]))
		}
		// CPA can synthesize missing Chat counters as zero or translate token
		// categories. Field presence cannot establish upstream completeness.
		s.recordUsage(v["usage"], false)
	}
	return nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string            { s, _ := v.(string); return s }

// completeUsage describes final host-protocol counters for native ignition
// routes, not independently verified upstream accounting. Translated routes
// remain partial even when their host payload contains both counters.
func (s *streamCapture) completeUsage(provider, protocol string) bool {
	native := (provider == "claude" && protocol == "claude") ||
		(provider == "codex" && protocol == "openai-response")
	_, input := s.usage["input_tokens"]
	_, output := s.usage["output_tokens"]
	return native && s.usageFinal && input && output
}

// recordUsage keeps allowlisted counters supplied by the CPA host, which may
// include translated or synthesized values. The plugin does not infer totals
// or turn absent values into zero.
func (s *streamCapture) recordUsage(v any, final bool) {
	m := object(v)
	observed := false
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "prompt_tokens", "completion_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if n, ok := m[key].(float64); ok && n >= 0 {
			if s.usage == nil {
				s.usage = map[string]any{}
			}
			s.usage[key] = n
			observed = true
		}
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
		details := map[string]any{}
		for _, field := range []string{"cached_tokens", "reasoning_tokens", "audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"} {
			if n, ok := object(m[key])[field].(float64); ok && n >= 0 {
				details[field] = n
			}
		}
		if len(details) > 0 {
			if s.usage == nil {
				s.usage = map[string]any{}
			}
			s.usage[key] = details
			observed = true
		}
	}
	if observed && final {
		s.usageFinal = true
	}
}
