// Package notify sends Bark and Feishu notifications for quota changes,
// ignition pauses and Did Codex Reset signals.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Levels accepted by Bark.
const (
	LevelActive        = "active"
	LevelTimeSensitive = "timeSensitive"
)

// Message is one notification.
type Message struct {
	Title   string
	Body    string
	Level   string
	JumpURL string
	// Severity is the quota severity the message was raised for. It is empty
	// for messages that are not about quota; Feishu falls back to the state
	// emoji in the title to pick a colour for those.
	Severity string
	// Label is the quota group the message is about, for the event log.
	// It is not sent to Bark.
	Label string
}

// Sender delivers a message and reports whether the channel accepted it.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ErrNotConfigured means bark_url is empty.
var ErrNotConfigured = errors.New("未配置 bark_url")

// Bark sends messages to a Bark server.
type Bark struct {
	URL       string
	Group     string
	Icon      string
	UserAgent string
	Client    *http.Client
}

// escape matches Python's urllib.parse.quote(value, safe=""): every byte
// except unreserved characters is percent-encoded, spaces as %20.
func escape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// Send delivers msg with a GET request: {url}/{title}/{body}?group&level&icon&url.
func (b *Bark) Send(ctx context.Context, msg Message) error {
	base := strings.TrimRight(strings.TrimSpace(b.URL), "/")
	if base == "" {
		return ErrNotConfigured
	}
	params := url.Values{}
	if b.Group != "" {
		params.Set("group", b.Group)
	}
	level := msg.Level
	if level == "" {
		level = LevelActive
	}
	params.Set("level", level)
	if b.Icon != "" {
		params.Set("icon", b.Icon)
	}
	if msg.JumpURL != "" {
		params.Set("url", msg.JumpURL)
	}
	target := fmt.Sprintf("%s/%s/%s?%s", base, escape(msg.Title), escape(msg.Body), params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if b.UserAgent != "" {
		req.Header.Set("User-Agent", b.UserAgent)
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Bark 推送失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Bark HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var payload struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Code != nil && *payload.Code != 200 {
		return fmt.Errorf("Bark 返回 code=%d: %s", *payload.Code, truncate(string(raw), 200))
	}
	return nil
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}
