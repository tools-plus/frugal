// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

//go:build !cgo

// Without cgo the sqlite driver is omitted (see internal/db for the same
// pattern); auth persistence requires a CGO server build.
package auth

const (
	driverAvailable = false
	driverName      = "sqlite3"
)
