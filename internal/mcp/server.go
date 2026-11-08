// Package mcp serves a stateless, JSON-only MCP Streamable HTTP endpoint.
package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Backend returns only model-safe DTOs, never management responses.
type Backend interface {
	CallTool(context.Context, string, map[string]string) (any, error)
}

// State describes the listener without its authentication key.
type State struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
	Running bool   `json:"running"`
	Error   string `json:"error"`
	Path    string `json:"path"`
}

// Server owns the listener and all HTTP handler lifetimes.
type Server struct {
	mu         sync.Mutex
	backend    Backend
	version    string
	state      State
	key        string
	http       *http.Server
	done       chan struct{}
	handlers   sync.WaitGroup
	serves     sync.WaitGroup
	stopped    bool
	retry      *time.Timer
	retries    sync.WaitGroup
	generation uint64

	requests       map[context.Context]context.CancelFunc
	toolSlots      chan struct{}
	queryTimeout   time.Duration
	refreshTimeout time.Duration
}

func New(backend Backend, version string) *Server {
	return &Server{
		backend: backend, version: version,
		state:          State{Listen: "127.0.0.1:8318", Path: "/mcp"},
		requests:       make(map[context.Context]context.CancelFunc),
		toolSlots:      make(chan struct{}, 4),
		queryTimeout:   10 * time.Second,
		refreshTimeout: 180 * time.Second,
	}
}

func (s *Server) State() State { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

// Configure closes the old listener before binding the replacement. A failed
// bind is retried every two seconds, including during hot-reload handover.
func (s *Server) Configure(enabled bool, listen, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if s.state.Enabled == enabled && s.state.Listen == listen && s.key == key && s.state.Running {
		return
	}
	s.closeLocked()
	s.state = State{Enabled: enabled, Listen: listen, Path: "/mcp"}
	s.key = key
	if !enabled {
		return
	}
	if key == "" {
		s.state.Error = "MCP key is empty"
		return
	}
	s.startLocked()
}

func (s *Server) startLocked() {
	listener, err := net.Listen("tcp", s.state.Listen)
	if err != nil {
		s.state.Error = "Cannot listen on MCP address: " + err.Error()
		generation := s.generation
		s.retries.Add(1)
		s.retry = time.AfterFunc(2*time.Second, func() {
			defer s.retries.Done()
			s.mu.Lock()
			defer s.mu.Unlock()
			if generation != s.generation {
				return
			}
			s.retry = nil
			if !s.stopped && s.state.Enabled && s.key != "" && !s.state.Running {
				s.startLocked()
			}
		})
		return
	}
	s.state.Error = ""
	server := &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 190 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	s.http, s.done, s.state.Running = server, make(chan struct{}), true
	done := s.done
	s.serves.Add(1)
	go func() {
		defer s.serves.Done()
		err := func() (err error) {
			// net/http recovers handler panics; protect the listener goroutine too.
			defer func() {
				if recover() != nil {
					_ = server.Close()
					err = errors.New("MCP listener panic")
				}
			}()
			return server.Serve(listener)
		}()
		close(done)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.http == server {
			s.state.Running = false
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.state.Error = "MCP listener stopped: " + err.Error()
			}
		}
	}()
}

func (s *Server) closeLocked() {
	s.generation++
	for _, cancel := range s.requests {
		cancel()
	}
	if s.retry != nil {
		if s.retry.Stop() {
			s.retries.Done()
		}
		s.retry = nil
	}
	if s.http != nil {
		_ = s.http.Close()
		<-s.done
		s.http = nil
	}
	s.state.Running = false
}

