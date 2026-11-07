package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMCPAndAccountOrderConfig(t *testing.T) {
	defaults, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.MCPEnabled || defaults.MCPListen != "127.0.0.1:8318" || defaults.MCPKey != "" || defaults.MCPKeySet {
		t.Fatalf("MCP defaults: %+v", defaults)
	}
	cfg, err := Parse([]byte("mcp_enabled: true\nmcp_listen: ' 127.0.0.1:9000 '\nmcp_key: ' secret '\nmcp_key_set: true\naccount_order: [b.json, a.json]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPEnabled || cfg.MCPListen != "127.0.0.1:9000" || cfg.MCPKey != "secret" || cfg.MCPKeySet || !slices.Equal(cfg.AccountOrder, []string{"b.json", "a.json"}) {
		t.Fatalf("parsed config: %+v", cfg)
	}
	cfg.MCPKeySet = true
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mcp_key_set") || !strings.Contains(string(raw), "mcp_key: secret") {
		t.Fatalf("persisted config: %s", raw)
	}
	roundTrip, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !roundTrip.MCPEnabled || roundTrip.MCPKey != "secret" || roundTrip.MCPKeySet || !slices.Equal(roundTrip.AccountOrder, cfg.AccountOrder) {
		t.Fatalf("YAML round trip: %+v", roundTrip)
	}
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["mcp_enabled"] != true || decoded["mcp_listen"] != cfg.MCPListen || decoded["mcp_key"] != "secret" || decoded["mcp_key_set"] != true {
		t.Fatalf("JSON fields: %s", raw)
	}
	if order, ok := decoded["account_order"].([]any); !ok || len(order) != 2 || order[0] != "b.json" || order[1] != "a.json" {
		t.Fatalf("JSON account_order: %s", raw)
	}
	cfg.MCPKey = ""
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Decode into a fresh map because omitted keys do not clear a reused map.
	decoded = nil
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded["mcp_key"]; exists {
		t.Fatalf("empty MCP key must be omitted: %s", raw)
	}
	blank, err := Parse([]byte("mcp_listen: ' '\n"))
	if err != nil || blank.MCPListen != defaults.MCPListen {
		t.Fatalf("blank MCP listen: %q, %v", blank.MCPListen, err)
	}
}

func TestThresholdsAreOrderedAndFinite(t *testing.T) {
	var output bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(writer) })
	for _, tc := range []struct {
		name, notice, low, critical string
		want                        [3]float64
		logged                      bool
	}{
		{"valid", "70", "30", "5", [3]float64{70, 30, 5}, false},
		{"bounds", "120", "-10", "-20", [3]float64{100, 0, 0}, true},
		{"crossed", "10", "80", "90", [3]float64{10, 10, 10}, true},
		{"negative notice", "-1", "20", "10", [3]float64{0, 0, 0}, true},
		{"positive infinity", ".inf", ".inf", ".inf", [3]float64{100, 100, 100}, true},
		{"negative infinity", "-.inf", "-.inf", "-.inf", [3]float64{0, 0, 0}, true},
		{"not a number", ".nan", ".nan", ".nan", [3]float64{50, 20, 10}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output.Reset()
			raw := fmt.Sprintf("notice_threshold: %s\nlow_threshold: %s\ncritical_threshold: %s\n", tc.notice, tc.low, tc.critical)
			cfg, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := [3]float64{cfg.NoticeThreshold, cfg.LowThreshold, cfg.CriticalThreshold}; got != tc.want {
				t.Fatalf("thresholds %v, want %v", got, tc.want)
			}
			if got := output.Len() > 0; got != tc.logged {
				t.Fatalf("logged=%v, want %v: %s", got, tc.logged, output.String())
			}
		})
	}
}
