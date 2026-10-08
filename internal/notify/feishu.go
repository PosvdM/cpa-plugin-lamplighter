package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrFeishuNotConfigured means feishu_webhook is empty.
var ErrFeishuNotConfigured = errors.New("未配置 feishu_webhook")

// Card header colours accepted by Feishu. The mapping follows the same
// thresholds the Management Center uses for quota.
const (
	feishuBlue   = "blue"
	feishuGreen  = "green"
	feishuYellow = "yellow"
	feishuOrange = "orange"
	feishuRed    = "red"
)

// feishuLabelMax bounds how long the left half of a "label：value" line may
// be before it is treated as prose instead of a field.
const feishuLabelMax = 16

// Feishu sends messages to a Feishu (Lark) custom bot webhook. Messages are
// rendered as interactive cards; a card the bot refuses is retried as plain
// text so a formatting problem can never swallow an alert.
type Feishu struct {
	Webhook   string
	Secret    string
	UserAgent string
	Client    *http.Client
	// Loc formats the card footer. Nil uses the process time zone.
	Loc *time.Location
	// Now is overridable for tests. Nil uses time.Now.
	Now func() time.Time
}

// Send delivers msg to the webhook.
func (f *Feishu) Send(ctx context.Context, msg Message) error {
	webhook := strings.TrimSpace(f.Webhook)
	if webhook == "" {
		return ErrFeishuNotConfigured
	}
	cardErr := f.post(ctx, webhook, f.cardPayload(msg))
	if cardErr == nil {
		return nil
	}
	// A card Feishu will not render must not swallow the alert, so fall back
	// to plain text before reporting failure.
	if textErr := f.post(ctx, webhook, f.textPayload(msg)); textErr != nil {
		return fmt.Errorf("飞书卡片推送失败（%v），纯文本回退也失败（%v）", cardErr, textErr)
	}
	return nil
}

// cardPayload builds the interactive card for msg.
func (f *Feishu) cardPayload(msg Message) map[string]any {
	elements := feishuElements(msg.Body)
	elements = append(elements,
		map[string]any{"tag": "hr"},
		map[string]any{
			"tag": "note",
			"elements": []any{
				map[string]any{"tag": "plain_text", "content": f.footer()},
			},
		},
	)
	return f.withAuth(map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"title":    map[string]any{"tag": "plain_text", "content": msg.Title},
				"template": feishuTemplate(msg),
			},
			"elements": elements,
		},
	})
}

// textPayload is the fallback used when the card is rejected.
func (f *Feishu) textPayload(msg Message) map[string]any {
	text := msg.Title
	if body := strings.TrimSpace(msg.Body); body != "" {
		text += "\n" + body
	}
	return f.withAuth(map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": text},
	})
}

// feishuTemplate picks the header colour. Messages built from quota carry an
// explicit severity; the rest are classified by the state emoji in the title.
func feishuTemplate(msg Message) string {
	switch msg.Severity {
	case SeverityNotice:
		return feishuYellow
	case SeverityLow:
		return feishuOrange
	case SeverityCritical, SeverityExhausted:
		return feishuRed
	}
	switch {
	case strings.HasPrefix(msg.Title, "✅"):
		return feishuGreen
	case strings.HasPrefix(msg.Title, "🔴"):
		return feishuRed
	case strings.HasPrefix(msg.Title, "🟡"):
		return feishuYellow
	case strings.HasPrefix(msg.Title, "⚠️"):
		return feishuOrange
	default:
		return feishuBlue
	}
}

// feishuElements turns the message body into card elements. Consecutive
// "label：value" lines become short fields so they sit side by side, and
// anything else becomes a plain text block.
func feishuElements(body string) []any {
	lines := strings.Split(body, "\n")
	elements := make([]any, 0, len(lines)+1)
	var fields []any
	flush := func() {
		if len(fields) == 0 {
			return
		}
		elements = append(elements, map[string]any{"tag": "div", "fields": fields})
		fields = nil
	}
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		label, value, ok := splitField(line)
		if !ok {
			flush()
			elements = append(elements, divElement(line))
			continue
		}
		fields = append(fields, map[string]any{
			"is_short": true,
			"text":     map[string]any{"tag": "lark_md", "content": "**" + label + "**\n" + value},
		})
	}
	flush()
	return elements
}

func divElement(content string) map[string]any {
	return map[string]any{
		"tag":  "div",
		"text": map[string]any{"tag": "lark_md", "content": content},
	}
}

// splitField splits "5h：100% | 04h" into its two halves. It only accepts a
// short label without path separators so URLs and prose stay intact.
func splitField(line string) (string, string, bool) {
	for _, sep := range []string{"：", ": "} {
		idx := strings.Index(line, sep)
		if idx < 0 {
			continue
		}
		label := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+len(sep):])
		if label == "" || value == "" {
			continue
		}
		if utf8.RuneCountInString(label) > feishuLabelMax {
			continue
		}
		if strings.ContainsAny(label, "/\\@") {
			continue
		}
		return label, value, true
	}
	return "", "", false
}

func (f *Feishu) footer() string {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	stamp := now()
	if f.Loc != nil {
		stamp = stamp.In(f.Loc)
	}
	return "Lamplighter · " + stamp.Format("01/02 15:04")
}

// withAuth adds the signature fields when a secret is configured.
func (f *Feishu) withAuth(payload map[string]any) map[string]any {
	secret := strings.TrimSpace(f.Secret)
	if secret == "" {
		return payload
	}
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	timestamp := strconv.FormatInt(now().Unix(), 10)
	payload["timestamp"] = timestamp
	payload["sign"] = feishuSign(secret, timestamp)
	return payload
}

// feishuSign computes a Feishu custom bot signature. Feishu uses
// timestamp+"\n"+secret as the HMAC *key* and signs an empty message, which
// is the opposite of the usual key/message split. Using the secret as the
// key instead is rejected with code 19021.
func feishuSign(secret, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	// Feishu signs the empty string, so nothing is written to mac.
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (f *Feishu) post(ctx context.Context, webhook string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("飞书请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("飞书 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var result struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &result) == nil && result.Code != nil && *result.Code != 0 {
		return fmt.Errorf("飞书返回 code=%d: %s", *result.Code, truncate(result.Msg, 200))
	}
	return nil
}