// Close releases the port and waits for canceled requests before plugin unload.
func (s *Server) Close() {
	s.mu.Lock()
	s.stopped = true
	s.closeLocked()
	s.mu.Unlock()
	s.handlers.Wait()
	s.retries.Wait()
	s.serves.Wait()
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, failure *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if failure != nil {
		out["error"] = failure
	} else {
		out["result"] = result
	}
	_ = json.NewEncoder(w).Encode(out)
}
func supported(v string) bool { return v == "2025-03-26" || v == "2025-06-18" }
func localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// 请求登记与停止检查共用互斥锁，防止 Close 开始等待后登记新请求。
	s.handlers.Add(1)
	ctx, cancel := context.WithCancel(r.Context())
	s.requests[ctx] = cancel
	key := s.key
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.requests, ctx)
		s.mu.Unlock()
		s.handlers.Done()
	}()
	r = r.WithContext(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/mcp" {
		w.WriteHeader(404)
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || !localOrigin(origins[0])) {
		w.WriteHeader(403)
		return
	}
	expected := sha256.Sum256([]byte("Bearer " + key))
	actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if key == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(401)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(405)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supported(v) {
		w.WriteHeader(400)
		reply(w, nil, nil, &rpcError{-32600, "Unsupported MCP protocol version"})
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		w.WriteHeader(415)
		return
	}
	accept := r.Header.Get("Accept")
	if accept != "" && !strings.Contains(accept, "application/json") && !strings.Contains(accept, "*/*") {
		w.WriteHeader(406)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
		if r.Header.Get("MCP-Protocol-Version") == "2025-06-18" {
			w.WriteHeader(http.StatusBadRequest)
			reply(w, nil, nil, &rpcError{-32600, "Batch requests are not supported in this protocol version"})
			return
		}
		var batch []json.RawMessage
		if json.Unmarshal(raw, &batch) != nil {
			w.WriteHeader(http.StatusBadRequest)
			reply(w, nil, nil, &rpcError{-32700, "Parse error"})
			return
		}
		if len(batch) == 0 || len(batch) > 50 {
			w.WriteHeader(http.StatusBadRequest)
			reply(w, nil, nil, &rpcError{-32600, "Batch must contain 1-50 messages"})
			return
		}
		results := []json.RawMessage{}
		for _, message := range batch {
			buffer := &messageWriter{header: make(http.Header)}
			s.handleMessage(buffer, r, message)
			if buffer.Len() > 0 {
				results = append(results, json.RawMessage(bytes.TrimSpace(buffer.Bytes())))
			}
		}
		if len(results) == 0 {
			w.WriteHeader(202)
			return
		}
		_ = json.NewEncoder(w).Encode(results)
		return
	}
	s.handleMessage(w, r, raw)
}

type messageWriter struct {
	bytes.Buffer
	header http.Header
}

func (w *messageWriter) Header() http.Header { return w.header }
func (w *messageWriter) WriteHeader(int)     {}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, raw []byte) {
	var req request
	if !json.Valid(raw) {
		w.WriteHeader(http.StatusBadRequest)
		reply(w, nil, nil, &rpcError{-32700, "Parse error"})
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || (len(req.ID) > 0 && req.ID[0] != '"' && (req.ID[0] < '0' || req.ID[0] > '9') && req.ID[0] != '-' && string(req.ID) != "null") {
		w.WriteHeader(http.StatusBadRequest)
		reply(w, nil, nil, &rpcError{-32600, "Invalid Request"})
		return
	}
	if req.Method == "" {
		valid := len(req.ID) > 0 && (len(req.Result) > 0) != (len(req.Error) > 0)
		if len(req.Error) > 0 {
			var failure struct {
				Code    *int    `json:"code"`
				Message *string `json:"message"`
			}
			valid = valid && json.Unmarshal(req.Error, &failure) == nil && failure.Code != nil && failure.Message != nil
		}
		if valid {
			w.WriteHeader(http.StatusAccepted)
		} else {
			reply(w, nil, nil, &rpcError{-32600, "Invalid Request"})
		}
		return
	}
	if len(req.Result) > 0 || len(req.Error) > 0 {
		reply(w, nil, nil, &rpcError{-32600, "Invalid Request"})
		return
	}
	if len(req.ID) == 0 {
		// Notifications never receive a JSON-RPC response.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion == "" {
			reply(w, req.ID, nil, &rpcError{-32602, "protocolVersion is required"})
			return
		}
		version := params.ProtocolVersion
		if !supported(version) {
			version = "2025-06-18"
		}
		reply(w, req.ID, map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "lamplighter", "version": s.version}, "instructions": "Quota forecasts are estimates, not guarantees. Check freshness and reset times before scheduling a retry. Model routing candidates are not a routing guarantee."}, nil)
	case "ping":
		reply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		reply(w, req.ID, map[string]any{"tools": toolDefinitions()}, nil)
	case "tools/call":
		var params struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(req.Params, &params) != nil || !knownTool(params.Name) {
			reply(w, req.ID, nil, &rpcError{-32602, "Invalid tool or arguments"})
			return
		}
		arguments := map[string]string{}
		for name, value := range params.Arguments {
			var text string
			if name == "limit" {
				var n int
				if json.Unmarshal(value, &n) != nil || string(value) == "null" {
					reply(w, req.ID, nil, &rpcError{-32602, "limit must be an integer"})
					return
				}
				text = fmt.Sprint(n)
			} else if json.Unmarshal(value, &text) != nil || string(value) == "null" {
				reply(w, req.ID, nil, &rpcError{-32602, "Arguments must be strings except limit"})
				return
			}
			arguments[name] = text
		}
		if err := validateArguments(params.Name, arguments); err != nil {
			reply(w, req.ID, nil, &rpcError{-32602, err.Error()})
			return
		}
		value, err := s.callTool(r.Context(), params.Name, arguments)
		if errors.Is(err, errServerBusy) {
			reply(w, req.ID, nil, &rpcError{-32000, "MCP server is busy"})
			return
		}
		if err != nil {
			reply(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": err.Error()}}, "isError": true}, nil)
			return
		}
		data, err := json.Marshal(value)
		if err != nil {
			reply(w, req.ID, nil, &rpcError{-32603, "Internal error"})
			return
		}
		reply(w, req.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": string(data)}}}, nil)
	default:
		reply(w, req.ID, nil, &rpcError{-32601, "Method not found"})
	}
}

