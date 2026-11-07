package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu    sync.Mutex
	calls []string
}

func (b *fakeBackend) CallTool(_ context.Context, name string, args map[string]string) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, name)
	return map[string]any{"tool": name, "args": args}, nil
}
func post(t *testing.T, client *http.Client, url, method, body, key, origin, version string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+key)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
	}
	return resp.StatusCode, raw
}
func TestProtocolRoundTrip(t *testing.T) {
	for _, version := range []string{"2025-03-26", "2025-06-18"} {
		t.Run(version, func(t *testing.T) {
			backend := &fakeBackend{}
			s := New(backend, "0.8.0")
			s.key = "test-key"
			defer s.Close()
			server := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			url := server.URL + "/mcp"
			call := func(method, body, key, origin string) (int, []byte) {
				return post(t, client, url, method, body, key, origin, version)
			}
			status, raw := call("POST", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, "test-key", "")
			if status != 200 || !bytes.Contains(raw, []byte(`"protocolVersion":"`+version+`"`)) {
				t.Fatalf("initialize %d %s", status, raw)
			}
			t.Logf("initialize: %d version=%s", status, version)
			status, raw = call("POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "test-key", "")
			if status != 202 || len(raw) != 0 {
				t.Fatalf("initialized: %d %s", status, raw)
			}
			t.Log("notifications/initialized: 202, empty body")
			status, raw = call("POST", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "test-key", "")
			var listed struct {
				Result struct {
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal(raw, &listed) != nil || len(listed.Result.Tools) != 4 || status != 200 {
				t.Fatalf("tools/list: %d %s", status, raw)
			}
			t.Log("tools/list: 200, four tools")
			for _, tool := range listed.Result.Tools {
				args := "{}"
				if tool.Name == "get_quota_events" {
					args = `{"limit":5}`
				}
				status, raw = call("POST", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+tool.Name+`","arguments":`+args+`}}`, "test-key", "")
				if status != 200 || !bytes.Contains(raw, []byte(`"content"`)) || bytes.Contains(raw, []byte(`"error"`)) {
					t.Fatalf("tools/call %s: %d %s", tool.Name, status, raw)
				}
				t.Logf("tools/call %s: 200, text JSON result", tool.Name)
			}
			for _, tc := range []struct {
				name, method, body, key, origin string
				status                          int
			}{
				{"authentication failure", "POST", `{}`, "wrong", "", 401},
				{"GET unsupported", "GET", "", "test-key", "", 405},
				{"foreign Origin", "POST", `{}`, "test-key", "https://attacker.example", 403},
				{"DNS rebinding Origin", "POST", `{}`, "test-key", "http://localhost.attacker.example", 403},
				{"local Origin", "POST", `{"jsonrpc":"2.0","id":4,"method":"ping"}`, "test-key", "http://127.0.0.1:3000", 200},
			} {
				status, raw = call(tc.method, tc.body, tc.key, tc.origin)
				if status != tc.status {
					t.Fatalf("%s: %d %s", tc.name, status, raw)
				}
				t.Logf("%s: %d", tc.name, status)
			}
			for _, tc := range []struct {
				body string
				code string
			}{
				{`{`, "-32700"}, {`[]`, "-32600"}, {`{"jsonrpc":"2.0","id":1,"method":"missing"}`, "-32601"},
				{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_quotas","arguments":{"unknown":"x"}}}`, "-32602"},
				{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_quota_events","arguments":{"limit":"5"}}}`, "-32602"},
			} {
				_, raw = call("POST", tc.body, "test-key", "")
				if !bytes.Contains(raw, []byte(tc.code)) {
					t.Fatalf("want %s: %s", tc.code, raw)
				}
			}
			status, raw = call("POST", `[{"jsonrpc":"2.0","id":7,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`, "test-key", "")
			if version == "2025-03-26" {
				if raw[0] != '[' || !bytes.Contains(raw, []byte(`"id":7`)) {
					t.Fatalf("batch: %s", raw)
				}
			} else if !bytes.Contains(raw, []byte("-32600")) {
				t.Fatalf("new version batch: %s", raw)
			}
		})
	}
}

func TestListenerReconfigureAndClose(t *testing.T) {
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.Addr().String()
	s := New(&fakeBackend{}, "test")
	defer s.Close()
	s.Configure(true, addr, "")
	if state := s.State(); state.Running || state.Error == "" || !state.Enabled {
		t.Fatalf("missing key %+v", state)
	}
	s.Configure(true, addr, "first")
	if state := s.State(); state.Running || state.Error == "" {
		t.Fatalf("bind failure %+v", state)
	}
	// A replacement library can register before the old one is quiesced.
	reserve.Close()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !s.State().Running {
		select {
		case <-deadline.C:
			t.Fatal("listener retry timed out")
		case <-ticker.C:
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if code, _ := post(t, client, "http://"+addr+"/mcp", "POST", body, "first", "", ""); code != 200 {
		t.Fatal(code)
	}
	s.Configure(true, addr, "second")
	if code, _ := post(t, client, "http://"+addr+"/mcp", "POST", body, "first", "", ""); code != 401 {
		t.Fatal(code)
	}
	if code, _ := post(t, client, "http://"+addr+"/mcp", "POST", body, "second", "", ""); code != 200 {
		t.Fatal(code)
	}
	s.Configure(false, addr, "second")
	if state := s.State(); state.Running || state.Error != "" {
		t.Fatalf("disable %+v", state)
	}
	rebound, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("disabled listener retained port: %v", err)
	}
	rebound.Close()
	s.Configure(true, addr, "second")
	s.Close()
	rebound, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("closed listener retained port: %v", err)
	}
	rebound.Close()
	s.Configure(true, addr, "third")
	if s.State().Running {
		t.Fatal("closed server restarted")
	}
}

type blockingBackend struct {
	started  chan struct{}
	finished chan struct{}
}

func (b *blockingBackend) CallTool(ctx context.Context, _ string, _ map[string]string) (any, error) {
	close(b.started)
	<-ctx.Done()
	close(b.finished)
	return nil, ctx.Err()
}
func TestCloseCancelsAndDrainsActiveRequests(t *testing.T) {
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.Addr().String()
	reserve.Close()
	backend := &blockingBackend{started: make(chan struct{}), finished: make(chan struct{})}
	s := New(backend, "test")
	defer s.Close()
	s.Configure(true, addr, "key")
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", "http://"+addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"refresh_quotas"}}`))
		req.Header.Set("Authorization", "Bearer key")
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, _ := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel and drain handlers")
	}
	select {
	case <-backend.finished:
	default:
		t.Fatal("Close returned with live backend handler")
	}
	<-done
}

func TestLocalOrigin(t *testing.T) {
	for _, origin := range []string{"http://localhost", "https://127.0.0.1:99", "http://[::1]:8318"} {
		if !localOrigin(origin) {
			t.Fatal(origin)
		}
	}
	for _, origin := range []string{"null", "file:///tmp/x", "http://127.0.0.1.evil", "https://evil@localhost", "http://localhost/path", "http://localhost?evil", "http://192.168.1.1"} {
		if localOrigin(origin) {
			t.Fatal(origin)
		}
	}
}
