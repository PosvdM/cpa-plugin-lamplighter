// Package plugin implements the CPA plugin RPC methods on top of the engine.
// Package main forwards every native call to Handle.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/engine"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/host"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/web"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	repository     = "https://github.com/PosvdM/cpa-plugin-lamplighter"
	apiBase        = "/v0/management/" + config.PluginID
	pagePath       = "/page"
	stopTimeout    = 10 * time.Second
	defaultHistory = 24 * time.Hour
)

// Plugin handles the RPC methods of one loaded plugin instance.
type Plugin struct {
	version string
	engine  *engine.Engine
	once    sync.Once
}

// New creates the plugin. dataDir is the default data directory derived from
// the plugin file location; hostConfigPath is the CPA config file.
func New(h host.Host, version, dataDir, hostConfigPath string) *Plugin {
	return &Plugin{
		version: version,
		engine: engine.New(engine.Options{
			Host:           h,
			Version:        version,
			DataDir:        dataDir,
			HostConfigPath: hostConfigPath,
		}),
	}
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ok(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

// ErrorEnvelope encodes a failed RPC result.
func ErrorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

// Handle runs one RPC method and returns the response envelope. A panic is
// turned into an error envelope so that it cannot crash CPA.
func (p *Plugin) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			out = ErrorEnvelope("plugin_panic", fmt.Sprint(r))
		}
	}()
	result, err := p.handle(method, request)
	if err != nil {
		return ErrorEnvelope("plugin_error", err.Error())
	}
	return result
}

// Shutdown stops the engine.
func (p *Plugin) Shutdown() {
	p.engine.Stop(stopTimeout)
}

func (p *Plugin) handle(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := config.Parse(req.ConfigYAML)
		p.engine.Configure(cfg, err)
		p.once.Do(p.engine.Start)
		return ok(p.registration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		p.engine.Stop(stopTimeout)
		return ok(struct{}{})
	case pluginabi.MethodManagementRegister:
		return ok(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return ok(p.handleManagement(req))
	case pluginabi.MethodUsageHandle:
		var record pluginapi.UsageRecord
		if err := json.Unmarshal(request, &record); err == nil {
			p.engine.ObserveUsage(record)
		}
		return ok(struct{}{})
	}
	return ErrorEnvelope("unknown_method", "unknown method: "+method), nil
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]bool    `json:"capabilities"`
}

func field(name string, kind pluginapi.ConfigFieldType, description string) pluginapi.ConfigField {
	return pluginapi.ConfigField{Name: name, Type: kind, Description: description}
}

func (p *Plugin) registration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Lamplighter",
			Version:          p.version,
			Author:           "PosvdM",
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				field("bark_url", pluginapi.ConfigFieldTypeString, "Bark 推送地址，到 device key 为止；为空时不推送"),
				field("bark_group", pluginapi.ConfigFieldTypeString, "Bark 通知分组"),
				field("models_api_key", pluginapi.ConfigFieldTypeString, "读取 /v1/models 用的专用 CPA API key"),
				field("cpa_base_url", pluginapi.ConfigFieldTypeString, "插件访问 CPA 自身的地址"),
				field("notice_threshold", pluginapi.ConfigFieldTypeNumber, "第一档提醒阈值（剩余百分比）"),
				field("low_threshold", pluginapi.ConfigFieldTypeNumber, "第二档提醒阈值（剩余百分比）"),
				field("critical_threshold", pluginapi.ConfigFieldTypeNumber, "第三档提醒阈值（剩余百分比）"),
				field("notify_recovery", pluginapi.ConfigFieldTypeBoolean, "额度恢复时是否通知"),
				field("notify_reset_reminders", pluginapi.ConfigFieldTypeBoolean, "重置前是否提醒"),
				field("timezone_offset_hours", pluginapi.ConfigFieldTypeNumber, "显示时间和点火时段使用的 UTC 偏移"),
				field("poll_interval_seconds", pluginapi.ConfigFieldTypeInteger, "主动查询间隔，对齐到整点"),
				field("passive_skip_seconds", pluginapi.ConfigFieldTypeInteger, "整点前多少秒内有被动数据时跳过主动查询，0 为不跳过"),
				field("passive_skip_max_minutes", pluginapi.ConfigFieldTypeInteger, "同一账号最多连续跳过多少分钟"),
				field("ignition", pluginapi.ConfigFieldTypeObject, "点火开关、每日时段和失败保护"),
				field("providers", pluginapi.ConfigFieldTypeObject, "codex、claude、antigravity 的监控和点火设置"),
				field("codex_reset_updates", pluginapi.ConfigFieldTypeObject, "转发 Did Codex Reset 的重置信号"),
				field("history_retention_days", pluginapi.ConfigFieldTypeInteger, "额度历史保留天数"),
				field("data_dir", pluginapi.ConfigFieldTypeString, "状态和历史目录，默认 plugins/data/lamplighter"),
			},
		},
		Capabilities: map[string]bool{
			"management_api": true,
			"usage_plugin":   true,
		},
	}
}

