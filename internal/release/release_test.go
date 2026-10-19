package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.4.0", "0.5.0", true},
		{"0.4.0", "0.4.1", true},
		{"0.9.0", "0.10.0", true},
		{"0.4", "0.4.1", true},
		{"0.4.0", "0.4.0", false},
		{"0.4.0", "0.4", false},
		{"0.5.0", "0.4.9", false},
		{"dev", "0.5.0", false},
		{"0.0.0-dev.1a2b3c4", "0.5.0", false},
		{"0.4.0", "", false},
		{"0.4.0", "0.5.0-rc.1", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestComparable(t *testing.T) {
	for version, want := range map[string]bool{"0.4.0": true, "1": true, "dev": false, "0.0.0-dev.abc": false, "": false, "v0.4.0": false} {
		if got := Comparable(version); got != want {
			t.Errorf("Comparable(%q) = %v, want %v", version, got, want)
		}
	}
}

func serve(t *testing.T, status int, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "cpa-plugin-lamplighter/0.4.0" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	old := LatestURL
	LatestURL = server.URL
	t.Cleanup(func() { LatestURL = old })
}

func TestLatest(t *testing.T) {
	serve(t, http.StatusOK, `{"tag_name":"v0.5.0","html_url":"https://github.com/PosvdM/cpa-plugin-lamplighter/releases/tag/v0.5.0"}`)
	got, err := Latest(context.Background(), http.DefaultClient, "cpa-plugin-lamplighter/0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	want := Release{Version: "0.5.0", URL: "https://github.com/PosvdM/cpa-plugin-lamplighter/releases/tag/v0.5.0"}
	if got != want {
		t.Fatalf("Latest = %+v, want %+v", got, want)
	}
}

func TestLatestFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"rate limited": {http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
		"bad json":     {http.StatusOK, `<html>`},
		"no tag":       {http.StatusOK, `{"tag_name":"","html_url":"https://github.com/x/y"}`},
		"bad url":      {http.StatusOK, `{"tag_name":"v0.5.0","html_url":"javascript:alert(1)"}`},
	} {
		t.Run(name, func(t *testing.T) {
			serve(t, c.status, c.body)
			if got, err := Latest(context.Background(), http.DefaultClient, "cpa-plugin-lamplighter/0.4.0"); err == nil {
				t.Fatalf("Latest = %+v, want an error", got)
			}
		})
	}
}

func TestLatestWithNonNumericTagIsNeverNewer(t *testing.T) {
	serve(t, http.StatusOK, `{"tag_name":"v0.6.0-beta","html_url":"https://github.com/PosvdM/cpa-plugin-lamplighter/releases/tag/v0.6.0-beta"}`)
	got, err := Latest(context.Background(), http.DefaultClient, "cpa-plugin-lamplighter/0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if Newer("0.4.0", got.Version) {
		t.Fatalf("%q is reported as newer", got.Version)
	}
}
