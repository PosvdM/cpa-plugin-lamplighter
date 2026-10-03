// Package models reads the CPA model list and orders ignition candidates.
//
// CPA has no host callback that lists the models of one credential, so the
// plugin reads /v1/models with a dedicated API key. That list is the union of
// all credentials, deduplicated by model ID, and its owned_by field can point
// at another provider when two providers serve the same ID. Candidates are
// therefore picked by name. When CPA rejects a candidate because the pinned
// credential does not serve it, the caller tries the next one.
package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Lister fetches and caches the CPA model list.
type Lister struct {
	Client *http.Client
	TTL    time.Duration

	mu      sync.Mutex
	cached  []string
	fetched time.Time
	key     string
}

// List returns the model IDs exposed by CPA at baseURL.
func (l *Lister) List(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("未配置 models_api_key，无法读取模型列表")
	}
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	cacheKey := baseURL + "\x00" + apiKey
	l.mu.Lock()
	if l.key == cacheKey && time.Since(l.fetched) < ttl && len(l.cached) > 0 {
		out := append([]string(nil), l.cached...)
		l.mu.Unlock()
		return out, nil
	}
	l.mu.Unlock()

	client := l.Client
	if client == nil {
		// The request goes to CPA itself, so it must not use a proxy.
		client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 CPA 模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 CPA 模型列表失败: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("CPA 模型列表格式错误: %w", err)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			ids = append(ids, id)
		}
	}
	l.mu.Lock()
	l.cached = append([]string(nil), ids...)
	l.fetched = time.Now()
	l.key = cacheKey
	l.mu.Unlock()
	return ids, nil
}

var digits = regexp.MustCompile(`\d+`)

func recencyKey(model string) []int {
	parts := digits.FindAllString(model, -1)
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, _ := strconv.Atoi(part)
		out = append(out, n)
	}
	return out
}

func newerFirst(a, b string) bool {
	ka, kb := recencyKey(a), recencyKey(b)
	for i := 0; i < len(ka) && i < len(kb); i++ {
		if ka[i] != kb[i] {
			return ka[i] > kb[i]
		}
	}
	if len(ka) != len(kb) {
		return len(ka) > len(kb)
	}
	return strings.ToLower(a) > strings.ToLower(b)
}

// newest returns the models matching predicate, newest first. Model families
// encode generation, version and date in numeric segments, so comparing the
// segments picks up new releases without listing them.
func newest(models []string, predicate func(string) bool) []string {
	var out []string
	for _, model := range models {
		if predicate(strings.ToLower(model)) {
			out = append(out, model)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return newerFirst(out[i], out[j]) })
	return out
}

func inOrder(models []string, predicates ...func(string) bool) []string {
	var out []string
	for _, predicate := range predicates {
		for _, model := range models {
			if predicate(strings.ToLower(model)) {
				out = append(out, model)
			}
		}
	}
	return out
}

func unique(models []string) []string {
	seen := make(map[string]bool, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		if !seen[model] {
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

// Candidates returns the models to try, in order, for one ignition target.
// groupLabel is the quota group, such as "Gemini" or "Claude / GPT" for
// Antigravity. A configured override is the only candidate and must exist in
// available.
func Candidates(provider, groupLabel, override string, available []string) ([]string, error) {
	if override != "" {
		for _, model := range available {
			if model == override {
				return []string{override}, nil
			}
		}
		return nil, fmt.Errorf("指定点火模型 %s 不在 CPA 模型列表中", override)
	}
	noThinking := func(m string) bool { return !strings.Contains(m, "thinking") }
	noImage := func(m string) bool { return !strings.Contains(m, "image") }

	var out []string
	switch provider {
	case "codex":
		out = append(out, newest(available, func(m string) bool { return strings.Contains(m, "luna") && noImage(m) })...)
		out = append(out, newest(available, func(m string) bool {
			return strings.HasPrefix(m, "gpt-") && !strings.Contains(m, "oss") && noImage(m)
		})...)
	case "claude":
		out = append(out, newest(available, func(m string) bool {
			return strings.HasPrefix(m, "claude-") && strings.Contains(m, "haiku") && noThinking(m)
		})...)
		out = append(out, newest(available, func(m string) bool {
			return strings.HasPrefix(m, "claude-") && strings.Contains(m, "sonnet") && noThinking(m)
		})...)
	case "antigravity":
		group := strings.ToLower(groupLabel)
		var candidates []string
		for _, model := range available {
			if noImage(strings.ToLower(model)) {
				candidates = append(candidates, model)
			}
		}
		if strings.Contains(group, "gemini") {
			gemini := newest(candidates, func(m string) bool { return strings.Contains(m, "gemini") })
			out = append(out, newest(gemini, func(m string) bool { return strings.Contains(m, "flash") })...)
			out = append(out, newest(gemini, func(m string) bool { return !strings.Contains(m, "pro") })...)
			out = append(out, newest(gemini, func(m string) bool { return strings.Contains(m, "pro") })...)
		}
		if strings.Contains(group, "claude") || strings.Contains(group, "gpt") {
			scoped := newest(candidates, func(m string) bool {
				return strings.Contains(m, "claude") || strings.Contains(m, "gpt")
			})
			out = append(out, inOrder(scoped,
				func(m string) bool { return strings.Contains(m, "haiku") && noThinking(m) },
				func(m string) bool { return strings.Contains(m, "sonnet") && noThinking(m) },
				func(m string) bool { return strings.Contains(m, "opus") && noThinking(m) },
				func(m string) bool { return strings.Contains(m, "gpt-oss") },
				noThinking,
			)...)
		}
	}
	out = unique(out)
	if len(out) == 0 {
		return nil, errors.New("CPA 模型列表中没有适合该凭证的点火模型")
	}
	return out, nil
}
