package quota

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	cases := map[string]any{
		"seconds":      float64(1791064435),
		"milliseconds": float64(1791064435000),
		"string":       "1791064435",
		"iso":          "2026-10-03T21:53:55Z",
	}
	for name, value := range cases {
		if got := ParseTime(value).Unix(); got != 1791064435 {
			t.Errorf("%s: got %d", name, got)
		}
	}
	if !ParseTime("").IsZero() || !ParseTime(nil).IsZero() {
		t.Error("empty values must give zero time")
	}
}

func TestKindPrefersThePeriod(t *testing.T) {
	cases := []struct {
		w    Window
		want string
	}{
		{Window{ID: WindowFiveHour, Label: LabelFiveHour}, KindFiveHour},
		{Window{ID: "gemini-5h", Label: LabelFiveHour}, KindFiveHour},
		{Window{ID: WindowSevenDay}, KindSevenDay},
		{Window{ID: "seven-day-opus", Label: "7 Day Opus"}, KindSevenDay},
		{Window{ID: WindowFable, Label: LabelFable}, KindSevenDay},
		{Window{ID: "limit-1", Label: "1d"}, ""},
		{Window{ID: "x", Label: "x", Period: 5 * time.Hour}, KindFiveHour},
		{Window{ID: "x", Label: "x", Period: 7*24*time.Hour + time.Minute}, KindSevenDay},
		{Window{ID: WindowFiveHour, Label: LabelFiveHour, Period: 24 * time.Hour}, ""},
		{Window{ID: "monthly", Label: "Month", Period: 30 * 24 * time.Hour}, ""},
	}
	for _, c := range cases {
		if got := Kind(c.w); got != c.want {
			t.Errorf("Kind(%+v) = %q, want %q", c.w, got, c.want)
		}
	}
	if id, label, ok := Standard(7 * 24 * time.Hour); !ok || id != WindowSevenDay || label != LabelSevenDay {
		t.Errorf("Standard(7d) = %s %s %v", id, label, ok)
	}
	if _, _, ok := Standard(24 * time.Hour); ok {
		t.Error("a day is not a standard window")
	}
}
