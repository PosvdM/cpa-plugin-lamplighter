package host

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestErrorEnvelopeCarriesStatus(t *testing.T) {
	rpc := RPC{Call: func(method string, payload []byte) ([]byte, error) {
		return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"auth_not_found: no auth available","http_status":503}}`), nil
	}}
	_, err := rpc.ModelExecute(pluginapi.HostModelExecutionRequest{Model: "m"})
	if err == nil || StatusOf(err) != 503 {
		t.Fatalf("err %v status %d", err, StatusOf(err))
	}
}

func TestAuthListDecodesFiles(t *testing.T) {
	rpc := RPC{Call: func(method string, payload []byte) ([]byte, error) {
		if method != "host.auth.list" {
			t.Fatalf("method %s", method)
		}
		return []byte(`{"ok":true,"result":{"files":[{"id":"a.json","auth_index":"7","name":"a.json","provider":"codex","email":"x@y.z"}]}}`), nil
	}}
	files, err := rpc.AuthList()
	if err != nil || len(files) != 1 || files[0].AuthIndex != "7" || files[0].Email != "x@y.z" {
		t.Fatalf("files %+v err %v", files, err)
	}
}

func TestHTTPDoCancelsAfterTimeout(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	release := make(chan struct{})
	rpc := RPC{Call: func(method string, payload []byte) ([]byte, error) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		switch method {
		case "host.http.operation_open":
			return []byte(`{"ok":true,"result":{"operation_id":"op1"}}`), nil
		case "host.http.cancel":
			var req map[string]string
			json.Unmarshal(payload, &req)
			if req["operation_id"] == "op1" {
				close(release)
			}
			return []byte(`{"ok":true,"result":{}}`), nil
		case "host.http.do":
			var req map[string]any
			json.Unmarshal(payload, &req)
			if req["operation_id"] != "op1" {
				t.Errorf("request must run inside the operation: %v", req)
			}
			<-release
			return []byte(`{"ok":false,"error":{"code":"host_call_failed","message":"context canceled"}}`), nil
		}
		return nil, nil
	}}
	start := time.Now()
	_, err := rpc.HTTPDo(context.Background(), pluginapi.HTTPRequest{Method: "GET", URL: "https://x"}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("timed out request must fail")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel did not unblock the request")
	}
}
