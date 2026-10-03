// Package web holds the management page. The HTML, CSS and JavaScript are
// kept in separate files for editing and served as one document.
package web

import (
	_ "embed"
	"strings"
	"sync"
)

var (
	//go:embed page.html
	pageHTML string
	//go:embed page.css
	pageCSS string
	//go:embed page.js
	pageJS string

	pageOnce  sync.Once
	pageBytes []byte
)

// Page returns the management page with its CSS and JavaScript inlined.
func Page() []byte {
	pageOnce.Do(func() {
		page := strings.Replace(pageHTML, "/*CSS*/", pageCSS, 1)
		page = strings.Replace(page, "/*JS*/", pageJS, 1)
		pageBytes = []byte(page)
	})
	return pageBytes
}
