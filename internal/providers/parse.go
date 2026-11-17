package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
)

// maxErrorBodyLength is how much of an error response is kept for
// failure.Diagnose; quota.HTTPError shows less of it.
const maxErrorBodyLength = 4096

func send(ctx context.Context, doer quota.Doer, cred Credential, req quota.Request) (map[string]any, error) {
	resp, err := doer.Do(ctx, cred.ProxyURL, req)
	if err != nil {
		return nil, err
	}
	// A 401 also keeps its body: it can name a disabled account, such as
	// OpenAI's account_deactivated. errors.Is(err, quota.ErrUnauthorized)
	// still holds.
	if resp.Status < 200 || resp.Status >= 300 {
		body := strings.TrimSpace(string(resp.Body))
		if len(body) > maxErrorBodyLength {
			body = body[:maxErrorBodyLength]
		}
		return nil, &quota.HTTPError{Status: resp.Status, Body: body}
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("上游返回不是 JSON object: %w", err)
	}
	return payload, nil
}

func bearer(cred Credential) http.Header {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	header.Set("Content-Type", "application/json")
	return header
}

// headerValue returns the last value of name, matching the name case-insensitively.
func headerValue(header http.Header, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[len(values)-1])
		}
	}
	return ""
}

// clampPercent limits value to 0–100 and drops float noise such as
// 19.999999999999996 from 100 - 0.8*100. It keeps six decimals, because
// Antigravity reports fractions like 0.9999985 that must stay below 100.
func clampPercent(value float64) float64 {
	value = math.Round(value*1e6) / 1e6
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
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
