package quota

import (
	"testing"
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
