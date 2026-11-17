package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Fingerprint identifies the login behind a CPA credential file, or returns
// "" when the file holds no secret it knows. It hashes the refresh token,
// else the API key, else the access token.
//
// CPA writes credential files on its own: every token refresh replaces the
// access token, and after an upstream error CPA adds fields such as
// tos_appeal_url. Neither the modification time nor the whole file tells a
// new login apart from that. Google, as used by Antigravity, keeps the
// refresh token across refreshes and issues a new one with a new login.
// Providers that rotate refresh tokens, such as Claude, change the
// fingerprint with a successful refresh, which a rejected credential
// cannot do.
func Fingerprint(raw json.RawMessage) string {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	metadata, _ := data["metadata"].(map[string]any)
	for _, keys := range [][]string{{"refresh_token", "refreshToken"}, {"api_key", "apiKey"}, {"access_token", "accessToken"}} {
		if value := firstString(data, metadata, keys...); value != "" {
			sum := sha256.Sum256([]byte(keys[0] + "\x00" + value))
			return hex.EncodeToString(sum[:8])
		}
	}
	return ""
}

// Credential holds the fields Lamplighter reads from a credential file.
type Credential struct {
	// Provider is the provider name CPA reports for the credential, which
	// can be an alias such as "kimi-ai". ID and Name are its runtime ID and
	// file name in CPA.
	Provider    string
	ID          string
	Name        string
	AccessToken string
	AccountID   string
	ProjectID   string
	ProxyURL    string
	// Data is the whole credential file, for fields only one provider reads.
	Data map[string]any
}

// ParseCredential extracts the access token, account ID, project ID and proxy
// URL from a CPA credential file.
func ParseCredential(raw json.RawMessage) Credential {
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Credential{}
	}
	metadata, _ := data["metadata"].(map[string]any)
	return Credential{
		AccessToken: firstString(data, metadata, "access_token", "accessToken"),
		AccountID:   firstString(data, metadata, "account_id", "chatgpt_account_id", "chatgptAccountId"),
		ProjectID:   projectID(data),
		ProxyURL:    firstString(data, nil, "proxy_url", "proxyUrl"),
		Data:        data,
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
