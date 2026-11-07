package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestMCPLifecycleAndEngineProtocol(t *testing.T) {
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserve.Addr().String()
	reserve.Close()
	p := New(nopHost{}, "0.8.0", t.TempDir(), "")
	defer p.Shutdown()
	configure := func(yaml string) {
		t.Helper()
		req, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml)})
		result(t, p.Handle("plugin.reconfigure", req))
	}
	configure(fmt.Sprintf("mcp_enabled: true\nmcp_listen: %s\n", addr))
	if state := p.status().MCP; state.Running || state.Error == "" {
		t.Fatalf("missing key %+v", state)
	}
	configure(fmt.Sprintf("mcp_enabled: true\nmcp_listen: %s\nmcp_key: integration-key\n", addr))
	if state := p.status().MCP; !state.Running || state.Error != "" || state.Path != "/mcp" {
		t.Fatalf("running state %+v", state)
	}
	raw, _ := json.Marshal(p.status())
	if strings.Contains(string(raw), "integration-key") || !strings.Contains(string(raw), `"mcp_key_set":true`) {
		t.Fatalf("status exposed key %s", raw)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(body string) (int, string) {
		req, _ := http.NewRequest("POST", "http://"+addr+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer integration-key")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(raw)
	}
	code, body := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if code != 200 || !strings.Contains(body, `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize %d %s", code, body)
	}
	code, body = call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != 202 || body != "" {
		t.Fatalf("initialized %d %s", code, body)
	}
	code, body = call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if code != 200 || !strings.Contains(body, "refresh_quotas") {
		t.Fatalf("tools/list %d %s", code, body)
	}
	for _, tool := range []string{"list_quotas", "get_quota_history", "refresh_quotas", "get_quota_events"} {
		code, body = call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`)
		if code != 200 || !strings.Contains(body, `"content"`) || strings.Contains(body, `"error":`) {
			t.Fatalf("%s %d %s", tool, code, body)
		}
		if tool == "refresh_quotas" && !strings.Contains(body, `"isError":true`) {
			t.Fatalf("startup refresh should report not running: %s", body)
		}
		t.Logf("real engine %s: %d (refresh during startup returns bounded tool error)", tool, code)
	}
	// Invalid YAML must preserve both the effective config and the old listener.
	configure("mcp_enabled: [invalid\n")
	if state := p.status(); !state.MCP.Running || state.ConfigError == "" {
		t.Fatalf("invalid reconfigure %+v", state.MCP)
	}
	request, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/lamplighter/status"})
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(result(t, p.Handle("management.handle", request)), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response.Body), `"mcp"`) {
		t.Fatal("management status omitted listener")
	}
	result(t, p.Handle("plugin.quiesce", nil))
	rebound, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("quiesce retained listener: %v", err)
	}
	rebound.Close()
	configure(fmt.Sprintf("mcp_enabled: true\nmcp_listen: %s\nmcp_key: integration-key\n", addr))
	if p.status().MCP.Running {
		t.Fatal("quiesced instance restarted")
	}
}
