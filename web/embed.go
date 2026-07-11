// Package web embeds the dashboard so the whole tool ships as one binary.
package web

import "embed"

//go:embed index.html login.html styles.css vendor js
var FS embed.FS
