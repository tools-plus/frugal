// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

//go:build cgo

package db

import _ "github.com/mattn/go-sqlite3"

const (
	driverAvailable = true
	driverName      = "sqlite3"
)
