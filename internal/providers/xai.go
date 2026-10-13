package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// Grok billing endpoint and headers, the same as the CPA Management Center
// quota page. Grok rejects client versions older than 1.0.13.
const (
	XAIBillingURL    = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"
	xaiClientVersion = "1.0.46"
	xaiUserAgent     = "grok-pager/" + xaiClientVersion + " grok-shell/" + xaiClientVersion + " (macos; aarch64)"
	xaiTitle         = "Grok"
)

// xai is Grok. The weekly credit window starts when the previous one ends,
// whether or not the account is used, so there is nothing to ignite.
var xai = &Provider{
	ID:      "xai",
	Aliases: []string{"x-ai", "x.ai", "grok"},
	Title:   xaiTitle,
	Monitor: true,
	Fetch:   fetchXAI,
}

func fetchXAI(ctx context.Context, doer quota.Doer, cred Credential, _ time.Time) ([]quota.RawGroup, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	header.Set("x-xai-token-auth", "xai-grok-cli")
	header.Set("x-grok-client-version", xaiClientVersion)
	header.Set("Accept", "*/*")
	header.Set("User-Agent", xaiUserAgent)
	metadata, _ := cred.Data["metadata"].(map[string]any)
	if user := firstString(cred.Data, metadata, "sub", "subject", "user_id", "userId"); user != "" {
		header.Set("x-userid", user)
	}
	payload, err := send(ctx, doer, cred, quota.Request{Method: http.MethodGet, URL: XAIBillingURL, Header: header})
	if err != nil {
		return nil, err
	}
	return ParseXAIBilling(payload)
}

// ParseXAIBilling converts a billing?format=credits response into the Grok
// group. creditUsagePercent is the used share of the current period, which
// is weekly for subscriptions. Per-product usage is part of that share and
// is not a quota of its own.
func ParseXAIBilling(payload map[string]any) ([]quota.RawGroup, error) {
	config, _ := payload["config"].(map[string]any)
	if config == nil {
		return nil, errors.New("Grok 未返回 config")
	}
	used, ok := floatValue(field(config, "creditUsagePercent", "credit_usage_percent"))
	if !ok {
		return nil, errors.New("Grok 未返回额度用量；按 API 用量计费的账号没有额度可查")
	}
	// The billing period is monthly and only describes the credits when the
	// response has no current period.
	period, _ := field(config, "currentPeriod", "current_period").(map[string]any)
	var start, end time.Time
	if period != nil {
		start, end = quota.ParseTime(field(period, "start")), quota.ParseTime(field(period, "end"))
	} else {
		start = quota.ParseTime(field(config, "billingPeriodStart", "billing_period_start"))
		end = quota.ParseTime(field(config, "billingPeriodEnd", "billing_period_end"))
	}
	var length time.Duration
	switch {
	case strings.Contains(strings.ToLower(scalarString(field(period, "type"))), "weekly"):
		length = quota.SevenDays
	case !start.IsZero() && end.After(start):
		length = end.Sub(start)
	}
	// A period of another length, such as a calendar month, keeps one ID so
	// that its history and alerts continue from one period to the next.
	id, label, ok := quota.Standard(length)
	if !ok {
		id, label = "credits", xaiTitle
	}
	return []quota.RawGroup{{Key: "xai:main", SourceLabel: xaiTitle, Windows: []quota.Window{{
		ID:        id,
		Label:     label,
		Remaining: clampPercent(100 - used),
		Reset:     end,
		Period:    length,
	}}}}, nil
}
