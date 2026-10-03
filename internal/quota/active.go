package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Upstream endpoints and headers. They match the requests sent by the CPA
// Management Center quota page.
const (
	CodexUsageURL      = "https://chatgpt.com/backend-api/wham/usage"
	CodexUserAgent     = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	ClaudeUsageURL     = "https://api.anthropic.com/api/oauth/usage"
	ClaudeUserAgent    = "claude-cli/2.1.280 (external, cli)"
	AntigravityAgent   = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	maxErrorBodyLength = 300
)

// AntigravityQuotaURLs are tried in order until one returns quota groups.
var AntigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// Request is one upstream HTTP request.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// Response is one upstream HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Doer sends an upstream request. proxyURL is the credential's own proxy and
// is empty when the credential has none.
type Doer interface {
	Do(ctx context.Context, proxyURL string, req Request) (Response, error)
}

// ErrUnauthorized means the upstream rejected the access token. CPA refreshes
// tokens in the background, so the caller skips this round.
var ErrUnauthorized = errors.New("上游返回 401，access token 可能刚过期，等待 CPA 刷新")

// FetchActive reads the quota windows of one credential from its provider.
func FetchActive(ctx context.Context, doer Doer, provider string, secret Secret, now time.Time) ([]RawGroup, error) {
	if secret.AccessToken == "" {
		return nil, errors.New("凭证文件中没有 access_token")
	}
	switch provider {
	case "codex":
		return fetchCodex(ctx, doer, secret, now)
	case "claude":
		return fetchClaude(ctx, doer, secret)
	case "antigravity":
		return fetchAntigravity(ctx, doer, secret)
	}
	return nil, fmt.Errorf("不支持的额度 provider: %s", provider)
}

func send(ctx context.Context, doer Doer, secret Secret, req Request) (map[string]any, error) {
	resp, err := doer.Do(ctx, secret.ProxyURL, req)
	if err != nil {
		return nil, err
	}
	if resp.Status == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.Status < 200 || resp.Status >= 300 {
		body := strings.TrimSpace(string(resp.Body))
		if len(body) > maxErrorBodyLength {
			body = body[:maxErrorBodyLength]
		}
		return nil, fmt.Errorf("上游 HTTP %d: %s", resp.Status, body)
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("上游返回不是 JSON object: %w", err)
	}
	return payload, nil
}

func bearer(secret Secret) http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+secret.AccessToken)
	header.Set("Content-Type", "application/json")
	return header
}

func fetchCodex(ctx context.Context, doer Doer, secret Secret, now time.Time) ([]RawGroup, error) {
	header := bearer(secret)
	header.Set("User-Agent", CodexUserAgent)
	if secret.AccountID != "" {
		header.Set("Chatgpt-Account-Id", secret.AccountID)
	}
	payload, err := send(ctx, doer, secret, Request{Method: http.MethodGet, URL: CodexUsageURL, Header: header})
	if err != nil {
		return nil, err
	}
	return ParseCodexUsage(payload, now)
}

// ParseCodexUsage converts a wham/usage response into the Codex group.
func ParseCodexUsage(payload map[string]any, now time.Time) ([]RawGroup, error) {
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

	var windows []Window
	if fiveIndex >= 0 {
		if w, ok := codexWindow(candidates[fiveIndex], WindowFiveHour, LabelFiveHour, now); ok {
			windows = append(windows, w)
		}
	}
	if weekIndex >= 0 {
		if w, ok := codexWindow(candidates[weekIndex], WindowSevenDay, LabelSevenDay, now); ok {
			windows = append(windows, w)
		}
	}
	if len(windows) == 0 {
		return nil, errors.New("Codex 未返回可识别的额度窗口")
	}
	return []RawGroup{{Key: "codex:main", SourceLabel: ProviderTitle("codex"), Windows: windows}}, nil
}

func codexWindow(window map[string]any, id, label string, now time.Time) (Window, bool) {
	if window == nil {
		return Window{}, false
	}
	used, ok := floatValue(field(window, "used_percent", "usedPercent"))
	if !ok {
		used = 0
		if reached, _ := field(window, "limit_reached", "limitReached").(bool); reached {
			used = 100
		}
	}
	reset := ParseTime(field(window, "reset_at", "resetAt"))
	if reset.IsZero() {
		if after, ok := floatValue(field(window, "reset_after_seconds", "resetAfterSeconds")); ok {
			reset = now.Add(time.Duration(after * float64(time.Second))).UTC()
		}
	}
	return Window{ID: id, Label: label, Remaining: clampPercent(100 - used), Reset: reset}, true
}

