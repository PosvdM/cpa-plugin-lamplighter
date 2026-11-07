package engine

import (
	"net"
	"testing"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/mcp"
)

func TestMCPBindFailureAndDisableStillAcknowledgeToken(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	e := New(Options{})
	server := mcp.New(NewMCPBackend(e), "test")
	defer server.Close()
	cfg := config.Default()
	cfg.MCPEnabled, cfg.MCPListen, cfg.MCPKey, cfg.ConfigToken = true, occupied.Addr().String(), "key", "bind-failed"
	rev := e.Configure(cfg, nil)
	server.Configure(cfg.MCPEnabled, cfg.MCPListen, cfg.MCPKey)
	if state := server.State(); state.Running || state.Error == "" {
		t.Fatalf("expected failed listener: %+v", state)
	}
	e.MCPConfigured(rev)
	e.applyPendingConfig(config.Default())
	if e.Status().ConfigAppliedToken != cfg.ConfigToken {
		t.Fatal("a recorded bind failure must acknowledge configuration execution")
	}
	applied := cfg
	cfg.MCPEnabled, cfg.ConfigToken = false, "disabled"
	rev = e.Configure(cfg, nil)
	server.Configure(cfg.MCPEnabled, cfg.MCPListen, cfg.MCPKey)
	e.MCPConfigured(rev)
	if e.Status().ConfigAppliedToken != "bind-failed" {
		t.Fatal("disabled MCP config must still wait for engine")
	}
	e.applyPendingConfig(applied)
	if e.Status().ConfigAppliedToken != "disabled" || server.State().Running || server.State().Error != "" {
		t.Fatal("disabled config was not fully acknowledged")
	}
}
