package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Claude endpoint and headers, the same as the CPA Management Center quota
// page. The User-Agent matches CPA's default Claude Code fingerprint.
const (
	ClaudeUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	ClaudeProfileURL = "https://api.anthropic.com/api/oauth/profile"
	ClaudeUserAgent  = "claude-cli/2.1.280 (external, cli)"
)

// FableGroupKey is the raw group key of Claude's Fable window, the same for
// active and passive data.
const FableGroupKey = "claude:" + quota.WindowFable

var claude = &Provider{
	ID:      "claude",
	Title:   "Claude",
	Monitor: true,
	Ignite:  true,
	Fetch:   fetchClaude,
	Plan:    claudePlan,
	Profile: fetchClaudeProfile,
	Headers: func(header http.Header, _ time.Time) []quota.RawGroup { return parseClaudeHeaders(header) },
	Ignition: &Ignition{
		Protocol: "claude",
		Body: func(model string) map[string]any {
			return map[string]any{
				"model":      model,
				"max_tokens": 4,
				"messages":   []any{map[string]any{"role": "user", "content": TriggerPrompt}},
			}
		},
		Candidates: claudeCandidates,
		Hint:       Text{Zh: "最新的 Haiku", En: "latest Haiku"},
	},
}

// claudeCandidates prefers the newest non-thinking Haiku, then Sonnet.
func claudeCandidates(_ string, available []string) []string {
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	out := models.Newest(available, func(m string) bool {
		return strings.HasPrefix(m, "claude-") && strings.Contains(m, "haiku") && noThinking(m)
	})
	return append(out, models.Newest(available, func(m string) bool {
		return strings.HasPrefix(m, "claude-") && strings.Contains(m, "sonnet") && noThinking(m)
	})...)
}

// claudeWindows maps oauth/usage keys to window IDs, labels and periods.
var claudeWindows = []struct {
	key, id, label string
	period         time.Duration
}{
	{"five_hour", quota.WindowFiveHour, quota.LabelFiveHour, quota.FiveHours},
	{"seven_day", quota.WindowSevenDay, quota.LabelSevenDay, quota.SevenDays},
	{"seven_day_oauth_apps", "seven-day-oauth-apps", "7 Day OAuth Apps", quota.SevenDays},
	{"seven_day_opus", "seven-day-opus", "7 Day Opus", quota.SevenDays},
	{"seven_day_sonnet", "seven-day-sonnet", "7 Day Sonnet", quota.SevenDays},
	{"seven_day_cowork", "seven-day-cowork", "7 Day Cowork", quota.SevenDays},
	{"iguana_necktie", quota.WindowFable, quota.LabelFable, quota.SevenDays},
}

func fetchClaude(ctx context.Context, doer quota.Doer, cred Credential, _ time.Time) ([]quota.RawGroup, error) {
	header := bearer(cred)
	header.Set("User-Agent", ClaudeUserAgent)
	header.Set("anthropic-beta", "oauth-2025-04-20")
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodGet, URL: ClaudeUsageURL, Header: header})
	if err != nil {
		return nil, err
	}
	return ParseClaudeUsage(payload)
}

// fetchClaudeProfile reads the plan of a Claude account from the OAuth
// profile, which the usage response does not state. The request carries the
// headers of the usage query.
func fetchClaudeProfile(ctx context.Context, doer quota.Doer, cred Credential, _ time.Time) (string, error) {
	header := bearer(cred)
	header.Set("User-Agent", ClaudeUserAgent)
	header.Set("anthropic-beta", "oauth-2025-04-20")
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodGet, URL: ClaudeProfileURL, Header: header})
	if err != nil {
		return "", err
	}
	return ParseClaudeProfile(payload)
}

// ParseClaudeProfile returns the raw plan of an OAuth profile, as claudePlan
// reads it: the organization type and the rate limit tier joined by a space,
// such as "claude_pro default_claude_ai". Without an organization type, the
// account's has_claude_max and has_claude_pro flags decide.
func ParseClaudeProfile(payload map[string]any) (string, error) {
	org, _ := payload["organization"].(map[string]any)
	kind := scalarString(field(org, "organization_type"))
	if kind == "" {
		account, _ := payload["account"].(map[string]any)
		switch {
		case account["has_claude_max"] == true:
			kind = "claude_max"
		case account["has_claude_pro"] == true:
			kind = "claude_pro"
		default:
			return "", errors.New("Claude 账号资料中没有套餐")
		}
	}
	return strings.TrimSpace(kind + " " + scalarString(field(org, "rate_limit_tier"))), nil
}

