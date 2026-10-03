// Package quota reads Codex, Claude and Antigravity quota windows.
//
// Active reads call the provider usage endpoints with the same URLs and
// headers as the CPA Management Center. Passive reads parse the rate-limit
// headers that CPA records from real model responses.
package quota

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Window labels. Notifications shorten them with ShortLabel.
const (
	LabelFiveHour  = "5 小时"
	LabelSevenDay  = "7 Day"
	LabelFable     = "7 Day Fable 5"
	WindowFiveHour = "five-hour"
	WindowSevenDay = "seven-day"
	WindowFable    = "seven-day-fable"
)

// Source marks where a window value came from.
type Source string

const (
	SourceActive  Source = "active"
	SourcePassive Source = "passive"
)

// Window is one quota window. Remaining is a percentage from 0 to 100.
// Reset is zero when the provider did not return a reset time.
type Window struct {
	ID        string
	Label     string
	Remaining float64
	Reset     time.Time
}

// RawGroup is a set of windows as returned by one provider, before it is
// attached to a credential.
type RawGroup struct {
	Key         string
	SourceLabel string
	Windows     []Window
}

// ShortLabel returns the compact window label used in notifications.
func ShortLabel(label string) string {
	switch label {
	case LabelFiveHour:
		return "5h"
	case LabelSevenDay, LabelFable:
		return "7d"
	}
	return label
}

// IsFiveHour reports whether w is a 5-hour window.
func IsFiveHour(w Window) bool {
	return w.ID == WindowFiveHour || ShortLabel(w.Label) == "5h"
}

// IsSevenDay reports whether w is a 7-day window.
func IsSevenDay(w Window) bool {
	return ShortLabel(w.Label) == "7d" || strings.HasPrefix(w.Label, "7 Day")
}

// FiveHourWindow returns the first 5-hour window of g.
func FiveHourWindow(windows []Window) (Window, bool) {
	for _, w := range windows {
		if IsFiveHour(w) {
			return w, true
		}
	}
	return Window{}, false
}

// ProviderTitle returns the display name of provider.
func ProviderTitle(provider string) string {
	switch provider {
	case "codex":
		return "ChatGPT"
	case "claude":
		return "Claude"
	case "antigravity":
		return "Antigravity"
	}
	if provider == "" {
		return ""
	}
	return strings.ToUpper(provider[:1]) + provider[1:]
}

// Secret holds the fields Lamplighter reads from a credential file.
type Secret struct {
	AccessToken string
	AccountID   string
	ProjectID   string
	ProxyURL    string
}

// ParseSecret extracts the access token, account ID, project ID and proxy URL
// from a CPA credential file.
func ParseSecret(raw json.RawMessage) Secret {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Secret{}
	}
	metadata, _ := data["metadata"].(map[string]any)
	return Secret{
		AccessToken: firstString(data, metadata, "access_token", "accessToken"),
		AccountID:   firstString(data, metadata, "account_id", "chatgpt_account_id", "chatgptAccountId"),
		ProjectID:   projectID(data),
		ProxyURL:    firstString(data, nil, "proxy_url", "proxyUrl"),
	}
}

func firstString(primary, secondary map[string]any, keys ...string) string {
	for _, source := range []map[string]any{primary, secondary} {
		if source == nil {
			continue
		}
		for _, key := range keys {
			if value := scalarString(source[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func projectID(data map[string]any) string {
	for _, key := range []string{"project_id", "projectId"} {
		if value := scalarString(data[key]); value != "" {
			return value
		}
	}
	for _, childKey := range []string{"metadata", "attributes", "installed", "web"} {
		child, ok := data[childKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"project_id", "projectId", "gemini_virtual_project"} {
			if value := scalarString(child[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// ParseTime parses an ISO 8601 timestamp, a Unix timestamp in seconds or
// milliseconds, or a numeric string holding either. It returns the zero time
// when value cannot be parsed.
func ParseTime(value any) time.Time {
	switch v := value.(type) {
	case nil:
		return time.Time{}
	case float64:
		return unixTime(v)
	case int64:
		return unixTime(float64(v))
	case int:
		return unixTime(float64(v))
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return unixTime(f)
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return unixTime(f)
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func unixTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 10_000_000_000 {
		seconds /= 1000
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * 1e9)
	return time.Unix(whole, nanos).UTC()
}

func floatValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func field(obj map[string]any, names ...string) any {
	if obj == nil {
		return nil
	}
	for _, name := range names {
		if value, ok := obj[name]; ok && value != nil {
			return value
		}
	}
	return nil
}
