// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/tools-plus/frugal/internal/secret"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS users (
	username      TEXT PRIMARY KEY,
	password_hash TEXT    NOT NULL,
	must_change   INTEGER NOT NULL DEFAULT 0,     -- 1 = force a password change at next login
	role          TEXT    NOT NULL DEFAULT 'viewer', -- 'admin' (manage users + view) | 'viewer' (read-only)
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	username   TEXT    NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS roles (
	name     TEXT PRIMARY KEY,
	is_admin INTEGER NOT NULL DEFAULT 0,      -- admin roles manage users/roles and see everything
	services TEXT    NOT NULL DEFAULT '[]'    -- JSON array of allowed service keys; ["*"] = all
);
CREATE TABLE IF NOT EXISTS config (
	id  INTEGER PRIMARY KEY CHECK (id = 1),   -- single row
	doc TEXT NOT NULL                         -- JSON config.Runtime; secret fields encrypted
);
`

// allServices is the sentinel in a role's service list meaning "every service".
const allServices = "*"

// systemUser is the built-in admin account seeded on first setup. Its role is
// locked so it can't be demoted out of admin (lockout protection).
const systemUser = "admin"

// SessionTTL is how long a login lasts before re-authentication is required.
const SessionTTL = 7 * 24 * time.Hour

// dummyHash lets Authenticate spend roughly the same time on a missing user as
// on a real one, so response timing doesn't reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equalizer"), bcrypt.DefaultCost)

type Store struct {
	sql    *sql.DB
	cipher *secret.Cipher
	logger *log.Logger
}

// Open creates/opens the control database at path (users, roles, sessions, and
// encrypted runtime config) and ensures the schema and default admin exist.
// cipher encrypts credential fields at rest; pass secret.New("") when no key is
// configured (secrets then can't be stored, but the rest still works).
func Open(path string, cipher *secret.Cipher, logger *log.Logger) (*Store, error) {
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
	s := &Store{sql: sq, cipher: cipher, logger: logger}
	if err := s.migrate(); err != nil {
		sq.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.seedRoles(); err != nil {
		sq.Close()
		return nil, err
	}
	if err := s.seed(); err != nil {
		sq.Close()
		return nil, err
	}
	logger.Printf("auth: enabled (db=%s)", path)
	return s, nil
}

// migrate brings older auth databases up to the current schema. Databases
// created before roles existed had no role column and treated every user as
// unrestricted, so those users are promoted to admin to preserve their access.
func (s *Store) migrate() error {
	has, err := s.hasColumn("users", "role")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.sql.Exec(`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'viewer'`); err != nil {
			return err
		}
		if _, err := s.sql.Exec(`UPDATE users SET role = 'admin'`); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) hasColumn(table, col string) (bool, error) {
	rows, err := s.sql.Query(`PRAGMA table_info(` + table + `)`) // table is a package constant, not user input
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
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
		`INSERT INTO users(username, password_hash, must_change, role, created_at) VALUES(?, ?, 1, 'admin', ?)`,
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

// ---------------------------------------------------------------- users / roles

// User is a directory entry (no password material).
type User struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	MustChange bool   `json:"must_change"`
}

// Role is a named permission set: an admin role manages users/roles and sees
// everything; a non-admin role grants view access only to Services (["*"] = all).
type Role struct {
	Name     string   `json:"name"`
	IsAdmin  bool     `json:"is_admin"`
	Services []string `json:"services"`
}

// seedRoles installs the built-in roles on first setup: admin (management +
// all services) and viewer (all services, read-only).
func (s *Store) seedRoles() error {
	var n int
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM roles`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	all := `["` + allServices + `"]`
	if _, err := s.sql.Exec(`INSERT INTO roles(name, is_admin, services) VALUES('admin', 1, ?)`, all); err != nil {
		return err
	}
	if _, err := s.sql.Exec(`INSERT INTO roles(name, is_admin, services) VALUES('viewer', 0, ?)`, all); err != nil {
		return err
	}
	return nil
}

func (s *Store) roleExists(name string) bool {
	var n int
	s.sql.QueryRow(`SELECT COUNT(*) FROM roles WHERE name = ?`, name).Scan(&n)
	return n > 0
}

// Role returns a user's role name, or "" if unknown.
func (s *Store) Role(user string) string {
	var r string
	if s.sql.QueryRow(`SELECT role FROM users WHERE username = ?`, user).Scan(&r) != nil {
		return ""
	}
	return r
}

// IsAdmin reports whether the user's role is an admin role.
func (s *Store) IsAdmin(user string) bool {
	var a int
	if s.sql.QueryRow(`SELECT r.is_admin FROM users u JOIN roles r ON u.role = r.name WHERE u.username = ?`, user).Scan(&a) != nil {
		return false
	}
	return a != 0
}

// UserAccess returns the effective access for a user: whether they are an admin
// (sees all + manages), and the list of service keys their role grants (may be
// ["*"]). Unknown users get no access.
func (s *Store) UserAccess(user string) (isAdmin bool, services []string) {
	var a int
	var svcJSON string
	if s.sql.QueryRow(`SELECT r.is_admin, r.services FROM users u JOIN roles r ON u.role = r.name WHERE u.username = ?`, user).Scan(&a, &svcJSON) != nil {
		return false, nil
	}
	json.Unmarshal([]byte(svcJSON), &services)
	return a != 0, services
}

