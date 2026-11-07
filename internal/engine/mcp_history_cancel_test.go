package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/mcp"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// scanWatchContext 在真实历史扫描反复检查取消时发出信号，不替代历史后端。
type scanWatchContext struct {
	context.Context
	checks   atomic.Int32
	scanning chan struct{}
}

func (c *scanWatchContext) Err() error {
	if c.checks.Add(1) == 100 {
		close(c.scanning)
	}
	return c.Context.Err()
}

type watchedMCPBackend struct {
	*MCPBackend
	scanning chan struct{}
	finished chan error
}

func (b *watchedMCPBackend) CallTool(ctx context.Context, name string, args map[string]string) (any, error) {
	value, err := b.MCPBackend.CallTool(&scanWatchContext{Context: ctx, scanning: b.scanning}, name, args)
	b.finished <- err
	return value, err
}

func largeHistoryEngine(t *testing.T) *Engine {
	t.Helper()
	now := time.Now().UTC()
	e := New(Options{Now: func() time.Time { return now }})
	e.dataDir = t.TempDir()
	e.creds["1"] = &Cred{AuthIndex: "1", Provider: "codex", Name: "history.json", Email: "history@example.com"}
	e.groups["codex:1:gpt"] = &GroupView{Key: "codex:1:gpt", AuthIndex: "1", Provider: "codex", SourceLabel: "GPT", startKnown: true,
		Windows: []WindowView{{Window: quota.Window{ID: quota.WindowFiveHour, Remaining: 50, Reset: now.Add(time.Hour)}, ObservedAt: now, Source: quota.SourceActive}}}
	dir := filepath.Join(e.dataDir, "history")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(store.Record{Kind: "s", Time: now.Add(-time.Minute).Unix(), Group: "codex:1:gpt", Window: "five-hour", Remaining: 50, Reset: now.Add(time.Hour).Unix()})
	// 大量重复样本迫使后端扫描文件，但聚合结果只需要一个桶。
	if err := os.WriteFile(filepath.Join(dir, now.Format("2006-01-02")+".jsonl"), bytes.Repeat(append(raw, '\n'), 150000), 0644); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestMCPRealHistoryCloseCancelsScanning(t *testing.T) {
	for _, tool := range []string{"list_quotas", "get_quota_history"} {
		t.Run(tool, func(t *testing.T) {
			e := largeHistoryEngine(t)
			backend := &watchedMCPBackend{MCPBackend: NewMCPBackend(e), scanning: make(chan struct{}), finished: make(chan error, 1)}
			server := mcp.New(backend, "test")
			defer server.Close()
			address := freeMCPAddress(t)
			server.Configure(true, address, "key")
			if !server.State().Running {
				t.Fatal(server.State())
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				req, _ := http.NewRequest("POST", "http://"+address+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`"}}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer key")
				client := &http.Client{Timeout: 3 * time.Second}
				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}()
			select {
			case <-backend.scanning:
			case <-time.After(3 * time.Second):
				t.Fatal("real backend did not enter scan")
			}
			closed := make(chan struct{})
			started := time.Now()
			go func() { server.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close did not promptly cancel real history backend")
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("Close took %v", elapsed)
			}
			if err := <-backend.finished; !errors.Is(err, context.Canceled) {
				t.Fatalf("scan returned %v", err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("HTTP client did not finish")
			}
		})
	}
}

func TestMCPRealHistoryCancellationAndDeadline(t *testing.T) {
	e := largeHistoryEngine(t)
	for _, tool := range []string{"list_quotas", "get_quota_history"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			watching := &scanWatchContext{Context: ctx, scanning: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := NewMCPBackend(e).CallTool(watching, tool, nil); done <- err }()
			select {
			case <-watching.scanning:
			case <-time.After(3 * time.Second):
				t.Fatal("scan did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("scan ignored cancellation")
			}
			deadline, stop := context.WithTimeout(context.Background(), time.Millisecond)
			defer stop()
			if _, err := NewMCPBackend(e).CallTool(deadline, tool, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline: %v", err)
			}
		})
	}
}

func TestForecastCalculationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	points := [][3]float64{{1, 90, 0}, {2, 80, 0}, {3, 70, 0}}
	if _, err := recentSlopeContext(ctx, points); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := forecastWindowContext(ctx, WindowStatus{}, TargetView{}, points, time.Now(), time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
