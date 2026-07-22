// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// Package web embeds the dashboard so the whole tool ships as one binary.
package web

import "embed"

//go:embed index.html login.html styles.css vendor js
var FS embed.FS
