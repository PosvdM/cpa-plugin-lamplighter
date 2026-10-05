package notify

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/PosvdM/cpa-plugin-lamplighter/internal/config"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/quota"
	"github.com/PosvdM/cpa-plugin-lamplighter/internal/store"
)

// Severities, from best to worst.
const (
	SeverityNormal    = "normal"
	SeverityNotice    = "notice"
	SeverityLow       = "low"
	SeverityCritical  = "critical"
	SeverityExhausted = "exhausted"
)

var severityRank = map[string]int{
	SeverityNormal: 0, SeverityNotice: 1, SeverityLow: 2, SeverityCritical: 3, SeverityExhausted: 4,
}

// resetTolerance treats two reset times as the same cycle when they differ
// by at most this much; providers return slightly different values per call.
const resetTolerance = 10 * time.Second

// Group is a quota group as seen by the alert logic.
type Group struct {
	Key     string
	Label   string
	Windows []quota.Window
}

// Alerts turns quota changes into Bark notifications.
type Alerts struct {
	Cfg    config.Config
	Sender Sender
	// Lang is the notification language, LangZH or LangEN.
	Lang string
	// OnSent is called for every delivered notification, for the event log.
	OnSent func(msg Message)
	// OnError is called when a delivery fails.
	OnError func(msg Message, err error)
}

// ExhaustedRemaining is the remaining percentage at or below which a window
// counts as used up.
const ExhaustedRemaining = 0.01

// Severity classifies a remaining percentage with the configured thresholds.
func (a *Alerts) Severity(remaining float64) string {
	switch {
	case remaining <= ExhaustedRemaining:
		return SeverityExhausted
	case remaining <= a.Cfg.CriticalThreshold:
		return SeverityCritical
	case remaining <= a.Cfg.LowThreshold:
		return SeverityLow
	case remaining <= a.Cfg.NoticeThreshold:
		return SeverityNotice
	}
	return SeverityNormal
}

