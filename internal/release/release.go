// Package release reads the latest Lamplighter release from GitHub so the
// management page can point to a newer version.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// LatestURL is the GitHub API address of the latest published release.
// Drafts and prereleases are not returned by this endpoint.
var LatestURL = "https://api.github.com/repos/PosvdM/cpa-plugin-lamplighter/releases/latest"

// Release is a published release.
type Release struct {
	// Version is the tag without the leading "v".
	Version string
	URL     string
}

// Latest reads the latest release.
func Latest(ctx context.Context, client *http.Client, userAgent string) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestURL, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Release{}, fmt.Errorf("GitHub 返回的 Release 无法解析: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(payload.TagName), "v")
	if parse(version) == nil || !strings.HasPrefix(payload.HTMLURL, "https://github.com/") {
		return Release{}, fmt.Errorf("GitHub 返回的 Release 无效: %q", payload.TagName)
	}
	return Release{Version: version, URL: payload.HTMLURL}, nil
}

// Comparable reports whether version is a release version such as "0.4.0".
// Development builds ("dev", "0.0.0-dev.<commit>") are not.
func Comparable(version string) bool {
	return parse(version) != nil
}

// Newer reports whether latest is a later version than current. Versions
// are compared by numeric segment; a version that is not comparable is
// never newer and never older.
func Newer(current, latest string) bool {
	a, b := parse(current), parse(latest)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return y > x
		}
	}
	return false
}

// parse splits a dotted numeric version, returning nil when any segment is
// not a number.
func parse(version string) []int {
	if version == "" {
		return nil
	}
	parts := strings.Split(version, ".")
	out := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || part != strconv.Itoa(n) {
			return nil
		}
		out[i] = n
	}
	return out
}