// claudeWindows maps oauth/usage keys to window IDs and labels.
var claudeWindows = []struct{ key, id, label string }{
	{"five_hour", WindowFiveHour, LabelFiveHour},
	{"seven_day", WindowSevenDay, LabelSevenDay},
	{"seven_day_oauth_apps", "seven-day-oauth-apps", "7 Day OAuth Apps"},
	{"seven_day_opus", "seven-day-opus", "7 Day Opus"},
	{"seven_day_sonnet", "seven-day-sonnet", "7 Day Sonnet"},
	{"seven_day_cowork", "seven-day-cowork", "7 Day Cowork"},
	{"iguana_necktie", WindowFable, LabelFable},
}

func fetchClaude(ctx context.Context, doer Doer, secret Secret) ([]RawGroup, error) {
	header := bearer(secret)
	header.Set("User-Agent", ClaudeUserAgent)
	header.Set("anthropic-beta", "oauth-2025-04-20")
	payload, err := send(ctx, doer, secret, Request{Method: http.MethodGet, URL: ClaudeUsageURL, Header: header})
	if err != nil {
		return nil, err
	}
	return ParseClaudeUsage(payload)
}

// ParseClaudeUsage converts an oauth/usage response into Claude groups. The
// 5-hour and 7-day windows form the main group; every other window, such as
// Fable, is a group of its own.
func ParseClaudeUsage(payload map[string]any) ([]RawGroup, error) {
	fable := findFableLimit(payload)
	var windows []Window
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
		windows = append(windows, Window{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - used),
			Reset:     ParseTime(item["resets_at"]),
		})
	}
	if fable != nil {
		if used, ok := floatValue(fable["percent"]); ok {
			windows = append(windows, Window{
				ID:        WindowFable,
				Label:     LabelFable,
				Remaining: clampPercent(100 - used),
				Reset:     ParseTime(fable["resets_at"]),
			})
		}
	}
	if len(windows) == 0 {
		return nil, errors.New("Claude 未返回可识别的额度窗口")
	}

	var groups []RawGroup
	var main []Window
	for _, w := range windows {
		if w.ID == WindowFiveHour || w.ID == WindowSevenDay {
			main = append(main, w)
		}
	}
	if len(main) > 0 {
		groups = append(groups, RawGroup{Key: "claude:main", SourceLabel: "Claude", Windows: main})
	}
	for _, w := range windows {
		if w.ID == WindowFiveHour || w.ID == WindowSevenDay {
			continue
		}
		label := "Claude"
		if w.ID == WindowFable {
			label = "Fable"
		}
		groups = append(groups, RawGroup{Key: "claude:" + w.ID, SourceLabel: label, Windows: []Window{w}})
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

func fetchAntigravity(ctx context.Context, doer Doer, secret Secret) ([]RawGroup, error) {
	if secret.ProjectID == "" {
		return nil, errors.New("Antigravity 凭证缺少 project_id")
	}
	body, _ := json.Marshal(map[string]string{"project": secret.ProjectID})
	var lastErr error
	for _, url := range AntigravityQuotaURLs {
		header := bearer(secret)
		header.Set("User-Agent", AntigravityAgent)
		payload, err := send(ctx, doer, secret, Request{Method: http.MethodPost, URL: url, Header: header, Body: body})
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return nil, err
			}
			lastErr = err
			continue
		}
		groups, err := ParseAntigravityQuota(payload)
		if err != nil {
			lastErr = err
			continue
		}
		return groups, nil
	}
	if lastErr == nil {
		lastErr = errors.New("Antigravity 未返回额度组")
	}
	return nil, lastErr
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
var spaces = regexp.MustCompile(`\s+`)

func antigravityGroupLabel(raw string) string {
	text := spaces.ReplaceAllString(strings.TrimSpace(raw), " ")
	switch strings.ToLower(text) {
	case "gemini models":
		return "Gemini"
	case "claude and gpt models":
		return "Claude / GPT"
	}
	if text == "" {
		return "Quota Group"
	}
	return text
}