func formatReset(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseReset(value string) time.Time {
	return quota.ParseTime(value)
}

// SameResetCycle reports whether two stored reset times belong to the same window.
func SameResetCycle(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	l, r := parseReset(left), parseReset(right)
	if l.IsZero() || r.IsZero() {
		return false
	}
	diff := l.Sub(r)
	if diff < 0 {
		diff = -diff
	}
	return diff <= resetTolerance
}

// CompactDuration formats a duration as 05h, 03d or 12m.
func CompactDuration(d time.Duration, lang string) string {
	seconds := d.Seconds()
	if seconds <= 0 {
		return T(lang, "due")
	}
	unit := func(size float64, suffix string) string {
		n := int(seconds/size + 0.5)
		if n < 1 {
			n = 1
		}
		return fmt.Sprintf("%02d%s", n, suffix)
	}
	switch {
	case seconds >= 86400:
		return unit(86400, "d")
	case seconds >= 3600:
		return unit(3600, "h")
	}
	return unit(60, "m")
}

// roundPercent rounds half to even, matching the notification format of the
// Python version this plugin replaces.
func roundPercent(value float64) int {
	return int(math.RoundToEven(value))
}

func (a *Alerts) windowLine(w quota.Window, now time.Time) string {
	label := quota.ShortLabel(w.Label)
	value := fmt.Sprintf("%d%%", roundPercent(w.Remaining))
	if w.Reset.IsZero() {
		return T(a.Lang, "window_line", label, value)
	}
	local := w.Reset.In(a.Cfg.Location())
	return T(a.Lang, "window_line_reset", label, value, CompactDuration(w.Reset.Sub(now), a.Lang), local.Format("01/02 15:04"))
}

func (a *Alerts) body(g Group, now time.Time) string {
	lines := make([]string, 0, len(g.Windows))
	for _, w := range g.Windows {
		lines = append(lines, a.windowLine(w, now))
	}
	return strings.Join(lines, "\n")
}

type change struct {
	window           quota.Window
	to               string
	down             bool
	resetRecoveryFor string
}

type reminder struct {
	window quota.Window
	reset  string
	stage  string
}

func (a *Alerts) buildChangeMessage(g Group, changes []change, now time.Time) Message {
	var worsening, recovering []change
	for _, c := range changes {
		if c.down {
			worsening = append(worsening, c)
		} else {
			recovering = append(recovering, c)
		}
	}
	if len(worsening) > 0 {
		worst := worsening[0]
		for _, c := range worsening[1:] {
			if severityRank[c.to] > severityRank[worst.to] {
				worst = c
			}
		}
		parts := make([]string, 0, len(worsening))
		for _, c := range worsening {
			text := fmt.Sprintf("%s %d%%", quota.ShortLabel(c.window.Label), roundPercent(c.window.Remaining))
			if !c.window.Reset.IsZero() {
				text += " | " + CompactDuration(c.window.Reset.Sub(now), a.Lang)
			}
			parts = append(parts, text)
		}
		// Yellow for the first threshold, red from the second one down; the
		// page colors quota the same way. ⚠️ is kept for errors.
		prefix, level := "🟡", LevelActive
		if worst.to != SeverityNotice {
			prefix = "🔴"
		}
		if worst.to == SeverityCritical || worst.to == SeverityExhausted {
			level = LevelTimeSensitive
		}
		return Message{
			Title: fmt.Sprintf("%s %s · %s", prefix, g.Label, strings.Join(parts, " / ")),
			Body:  a.body(g, now),
			Level: level,
			Label: g.Label,
		}
	}
	labels := make([]string, 0, len(recovering))
	for _, c := range recovering {
		labels = append(labels, quota.ShortLabel(c.window.Label))
	}
	return Message{
		Title: T(a.Lang, "recovered", g.Label, strings.Join(labels, " / ")),
		Body:  a.body(g, now),
		Level: LevelActive,
		Label: g.Label,
	}
}

func (a *Alerts) buildReminderMessage(g Group, reminders []reminder, now time.Time) Message {
	labels := make([]string, 0, len(reminders))
	for _, r := range reminders {
		labels = append(labels, quota.ShortLabel(r.window.Label))
	}
	return Message{
		Title: T(a.Lang, "reset_reminder", g.Label, strings.Join(labels, " / ")),
		Body:  a.body(g, now),
		Level: LevelActive,
		Label: g.Label,
	}
}

func (a *Alerts) detectResetRecovery(old *store.WindowState, w quota.Window, now time.Time, current string) string {
	pending := old.PendingResetRecoveryFor
	if pending != "" && current == SeverityNormal && !SameResetCycle(old.ResetRecoveryFor, pending) {
		return pending
	}
	if current != SeverityNormal {
		return ""
	}
	cycle := old.ResetNotice1hFor
	if cycle == "" || SameResetCycle(old.ResetRecoveryFor, cycle) {
		return ""
	}
	target := parseReset(cycle)
	if target.IsZero() || now.Before(target) {
		return ""
	}
	currentReset := formatReset(w.Reset)
	if old.Reset != "" && SameResetCycle(old.Reset, cycle) {
		if currentReset == "" || !SameResetCycle(currentReset, cycle) {
			return cycle
		}
	}
	return ""
}

func (a *Alerts) send(ctx context.Context, msg Message) bool {
	if a.Sender == nil {
		return false
	}
	if err := a.Sender.Send(ctx, msg); err != nil {
		if a.OnError != nil {
			a.OnError(msg, err)
		}
		return false
	}
	if a.OnSent != nil {
		a.OnSent(msg)
	}
	return true
}

// ProcessGroup compares g with the stored baseline, sends threshold,
// recovery and reset-reminder notifications, and updates the baseline. A
// window seen for the first time only records its baseline.
func (a *Alerts) ProcessGroup(ctx context.Context, st *store.State, g Group, now time.Time) {
	groupState := st.Group(g.Key)
	var changes []change
	var reminders []reminder

	for _, w := range g.Windows {
		current := a.Severity(w.Remaining)
		old, seen := groupState.Windows[w.ID]
		if !seen || old == nil {
			groupState.Windows[w.ID] = &store.WindowState{
				Severity:         current,
				NotifiedSeverity: current,
				Remaining:        w.Remaining,
				Reset:            formatReset(w.Reset),
				LastSeen:         now.Unix(),
			}
			continue
		}
		notified := old.NotifiedSeverity
		if notified == "" {
			notified = SeverityNormal
		}
		resetRecoveryFor := a.detectResetRecovery(old, w, now, current)

		direction := ""
		silentRecovery := false
		switch {
		case severityRank[current] > severityRank[notified]:
			direction = "down"
		case severityRank[notified] > severityRank[current]:
			if a.Cfg.NotifyRecovery {
				direction = "up"
			} else {
				// Recovery notifications may be off, but the baseline still
				// follows the recovered quota; otherwise an old exhausted
				// state would suppress alerts in the next cycle.
				silentRecovery = true
			}
		case a.Cfg.NotifyRecovery && resetRecoveryFor != "":
			direction = "up"
		}
		if direction != "" {
			c := change{window: w, to: current, down: direction == "down"}
			if !c.down {
				c.resetRecoveryFor = resetRecoveryFor
			}
			changes = append(changes, c)
		}

		resetValue := formatReset(w.Reset)
		if a.Cfg.NotifyResetReminders && resetValue != "" {
			untilReset := w.Reset.Sub(now)
			switch {
			case untilReset > 0 && untilReset <= time.Hour && !SameResetCycle(old.ResetNotice1hFor, resetValue):
				reminders = append(reminders, reminder{window: w, reset: resetValue, stage: "1h"})
			case quota.IsSevenDay(w) && untilReset > time.Hour && untilReset <= 24*time.Hour &&
				!SameResetCycle(old.ResetNotice1dFor, resetValue):
				reminders = append(reminders, reminder{window: w, reset: resetValue, stage: "1d"})
			}
		}

		old.Severity = current
		old.Remaining = w.Remaining
		old.Reset = resetValue
		old.LastSeen = now.Unix()
		if silentRecovery {
			old.NotifiedSeverity = current
		}
		if resetRecoveryFor != "" {
			old.PendingResetRecoveryFor = resetRecoveryFor
		}
	}

	if len(changes) > 0 {
		msg := a.buildChangeMessage(g, changes, now)
		if a.send(ctx, msg) {
			worsening := false
			for _, c := range changes {
				if c.down {
					worsening = true
				}
			}
			for _, c := range changes {
				ws := groupState.Windows[c.window.ID]
				ws.NotifiedSeverity = c.to
				if c.resetRecoveryFor != "" && !worsening {
					ws.ResetRecoveryFor = c.resetRecoveryFor
					ws.PendingResetRecoveryFor = ""
				}
			}
		}
	}
	if len(reminders) > 0 {
		msg := a.buildReminderMessage(g, reminders, now)
		if a.send(ctx, msg) {
			for _, r := range reminders {
				ws := groupState.Windows[r.window.ID]
				if r.stage == "1h" {
					ws.ResetNotice1hFor = r.reset
				} else {
					ws.ResetNotice1dFor = r.reset
				}
			}
		}
	}
	groupState.Label = g.Label
	groupState.LastSeen = now.Unix()
}

// CooldownMessage is sent once when CPA keeps an account in a cooldown
// although its quota has recovered.
func CooldownMessage(lang, label string, until time.Time, loc *time.Location) Message {
	return Message{
		Title: T(lang, "cooldown_title", label),
		Body:  T(lang, "cooldown_body", until.In(loc).Format("01/02 15:04")),
		Level: LevelTimeSensitive,
		Label: label,
	}
}

// CircuitMessage is sent once when ignition pauses until the next day.
func CircuitMessage(lang, label string, until time.Time, reason string, loc *time.Location) Message {
	return Message{
		Title: T(lang, "circuit_title", label),
		Body:  T(lang, "circuit_body", until.In(loc).Format("01/02 15:04"), truncate(reason, 220)),
		Level: LevelTimeSensitive,
		Label: label,
	}
}
