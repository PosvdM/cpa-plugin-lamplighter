package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Codex endpoint and headers, the same as the CPA Management Center quota
// page. The User-Agent matches the CPA Codex executor.
const (
	CodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	CodexUserAgent = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
)

// codexTitle is also the source label of the Codex group.
const codexTitle = "ChatGPT"

var codex = &Provider{
	ID:      "codex",
	Title:   codexTitle,
	Monitor: true,
	Ignite:  true,
	Fetch:   fetchCodex,
	Headers: func(header http.Header, now time.Time) []quota.RawGroup {
		if group, ok := parseCodexHeaders(header, now); ok {
			return []quota.RawGroup{group}
		}
		return nil
	},
	Ignition: &Ignition{
		Protocol: "openai-response",
		Body: func(model string) map[string]any {
			return map[string]any{
				"model": model,
				"input": []any{map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "input_text", "text": TriggerPrompt}},
				}},
				"reasoning":         map[string]any{"effort": "none"},
				"tools":             []any{},
				"stream":            false,
				"store":             false,
				"max_output_tokens": 4,
			}
		},
		Candidates: codexCandidates,
		Hint:       Text{Zh: "最新的 Luna", En: "latest Luna"},
	},
}

// codexCandidates prefers the newest Luna, then other GPT models.
func codexCandidates(_ string, available []string) []string {
	noImage := func(m string) bool { return !strings.Contains(m, "image") }
	out := models.Newest(available, func(m string) bool { return strings.Contains(m, "luna") && noImage(m) })
	return append(out, models.Newest(available, func(m string) bool {
		return strings.HasPrefix(m, "gpt-") && !strings.Contains(m, "oss") && noImage(m)
	})...)
}

func fetchCodex(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) ([]quota.RawGroup, error) {
	header := bearer(cred)
	header.Set("User-Agent", CodexUserAgent)
	if cred.AccountID != "" {
		header.Set("Chatgpt-Account-Id", cred.AccountID)
	}
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodGet, URL: CodexUsageURL, Header: header})
	if err != nil {
		return nil, err
	}
	return ParseCodexUsage(payload, now)
}

// ParseCodexUsage converts a wham/usage response into the Codex group.
func ParseCodexUsage(payload map[string]any, now time.Time) ([]quota.RawGroup, error) {
	rateLimit, _ := field(payload, "rate_limit", "rateLimit").(map[string]any)
	if rateLimit == nil {
		return nil, errors.New("Codex 未返回 rate_limit")
	}
	primary, _ := field(rateLimit, "primary_window", "primaryWindow").(map[string]any)
	secondary, _ := field(rateLimit, "secondary_window", "secondaryWindow").(map[string]any)

	candidates := []map[string]any{primary, secondary}
	fiveIndex, weekIndex := -1, -1
	for i, window := range candidates {
		if window == nil {
			continue
		}
		seconds, _ := floatValue(field(window, "limit_window_seconds", "limitWindowSeconds"))
		switch {
		case int(seconds) == 18000 && fiveIndex < 0:
			fiveIndex = i
		case int(seconds) == 604800 && weekIndex < 0:
			weekIndex = i
		}
	}
	// Without a window length, the primary window is the 5-hour one and the
	// secondary window is the weekly one.
	if fiveIndex < 0 && primary != nil && weekIndex != 0 {
		fiveIndex = 0
	}
	if weekIndex < 0 && secondary != nil && fiveIndex != 1 {
		weekIndex = 1
	}

	var windows []quota.Window
	if fiveIndex >= 0 {
		if w, ok := codexWindow(candidates[fiveIndex], quota.WindowFiveHour, quota.LabelFiveHour, now); ok {
			windows = append(windows, w)
		}
	}
	if weekIndex >= 0 {
		if w, ok := codexWindow(candidates[weekIndex], quota.WindowSevenDay, quota.LabelSevenDay, now); ok {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, errors.New("Codex 未返回可识别的额度窗口")
	}
	return []quota.RawGroup{{Key: "codex:main", SourceLabel: codexTitle, Windows: windows}}, nil
}

func codexWindow(window map[string]any, id, label string, now time.Time) (quota.Window, bool) {
	if window == nil {
		return quota.Window{}, false
	}
	used, ok := floatValue(field(window, "used_percent", "usedPercent"))
	if !ok {
		used = 0
		if reached, _ := field(window, "limit_reached", "limitReached").(bool); reached {
			used = 100
		}
	}
	reset := quota.ParseTime(field(window, "reset_at", "resetAt"))
	if reset.IsZero() {
		if after, ok := floatValue(field(window, "reset_after_seconds", "resetAfterSeconds")); ok {
			reset = now.Add(time.Duration(after * float64(time.Second))).UTC()
		}
	}
	return quota.Window{ID: id, Label: label, Remaining: clampPercent(100 - used), Reset: reset}, true
}

func parseCodexHeaders(header http.Header, now time.Time) (quota.RawGroup, bool) {
	type parsed struct {
		minutes float64
		window  quota.Window
	}
	var found []parsed
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		used, ok := floatValue(headerValue(header, prefix+"Used-Percent"))
		if !ok {
			continue
		}
		reset := quota.ParseTime(headerValue(header, prefix+"Reset-At"))
		if reset.IsZero() {
			if after, ok := floatValue(headerValue(header, prefix+"Reset-After-Seconds")); ok {
				reset = now.Add(time.Duration(after * float64(time.Second))).UTC()
			}
		}
		minutes, _ := floatValue(headerValue(header, prefix+"Window-Minutes"))
		found = append(found, parsed{minutes: minutes, window: quota.Window{Remaining: clampPercent(100 - used), Reset: reset}})
	}
	if len(found) == 0 {
		return quota.RawGroup{}, false
	}
	group := quota.RawGroup{Key: "codex:main", SourceLabel: codexTitle}
	haveFive, haveWeek := false, false
	assign := func(item parsed, id, label string) {
		item.window.ID = id
		item.window.Label = label
		group.Windows = append(group.Windows, item.window)
	}
	var unassigned []parsed
	for _, item := range found {
		switch {
		case int(item.minutes) == 300 && !haveFive:
			assign(item, quota.WindowFiveHour, quota.LabelFiveHour)
			haveFive = true
		case int(item.minutes) == 10080 && !haveWeek:
			assign(item, quota.WindowSevenDay, quota.LabelSevenDay)
			haveWeek = true
		default:
			unassigned = append(unassigned, item)
		}
	}
	// Without a window length, the primary window is the 5-hour one and the
	// secondary window is the weekly one.
	for _, item := range unassigned {
		switch {
		case !haveFive:
			assign(item, quota.WindowFiveHour, quota.LabelFiveHour)
			haveFive = true
		case !haveWeek:
			assign(item, quota.WindowSevenDay, quota.LabelSevenDay)
			haveWeek = true
		}
	}
	return group, len(group.Windows) > 0
}
