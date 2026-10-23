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
	"sync"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"gopkg.in/yaml.v3"
)

// The User-Agent of Antigravity quota requests follows the CPA Antigravity
// executor: the credential's user_agent when set, otherwise the Antigravity
// Hub UA with the latest Hub version from the Hub update manifest. Cloud Code
// leaves newer model groups, such as Gemini, out of the quota of older client
// UAs.
const (
	AntigravityHubManifestURL = "https://antigravity-hub-auto-updater-974169037036.us-central1.run.app/manifest/latest-arm64-mac.yml"
	// antigravityFallbackVersion is used until the manifest has been read,
	// the same as the CPA Management Center.
	antigravityFallbackVersion = "2.23.0"
	antigravityHubPlatform     = "darwin/arm64"
	// The manifest is read again after antigravityVersionTTL, and after
	// antigravityVersionRetry when a read fails.
	antigravityVersionTTL   = 3 * time.Hour
	antigravityVersionRetry = 30 * time.Minute
)

var antigravityHub hubVersion

// hubVersion caches the latest Antigravity Hub version.
type hubVersion struct {
	mu      sync.Mutex
	version string
	next    time.Time
}

// get returns the cached version, reading the manifest first when it is
// due. A failed read keeps the cached version.
func (h *hubVersion) get(ctx context.Context, doer quota.Doer, now time.Time) string {
	h.mu.Lock()
	due := !now.Before(h.next)
	if due {
		// Other requests keep the cached version while this one reads.
		h.next = now.Add(antigravityVersionRetry)
	}
	h.mu.Unlock()
	if due {
		if version, err := fetchHubVersion(ctx, doer); err == nil {
			h.mu.Lock()
			h.version, h.next = version, now.Add(antigravityVersionTTL)
			h.mu.Unlock()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.version == "" {
		return antigravityFallbackVersion
	}
	return h.version
}

var hubSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// fetchHubVersion reads the version from the Hub update manifest with the
// same headers as CPA, through the CPA egress.
func fetchHubVersion(ctx context.Context, doer quota.Doer) (string, error) {
	header := http.Header{}
	header.Set("User-Agent", "electron-builder")
	header.Set("Cache-Control", "no-cache")
	resp, err := doer.Do(ctx, "", quota.Request{Method: http.MethodGet, URL: AntigravityHubManifestURL, Header: header})
	if err != nil {
		return "", err
	}
	if resp.Status != http.StatusOK {
		return "", fmt.Errorf("Antigravity Hub 更新清单返回 HTTP %d", resp.Status)
	}
	var manifest struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(resp.Body, &manifest); err != nil {
		return "", err
	}
	version := strings.TrimSpace(manifest.Version)
	if !hubSemver.MatchString(version) {
		return "", fmt.Errorf("Antigravity Hub 更新清单的版本无效: %q", version)
	}
	return version, nil
}

// antigravityUserAgent returns the User-Agent CPA uses for the credential.
func antigravityUserAgent(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) string {
	attributes, _ := cred.Data["attributes"].(map[string]any)
	metadata, _ := cred.Data["metadata"].(map[string]any)
	ua := firstString(attributes, cred.Data, "user_agent")
	if ua == "" {
		ua = firstString(metadata, nil, "user_agent")
	}
	if ua == "" {
		return "antigravity/hub/" + antigravityHub.get(ctx, doer, now) + " " + antigravityHubPlatform
	}
	// Like CPA, a long Antigravity UA is cut to its short form.
	if strings.HasPrefix(strings.ToLower(ua), "antigravity/") {
		if i := strings.Index(strings.ToLower(ua), " google-api-nodejs-client/"); i > 0 {
			ua = strings.TrimSpace(ua[:i])
		}
	}
	return ua
}

// AntigravityQuotaURLs are tried in order until one returns quota groups.
var AntigravityQuotaURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

// AntigravityLoadCodeAssistURL states the subscription of an account, the
// endpoint CPA reads Antigravity credits from.
const AntigravityLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

var antigravity = &Provider{
	ID:      "antigravity",
	Title:   "Antigravity",
	Monitor: true,
	Fetch:   fetchAntigravity,
	Plan:    antigravityPlan,
	Profile: fetchAntigravityProfile,
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

func fetchAntigravity(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) ([]quota.RawGroup, error) {
	if cred.ProjectID == "" {
		return nil, errors.New("Antigravity 凭证缺少 project_id")
	}
	body, _ := json.Marshal(map[string]string{"project": cred.ProjectID})
	agent := antigravityUserAgent(ctx, doer, cred, now)
	var lastErr error
	for _, url := range AntigravityQuotaURLs {
		header := bearer(cred)
		header.Set("User-Agent", agent)
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

// fetchAntigravityProfile reads the plan of an Antigravity account with the
// request body and User-Agent CPA uses for loadCodeAssist.
func fetchAntigravityProfile(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) (string, error) {
	body, _ := json.Marshal(map[string]any{"metadata": map[string]string{"ideType": "ANTIGRAVITY"}})
	header := bearer(cred)
	header.Set("User-Agent", antigravityUserAgent(ctx, doer, cred, now))
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodPost, URL: AntigravityLoadCodeAssistURL, Header: header, Body: body})
	if err != nil {
		return "", err
	}
	return ParseAntigravityProfile(payload)
}

// ParseAntigravityProfile returns the raw plan of a loadCodeAssist response:
// the id of paidTier, such as "g1-pro-tier", or, without a paid plan, the id
// of currentTier, such as "free-tier". currentTier stays "free-tier" on a
// Google AI Pro account, so paidTier decides.
func ParseAntigravityProfile(payload map[string]any) (string, error) {
	for _, name := range []string{"paidTier", "currentTier"} {
		tier, _ := payload[name].(map[string]any)
		if id := strings.TrimSpace(scalarString(tier["id"])); id != "" {
			return id, nil
		}
	}
	return "", errors.New("Antigravity 账号资料中没有套餐")
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
			order, period := 9, time.Duration(0)
			switch windowRaw {
			case "5h", "five-hour", "five_hour":
				order, period = 0, quota.FiveHours
			case "weekly", "week":
				order, period = 1, quota.SevenDays
			}
			windows = append(windows, ordered{order: order, w: quota.Window{
				ID:        id,
				Label:     antigravityBucketLabel(bucket),
				Remaining: clampPercent(fraction * 100),
				Reset:     quota.ParseTime(field(bucket, "resetTime", "reset_time")),
				Period:    period,
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
