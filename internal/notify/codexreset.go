package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// DidCodexResetURL lists the latest Did Codex Reset records.
const DidCodexResetURL = "https://didcodexreset.com/openapi/v1/records?kind=all&page=1&pageSize=10"

const maxSeenKeys = 100

// ResetRecord is one Did Codex Reset record.
type ResetRecord map[string]any

func (r ResetRecord) str(key string) string {
	if value, ok := r[key]; ok && value != nil {
		return strings.TrimSpace(fmt.Sprint(value))
	}
	return ""
}

// FetchResetRecords reads the latest records.
func FetchResetRecords(ctx context.Context, client *http.Client, userAgent string) ([]ResetRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DidCodexResetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Did Codex Reset HTTP %d", resp.StatusCode)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Items []ResetRecord `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || !payload.OK {
		return nil, errors.New("Did Codex Reset API 返回异常")
	}
	var out []ResetRecord
	for _, item := range payload.Data.Items {
		if item != nil && item.str("id") != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

func stringList(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// RecordKey identifies a record across polls. Manual records can change their
// ID while describing the same event, so they are keyed by content.
func RecordKey(r ResetRecord) string {
	id := r.str("id")
	if id != "" && !strings.HasPrefix(id, "manual:") {
		return "id:" + id
	}
	scope, _ := r["scope"].(map[string]any)
	plans := stringList(scope["plans"])
	windows := stringList(scope["windows"])
	sort.Strings(plans)
	sort.Strings(windows)
	parts := []string{
		r.str("kind"), r.str("resetType"), r.str("announcedAt"), r.str("effectiveAt"),
		r.str("completedAt"), r.str("scheduleState"), strings.Join(plans, ","), strings.Join(windows, ","),
	}
	return "manual:" + strings.Join(parts, "|")
}

func resetTypeLabel(value string) string {
	switch strings.ToLower(value) {
	case "global":
		return "全局重置"
	case "banked":
		return "重置卡"
	case "global_and_banked":
		return "全局重置 + 重置卡"
	}
	return "Codex 重置"
}

func scopeLabel(scope any) string {
	m, _ := scope.(map[string]any)
	plans := stringList(m["plans"])
	if len(plans) == 0 {
		return ""
	}
	names := map[string]string{"plus": "Plus", "pro": "Pro", "business": "Business", "team": "Team", "enterprise": "Enterprise"}
	var out []string
	for _, plan := range plans {
		plan = strings.ToLower(plan)
		if plan == "all" {
			return "全部套餐"
		}
		if name, ok := names[plan]; ok {
			out = append(out, name)
		} else {
			out = append(out, plan)
		}
	}
	return strings.Join(out, " · ")
}

// ResetMessage builds the notification for one record.
func ResetMessage(r ResetRecord, loc *time.Location) Message {
	resetType := resetTypeLabel(r.str("resetType"))
	confidence := ""
	if value, ok := r["confidence"].(float64); ok {
		confidence = fmt.Sprintf("%d%%", int(math.RoundToEven(value*100)))
	}
	formatTime := func(values ...string) string {
		for _, value := range values {
			if t := quota.ParseTime(value); !t.IsZero() {
				return t.In(loc).Format("01/02 15:04")
			}
		}
		return ""
	}
	var title string
	var lines []string
	if strings.ToLower(r.str("kind")) == "reset_scheduled" {
		title = fmt.Sprintf("📅 Codex %s已排期", resetType)
		if text := formatTime(r.str("effectiveAt")); text != "" {
			lines = append(lines, "预计："+text)
		}
	} else {
		suffix := "已完成"
		if r.str("resetType") == "banked" {
			suffix = "已到账"
		}
		title = fmt.Sprintf("✅ Codex %s%s", resetType, suffix)
		if text := formatTime(r.str("effectiveAt"), r.str("completedAt"), r.str("announcedAt")); text != "" {
			lines = append(lines, "时间："+text)
		}
	}
	if confidence != "" {
		lines = append(lines, "置信度："+confidence)
	}
	if scope := scopeLabel(r["scope"]); scope != "" {
		lines = append(lines, "范围："+scope)
	}
	body := strings.Join(lines, "\n")
	if body == "" {
		body = "Did Codex Reset 发布了新的重置信号"
	}
	msg := Message{Title: title, Body: body, Level: LevelActive}
	if announced := quota.ParseTime(r.str("announcedAt")); !announced.IsZero() {
		msg.JumpURL = fmt.Sprintf("https://didcodexreset.com/zh/history/%d.html", announced.UnixMilli())
	}
	return msg
}

// ProcessResetRecords sends notifications for unseen records and returns how
// many were sent. On the first run only the current pending schedule is sent
// (when notifyPending is set); older records are marked as seen.
func ProcessResetRecords(ctx context.Context, st *store.State, records []ResetRecord, notifyPending bool, sender Sender, loc *time.Location, now time.Time) int {
	if st.CodexReset == nil {
		st.CodexReset = &store.CodexResetState{}
	}
	root := st.CodexReset
	seen := map[string]bool{}
	for _, key := range root.SeenKeys {
		seen[key] = true
	}
	var candidates []ResetRecord
	if !root.Initialized {
		root.Initialized = true
		pendingKey := ""
		if notifyPending {
			for _, r := range records {
				if r.str("kind") == "reset_scheduled" && r.str("scheduleState") == "pending" {
					pendingKey = RecordKey(r)
					candidates = append(candidates, r)
					break
				}
			}
		}
		for _, r := range records {
			if key := RecordKey(r); key != pendingKey {
				seen[key] = true
			}
		}
	} else {
		for i := len(records) - 1; i >= 0; i-- {
			if !seen[RecordKey(records[i])] {
				candidates = append(candidates, records[i])
			}
		}
	}

	sent := 0
	for _, r := range candidates {
		if sender == nil {
			break
		}
		if err := sender.Send(ctx, ResetMessage(r, loc)); err == nil {
			seen[RecordKey(r)] = true
			sent++
		}
	}

	var ordered []string
	added := map[string]bool{}
	for _, r := range records {
		if key := RecordKey(r); seen[key] && !added[key] {
			ordered = append(ordered, key)
			added[key] = true
		}
	}
	for _, key := range root.SeenKeys {
		if seen[key] && !added[key] {
			ordered = append(ordered, key)
			added[key] = true
		}
	}
	if len(ordered) > maxSeenKeys {
		ordered = ordered[:maxSeenKeys]
	}
	root.SeenKeys = ordered
	root.LastCheckEpoch = now.Unix()
	return sent
}
