// Package quota holds the quota windows that every service reports and the
// upstream request types used to read them. The services themselves are in
// internal/providers.
package quota

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Window labels. Notifications shorten them with ShortLabel.
const (
	LabelFiveHour  = "5 小时"
	LabelSevenDay  = "7 Day"
	LabelFable     = "7 Day Fable 5"
	WindowFiveHour = "five-hour"
	WindowSevenDay = "seven-day"
	WindowFable    = "seven-day-fable"
)

// Source marks where a window value came from.
type Source string

const (
	SourceActive  Source = "active"
	SourcePassive Source = "passive"
)

// Window is one quota window. Remaining is a percentage from 0 to 100.
// Reset is zero when the provider did not return a reset time. Period is the
// length of the window, zero when the provider does not state it.
type Window struct {
	ID        string
	Label     string
	Remaining float64
	Reset     time.Time
	Period    time.Duration
}

// Window kinds.
const (
	KindFiveHour = "5h"
	KindSevenDay = "7d"
)

// Window periods.
const (
	FiveHours = 5 * time.Hour
	SevenDays = 7 * 24 * time.Hour
)

// RawGroup is a set of windows as returned by one provider, before it is
// attached to a credential.
type RawGroup struct {
	Key         string
	SourceLabel string
	Windows     []Window
}

// ShortLabel returns the compact window label used in notifications.
func ShortLabel(label string) string {
	switch label {
	case LabelFiveHour:
		return "5h"
	case LabelSevenDay, LabelFable:
		return "7d"
	}
	return label
}

// Kind returns KindFiveHour, KindSevenDay or "" for a window of another
// length. The period decides when the provider states one; other windows
// are known by their ID and label.
func Kind(w Window) string {
	if w.Period > 0 {
		switch {
		case near(w.Period, FiveHours):
			return KindFiveHour
		case near(w.Period, SevenDays):
			return KindSevenDay
		}
		return ""
	}
	switch {
	case w.ID == WindowFiveHour || ShortLabel(w.Label) == "5h":
		return KindFiveHour
	case w.ID == WindowSevenDay || ShortLabel(w.Label) == "7d" || strings.HasPrefix(w.Label, "7 Day"):
		return KindSevenDay
	}
	return ""
}

// near reports whether d is within 5% of target.
func near(d, target time.Duration) bool {
	return d >= target-target/20 && d <= target+target/20
}

// IsFiveHour reports whether w is a 5-hour window.
func IsFiveHour(w Window) bool { return Kind(w) == KindFiveHour }

// IsSevenDay reports whether w is a 7-day window.
func IsSevenDay(w Window) bool { return Kind(w) == KindSevenDay }

// Standard returns the ID and label of a 5-hour or 7-day window of the given
// period, and false for a period of another length.
func Standard(period time.Duration) (id, label string, ok bool) {
	switch {
	case period <= 0:
		return "", "", false
	case near(period, FiveHours):
		return WindowFiveHour, LabelFiveHour, true
	case near(period, SevenDays):
		return WindowSevenDay, LabelSevenDay, true
	}
	return "", "", false
}

// PeriodLabel returns a compact label for a window of the given period, such
// as "30d" or "12h", for windows that are neither 5-hour nor 7-day ones.
func PeriodLabel(period time.Duration) string {
	switch {
	case period >= 24*time.Hour && period%(24*time.Hour) == 0:
		return strconv.Itoa(int(period/(24*time.Hour))) + "d"
	case period >= time.Hour && period%time.Hour == 0:
		return strconv.Itoa(int(period/time.Hour)) + "h"
	}
	return strconv.Itoa(int(period.Round(time.Minute)/time.Minute)) + "m"
}

// FiveHourWindow returns the first 5-hour window of g.
func FiveHourWindow(windows []Window) (Window, bool) {
	for _, w := range windows {
		if IsFiveHour(w) {
			return w, true
		}
	}
	return Window{}, false
}

// ParseTime parses an ISO 8601 timestamp, a Unix timestamp in seconds or
// milliseconds, or a numeric string holding either. It returns the zero time
// when value cannot be parsed.
func ParseTime(value any) time.Time {
	switch v := value.(type) {
	case nil:
		return time.Time{}
	case float64:
		return unixTime(v)
	case int64:
		return unixTime(float64(v))
	case int:
		return unixTime(float64(v))
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return unixTime(f)
		}
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return time.Time{}
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return unixTime(f)
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, text); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

func unixTime(seconds float64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	if seconds > 10_000_000_000 {
		seconds /= 1000
	}
	whole := int64(seconds)
	nanos := int64((seconds - float64(whole)) * 1e9)
	return time.Unix(whole, nanos).UTC()
}
