// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

//go:build !cgo

// Without cgo (e.g. CGO_ENABLED=0 cross-builds for agents) the sqlite
// driver is omitted; the server falls back to in-memory only. For a pure-Go
// server binary, replace the mattn import in driver_cgo.go with
// modernc.org/sqlite (driver name "sqlite") and drop the build tags.

package db

const (
	driverAvailable = false
	driverName      = "sqlite3"
)
