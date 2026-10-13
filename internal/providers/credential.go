package providers

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Credential holds the fields Lamplighter reads from a credential file.
type Credential struct {
	// Provider is the provider name CPA reports for the credential, which
	// can be an alias such as "kimi-ai".
	Provider    string
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
