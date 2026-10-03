package quota

import (
	"net/http"
	"strings"
	"time"
)

// headerValue returns the last value of name, matching the name case-insensitively.
func headerValue(header http.Header, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[len(values)-1])
		}
	}
	return ""
}

// ParsePassive extracts quota windows from the upstream response headers of a
// Claude or Codex model request. It returns false when the headers carry no
// quota information. The returned group only holds the windows present in the
// headers; Claude headers do not include the Fable window.
func ParsePassive(provider string, header http.Header, now time.Time) (RawGroup, bool) {
	switch provider {
	case "claude":
		return parseClaudeHeaders(header)
	case "codex":
		return parseCodexHeaders(header, now)
	}
	return RawGroup{}, false
}

func parseClaudeHeaders(header http.Header) (RawGroup, bool) {
	group := RawGroup{Key: "claude:main", SourceLabel: "Claude"}
	for _, spec := range []struct{ prefix, id, label string }{
		{"Anthropic-Ratelimit-Unified-5h-", WindowFiveHour, LabelFiveHour},
		{"Anthropic-Ratelimit-Unified-7d-", WindowSevenDay, LabelSevenDay},
	} {
		utilization, ok := floatValue(headerValue(header, spec.prefix+"Utilization"))
		if !ok {
			continue
		}
		// The header is a fraction of the window, for example 0.2 for 20%.
		group.Windows = append(group.Windows, Window{
			ID:        spec.id,
			Label:     spec.label,
			Remaining: clampPercent(100 - utilization*100),
			Reset:     ParseTime(headerValue(header, spec.prefix+"Reset")),
		})
	}
	return group, len(group.Windows) > 0
}

func parseCodexHeaders(header http.Header, now time.Time) (RawGroup, bool) {
	type parsed struct {
		minutes float64
		window  Window
	}
	var found []parsed
	for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		used, ok := floatValue(headerValue(header, prefix+"Used-Percent"))
		if !ok {
			continue
		}
		reset := ParseTime(headerValue(header, prefix+"Reset-At"))
		if reset.IsZero() {
			if after, ok := floatValue(headerValue(header, prefix+"Reset-After-Seconds")); ok {
				reset = now.Add(time.Duration(after * float64(time.Second))).UTC()
			}
		}
		minutes, _ := floatValue(headerValue(header, prefix+"Window-Minutes"))
		found = append(found, parsed{minutes: minutes, window: Window{Remaining: clampPercent(100 - used), Reset: reset}})
	}
	if len(found) == 0 {
		return RawGroup{}, false
	}
	group := RawGroup{Key: "codex:main", SourceLabel: ProviderTitle("codex")}
	haveFive, haveWeek := false, false
	assign := func(item parsed, id, label string) {
		item.window.ID = id
		item.window.Label = label
		group.Windows = append(group.Windows, item.window)
	}
	var unassigned []parsed
	for _, item := range found {
		switch {
		case int(item.minutes) == 300 && !haveFive:
			assign(item, WindowFiveHour, LabelFiveHour)
			haveFive = true
		case int(item.minutes) == 10080 && !haveWeek:
			assign(item, WindowSevenDay, LabelSevenDay)
			haveWeek = true
		default:
			unassigned = append(unassigned, item)
		}
	}
	// Without a window length, the primary window is the 5-hour one and the
	// secondary window is the weekly one.
	for _, item := range unassigned {
		switch {
		case !haveFive:
			assign(item, WindowFiveHour, LabelFiveHour)
			haveFive = true
		case !haveWeek:
			assign(item, WindowSevenDay, LabelSevenDay)
			haveWeek = true
		}
	}
	return group, len(group.Windows) > 0
}
