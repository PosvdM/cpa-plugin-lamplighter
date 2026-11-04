package providers

import (
	"encoding/json"
	"testing"
)

func TestIgnitionOutputTokenBudgets(t *testing.T) {
	for _, tt := range []struct {
		provider string
		model    string
		field    string
		budget   float64
	}{
		{"codex", "gpt-6-luna", "max_output_tokens", 16},
		{"codex", "gpt-5.6-luna", "max_output_tokens", 16},
		{"claude", "claude-haiku-5-5", "max_tokens", 4},
		{"antigravity", "gemini-3.8-flash-high", "max_tokens", 4},
		{"antigravity", "claude-sonnet-4-6", "max_tokens", 4},
	} {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			_, raw, err := Request(Lookup(tt.provider), tt.model)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if got := body[tt.field]; got != tt.budget {
				t.Fatalf("%s = %v, want %v", tt.field, got, tt.budget)
			}
			other := "max_tokens"
			if tt.field == other {
				other = "max_output_tokens"
			}
			if _, ok := body[other]; ok {
				t.Fatalf("unexpected %s in %s request", other, tt.provider)
			}
		})
	}
}
