// Package auth is a small user/session store backed by its own SQLite file,
// separate from the metrics database. On first setup it seeds a default
// admin/admin user flagged must-change, so the operator is forced to pick a
// real password on first login. Passwords are bcrypt-hashed; sessions are
// random opaque tokens stored server-side (handed to the browser as an
// HttpOnly cookie).
package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS users (
	username      TEXT PRIMARY KEY,
	password_hash TEXT    NOT NULL,
	must_change   INTEGER NOT NULL DEFAULT 0,  -- 1 = force a password change at next login
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	username   TEXT    NOT NULL,
	expires_at INTEGER NOT NULL
);
`

// SessionTTL is how long a login lasts before re-authentication is required.
const SessionTTL = 7 * 24 * time.Hour

// dummyHash lets Authenticate spend roughly the same time on a missing user as
// on a real one, so response timing doesn't reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)

type Store struct {
	sql    *sql.DB
	logger *log.Logger
}

// Open creates/opens the auth database at path and ensures the schema and the
// default admin user exist.
func Open(path string, logger *log.Logger) (*Store, error) {
	if !driverAvailable {
		return nil, fmt.Errorf("sqlite driver not compiled in (build the server with CGO_ENABLED=1)")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	sq, err := sql.Open(driverName, path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1)
	if _, err := sq.Exec(schema); err != nil {
		sq.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	s := &Store{sql: sq, logger: logger}
	if err := s.seed(); err != nil {
		sq.Close()
		return nil, err
	}
	logger.Printf("auth: enabled (db=%s)", path)
	return s, nil
}

// seed installs the default admin/admin (must-change) on first setup only.
func (s *Store) seed() error {
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, err := s.sql.Exec(
		`INSERT INTO users(username, password_hash, must_change, created_at) VALUES(?, ?, 1, ?)`,
		"admin", string(hash), time.Now().Unix()); err != nil {
		return err
	}
	s.logger.Printf("auth: first-time setup — created default user admin/admin (you must set a new password at first login)")
	return nil
}

func (s *Store) Close() error { return s.sql.Close() }

// Authenticate verifies a username/password. mustChange reports whether the
// user still needs to replace a seeded/reset password before proceeding.
func (s *Store) Authenticate(user, pass string) (mustChange, ok bool) {
	var hash string
	var mc int
	err := s.sql.QueryRow(`SELECT password_hash, must_change FROM users WHERE username = ?`, user).Scan(&hash, &mc)
	if err != nil {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(pass)) // equalize timing for unknown users
		return false, false
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return false, false
	}
	return mc != 0, true
}

// MustChange reports whether the user must change their password.
func (s *Store) MustChange(user string) bool {
	var mc int
	if s.sql.QueryRow(`SELECT must_change FROM users WHERE username = ?`, user).Scan(&mc) != nil {
		return false
	}
	return mc != 0
}

// SetPassword updates a user's password and clears the must-change flag.
func (s *Store) SetPassword(user, newPass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.sql.Exec(`UPDATE users SET password_hash = ?, must_change = 0 WHERE username = ?`, string(hash), user)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown user %q", user)
	}
	return nil
}

// CreateSession issues a new opaque session token for a user.
func (s *Store) CreateSession(user string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if _, err := s.sql.Exec(`INSERT INTO sessions(token, username, expires_at) VALUES(?, ?, ?)`,
		token, user, time.Now().Add(SessionTTL).Unix()); err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser returns the user for a valid, unexpired session token.
func (s *Store) SessionUser(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	var user string
	var exp int64
	if s.sql.QueryRow(`SELECT username, expires_at FROM sessions WHERE token = ?`, token).Scan(&user, &exp) != nil {
		return "", false
	}
	if time.Now().Unix() > exp {
		s.sql.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return "", false
	}
	return user, true
}

// DeleteSession invalidates a session token (logout).
func (s *Store) DeleteSession(token string) {
	if token != "" {
		s.sql.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	}
}
