// Package web embeds the dashboard so the whole tool ships as one binary.
package web

import "embed"

//go:embed index.html
var FS embed.FS