func antigravityBucketLabel(bucket map[string]any) string {
	window := strings.ToLower(strings.TrimSpace(scalarString(bucket["window"])))
	raw := strings.TrimSpace(scalarString(field(bucket, "displayName", "display_name")))
	low := strings.ToLower(raw)
	switch {
	case window == "5h" || window == "five-hour" || window == "five_hour" ||
		low == "5 hour limit" || low == "5-hour limit" || low == "five hour limit":
		return LabelFiveHour
	case window == "weekly" || window == "week" || low == "weekly limit":
		return LabelSevenDay
	case raw != "":
		return raw
	case window != "":
		return window
	}
	return "额度"
}

func parseFraction(value any) (float64, bool) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if strings.HasSuffix(text, "%") {
			f, ok := floatValue(strings.TrimSpace(strings.TrimSuffix(text, "%")))
			if !ok {
				return 0, false
			}
			return min(max(f/100, 0), 1), true
		}
	}
	f, ok := floatValue(value)
	if !ok {
		return 0, false
	}
	if f > 1 {
		f /= 100
	}
	return min(max(f, 0), 1), true
}

// ParseAntigravityQuota converts a retrieveUserQuotaSummary response into
// one group per quota group, such as Gemini and Claude / GPT.
func ParseAntigravityQuota(payload map[string]any) ([]RawGroup, error) {
	if _, ok := payload["groups"].([]any); !ok {
		switch body := payload["body"].(type) {
		case map[string]any:
			payload = body
		case string:
			var inner map[string]any
			if err := json.Unmarshal([]byte(body), &inner); err == nil {
				payload = inner
			}
		}
	}
	rawGroups, ok := payload["groups"].([]any)
	if !ok {
		return nil, errors.New("Antigravity 返回中没有 groups")
	}
	var groups []RawGroup
	for gi, raw := range rawGroups {
		group, _ := raw.(map[string]any)
		if group == nil {
			continue
		}
		rawLabel := scalarString(field(group, "displayName", "display_name"))
		label := antigravityGroupLabel(rawLabel)
		buckets, _ := group["buckets"].([]any)
		type ordered struct {
			order int
			w     Window
		}
		var windows []ordered
		for bi, rawBucket := range buckets {
			bucket, _ := rawBucket.(map[string]any)
			if bucket == nil {
				continue
			}
			fraction, ok := parseFraction(field(bucket, "remainingFraction", "remaining_fraction"))
			if !ok {
				continue
			}
			windowRaw := strings.ToLower(strings.TrimSpace(scalarString(bucket["window"])))
			id := scalarString(field(bucket, "bucketId", "bucket_id"))
			if id == "" {
				prefix := windowRaw
				if prefix == "" {
					prefix = "bucket"
				}
				id = fmt.Sprintf("%s-%d", prefix, bi+1)
			}
			order := 9
			switch windowRaw {
			case "5h", "five-hour", "five_hour":
				order = 0
			case "weekly", "week":
				order = 1
			}
			windows = append(windows, ordered{order: order, w: Window{
				ID:        id,
				Label:     antigravityBucketLabel(bucket),
				Remaining: clampPercent(fraction * 100),
				Reset:     ParseTime(field(bucket, "resetTime", "reset_time")),
			}})
		}
		if len(windows) == 0 {
			continue
		}
		sort.SliceStable(windows, func(i, j int) bool {
			if windows[i].order != windows[j].order {
				return windows[i].order < windows[j].order
			}
			return windows[i].w.Label < windows[j].w.Label
		})
		keySource := rawLabel
		if keySource == "" {
			keySource = label
		}
		safeKey := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(keySource), "-"), "-")
		if safeKey == "" {
			safeKey = fmt.Sprintf("group-%d", gi+1)
		}
		out := RawGroup{Key: "antigravity:" + safeKey, SourceLabel: label}
		for _, item := range windows {
			out.Windows = append(out.Windows, item.w)
		}
		groups = append(groups, out)
	}
	if len(groups) == 0 {
		return nil, errors.New("Antigravity 未返回可识别的额度窗口")
	}
	return groups, nil
}
