package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// AntigravityAgent is the User-Agent of Antigravity quota requests, the same
// as the CPA Management Center quota page.
const AntigravityAgent = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"

// AntigravityQuotaURLs are tried in order until one returns quota groups.
var AntigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

var antigravity = &Provider{
	ID:      "antigravity",
	Title:   "Antigravity",
	Monitor: true,
	Fetch:   fetchAntigravity,
	Ignition: &Ignition{
		Protocol: "openai",
		Body: func(model string) map[string]any {
			return chatBody(model, map[string]any{"temperature": 0, "reasoning_effort": "none"})
		},
		Candidates: antigravityCandidates,
		Hint:       Text{Zh: "最新的 Flash", En: "latest Flash"},
		// The settings show the Gemini model, which matches "latest Flash".
		HintGroup: "Gemini",
		Groups:    []string{"Gemini", "Claude / GPT"},
		GroupsHint: Text{
			Zh: "Antigravity 按模型分成多个独立的 5 小时额度组。只有勾选的额度组会自动点火；Claude / GPT 额度组较小，默认不点。",
			En: "Antigravity splits quota into separate 5-hour groups by model. Only checked groups are ignited; the Claude / GPT group is small and off by default.",
		},
		// The Claude / GPT group is much smaller than the Gemini one, so only
		// Gemini is ignited unless groups says otherwise.
		DefaultGroups: []string{"Gemini"},
	},
}

// antigravityCandidates orders the models of one quota group: Flash first
// for Gemini; Haiku, Sonnet, Opus and GPT-OSS for Claude / GPT.
func antigravityCandidates(groupLabel string, available []string) []string {
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	group := strings.ToLower(groupLabel)
	var candidates []string
	for _, model := range available {
		if !strings.Contains(strings.ToLower(model), "image") {
			candidates = append(candidates, model)
		}
	}
	var out []string
	if strings.Contains(group, "gemini") {
		gemini := models.Newest(candidates, func(m string) bool { return strings.Contains(m, "gemini") })
		out = append(out, models.Newest(gemini, func(m string) bool { return strings.Contains(m, "flash") })...)
		out = append(out, models.Newest(gemini, func(m string) bool { return !strings.Contains(m, "pro") })...)
		out = append(out, models.Newest(gemini, func(m string) bool { return strings.Contains(m, "pro") })...)
	}
	if strings.Contains(group, "claude") || strings.Contains(group, "gpt") {
		scoped := models.Newest(candidates, func(m string) bool {
			return strings.Contains(m, "claude") || strings.Contains(m, "gpt")
		})
		out = append(out, models.InOrder(scoped,
			func(m string) bool { return strings.Contains(m, "haiku") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "sonnet") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "opus") && noThinking(m) },
			func(m string) bool { return strings.Contains(m, "gpt-oss") },
			noThinking,
		)...)
	}
	return out
}

func fetchAntigravity(ctx context.Context, doer quota.Doer, cred Credential, _ time.Time) ([]quota.RawGroup, error) {
	if cred.ProjectID == "" {
		return nil, errors.New("Antigravity 凭证缺少 project_id")
	}
	body, _ := json.Marshal(map[string]string{"project": cred.ProjectID})
	var lastErr error
	for _, url := range AntigravityQuotaURLs {
		header := bearer(cred)
		header.Set("User-Agent", AntigravityAgent)
		payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodPost, URL: url, Header: header, Body: body})
		if err != nil {
			if errors.Is(err, quota.ErrUnauthorized) {
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
		return quota.LabelFiveHour
	case window == "weekly" || window == "week" || low == "weekly limit":
		return quota.LabelSevenDay
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
func ParseAntigravityQuota(payload map[string]any) ([]quota.RawGroup, error) {
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
	var groups []quota.RawGroup
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
			w     quota.Window
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
			windows = append(windows, ordered{order: order, w: quota.Window{
				ID:        id,
				Label:     antigravityBucketLabel(bucket),
				Remaining: clampPercent(fraction * 100),
				Reset:     quota.ParseTime(field(bucket, "resetTime", "reset_time")),
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
		out := quota.RawGroup{Key: "antigravity:" + safeKey, SourceLabel: label}
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
