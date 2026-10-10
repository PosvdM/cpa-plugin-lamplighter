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

// timeLayout is how reset times are shown in notifications.
const timeLayout = "01/02 15:04"

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

func isSchedule(r ResetRecord) bool {
	return strings.ToLower(r.str("kind")) == "reset_scheduled"
}

// maxRecordAge is how old a completed reset can be and still be sent. An old
// record can enter the latest 10 late, for example when a schedule loses its
// completion link and returns to the list.
const maxRecordAge = 48 * time.Hour

// scheduleWindow returns the UTC window of a scheduled reset. A date-level
// schedule covers a whole day in the source time zone; an exact time has the
// same start and end.
func scheduleWindow(r ResetRecord) (start, end time.Time) {
	if window, ok := r["scheduleWindow"].(map[string]any); ok {
		start, end = quota.ParseTime(window["startAt"]), quota.ParseTime(window["endAt"])
	}
	if start.IsZero() {
		start = quota.ParseTime(r.str("effectiveAt"))
	}
	if end.Before(start) {
		end = start
	}
	return start, end
}

// eventKey identifies a scheduled reset across records. Every X post is its
// own record, so a reply that repeats a schedule describes the same event.
// The key uses the start of the window only: some records carry just
// effectiveAt, others the whole scheduleWindow, for the same schedule.
func eventKey(r ResetRecord) string {
	if !isSchedule(r) {
		return ""
	}
	start, _ := scheduleWindow(r)
	if start.IsZero() {
		return ""
	}
	scope, _ := r["scope"].(map[string]any)
	var lists []string
	for _, field := range []string{"plans", "windows"} {
		items := stringList(scope[field])
		for i := range items {
			items[i] = strings.ToLower(items[i])
		}
		sort.Strings(items)
		lists = append(lists, strings.Join(items, ","))
	}
	return "event:" + strings.ToLower(r.str("resetType")) + "|" + start.UTC().Format(time.RFC3339) + "|" + strings.Join(lists, "|")
}

// notifiable reports whether a record is still news. Only pending schedules
// whose window has not ended are sent; elapsed, fulfilled and unknown
// schedules are history, and so are completed resets older than maxRecordAge.
func notifiable(r ResetRecord, now time.Time) bool {
	if isSchedule(r) {
		_, end := scheduleWindow(r)
		return r.str("scheduleState") == "pending" && (end.IsZero() || end.After(now))
	}
	var latest time.Time
	for _, key := range []string{"completedAt", "effectiveAt", "announcedAt"} {
		if t := quota.ParseTime(r.str(key)); t.After(latest) {
			latest = t
		}
	}
	return latest.IsZero() || now.Sub(latest) <= maxRecordAge
}

// superseded reports whether another notifiable post in records announces
// the same scheduled reset and comes later. Did Codex Reset shows the latest
// post for a window, so only that one is sent. Posts with the same announcedAt
// are ordered by RecordKey, so exactly one of them is sent.
func superseded(r ResetRecord, records []ResetRecord, now time.Time) bool {
	key := eventKey(r)
	if key == "" {
		return false
	}
	announced, rk := quota.ParseTime(r.str("announcedAt")), RecordKey(r)
	for _, other := range records {
		ok := RecordKey(other)
		if ok == rk || eventKey(other) != key || !notifiable(other, now) {
			continue
		}
		t := quota.ParseTime(other.str("announcedAt"))
		if t.After(announced) || (t.Equal(announced) && ok > rk) {
			return true
		}
	}
	return false
}

// scheduleText formats when a scheduled reset is expected, in loc. A
// date-level window is shown as its start and end; a deadline shows only its
// end.
func scheduleText(r ResetRecord, loc *time.Location, lang string) string {
	start, end := scheduleWindow(r)
	switch {
	case start.IsZero():
		return ""
	case !end.After(start):
		return start.In(loc).Format(timeLayout)
	case strings.ToLower(r.str("scheduleConstraint")) == "deadline":
		return T(lang, "schedule_before", end.In(loc).Format(timeLayout))
	}
	return T(lang, "schedule_range", start.In(loc).Format(timeLayout), end.In(loc).Format(timeLayout))
}

func resetTypeLabel(value, lang string) string {
	switch strings.ToLower(value) {
	case "global":
		return T(lang, "reset_global")
	case "banked":
		return T(lang, "reset_banked")
	case "global_and_banked":
		return T(lang, "reset_both")
	}
	return T(lang, "reset_other")
}

