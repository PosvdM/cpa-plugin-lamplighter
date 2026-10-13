// Package providers describes the services Lamplighter monitors. Each
// service, in its own file, says how to read its quota, which response
// headers carry quota, and how to ignite its 5-hour window.
//
// Active reads call the provider usage endpoints with the same URLs and
// headers as the CPA Management Center. Passive reads parse the rate-limit
// headers that CPA records from real model responses.
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Provider is one service.
type Provider struct {
	// ID is the provider name in the config, in quota group keys and on the
	// page, such as "codex".
	ID string
	// Aliases are other names CPA uses for the provider of a credential.
	Aliases []string
	// Title is the display name, such as "ChatGPT".
	Title string
	// Monitor and Ignite are the default settings.
	Monitor bool
	Ignite  bool
	// Fetch reads the quota windows of one credential.
	Fetch func(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) ([]quota.RawGroup, error)
	// Headers reads quota windows from the response headers of a model
	// request. It is nil when the provider sends no quota in its headers.
	Headers func(header http.Header, now time.Time) []quota.RawGroup
	// Ignition is nil when the provider cannot be ignited.
	Ignition *Ignition
}

// Ignition describes the ignition request of a provider.
type Ignition struct {
	// Protocol is the CPA entry protocol of the request body.
	Protocol string
	// Body returns the request body for model.
	Body func(model string) map[string]any
	// Candidates returns the models to try for a quota group, in order.
	Candidates func(group string, available []string) []string
	// Hint describes the automatic model choice in the settings.
	Hint Text
	// HintGroup is the quota group whose first candidate the settings show.
	HintGroup string
	// Groups are the quota groups the settings offer as checkboxes, with
	// GroupsHint below them, and DefaultGroups those ignited by default.
	// Without Groups, every group of the provider is ignited.
	Groups        []string
	GroupsHint    Text
	DefaultGroups []string
}

// Text is a user-facing text in Chinese and English.
type Text struct {
	Zh string `json:"zh"`
	En string `json:"en"`
}

var registry = []*Provider{codex, claude, antigravity, xai}

// All returns the providers in display order.
func All() []*Provider { return registry }

// IDs returns the provider IDs in display order.
func IDs() []string {
	out := make([]string, 0, len(registry))
	for _, p := range registry {
		out = append(out, p.ID)
	}
	return out
}

// Lookup returns the provider for a provider name from CPA or the config,
// or nil when Lamplighter does not support it.
func Lookup(name string) *Provider {
	name = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	for _, p := range registry {
		if p.ID == name {
			return p
		}
		for _, alias := range p.Aliases {
			if alias == name {
				return p
			}
		}
	}
	return nil
}

// Rank returns the display position of a provider.
func Rank(id string) int {
	for i, p := range registry {
		if p.ID == id {
			return i
		}
	}
	return len(registry)
}

// Title returns the display name of provider.
func Title(provider string) string {
	if p := Lookup(provider); p != nil {
		return p.Title
	}
	if provider == "" {
		return ""
	}
	return strings.ToUpper(provider[:1]) + provider[1:]
}

// groupOrder lists quota groups in the order the page shows them: the main
// group of each service, then the separately counted ones. Other groups
// follow.
var groupOrder = []string{"Claude", "ChatGPT", "Gemini", "Grok", "Fable", "Claude / GPT"}

// GroupOrder returns the display order of quota groups by source label.
func GroupOrder() []string { return groupOrder }

// GroupRank returns the display position of a quota group.
func GroupRank(sourceLabel string) int {
	for i, label := range groupOrder {
		if label == sourceLabel {
			return i
		}
	}
	return len(groupOrder)
}

// Fetch reads the quota windows of one credential.
func Fetch(ctx context.Context, p *Provider, doer quota.Doer, cred Credential, now time.Time) ([]quota.RawGroup, error) {
	if cred.AccessToken == "" {
		return nil, errors.New("凭证文件中没有 access_token")
	}
	return p.Fetch(ctx, doer, cred, now)
}

// TriggerPrompt is the only content of an ignition request.
const TriggerPrompt = "This is an automated quota-window trigger. " +
	"Do not think, reason, deliberate, use tools, or perform any other task. " +
	"Reply with exactly OK and nothing else."

// Request returns the entry protocol and body of the ignition request.
func Request(p *Provider, model string) (string, []byte, error) {
	if p.Ignition == nil {
		return "", nil, errors.New("不支持点火的 provider: " + p.ID)
	}
	body, err := json.Marshal(p.Ignition.Body(model))
	if err != nil {
		return "", nil, err
	}
	return p.Ignition.Protocol, body, nil
}

// Candidates returns the models to try, in order, for one ignition target.
// group is the quota group, such as "Gemini" or "Claude / GPT" for
// Antigravity. A configured override is the only candidate and must exist in
// available.
func Candidates(p *Provider, group, override string, available []string) ([]string, error) {
	if override != "" {
		for _, model := range available {
			if model == override {
				return []string{override}, nil
			}
		}
		return nil, fmt.Errorf("指定点火模型 %s 不在 CPA 模型列表中", override)
	}
	var out []string
	if p.Ignition != nil {
		out = models.Unique(p.Ignition.Candidates(group, available))
	}
	if len(out) == 0 {
		return nil, errors.New("CPA 模型列表中没有适合该凭证的点火模型")
	}
	return out, nil
}

// chatBody is an OpenAI chat completion request for model.
func chatBody(model string, extra map[string]any) map[string]any {
	body := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": TriggerPrompt}},
		"max_tokens": 4,
		"stream":     false,
	}
	for key, value := range extra {
		body[key] = value
	}
	return body
}
