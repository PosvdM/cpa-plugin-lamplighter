package host

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestModelStreamCallbacks(t *testing.T) {
	var methods []string
	rpc := RPC{Call: func(method string, payload []byte) ([]byte, error) {
		methods = append(methods, method)
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatal(err)
		}
		switch method {
		case "host.model.execute_stream":
			if req["stream"] != true || req["auth_id"] != "pinned" || req["forced_provider"] != "claude" || req["entry_protocol"] != "claude" || req["exit_protocol"] != "claude" {
				t.Fatal(req)
			}
			return []byte(`{"ok":true,"result":{"status_code":200,"headers":{"X-Quota":["yes"]},"stream_id":"s1"}}`), nil
		case "host.model.stream_read":
			if req["stream_id"] != "s1" {
				t.Fatal(req)
			}
			return []byte(`{"ok":true,"result":{"payload":"T0s=","error":"terminal","done":true}}`), nil
		case "host.model.stream_close":
			if req["stream_id"] != "s1" {
				t.Fatal(req)
			}
			return []byte(`{"ok":true,"result":{}}`), nil
		default:
			t.Fatalf("unexpected callback %s", method)
			return nil, nil
		}
	}}
	opened, err := rpc.ModelExecuteStream(pluginapi.HostModelExecutionRequest{Model: "m", Stream: true, AuthID: "pinned", ForcedProvider: "claude", EntryProtocol: "claude", ExitProtocol: "claude"})
	if err != nil || opened.StreamID != "s1" || opened.Headers.Get("X-Quota") != "yes" {
		t.Fatalf("opened=%+v err=%v", opened, err)
	}
	chunk, err := rpc.ModelStreamRead(opened.StreamID)
	if err != nil || string(chunk.Payload) != "OK" || !chunk.Done || chunk.Error != "terminal" {
		t.Fatalf("chunk=%+v err=%v", chunk, err)
	}
	if err := rpc.ModelStreamClose(opened.StreamID); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 3 {
		t.Fatal(methods)
	}
}