var errServerBusy = errors.New("MCP server is busy")

func (s *Server) callTool(ctx context.Context, name string, arguments map[string]string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 每次工具调用独立占用名额，批处理与其他请求共享服务级并发上限。
	select {
	case s.toolSlots <- struct{}{}:
		defer func() { <-s.toolSlots }()
	default:
		return nil, errServerBusy
	}
	timeout := s.queryTimeout
	if name == "refresh_quotas" {
		timeout = s.refreshTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.backend.CallTool(ctx, name, arguments)
}

func knownTool(name string) bool {
	return name == "list_quotas" || name == "get_quota_history" || name == "refresh_quotas" || name == "get_quota_events"
}
func argumentNames(name string) []string {
	switch name {
	case "list_quotas":
		return []string{"provider", "account", "model"}
	case "get_quota_history":
		return []string{"provider", "account", "window", "range"}
	case "refresh_quotas":
		return []string{"account"}
	default:
		return []string{"since", "limit"}
	}
}
func validateArguments(name string, args map[string]string) error {
	for key := range args {
		found := false
		for _, allowed := range argumentNames(name) {
			if key == allowed {
				found = true
			}
		}
		if !found {
			return errors.New("Unknown argument: " + key)
		}
	}
	return nil
}
func toolDefinitions() []any {
	descriptions := map[string]string{
		"list_quotas":       "Read current quota windows grouped by provider and quota pool, weighted remaining totals and earliest account exhaustion. Per-account fields include remaining percent, reset/start state, percentage points consumed per hour, estimated exhaustion and pre-reset risk, seven-day even-pace budget, freshness/source/error/credential state, next ignition, plan and weight. Time objects contain ISO 8601 with timezone and relative time. Null means unknown. Forecasts are estimates. Model filtering returns conservative candidate pools with certain=false because CPA aliases, affinity and live credential selection are unavailable to this plugin. Accounts use the first note line with embedded emails masked, else *** plus the email user name's last two characters (or all characters if shorter; e.g. ***rk), else account-number. Duplicate labels receive unique #n suffixes; full emails are never returned.",
		"get_quota_history": "Read downsampled quota history, including retired windows. At most 2000 points total and 200 per series. provider is a provider ID; account is an exact account label or email. window accepts 5h, 7d or a window ID. range accepts 1h, 3h, 6h, 12h, 24h, 26h, 4d, 8d, 15d, 1mo, 36d (default 24h). Each point has timestamp, relative time, remaining percent and active/passive source.",
		"refresh_quotas":    "Request an active quota refresh for all accounts or one exact account label or email. Requires a running engine, but does not wait for its first full poll. A non-running engine returns ready=false, state=starting and retry_after_seconds=30; a disabled plugin returns state=disabled. These responses do not consume the refresh limit. Running-engine attempts are globally limited to once per 60 seconds, including refresh failures. Ambiguous account filters are rejected. Returns retry_after_seconds when throttled. Wait is bounded to 180 seconds; timeout does not imply upstream work was canceled. Read list_quotas afterward for per-account freshness and errors.",
		"get_quota_events":  "Read recent quota reset, exhaustion and ignition success/failure events, newest first. since is an RFC3339 timestamp with timezone; limit is an integer, 1-100 (default 50). In-memory retention is at most 200 events of all kinds. Account filters elsewhere use exact account labels or email, never credential paths.",
	}
	var out []any
	for _, name := range []string{"list_quotas", "get_quota_history", "refresh_quotas", "get_quota_events"} {
		props := map[string]any{}
		for _, arg := range argumentNames(name) {
			props[arg] = map[string]string{"type": "string", "description": map[string]string{"provider": "Provider ID, e.g. codex or claude; omitted means all.", "account": "Exact returned account label (unique), base label before #n, original first note line, or full email; omitted means all. Shared base labels or note names select multiple accounts, which refresh rejects. Exact returned labels take priority over other matches.", "model": "CPA model ID or alias. Returns conservative candidates, never a definitive routing claim.", "window": "Window kind (5h, 7d) or exact window ID.", "range": "Named history range; default 24h.", "since": "RFC3339 timestamp with timezone; omitted means all retained events.", "limit": "Integer, 1-100; default 50."}[arg]}
		}
		if name == "get_quota_events" {
			props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum events, default 50."}
		}
		description := descriptions[name]
		if name != "refresh_quotas" {
			description += " Returns ready=false, certain=false and state=starting with retry_after_seconds=30 instead of data until the engine runs and completes its first full poll; a credential-list failure counts as ready. A disabled plugin returns ready=false, certain=false and state=disabled instead of data. These are successful tool results, not errors."
		}
		out = append(out, map[string]any{"name": name, "description": description, "inputSchema": map[string]any{"type": "object", "properties": props, "additionalProperties": false}})
	}
	return out
}