// ParseClaudeUsage converts an oauth/usage response into Claude groups. The
// 5-hour and 7-day windows form the main group; every other window, such as
// Fable, is a group of its own.
func ParseClaudeUsage(payload map[string]any) ([]quota.RawGroup, error) {
	fable := findFableLimit(payload)
	var windows []quota.Window
	for _, spec := range claudeWindows {
		if spec.key == "iguana_necktie" && fable != nil {
			continue
		}
		item, _ := payload[spec.key].(map[string]any)
		if item == nil {
			continue
		}
		used, ok := floatValue(item["utilization"])
		if !ok {
			continue
		}
		windows = append(windows, quota.Window{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - used),
			Reset:     quota.ParseTime(item["resets_at"]),
			Period:    spec.period,
		})
	}
	if fable != nil {
		if used, ok := floatValue(fable["percent"]); ok {
			windows = append(windows, quota.Window{
				ID:        quota.WindowFable,
				Label:     quota.LabelFable,
				Remaining: clampPercent(100 - used),
				Reset:     quota.ParseTime(fable["resets_at"]),
				Period:    quota.SevenDays,
			})
		}
	}
	if len(windows) == 0 {
		return nil, errors.New("Claude 未返回可识别的额度窗口")
	}

	var groups []quota.RawGroup
	var main []quota.Window
	for _, w := range windows {
		if w.ID == quota.WindowFiveHour || w.ID == quota.WindowSevenDay {
			main = append(main, w)
		}
	}
	if len(main) > 0 {
		groups = append(groups, quota.RawGroup{Key: "claude:main", SourceLabel: "Claude", Windows: main})
	}
	for _, w := range windows {
		if w.ID == quota.WindowFiveHour || w.ID == quota.WindowSevenDay {
			continue
		}
		label := "Claude"
		if w.ID == quota.WindowFable {
			label = "Fable"
		}
		key := "claude:" + w.ID
		if w.ID == quota.WindowFable {
			key = FableGroupKey
		}
		groups = append(groups, quota.RawGroup{Key: key, SourceLabel: label, Windows: []quota.Window{w}})
	}
	return groups, nil
}

func findFableLimit(payload map[string]any) map[string]any {
	limits, _ := payload["limits"].([]any)
	var candidates []map[string]any
	for _, raw := range limits {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["kind"])))
		scope, _ := item["scope"].(map[string]any)
		model, _ := field(scope, "model").(map[string]any)
		name := strings.ToLower(strings.TrimSpace(scalarString(field(model, "display_name"))))
		if kind == "weekly_scoped" && (name == "fable" || name == "fable 5") && item["percent"] != nil {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	for _, item := range candidates {
		if active, _ := item["is_active"].(bool); active {
			return item
		}
	}
	return candidates[0]
}

func parseClaudeHeaders(header http.Header) []quota.RawGroup {
	var groups []quota.RawGroup
	if main, ok := parseClaudeMainHeaders(header); ok {
		groups = append(groups, main)
	}
	// Anthropic reports the Fable window as 7d_oi, which CPA also treats as
	// the Fable-specific 7-day window.
	if utilization, ok := floatValue(headerValue(header, "Anthropic-Ratelimit-Unified-7d_oi-Utilization")); ok {
		groups = append(groups, quota.RawGroup{Key: FableGroupKey, SourceLabel: "Fable", Windows: []quota.Window{{
			ID:        quota.WindowFable,
			Label:     quota.LabelFable,
			Remaining: clampPercent(100 - utilization*100),
			Reset:     quota.ParseTime(headerValue(header, "Anthropic-Ratelimit-Unified-7d_oi-Reset")),
			Period:    quota.SevenDays,
		}}})
	}
	return groups
}

func parseClaudeMainHeaders(header http.Header) (quota.RawGroup, bool) {
	group := quota.RawGroup{Key: "claude:main", SourceLabel: "Claude"}
	for _, spec := range []struct {
		prefix, id, label string
		period            time.Duration
	}{
		{"Anthropic-Ratelimit-Unified-5h-", quota.WindowFiveHour, quota.LabelFiveHour, quota.FiveHours},
		{"Anthropic-Ratelimit-Unified-7d-", quota.WindowSevenDay, quota.LabelSevenDay, quota.SevenDays},
	} {
		utilization, ok := floatValue(headerValue(header, spec.prefix+"Utilization"))
		if !ok {
			continue
		}
		// The header is a fraction of the window, for example 0.2 for 20%.
		group.Windows = append(group.Windows, quota.Window{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - utilization*100),
			Reset:     quota.ParseTime(headerValue(header, spec.prefix+"Reset")),
			Period:    spec.period,
		})
	}
	return group, len(group.Windows) > 0
}
