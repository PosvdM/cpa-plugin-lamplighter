package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type backendFunc func(context.Context, string, map[string]string) (any, error)

func (f backendFunc) CallTool(ctx context.Context, name string, args map[string]string) (any, error) {
	return f(ctx, name, args)
}

func toolMessage(id int, name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q}}`, id, name)
}

func recordMCP(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.serveHTTP(w, req)
	return w
}

func awaitSignal(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestToolConcurrencyLimitIncludesBatch(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	s := New(backendFunc(func(ctx context.Context, _ string, _ map[string]string) (any, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return map[string]bool{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}), "test")
	s.key = "key"
	defer s.Close()
	responses := make(chan *httptest.ResponseRecorder, 4)
	names := []string{"list_quotas", "get_quota_history", "get_quota_events", "refresh_quotas"}
	for i, name := range names {
		go func(id int, name string) { responses <- recordMCP(s, toolMessage(id, name)) }(i, name)
		awaitSignal(t, started, "tool did not acquire a concurrency slot")
	}
	for _, name := range names {
		w := recordMCP(s, toolMessage(9, name))
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"code":-32000`)) || !bytes.Contains(w.Body.Bytes(), []byte("busy")) {
			t.Fatalf("excess %s call: %d %s", name, w.Code, w.Body)
		}
	}
	batch := "[" + toolMessage(10, "list_quotas") + "," + toolMessage(11, "get_quota_events") + "]"
	w := recordMCP(s, batch)
	var results []struct {
		ID    int `json:"id"`
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &results); err != nil || len(results) != 2 {
		t.Fatalf("busy batch: %s", w.Body)
	}
	for i, result := range results {
		if result.ID != 10+i || result.Error.Code != -32000 {
			t.Fatalf("busy batch item: %+v", result)
		}
	}
	for _, method := range []string{"ping", "tools/list"} {
		w := recordMCP(s, fmt.Sprintf(`{"jsonrpc":"2.0","id":20,"method":%q}`, method))
		if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte(`"error"`)) {
			t.Fatalf("non-tool request was limited: %s", w.Body)
		}
	}
	close(release)
	for range names {
		select {
		case w := <-responses:
			if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"content"`)) || bytes.Contains(w.Body.Bytes(), []byte(`"isError"`)) {
				t.Fatalf("accepted tool: %s", w.Body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("accepted tool did not finish")
		}
	}
	// 批处理按工具调用释放名额，批次长度不占用并发名额。
	w = recordMCP(s, "["+strings.Join([]string{
		toolMessage(1, names[0]), toolMessage(2, names[1]), toolMessage(3, names[2]),
		toolMessage(4, names[3]), toolMessage(5, names[0]),
	}, ",")+"]")
	var completed []json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &completed) != nil || len(completed) != 5 || bytes.Contains(w.Body.Bytes(), []byte(`"error"`)) {
		t.Fatalf("slots were not released between batch items: %s", w.Body)
	}
}

func TestToolTimeoutBudgets(t *testing.T) {
	for _, tc := range []struct {
		name string
		want time.Duration
	}{
		{"list_quotas", 10 * time.Second},
		{"get_quota_history", 10 * time.Second},
		{"get_quota_events", 10 * time.Second},
		{"refresh_quotas", 180 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var remaining time.Duration
			s := New(backendFunc(func(ctx context.Context, _ string, _ map[string]string) (any, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					return nil, errors.New("missing deadline")
				}
				remaining = time.Until(deadline)
				return map[string]bool{"ok": true}, nil
			}), "test")
			s.key = "key"
			defer s.Close()
			w := recordMCP(s, toolMessage(1, tc.name))
			if remaining <= tc.want-time.Second || remaining > tc.want || bytes.Contains(w.Body.Bytes(), []byte(`"isError"`)) {
				t.Fatalf("deadline budget %s, want %s; response: %s", remaining, tc.want, w.Body)
			}
		})
	}
}

func TestToolCallsTimeOut(t *testing.T) {
	for _, name := range []string{"list_quotas", "get_quota_history", "get_quota_events", "refresh_quotas"} {
		t.Run(name, func(t *testing.T) {
			var canceled error
			s := New(backendFunc(func(ctx context.Context, _ string, _ map[string]string) (any, error) {
				<-ctx.Done()
				canceled = ctx.Err()
				return nil, canceled
			}), "test")
			s.key = "key"
			s.queryTimeout = 15 * time.Millisecond
			s.refreshTimeout = 30 * time.Millisecond
			defer s.Close()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- recordMCP(s, toolMessage(1, name)) }()
			select {
			case w := <-done:
				if !errors.Is(canceled, context.DeadlineExceeded) || !bytes.Contains(w.Body.Bytes(), []byte(`"isError":true`)) || !bytes.Contains(w.Body.Bytes(), []byte("context deadline exceeded")) {
					t.Fatalf("timeout result: %s; backend error: %v", w.Body, canceled)
				}
				if len(s.toolSlots) != 0 {
					t.Fatal("timed-out call retained its concurrency slot")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("tool timeout did not cancel backend")
			}
		})
	}
}

func TestLifecycleCancelsQueriesAndBatch(t *testing.T) {
	for _, action := range []string{"close", "disable", "reconfigure"} {
		for _, name := range []string{"list_quotas", "get_quota_history", "get_quota_events", "refresh_quotas"} {
			t.Run(action+"/"+name, func(t *testing.T) {
				started := make(chan struct{})
				finished := make(chan struct{})
				calls := 0
				s := New(backendFunc(func(ctx context.Context, _ string, _ map[string]string) (any, error) {
					calls++
					close(started)
					<-ctx.Done()
					close(finished)
					return nil, ctx.Err()
				}), "test")
				defer s.Close()
				s.Configure(true, "127.0.0.1:0", "key")
				if !s.State().Running {
					t.Fatalf("listener failed: %+v", s.State())
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					done <- recordMCP(s, "["+toolMessage(1, name)+","+toolMessage(2, name)+"]")
				}()
				awaitSignal(t, started, "query did not start")
				changed := make(chan struct{})
				go func() {
					switch action {
					case "close":
						s.Close()
					case "disable":
						s.Configure(false, "127.0.0.1:0", "key")
					case "reconfigure":
						s.Configure(true, "127.0.0.1:0", "replacement")
					}
					close(changed)
				}()
				awaitSignal(t, changed, "lifecycle operation did not finish")
				awaitSignal(t, finished, "lifecycle operation did not cancel backend")
				select {
				case w := <-done:
					if calls != 1 || bytes.Count(w.Body.Bytes(), []byte("context canceled")) != 2 {
						t.Fatalf("canceled batch invoked backend %d times: %s", calls, w.Body)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("canceled batch did not finish")
				}
				if action == "reconfigure" {
					req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
					req.Header.Set("Authorization", "Bearer replacement")
					req.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					s.serveHTTP(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("replacement listener rejected request: %d", w.Code)
					}
				}
			})
		}
	}
}

func TestCloseWaitsForCanceledBackend(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	s := New(backendFunc(func(ctx context.Context, _ string, _ map[string]string) (any, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, ctx.Err()
	}), "test")
	s.key = "key"
	defer s.Close()
	defer unblock()
	done := make(chan struct{})
	go func() { recordMCP(s, toolMessage(1, "get_quota_history")); close(done) }()
	awaitSignal(t, started, "query did not start")
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	awaitSignal(t, canceled, "Close did not cancel backend")
	select {
	case <-closed:
		t.Fatal("Close returned before the backend finished")
	default:
	}
	unblock()
	awaitSignal(t, closed, "Close did not drain the backend")
	awaitSignal(t, done, "handler did not finish")
}

func TestCloseConcurrentHandlerRegistration(t *testing.T) {
	for i := 0; i < 20; i++ {
		s := New(&fakeBackend{}, "test")
		s.key = "key"
		start := make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < 20; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				recordMCP(s, toolMessage(1, "list_quotas"))
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.Close()
		}()
		close(start)
		wg.Wait()
		s.mu.Lock()
		remaining := len(s.requests)
		s.mu.Unlock()
		if remaining != 0 || len(s.toolSlots) != 0 {
			t.Fatalf("Close retained %d handlers and %d tool slots", remaining, len(s.toolSlots))
		}
		if w := recordMCP(s, toolMessage(2, "list_quotas")); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("closed server accepted request: %d", w.Code)
		}
	}
}
