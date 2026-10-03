// Package host wraps the CPA host callbacks used by Lamplighter.
//
// The native ABI exchanges JSON envelopes. Package main supplies a Caller that
// crosses the C boundary; everything above it works with Go values so that the
// rest of the plugin can be tested with a fake host.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Caller sends one host callback and returns the raw envelope bytes.
type Caller func(method string, payload []byte) ([]byte, error)

// Error is a failed host callback. Status carries the HTTP status that CPA
// attached to the error, or 0 when it did not set one.
type Error struct {
	Method  string
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var hostErr *Error
	if errors.As(err, &hostErr) {
		return hostErr.Status
	}
	return 0
}

// Host is the set of host callbacks Lamplighter uses.
type Host interface {
	AuthList() ([]pluginapi.HostAuthFileEntry, error)
	AuthGet(authIndex string) (json.RawMessage, error)
	ModelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error)
	HTTPDo(ctx context.Context, req pluginapi.HTTPRequest, timeout time.Duration) (pluginapi.HTTPResponse, error)
	Log(level, message string, fields map[string]any)
}

// RPC implements Host on top of a Caller.
type RPC struct {
	Call Caller
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status,omitempty"`
	} `json:"error,omitempty"`
}

func (r RPC) call(method string, payload any, out any) error {
	if r.Call == nil {
		return &Error{Method: method, Code: "host_unavailable", Message: "host API is not initialized"}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := r.Call(method, raw)
	if err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: err.Error()}
	}
	if len(resp) == 0 {
		return &Error{Method: method, Code: "host_call_failed", Message: "empty response"}
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: "invalid envelope: " + err.Error()}
	}
	if !env.OK {
		hostErr := &Error{Method: method, Code: "host_call_failed", Message: "host callback failed"}
		if env.Error != nil {
			hostErr.Code = env.Error.Code
			hostErr.Message = env.Error.Message
			hostErr.Status = env.Error.HTTPStatus
		}
		return hostErr
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &Error{Method: method, Code: "host_call_failed", Message: "invalid result: " + err.Error()}
	}
	return nil
}

// AuthList returns every credential known to CPA.
func (r RPC) AuthList() ([]pluginapi.HostAuthFileEntry, error) {
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := r.call(pluginabi.MethodHostAuthList, map[string]any{}, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// AuthGet returns the credential file JSON for authIndex.
func (r RPC) AuthGet(authIndex string) (json.RawMessage, error) {
	var resp pluginapi.HostAuthGetResponse
	if err := r.call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex}, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// ModelExecute runs one non-streaming request through CPA's model executor.
func (r RPC) ModelExecute(req pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	var resp pluginapi.HostModelExecutionResponse
	err := r.call(pluginabi.MethodHostModelExecute, req, &resp)
	return resp, err
}

type httpRequest struct {
	OperationID string      `json:"operation_id,omitempty"`
	Method      string      `json:"method,omitempty"`
	URL         string      `json:"url,omitempty"`
	Headers     http.Header `json:"headers,omitempty"`
	Body        []byte      `json:"body,omitempty"`
}

// HTTPDo sends req through CPA's HTTP client, which applies the global
// proxy-url, then environment proxies, then a direct connection. host.http.do
// has no timeout of its own, so the request runs inside a host HTTP operation
// that is cancelled when timeout or ctx expires.
func (r RPC) HTTPDo(ctx context.Context, req pluginapi.HTTPRequest, timeout time.Duration) (pluginapi.HTTPResponse, error) {
	var opened struct {
		OperationID string `json:"operation_id"`
	}
	if err := r.call(pluginabi.MethodHostHTTPOperationOpen, map[string]any{}, &opened); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	operationID := strings.TrimSpace(opened.OperationID)
	if operationID == "" {
		return pluginapi.HTTPResponse{}, &Error{Method: pluginabi.MethodHostHTTPOperationOpen, Code: "host_call_failed", Message: "empty operation id"}
	}

	done := make(chan struct{})
	defer close(done)
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	go func() {
		defer func() { _ = recover() }()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
		case <-ctx.Done():
		}
		_ = r.call(pluginabi.MethodHostHTTPCancel, map[string]any{"operation_id": operationID}, nil)
	}()

	var resp pluginapi.HTTPResponse
	err := r.call(pluginabi.MethodHostHTTPDo, httpRequest{
		OperationID: operationID,
		Method:      req.Method,
		URL:         req.URL,
		Headers:     req.Headers,
		Body:        req.Body,
	}, &resp)
	if err != nil {
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		default:
		}
		return resp, err
	}
	return resp, nil
}

// Log writes one entry to the CPA log.
func (r RPC) Log(level, message string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["plugin"] = "lamplighter"
	_ = r.call(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	}, nil)
}
