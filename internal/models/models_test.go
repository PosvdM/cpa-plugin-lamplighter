package models

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The model list on the reference server: the union of a Codex, a Claude and
// an Antigravity credential.
var union = []string{
	"claude-3-5-haiku-20241022", "claude-haiku-4-5-20251001", "claude-opus-4-6-thinking", "claude-opus-5-5",
	"claude-sonnet-4-6", "claude-sonnet-5", "claude-sonnet-5-5-high", "codex-auto-review",
	"gemini-3-flash", "gemini-3.1-flash-image", "gemini-3.1-flash-lite", "gemini-3.1-pro-low",
	"gemini-3.8-flash-high", "gemini-pro-agent", "gpt-5.5", "gpt-5.6-luna", "gpt-6-luna", "gpt-6-sol",
	"gpt-image-2", "gpt-oss-120b-medium",
}

func TestCodexPrefersNewestLuna(t *testing.T) {
	got, err := Candidates("codex", "ChatGPT", "", union)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "gpt-6-luna" || got[1] != "gpt-5.6-luna" {
		t.Fatalf("got %v", got)
	}
	for _, model := range got {
		if model == "gpt-image-2" || model == "gpt-oss-120b-medium" {
			t.Fatalf("%s must not be a Codex candidate", model)
		}
	}
}

func TestClaudePrefersNewestHaiku(t *testing.T) {
	got, _ := Candidates("claude", "Claude", "", union)
	if got[0] != "claude-haiku-4-5-20251001" || got[1] != "claude-3-5-haiku-20241022" {
		t.Fatalf("got %v", got)
	}
}

func TestAntigravityGeminiPrefersNewestFlashAndSkipsImages(t *testing.T) {
	got, _ := Candidates("antigravity", "Gemini", "", union)
	if got[0] != "gemini-3.8-flash-high" {
		t.Fatalf("got %v", got)
	}
	for _, model := range got {
		if model == "gemini-3.1-flash-image" {
			t.Fatal("image models must be skipped")
		}
	}
	if got[len(got)-1] != "gemini-3.1-pro-low" && got[len(got)-1] != "gemini-pro-agent" {
		t.Fatalf("Pro models come last: %v", got)
	}
}

func TestAntigravityClaudeGPTPrefersNonThinkingClaude(t *testing.T) {
	got, _ := Candidates("antigravity", "Claude / GPT", "", union)
	if got[0] != "claude-haiku-4-5-20251001" {
		t.Fatalf("got %v", got)
	}
	idx := map[string]int{}
	for i, model := range got {
		idx[model] = i
	}
	if idx["claude-sonnet-4-6"] > idx["gpt-oss-120b-medium"] {
		t.Fatalf("Sonnet comes before GPT-OSS: %v", got)
	}
}

func TestOverrideMustExist(t *testing.T) {
	if got, err := Candidates("codex", "", "gpt-6-sol", union); err != nil || !reflect.DeepEqual(got, []string{"gpt-6-sol"}) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := Candidates("codex", "", "missing", union); err == nil {
		t.Fatal("unknown override must fail")
	}
}

func TestListerSendsAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"gpt-6-luna","owned_by":"openai"}]}`))
	}))
	defer server.Close()
	got, err := (&Lister{}).List(context.Background(), server.URL, "k")
	if err != nil || !reflect.DeepEqual(got, []string{"gpt-6-luna"}) {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := (&Lister{}).List(context.Background(), server.URL, ""); err == nil {
		t.Fatal("missing key must fail")
	}
}
