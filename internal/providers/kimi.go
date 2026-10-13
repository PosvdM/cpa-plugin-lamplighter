package providers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/models"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Kimi usage endpoints, the same as the CPA Management Center quota page.
// Accounts of the international site (kimi.ai) use the second one.
const (
	KimiUsageURL   = "https://api.kimi.com/coding/v1/usages"
	KimiAIUsageURL = "https://api.kimi.ai/coding/v1/usages"
	kimiTitle      = "Kimi"
)

// kimi has not been tried with a real account yet: requests and parsing
// follow the CPA Management Center.
var kimi = &Provider{
	ID:      "kimi",
	Aliases: []string{"kimi-ai", "kimi.ai", "kimi.com"},
	Title:   kimiTitle,
	Monitor: true,
	Ignite:  true,
	Fetch:   fetchKimi,
	Ignition: &Ignition{
		// CPA's Kimi executor speaks the OpenAI protocol, so the request
		// needs no translation.
		Protocol:   "openai",
		Body:       func(model string) map[string]any { return chatBody(model, nil) },
		Candidates: kimiCandidates,
		Hint:       Text{Zh: "最新的 Kimi", En: "latest Kimi"},
	},
}

// kimiCandidates prefers the newest Kimi model without thinking or a
// high-speed variant.
func kimiCandidates(_ string, available []string) []string {
	return models.Newest(available, func(m string) bool {
		return strings.HasPrefix(m, "kimi-") && !strings.Contains(m, "thinking") && !strings.Contains(m, "highspeed")
	})
}

func fetchKimi(ctx context.Context, doer quota.Doer, cred Credential, now time.Time) ([]quota.RawGroup, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodGet, URL: kimiUsageURL(cred), Header: header})
	if err != nil {
		return nil, err
	}
	return ParseKimiUsage(payload, now)
}

// kimiUsageURL picks the endpoint like CPA's credential loader: an explicit
// domain, then base_url, then the credential type and provider name.
func kimiUsageURL(cred Credential) string {
	if kimiDomain(cred) == "ai" {
		return KimiAIUsageURL
	}
	return KimiUsageURL
}

func kimiDomain(cred Credential) string {
	data := cred.Data
	if domain := scalarString(data["domain"]); domain != "" {
		if d := kimiDomainOf(domain); d != "" {
			return d
		}
		return "com"
	}
	baseURL, present := data["base_url"]
	if !present {
		baseURL = data["base-url"]
	}
	if u, err := url.Parse(scalarString(baseURL)); err == nil && u.Hostname() != "" {
		if d := kimiDomainOf(u.Hostname()); d != "" {
			return d
		}
	}
	for _, value := range []string{scalarString(data["type"]), strings.ReplaceAll(cred.Provider, "_", "-")} {
		if d := kimiDomainOf(value); d != "" {
			return d
		}
	}
	return "com"
}

func kimiDomainOf(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "ai" || value == "kimi-ai" || value == "kimi.ai" || strings.HasSuffix(value, ".kimi.ai"):
		return "ai"
	case value == "com" || value == "kimi" || value == "kimi.com" || strings.HasSuffix(value, ".kimi.com"):
		return "com"
	}
	return ""
}

// ParseKimiUsage converts a coding/v1/usages response into the Kimi group.
// Each item of limits is a window whose length is stated in window.duration
// and window.timeUnit (TIME_UNIT_MINUTE and so on); usage is the weekly
// total. Values are counts, not percentages. The monthly share that plans
// without a weekly limit report in usages is not read yet.
func ParseKimiUsage(payload map[string]any, now time.Time) ([]quota.RawGroup, error) {
	group := quota.RawGroup{Key: "kimi:main", SourceLabel: kimiTitle}
	used := map[string]bool{}
	add := func(w quota.Window, fallbackID string) {
		if id, label, ok := quota.Standard(w.Period); ok && !used[id] {
			w.ID, w.Label = id, label
		} else {
			w.ID = fallbackID
		}
		used[w.ID] = true
		group.Windows = append(group.Windows, w)
	}
	limits, _ := payload["limits"].([]any)
	for i, raw := range limits {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		detail, _ := item["detail"].(map[string]any)
		if detail == nil {
			detail = item
		}
		window, _ := item["window"].(map[string]any)
		duration, _ := floatValue(firstValue(field(window, "duration"), field(item, "duration"), field(detail, "duration")))
		unit := scalarString(firstValue(field(window, "timeUnit"), field(item, "timeUnit"), field(detail, "timeUnit")))
		name := ""
		for _, key := range []string{"name", "title", "scope"} {
			if name = scalarString(firstValue(field(item, key), field(detail, key))); name != "" {
				break
			}
		}
		w, ok := kimiWindow(detail, name, kimiPeriod(duration, unit, name), now)
		if !ok {
			continue
		}
		if w.Label == "" {
			w.Label = "Limit " + strconv.Itoa(i+1)
		}
		add(w, "limit-"+strconv.Itoa(i))
	}
	if usage, _ := payload["usage"].(map[string]any); usage != nil {
		if w, ok := kimiWindow(usage, "", quota.SevenDays, now); ok {
			add(w, "weekly-total")
		}
	}
	if len(group.Windows) == 0 {
		return nil, errors.New("Kimi 未返回可识别的额度窗口")
	}
	return []quota.RawGroup{group}, nil
}

// kimiWindow reads the used and limit counts and the reset of one window.
// It returns false when the window has no limit to compare with.
func kimiWindow(data map[string]any, name string, period time.Duration, now time.Time) (quota.Window, bool) {
	limit, ok := floatValue(data["limit"])
	if !ok || limit <= 0 {
		return quota.Window{}, false
	}
	used, ok := floatValue(data["used"])
	if !ok {
		remaining, hasRemaining := floatValue(data["remaining"])
		if !hasRemaining {
			return quota.Window{}, false
		}
		used = limit - remaining
	}
	reset := quota.ParseTime(field(data, "reset_at", "resetAt", "reset_time", "resetTime"))
	if reset.IsZero() {
		if after, ok := floatValue(field(data, "reset_in", "resetIn", "ttl")); ok && after > 0 {
			reset = now.Add(time.Duration(after * float64(time.Second))).UTC()
		}
	}
	if label := scalarString(firstValue(data["name"], data["title"])); label != "" {
		name = label
	}
	if name == "" && period > 0 {
		name = quota.PeriodLabel(period)
	}
	return quota.Window{Label: name, Remaining: clampPercent(100 - used/limit*100), Reset: reset, Period: period}, true
}

// kimiPeriod returns the window length from a duration and a unit such as
// TIME_UNIT_MINUTE, where no unit means minutes, or from words in the name.
func kimiPeriod(duration float64, unit, name string) time.Duration {
	if duration > 0 {
		unit = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(unit)), "TIME_UNIT_")
		step := time.Minute
		switch strings.TrimSuffix(unit, "S") {
		case "SECOND":
			step = time.Second
		case "HOUR":
			step = time.Hour
		case "DAY":
			step = 24 * time.Hour
		case "WEEK":
			step = quota.SevenDays
		}
		return time.Duration(duration * float64(step))
	}
	text := strings.ToLower(name)
	switch {
	case strings.Contains(text, "day"):
		return 24 * time.Hour
	case strings.Contains(text, "week"):
		return quota.SevenDays
	case strings.Contains(text, "month"):
		return 30 * 24 * time.Hour
	case strings.Contains(text, "5h") || strings.Contains(text, "hour"):
		return quota.FiveHours
	}
	return 0
}

func firstValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
