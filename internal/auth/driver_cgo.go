//go:build cgo

package auth

import _ "github.com/mattn/go-sqlite3"

const (
	driverAvailable = true
	driverName      = "sqlite3"
)