// ---------------------------------------------------------------- role CRUD

func (s *Store) ListRoles() ([]Role, error) {
	rows, err := s.sql.Query(`SELECT name, is_admin, services FROM roles ORDER BY is_admin DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		var a int
		var svc string
		if err := rows.Scan(&r.Name, &a, &svc); err != nil {
			return nil, err
		}
		r.IsAdmin = a != 0
		json.Unmarshal([]byte(svc), &r.Services)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateRole adds a non-admin, service-scoped role. (Admin is built-in; new
// roles grant read access to the listed services.)
func (s *Store) CreateRole(name string, services []string) error {
	if name == "" {
		return fmt.Errorf("role name required")
	}
	if name == "admin" || name == "viewer" {
		return fmt.Errorf("%q is a built-in role", name)
	}
	if len(services) == 0 {
		return fmt.Errorf("choose at least one service")
	}
	if s.roleExists(name) {
		return fmt.Errorf("role %q already exists", name)
	}
	svc, _ := json.Marshal(services)
	_, err := s.sql.Exec(`INSERT INTO roles(name, is_admin, services) VALUES(?, 0, ?)`, name, string(svc))
	return err
}

// UpdateRole changes the services of an existing non-admin role. The built-in
// admin role cannot be edited.
func (s *Store) UpdateRole(name string, services []string) error {
	if name == "admin" {
		return fmt.Errorf("the admin role cannot be edited")
	}
	if len(services) == 0 {
		return fmt.Errorf("choose at least one service")
	}
	svc, _ := json.Marshal(services)
	res, err := s.sql.Exec(`UPDATE roles SET services = ? WHERE name = ? AND is_admin = 0`, string(svc), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown role %q", name)
	}
	return nil
}

// DeleteRole removes a role. Built-in roles and roles still assigned to a user
// cannot be deleted.
func (s *Store) DeleteRole(name string) error {
	if name == "admin" || name == "viewer" {
		return fmt.Errorf("%q is a built-in role", name)
	}
	var inUse int
	s.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, name).Scan(&inUse)
	if inUse > 0 {
		return fmt.Errorf("role %q is assigned to %d user(s)", name, inUse)
	}
	res, err := s.sql.Exec(`DELETE FROM roles WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown role %q", name)
	}
	return nil
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.sql.Query(`SELECT username, role, must_change FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var mc int
		if err := rows.Scan(&u.Username, &u.Role, &mc); err != nil {
			return nil, err
		}
		u.MustChange = mc != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// CreateUser adds a user with an admin-assigned (temporary) password: they are
// forced to set their own at first login.
func (s *Store) CreateUser(username, password, role string) error {
	if username == "" {
		return fmt.Errorf("username required")
	}
	if len(password) < 6 {
		return fmt.Errorf("password must be at least 6 characters")
	}
	if !s.roleExists(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	var n int
	s.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, username).Scan(&n)
	if n > 0 {
		return fmt.Errorf("user %q already exists", username)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.sql.Exec(
		`INSERT INTO users(username, password_hash, must_change, role, created_at) VALUES(?, ?, 1, ?, ?)`,
		username, string(hash), role, time.Now().Unix())
	return err
}

// DeleteUser removes a user and their sessions. The last admin cannot be
// removed (lockout protection).
func (s *Store) DeleteUser(username string) error {
	if s.IsAdmin(username) && s.adminCount() <= 1 {
		return fmt.Errorf("cannot delete the last admin")
	}
	res, err := s.sql.Exec(`DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown user %q", username)
	}
	s.sql.Exec(`DELETE FROM sessions WHERE username = ?`, username)
	return nil
}

// ResetPassword sets a user's password to an admin-assigned temporary one:
// must-change is set and existing sessions are invalidated.
func (s *Store) ResetPassword(username, newPass string) error {
	if len(newPass) < 6 {
		return fmt.Errorf("password must be at least 6 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	res, err := s.sql.Exec(`UPDATE users SET password_hash = ?, must_change = 1 WHERE username = ?`, string(hash), username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown user %q", username)
	}
	s.sql.Exec(`DELETE FROM sessions WHERE username = ?`, username)
	return nil
}

// SetRole changes a user's role. The last admin cannot be demoted.
func (s *Store) SetRole(username, role string) error {
	if username == systemUser {
		return fmt.Errorf("cannot change the role of the built-in %q user", systemUser)
	}
	if !s.roleExists(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	var newIsAdmin int
	s.sql.QueryRow(`SELECT is_admin FROM roles WHERE name = ?`, role).Scan(&newIsAdmin)
	if newIsAdmin == 0 && s.IsAdmin(username) && s.adminCount() <= 1 {
		return fmt.Errorf("cannot demote the last admin")
	}
	res, err := s.sql.Exec(`UPDATE users SET role = ? WHERE username = ?`, role, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown user %q", username)
	}
	return nil
}

func (s *Store) adminCount() int {
	var n int
	s.sql.QueryRow(`SELECT COUNT(*) FROM users u JOIN roles r ON u.role = r.name WHERE r.is_admin = 1`).Scan(&n)
	return n
}
