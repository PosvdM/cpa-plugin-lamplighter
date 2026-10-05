package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type nopHost struct{}

func (nopHost) AuthList() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }
func (nopHost) AuthGet(string) (json.RawMessage, error)          { return nil, nil }
func (nopHost) ModelExecute(pluginapi.HostModelExecutionRequest) (pluginapi.HostModelExecutionResponse, error) {
	return pluginapi.HostModelExecutionResponse{}, nil
}
func (nopHost) HTTPDo(context.Context, pluginapi.HTTPRequest, time.Duration) (pluginapi.HTTPResponse, error) {
	return pluginapi.HTTPResponse{}, nil
}
func (nopHost) Log(string, string, map[string]any) {}

var _ host.Host = nopHost{}

func result(t *testing.T, raw []byte) json.RawMessage {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("bad envelope %s", raw)
	}
	return env.Result
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\nbark_url: https://api.day.app/k\n"), "schema_version": 6})
	var reg struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct {
			Name    string
			Version string
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	json.Unmarshal(result(t, p.Handle("plugin.register", req)), &reg)
	if reg.SchemaVersion != 6 || reg.Metadata.Name != "Lamplighter" || reg.Metadata.Version != "1.2.3" {
		t.Fatalf("registration %+v", reg)
	}
	if !reg.Capabilities["management_api"] || !reg.Capabilities["usage_plugin"] {
		t.Fatalf("capabilities %+v", reg.Capabilities)
	}
}

func TestManagementRoutesAndPage(t *testing.T) {
	p := New(nopHost{}, "1.2.3", t.TempDir(), "")
	defer p.Shutdown()
	var reg struct {
		Routes    []struct{ Method, Path string } `json:"routes"`
		Resources []struct{ Path, Menu string }   `json:"resources"`
	}
	json.Unmarshal(result(t, p.Handle("management.register", nil)), &reg)
	if len(reg.Routes) != 6 || reg.Resources[0].Path != "/page" || reg.Resources[0].Menu != "Lamplighter" {
		t.Fatalf("registration %+v", reg)
	}

	req, _ := json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/lamplighter/page"})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), "<title>Lamplighter</title>") || strings.Contains(string(resp.Body), "/*JS*/") {
		t.Fatalf("page response %d", resp.StatusCode)
	}

	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/lamplighter/status"})
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	var status struct {
		Version string
		Config  struct {
			BarkURL string `json:"bark_url"`
		} `json:"config"`
	}
	json.Unmarshal(resp.Body, &status)
	if resp.StatusCode != 200 || status.Version != "1.2.3" {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}

	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/lamplighter/refresh"})
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	if resp.StatusCode != 503 {
		t.Fatalf("actions before the loop starts must answer 503, got %d", resp.StatusCode)
	}
}

func TestStatusHidesSecrets(t *testing.T) {
	p := New(nopHost{}, "1", t.TempDir(), "")
	defer p.Shutdown()
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte("bark_url: https://api.day.app/secret\nmodels_api_key: sk-secret\n")})
	p.Handle("plugin.register", req)
	req, _ = json.Marshal(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/lamplighter/status"})
	var resp pluginapi.ManagementResponse
	json.Unmarshal(result(t, p.Handle("management.handle", req)), &resp)
	if strings.Contains(string(resp.Body), "secret") {
		t.Fatalf("status leaks secrets: %s", resp.Body)
	}
}

func TestUnknownMethodAndUsage(t *testing.T) {
	p := New(nopHost{}, "1", t.TempDir(), "")
	defer p.Shutdown()
	if !strings.Contains(string(p.Handle("nope", nil)), "unknown_method") {
		t.Fatal("unknown methods must return an error envelope")
	}
	record, _ := json.Marshal(pluginapi.UsageRecord{Provider: "claude", AuthIndex: "1"})
	result(t, p.Handle("usage.handle", record))
}
