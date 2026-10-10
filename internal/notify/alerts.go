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

// RecoveryJump is the rise, in percentage points since the previous reading,
// that counts as a recovery when the reset time has not changed. Smaller
// rises are noise: usage endpoints and response headers can differ by a
// point. The page uses the same value to find resets in the chart.
const RecoveryJump = 5

// Group is a quota group as seen by the alert logic.
type Group struct {
	Key     string
	Label   string
	Windows []quota.Window
}

// Alerts turns quota changes into notifications.
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
	window quota.Window
	to     string
}

type recovery struct {
	window   quota.Window
	previous *float64
}

type reminder struct {
	window quota.Window
	reset  string
	stage  string
}

func (a *Alerts) buildChangeMessage(g Group, changes []change, now time.Time) Message {
	worst := changes[0]
	for _, c := range changes[1:] {
		if severityRank[c.to] > severityRank[worst.to] {
			worst = c
		}
	}
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
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
		Kind:  KindQuota,
		Title: fmt.Sprintf("%s %s · %s", prefix, g.Label, strings.Join(parts, " / ")),
		Body:  a.body(g, now),
		Level: level,
		Label: g.Label,
	}
}

// buildRecoveryMessage lists the reset windows in the title. The body starts
// with what was left of each window in the ended cycle, when known.
func (a *Alerts) buildRecoveryMessage(g Group, recoveries []recovery, now time.Time) Message {
	labels := make([]string, 0, len(recoveries))
	var previous []string
	for _, r := range recoveries {
		label := quota.ShortLabel(r.window.Label)
		labels = append(labels, label)
		if r.previous != nil {
			previous = append(previous, fmt.Sprintf("%s %d%%", label, roundPercent(*r.previous)))
		}
	}
	body := a.body(g, now)
	if len(previous) > 0 {
		body = T(a.Lang, "previous_cycle", strings.Join(previous, " / ")) + "\n" + body
	}
	return Message{
		Kind:  KindRecovery,
		Title: T(a.Lang, "recovered", g.Label, strings.Join(labels, " / ")),
		Body:  body,
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
		Kind:  KindReminder,
		Title: T(a.Lang, "reset_reminder", g.Label, strings.Join(labels, " / ")),
		Body:  a.body(g, now),
		Level: LevelActive,
		Label: g.Label,
	}
}

// SevenDayClass reports whether the recovery and reminder settings treat w
// as a 7-day window. 5-hour and 7-day windows are known by their labels. A
// window of another kind counts as a 7-day window once it was seen more
// than a day before its reset; ws may be nil.
func SevenDayClass(w quota.Window, ws *store.WindowState) bool {
	switch {
	case quota.IsSevenDay(w):
		return true
	case quota.IsFiveHour(w):
		return false
	}
	return ws != nil && ws.Long
}

// cycleEnded reports whether w belongs to a new cycle after the reading in
// old. When both reset times are known, the reset time must have moved and
// either the old one must have passed or the quota must have risen by
// RecoveryJump; Codex reports a reset time that moves forward with every
// query while a window is unused, and that alone is no reset. Without a new
// reset time, a passed old reset time or a rise by RecoveryJump counts.
func cycleEnded(old *store.WindowState, w quota.Window, now time.Time) bool {
	rose := w.Remaining-old.Remaining >= RecoveryJump
	oldReset := parseReset(old.Reset)
	passed := !oldReset.IsZero() && !now.Before(oldReset.Add(-resetTolerance))
	if !oldReset.IsZero() && !w.Reset.IsZero() {
		if SameResetCycle(old.Reset, formatReset(w.Reset)) {
			return false
		}
		return passed || rose
	}
	return passed || rose
}

// staleReading reports whether w reports a reset time that already passed
// and belongs to a cycle that ended or was followed by a later one. Usage
// records arrive when a request completes, so a long request started before
// a reset delivers the old cycle's headers after it.
func staleReading(old *store.WindowState, w quota.Window, now time.Time) bool {
	if w.Reset.IsZero() || now.Before(w.Reset) {
		return false
	}
	reset := formatReset(w.Reset)
	if SameResetCycle(reset, old.EndedReset) {
		return true
	}
	oldReset := parseReset(old.Reset)
	return !oldReset.IsZero() && w.Reset.Before(oldReset.Add(-resetTolerance))
}

// previousReadingAge is how old the last reading of an ended cycle may be,
// measured at the reset, to be reported as what was left of that cycle.
const previousReadingAge = 15 * time.Minute

// pendingRecoveryAge is how long an undelivered recovery notification is
// retried.
const pendingRecoveryAge = time.Hour

// endedCycle returns the last remaining percentage of the cycle that ended
// before w, or nil when no reading close enough to its end exists. With a
// known reset time only readings taken before it count; a provider may keep
// reporting the passed reset time with the new cycle's value. A used-up
// window stays used up until its reset, so that reading never gets old.
func endedCycle(old *store.WindowState, now time.Time) *float64 {
	previous, seen, end := old.Remaining, old.LastSeen, now
	if reset := parseReset(old.Reset); !reset.IsZero() {
		if !SameResetCycle(old.CycleReset, old.Reset) {
			return nil
		}
		previous, seen = old.CycleRemaining, old.CycleSeen
		if reset.Before(now) {
			end = reset
		}
	}
	if previous <= ExhaustedRemaining {
		return &previous
	}
	if seen == 0 || end.Sub(time.Unix(seen, 0)) > previousReadingAge {
		return nil
	}
	return &previous
}