func scopeLabel(scope any, lang string) string {
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
			return T(lang, "plans_all")
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
func ResetMessage(r ResetRecord, loc *time.Location, lang string) Message {
	resetType := resetTypeLabel(r.str("resetType"), lang)
	confidence := ""
	if value, ok := r["confidence"].(float64); ok {
		confidence = fmt.Sprintf("%d%%", int(math.RoundToEven(value*100)))
	}
	formatTime := func(values ...string) string {
		for _, value := range values {
			if t := quota.ParseTime(value); !t.IsZero() {
				return t.In(loc).Format(timeLayout)
			}
		}
		return ""
	}
	var title string
	var lines []string
	if isSchedule(r) {
		title = T(lang, "reset_scheduled", resetType)
		if text := scheduleText(r, loc, lang); text != "" {
			lines = append(lines, T(lang, "line_expected", text))
		}
	} else {
		done := "reset_done"
		if r.str("resetType") == "banked" {
			done = "reset_credited"
		}
		title = T(lang, done, resetType)
		if text := formatTime(r.str("effectiveAt"), r.str("completedAt"), r.str("announcedAt")); text != "" {
			lines = append(lines, T(lang, "line_time", text))
		}
	}
	if confidence != "" {
		lines = append(lines, T(lang, "line_confidence", confidence))
	}
	if scope := scopeLabel(r["scope"], lang); scope != "" {
		lines = append(lines, T(lang, "line_scope", scope))
	}
	body := strings.Join(lines, "\n")
	if body == "" {
		body = T(lang, "reset_signal")
	}
	msg := Message{Kind: KindCodexReset, Title: title, Body: body, Level: LevelActive}
	if announced := quota.ParseTime(r.str("announcedAt")); !announced.IsZero() {
		msg.JumpURL = T(lang, "reset_history_link", announced.UnixMilli())
	}
	return msg
}

// eventContent is what a schedule notification says, independent of the
// plugin time zone and language.
func eventContent(r ResetRecord) string {
	return ResetMessage(r, time.UTC, LangEN).Body
}

// isUpdate reports whether r changes a schedule that was already sent: it is
// a later post and the notification would say something different.
func isUpdate(r ResetRecord, prev store.SentEvent) bool {
	return quota.ParseTime(r.str("announcedAt")).Unix() > prev.AnnouncedAt && eventContent(r) != prev.Content
}

// ProcessResetRecords sends notifications for unseen records and returns how
// many were sent. On the first run only the current pending schedule is sent
// (when notifyPending is set); older records are marked as seen. Records that
// are no longer news, schedules superseded by a later post about the same
// window, and posts that repeat a schedule already sent are marked as seen
// without a notification. A later post that changes a sent schedule is sent as
// an update.
func ProcessResetRecords(ctx context.Context, st *store.State, records []ResetRecord, notifyPending bool, sender Sender, loc *time.Location, lang string, now time.Time) int {
	if st.CodexReset == nil {
		st.CodexReset = &store.CodexResetState{}
	}
	root := st.CodexReset
	seen := map[string]bool{}
	for _, key := range root.SeenKeys {
		seen[key] = true
	}
	sentEvents := map[string]store.SentEvent{}
	for _, e := range root.SentEvents {
		sentEvents[e.Key] = e
	}
	var candidates []ResetRecord
	if !root.Initialized {
		root.Initialized = true
		pendingKey := ""
		if notifyPending {
			for _, r := range records {
				if isSchedule(r) && notifiable(r, now) && !superseded(r, records, now) {
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
			r := records[i]
			if seen[RecordKey(r)] {
				continue
			}
			prev, sentBefore := sentEvents[eventKey(r)]
			if notifiable(r, now) && !superseded(r, records, now) && (!sentBefore || isUpdate(r, prev)) {
				candidates = append(candidates, r)
			} else {
				seen[RecordKey(r)] = true
			}
		}
	}

	sent := 0
	var newEvents []store.SentEvent
	for _, r := range candidates {
		if sender == nil {
			break
		}
		key := eventKey(r)
		msg := ResetMessage(r, loc, lang)
		if _, ok := sentEvents[key]; ok && key != "" {
			msg.Title = T(lang, "reset_rescheduled", resetTypeLabel(r.str("resetType"), lang))
		}
		if err := sender.Send(ctx, msg); err == nil {
			seen[RecordKey(r)] = true
			if key != "" {
				newEvents = append(newEvents, store.SentEvent{
					Key: key, AnnouncedAt: quota.ParseTime(r.str("announcedAt")).Unix(), Content: eventContent(r),
				})
			}
			sent++
		}
	}
	if len(newEvents) > 0 {
		events := newEvents
		for _, e := range root.SentEvents {
			replaced := false
			for _, n := range newEvents {
				replaced = replaced || n.Key == e.Key
			}
			if !replaced {
				events = append(events, e)
			}
		}
		if len(events) > maxSeenKeys {
			events = events[:maxSeenKeys]
		}
		root.SentEvents = events
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