type route struct {
	Method      string
	Path        string
	Description string
}

type resource struct {
	Path        string
	Menu        string
	Description string
}

func managementRegistration() map[string]any {
	return map[string]any{
		"routes": []route{
			{Method: http.MethodGet, Path: apiBase + "/status", Description: "Lamplighter status"},
			{Method: http.MethodGet, Path: apiBase + "/history", Description: "Lamplighter quota history"},
			{Method: http.MethodPost, Path: apiBase + "/refresh", Description: "Query quota now"},
			{Method: http.MethodPost, Path: apiBase + "/ignite", Description: "Ignite one quota window now"},
			{Method: http.MethodPost, Path: apiBase + "/test-bark", Description: "Send a Bark test notification"},
		},
		"resources": []resource{
			{Path: pagePath, Menu: "Lamplighter", Description: "额度监控、自动点火和额度图表"},
		},
	}
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		raw = []byte(`{"error":"encode response failed"}`)
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}},
		Body:       raw,
	}
}

func errorResponse(status int, err error) pluginapi.ManagementResponse {
	return jsonResponse(status, map[string]string{"error": err.Error()})
}

func (p *Plugin) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	path := strings.TrimRight(req.Path, "/")
	if strings.HasSuffix(path, "/"+config.PluginID+pagePath) && strings.Contains(path, "/v0/resource/plugins/") {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":  []string{"text/html; charset=utf-8"},
				"Cache-Control": []string{"no-store"},
			},
			Body: web.Page(),
		}
	}
	var body struct {
		AuthIndex string `json:"auth_index"`
		Target    string `json:"target"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	switch strings.ToUpper(req.Method) + " " + path {
	case "GET " + apiBase + "/status":
		return jsonResponse(http.StatusOK, p.engine.Status())
	case "GET " + apiBase + "/history":
		span := parseRange(req.Query)
		resp, err := p.engine.History(span)
		if err != nil {
			return errorResponse(http.StatusInternalServerError, err)
		}
		return jsonResponse(http.StatusOK, resp)
	case "POST " + apiBase + "/refresh":
		return actionResponse(p.engine.Refresh(strings.TrimSpace(body.AuthIndex)), p)
	case "POST " + apiBase + "/ignite":
		if strings.TrimSpace(body.Target) == "" {
			return errorResponse(http.StatusBadRequest, errors.New("缺少 target"))
		}
		return actionResponse(p.engine.Ignite(strings.TrimSpace(body.Target)), p)
	case "POST " + apiBase + "/test-bark":
		return actionResponse(p.engine.TestBark(), p)
	}
	return errorResponse(http.StatusNotFound, fmt.Errorf("unknown route %s %s", req.Method, req.Path))
}

func actionResponse(err error, p *Plugin) pluginapi.ManagementResponse {
	if errors.Is(err, engine.ErrNotRunning) {
		return errorResponse(http.StatusServiceUnavailable, err)
	}
	if err != nil {
		return jsonResponse(http.StatusOK, map[string]any{"ok": false, "error": err.Error(), "status": p.engine.Status()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"ok": true, "status": p.engine.Status()})
}

func parseRange(query url.Values) time.Duration {
	switch query.Get("range") {
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	}
	return defaultHistory
}