// unusedCycle reports whether the cycle that ended before w was never used:
// its last reading before the reset still showed full quota. Codex moves
// the reset time of an unused window forward, so after a gap longer than
// the window its reset looks like a recovery.
func unusedCycle(old *store.WindowState) bool {
	return old.Reset != "" && SameResetCycle(old.CycleReset, old.Reset) && roundPercent(old.CycleRemaining) >= 100
}

// renewed reports whether w really recovered since the previous reading:
// its reset time moved to a new window, or it rose by RecoveryJump or more.
// Within a window the notified severity only moves down, so a reading that
// bounces around a threshold notifies once.
func renewed(old *store.WindowState, w quota.Window) bool {
	reset := formatReset(w.Reset)
	if old.Reset != "" && reset != "" && !SameResetCycle(old.Reset, reset) {
		return true
	}
	return w.Remaining-old.Remaining >= RecoveryJump
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
	var recoveries []recovery
	var reminders []reminder

	for _, w := range g.Windows {
		current := a.Severity(w.Remaining)
		exhausted := w.Remaining <= ExhaustedRemaining
		long := !w.Reset.IsZero() && w.Reset.Sub(now) > 24*time.Hour
		old, seen := groupState.Windows[w.ID]
		if !seen || old == nil {
			groupState.Windows[w.ID] = &store.WindowState{
				Severity:         current,
				NotifiedSeverity: current,
				Remaining:        w.Remaining,
				Reset:            formatReset(w.Reset),
				LastSeen:         now.Unix(),
				Exhausted:        exhausted,
				Long:             long,
			}
			if !w.Reset.IsZero() && now.Before(w.Reset) {
				ws := groupState.Windows[w.ID]
				ws.CycleReset, ws.CycleRemaining, ws.CycleSeen = ws.Reset, w.Remaining, now.Unix()
			}
			continue
		}
		if staleReading(old, w, now) {
			continue
		}
		if long {
			old.Long = true
		}
		sevenDay := SevenDayClass(w, old)
		notified := old.NotifiedSeverity
		if notified == "" {
			notified = SeverityNormal
		}

		switch {
		case severityRank[current] > severityRank[notified]:
			changes = append(changes, change{window: w, to: current})
		case severityRank[notified] > severityRank[current] && renewed(old, w):
			// The baseline follows the recovered quota; otherwise an old
			// exhausted state would suppress alerts in the next cycle.
			old.NotifiedSeverity = current
		}

		if cycleEnded(old, w, now) {
			if !unusedCycle(old) {
				old.PendingRecovery = &store.PendingRecovery{At: now.Unix(), Previous: endedCycle(old, now), Exhausted: old.Exhausted}
			}
			if old.Reset != "" {
				old.EndedReset = old.Reset
			}
			old.Exhausted = false
		}
		if exhausted {
			old.Exhausted = true
		}
		if p := old.PendingRecovery; p != nil {
			mode := a.Cfg.RecoveryNotify.For(sevenDay)
			switch {
			case now.Sub(time.Unix(p.At, 0)) > pendingRecoveryAge,
				mode == config.RecoveryOff,
				mode == config.RecoveryAfterExhausted && !p.Exhausted:
				old.PendingRecovery = nil
			default:
				recoveries = append(recoveries, recovery{window: w, previous: p.Previous})
			}
		}

		resetValue := formatReset(w.Reset)
		mode := a.Cfg.ResetReminder.For(sevenDay)
		if resetValue != "" && mode != config.ReminderOff &&
			(mode == config.ReminderAll || w.Remaining > math.Max(a.Cfg.CriticalThreshold, ExhaustedRemaining)) {
			untilReset := w.Reset.Sub(now)
			switch {
			case !sevenDay && untilReset > 0 && untilReset <= time.Hour && !SameResetCycle(old.ResetNotice1hFor, resetValue):
				reminders = append(reminders, reminder{window: w, reset: resetValue, stage: "1h"})
			case sevenDay && untilReset > 0 && untilReset <= 24*time.Hour && !SameResetCycle(old.ResetNotice1dFor, resetValue):
				reminders = append(reminders, reminder{window: w, reset: resetValue, stage: "1d"})
			}
		}

		old.Severity = current
		old.Remaining = w.Remaining
		old.Reset = resetValue
		old.LastSeen = now.Unix()
		if !w.Reset.IsZero() && now.Before(w.Reset) {
			old.CycleReset, old.CycleRemaining, old.CycleSeen = resetValue, w.Remaining, now.Unix()
		}
	}

	if len(changes) > 0 {
		if a.send(ctx, a.buildChangeMessage(g, changes, now)) {
			for _, c := range changes {
				groupState.Windows[c.window.ID].NotifiedSeverity = c.to
			}
		}
	}
	if len(recoveries) > 0 {
		if a.send(ctx, a.buildRecoveryMessage(g, recoveries, now)) {
			for _, r := range recoveries {
				groupState.Windows[r.window.ID].PendingRecovery = nil
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
		Kind:  KindCooldown,
		Title: T(lang, "cooldown_title", label),
		Body:  T(lang, "cooldown_body", until.In(loc).Format("01/02 15:04")),
		Level: LevelTimeSensitive,
		Label: label,
	}
}

// CircuitMessage is sent once when ignition pauses until the next day.
func CircuitMessage(lang, label string, until time.Time, reason string, loc *time.Location) Message {
	return Message{
		Kind:  KindCircuit,
		Title: T(lang, "circuit_title", label),
		Body:  T(lang, "circuit_body", until.In(loc).Format("01/02 15:04"), truncate(reason, 220)),
		Level: LevelTimeSensitive,
		Label: label,
	}
}
