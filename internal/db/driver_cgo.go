//go:build cgo

package db

import _ "github.com/mattn/go-sqlite3"

const (
	driverAvailable = true
	driverName      = "sqlite3"
)
