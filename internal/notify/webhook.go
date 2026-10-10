package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
)

// Placeholders are the names a webhook may use as {{name}}.
var Placeholders = []string{"title", "body", "text", "url", "priority", "kind", "label", "time"}

var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z_]+)\s*\}\}`)

var headerNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Webhook sends messages to a user-defined HTTP endpoint. Placeholders are
// escaped for where they appear: URL-encoded in the URL, JSON-escaped in a
// JSON body, form-encoded in a form body, and with line breaks replaced in
// headers, so message text cannot break the request.
type Webhook struct {
	URL         string
	Method      string
	Headers     map[string]string
	Body        string
	SuccessJSON map[string]any
	UserAgent   string
	Client      *http.Client
	// Loc is the time zone of {{time}}.
	Loc *time.Location
	// Now is overridable for tests. Nil uses time.Now.
	Now func() time.Time
}

// NewWebhook checks cfg and returns the sender, or nil when cfg.URL is empty.
func NewWebhook(cfg config.Webhook) (*Webhook, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	w := &Webhook{URL: cfg.URL, Method: cfg.Method, Headers: cfg.Headers, Body: cfg.Body, SuccessJSON: cfg.SuccessJSON}
	if w.Method == "" {
		w.Method = http.MethodPost
	}
	switch w.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut:
	default:
		return nil, fmt.Errorf("webhook.method 只能是 GET、POST 或 PUT，当前为 %s", w.Method)
	}
	if w.Method == http.MethodGet && strings.TrimSpace(w.Body) != "" {
		return nil, errors.New("webhook.method 为 GET 时不能设置 body")
	}
	parsed, err := url.Parse(placeholderPattern.ReplaceAllString(w.URL, "x"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("webhook.url 必须是 http:// 或 https:// 开头的地址")
	}
	texts := []string{w.URL, w.Body}
	for name, value := range w.Headers {
		if !headerNamePattern.MatchString(name) {
			return nil, fmt.Errorf("webhook.headers 中的名称不合法：%q", name)
		}
		texts = append(texts, value)
	}
	for _, text := range texts {
		for _, match := range placeholderPattern.FindAllStringSubmatch(text, -1) {
			if !isPlaceholder(match[1]) {
				return nil, fmt.Errorf("webhook 占位符 %s 不存在，可用的有：%s", match[0], strings.Join(Placeholders, "、"))
			}
		}
	}
	if w.SuccessJSON != nil {
		// Decoding the expected values the way the response is decoded makes
		// YAML integers equal to JSON numbers.
		raw, err := json.Marshal(w.SuccessJSON)
		if err != nil {
			return nil, fmt.Errorf("webhook.success_json 无法解析：%v", err)
		}
		var expected map[string]any
		if err := json.Unmarshal(raw, &expected); err != nil {
			return nil, fmt.Errorf("webhook.success_json 无法解析：%v", err)
		}
		w.SuccessJSON = expected
	}
	return w, nil
}

func isPlaceholder(name string) bool {
	for _, candidate := range Placeholders {
		if name == candidate {
			return true
		}
	}
	return false
}

// Checked reports whether a successful delivery also checks the response
// content. Without success_json any 2xx response counts.
func (w *Webhook) Checked() bool { return len(w.SuccessJSON) > 0 }

// Send delivers msg.
func (w *Webhook) Send(ctx context.Context, msg Message) error {
	_, err := w.Deliver(ctx, msg)
	return err
}

// Deliver sends msg and returns the start of the response, with secrets
// removed, for the test result.
func (w *Webhook) Deliver(ctx context.Context, msg Message) (string, error) {
	values := w.values(msg)
	var body io.Reader
	contentType := ""
	if w.Method != http.MethodGet {
		contentType = w.contentType()
		text := w.Body
		if strings.TrimSpace(text) == "" {
			text = defaultBody(values)
		} else {
			text = fill(text, values, bodyEscaper(contentType))
		}
		body = strings.NewReader(text)
	}
	req, err := http.NewRequestWithContext(ctx, w.Method, fill(w.URL, values, escape), body)
	if err != nil {
		return "", w.redactErr(fmt.Errorf("Webhook 请求无法创建: %w", unwrapURLError(err)))
	}
	for name, value := range w.Headers {
		req.Header.Set(name, fill(value, values, headerEscape))
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if w.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", w.UserAgent)
	}
	client := w.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", w.redactErr(fmt.Errorf("Webhook 推送失败: %w", unwrapURLError(err)))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	snippet := truncate(w.redact(strings.TrimSpace(string(raw))), 200)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return snippet, fmt.Errorf("Webhook HTTP %d: %s", resp.StatusCode, snippet)
	}
	if w.Checked() {
		var got map[string]any
		if json.Unmarshal(raw, &got) != nil {
			return snippet, fmt.Errorf("Webhook 响应不是 JSON 对象，无法检查 success_json: %s", snippet)
		}
		keys := make([]string, 0, len(w.SuccessJSON))
		for key := range w.SuccessJSON {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !reflect.DeepEqual(got[key], w.SuccessJSON[key]) {
				return snippet, fmt.Errorf("Webhook 响应不符合 success_json（%s）: %s", key, snippet)
			}
		}
	}
	return snippet, nil
}

// values returns the placeholder values for msg.
func (w *Webhook) values(msg Message) map[string]string {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	if w.Loc != nil {
		now = now.In(w.Loc)
	}
	text := msg.Title
	if msg.Body != "" {
		text += "\n" + msg.Body
	}
	priority := "normal"
	if msg.Level == LevelTimeSensitive {
		priority = "high"
	}
	return map[string]string{
		"title":    msg.Title,
		"body":     msg.Body,
		"text":     text,
		"url":      msg.JumpURL,
		"priority": priority,
		"kind":     msg.Kind,
		"label":    msg.Label,
		"time":     now.Format(time.RFC3339),
	}
}

// defaultBody is the JSON message sent when webhook.body is empty.
func defaultBody(values map[string]string) string {
	payload := map[string]string{"source": "lamplighter"}
	for _, name := range Placeholders {
		payload[name] = values[name]
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

// contentType is the Content-Type header when set, otherwise JSON for a body
// that starts with { or [ outside a placeholder and plain text for anything
// else.
func (w *Webhook) contentType() string {
	for name, value := range w.Headers {
		if strings.EqualFold(name, "Content-Type") {
			return value
		}
	}
	if strings.TrimSpace(w.Body) == "" {
		return "application/json; charset=utf-8"
	}
	body := strings.TrimSpace(placeholderPattern.ReplaceAllString(w.Body, ""))
	if strings.HasPrefix(body, "{") || strings.HasPrefix(body, "[") {
		return "application/json; charset=utf-8"
	}
	return "text/plain; charset=utf-8"
}

func bodyEscaper(contentType string) func(string) string {
	lower := strings.ToLower(contentType)
	switch {
	case strings.Contains(lower, "json"):
		return jsonEscape
	case strings.Contains(lower, "x-www-form-urlencoded"):
		return url.QueryEscape
	}
	return func(value string) string { return value }
}

// jsonEscape escapes value for use inside a JSON string, without the quotes.
func jsonEscape(value string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	out := strings.TrimSuffix(buf.String(), "\n")
	return out[1 : len(out)-1]
}

func headerEscape(value string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)
}

func fill(template string, values map[string]string, escape func(string) string) string {
	return placeholderPattern.ReplaceAllStringFunc(template, func(match string) string {
		return escape(values[placeholderPattern.FindStringSubmatch(match)[1]])
	})
}

// secrets returns the parts of the settings that may be keys or tokens: the
// URL, its longer path segments and query values, and the header values and
// their longer words.
func (w *Webhook) secrets() []string {
	found := []string{w.URL}
	add := func(value string) {
		if len(value) >= 8 && !placeholderPattern.MatchString(value) {
			found = append(found, value)
		}
	}
	if parsed, err := url.Parse(w.URL); err == nil {
		for _, segment := range strings.Split(parsed.Path, "/") {
			add(segment)
		}
		for _, list := range parsed.Query() {
			for _, value := range list {
				add(value)
			}
		}
	}
	for _, value := range w.Headers {
		add(value)
		for _, word := range strings.Fields(value) {
			add(word)
		}
	}
	// Longer values first, so a token inside the URL goes with the URL.
	sort.Slice(found, func(i, j int) bool { return len(found[i]) > len(found[j]) })
	return found
}

func (w *Webhook) redact(text string) string {
	for _, secret := range w.secrets() {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func (w *Webhook) redactErr(err error) error {
	return errors.New(w.redact(err.Error()))
}
