// Package webui: embedded control panel (single HTML, go:embed).
package webui

import _ "embed"

//go:embed page.html
var page string

func Page() string { return page }
