// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// Package server exposes the dashboard and its APIs. Live data flows over
// Server-Sent Events — one-directional push is all a chart needs, and it
// keeps the binary free of websocket dependencies.
package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tools-plus/frugal/internal/auth"
	"github.com/tools-plus/frugal/internal/config"
	"github.com/tools-plus/frugal/internal/k8s"
	"github.com/tools-plus/frugal/internal/logstore"
	"github.com/tools-plus/frugal/internal/store"
)

// Historian answers on-demand range queries against the metric origin
// (CloudWatch) for windows longer than the in-memory ring buffer.
type Historian interface {
	History(ctx context.Context, id string, from, to time.Time) ([]store.Point, error)
}

// Cluster pairs a cluster name with its API client.
type Cluster struct {
	Name   string
	Client *k8s.Client
}

// Authenticator is the user/session backend (internal/auth). Nil disables
// authentication, restoring the original open-dashboard behavior.
type Authenticator interface {
	Authenticate(user, pass string) (mustChange, ok bool)
	CreateSession(user string) (string, error)
	SessionUser(token string) (string, bool)
	MustChange(user string) bool
	SetPassword(user, newPass string) error
	DeleteSession(token string)

	// multi-user management (admin only, see adminOnly)
	Role(user string) string
	IsAdmin(user string) bool
	UserAccess(user string) (isAdmin bool, services []string)
	ListUsers() ([]auth.User, error)
	CreateUser(user, pass, role string) error
	DeleteUser(user string) error
	ResetPassword(user, newPass string) error
	SetRole(user, role string) error

	// role management (admin only)
	ListRoles() ([]auth.Role, error)
	CreateRole(name string, services []string) error
	UpdateRole(name string, services []string) error
	DeleteRole(name string) error
}

// Deps are the web server's dependencies. Collector-derived values (clusters,
// historian, status, ingest token) are getters because the collector service
// is reconfigured at runtime; the auth store is always present (it also holds
// config), and AuthEnabled toggles whether the login is enforced.
type Deps struct {
	Store    *store.Store
	Logs     *logstore.Store
	Inv      *k8s.Inventory
	Clusters func() []Cluster
	Hist     func() Historian
	// HistoryDB serves persisted history from the local store (all sources,
	// including k8s/agent/native that have no external origin). nil when
	// persistence is disabled. step > 1 downsamples into step-second buckets.
	HistoryDB    func(id string, from, to, step int64) ([]store.Point, error)
	Status       func() map[string]any
	IngestToken  func() string
	Authn        Authenticator
	AuthEnabled  bool
	GetConfig    func() (config.Runtime, error)
	SaveConfig   func(config.Runtime) error // persists + re-applies collectors
	HasSecretKey func() bool
	Assets       embed.FS
	Logger       *log.Logger
}

type Server struct {
	store       *store.Store
	logs        *logstore.Store
	inv         *k8s.Inventory
	getClusters func() []Cluster
	getHist     func() Historian
	historyDB   func(id string, from, to, step int64) ([]store.Point, error)
	getStatus   func() map[string]any
	ingestToken func() string
	authn       Authenticator // always non-nil (control store)
	authEnabled bool          // whether the login is enforced
	getConfig   func() (config.Runtime, error)
	saveConfig  func(config.Runtime) error
	hasKey      func() bool
	logger      *log.Logger
	assets      embed.FS
	mux         *http.ServeMux
}

func New(d Deps) *Server {
	s := &Server{
		store: d.Store, logs: d.Logs, inv: d.Inv,
		getClusters: d.Clusters, getHist: d.Hist, historyDB: d.HistoryDB, getStatus: d.Status, ingestToken: d.IngestToken,
		authn: d.Authn, authEnabled: d.AuthEnabled,
		getConfig: d.GetConfig, saveConfig: d.SaveConfig, hasKey: d.HasSecretKey,
		logger: d.Logger, assets: d.Assets,
	}
	assets := d.Assets
	mux := http.NewServeMux()

	// Public: the login page, the auth endpoints, and static assets (JS/CSS
	// carry no data — the sensitive surface is the dashboard HTML and the data
	// APIs, which are gated below).
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("POST /api/change-password", s.handleChangePassword)
	mux.HandleFunc("GET /api/me", s.handleMe)

	// User management — admin role only.
	mux.HandleFunc("GET /api/users", s.adminOnly(s.handleListUsers))
	mux.HandleFunc("POST /api/users", s.adminOnly(s.handleCreateUser))
	mux.HandleFunc("DELETE /api/users/{name}", s.adminOnly(s.handleDeleteUser))
	mux.HandleFunc("POST /api/users/{name}/password", s.adminOnly(s.handleResetUserPassword))
	mux.HandleFunc("POST /api/users/{name}/role", s.adminOnly(s.handleSetUserRole))
	mux.HandleFunc("GET /api/roles", s.adminOnly(s.handleListRoles))
	mux.HandleFunc("POST /api/roles", s.adminOnly(s.handleCreateRole))
	mux.HandleFunc("POST /api/roles/{name}", s.adminOnly(s.handleUpdateRole))
	mux.HandleFunc("DELETE /api/roles/{name}", s.adminOnly(s.handleDeleteRole))
	mux.HandleFunc("GET /api/settings", s.adminOnly(s.handleGetSettings))
	mux.HandleFunc("POST /api/settings", s.adminOnly(s.handleSaveSettings))
	assetFS := http.FileServerFS(assets)
	mux.Handle("GET /styles.css", assetFS)
	mux.Handle("GET /js/", assetFS)
	mux.Handle("GET /vendor/", assetFS)

	// Gated: the dashboard and every data endpoint require a valid session
	// (and a completed password change) when auth is enabled.
	mux.HandleFunc("GET /", s.gate(s.handleIndex))
	mux.HandleFunc("GET /api/series", s.gate(s.handleSeries))
	mux.HandleFunc("GET /api/series/data", s.gate(s.handleSeriesData))
	mux.HandleFunc("GET /api/history", s.gate(s.handleHistory))
	mux.HandleFunc("GET /api/stream", s.gate(s.handleStream))
	mux.HandleFunc("GET /api/pods", s.gate(s.handlePods))
	mux.HandleFunc("GET /api/status", s.gate(s.handleStatus))
	mux.HandleFunc("GET /api/logs", s.gate(s.handleLogs))
	mux.HandleFunc("GET /api/agentlogs", s.gate(s.handleAgentLogs))

	// Agent push endpoints stay on the shared bearer token (agents can't do
	// an interactive login).
	mux.HandleFunc("POST /api/ingest", s.auth(s.handleIngest))
	mux.HandleFunc("POST /api/ingest/logs", s.auth(s.handleIngestLogs))
	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := s.assets.ReadFile("index.html")
	if err != nil {
		http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// GET /api/series?filter=cw|RDS — filtered to the services the caller's role
// allows (admins and auth-disabled see everything).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	_, allow := s.access(r)
	all := s.store.List(r.URL.Query().Get("filter"))
	out := make([]store.SeriesMeta, 0, len(all))
	for _, m := range all {
		if allow(serviceOf(m.Labels)) {
			out = append(out, m)
		}
	}
	writeJSON(w, out)
}

// GET /api/series/data?id=<series id>
func (s *Server) handleSeriesData(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if isAdmin, allow := s.access(r); !isAdmin {
		labels, ok := s.store.Labels(id)
		if !ok || !allow(serviceOf(labels)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	writeJSON(w, s.store.Data(id))
}

// GET /api/history?id=<series id>&from=<unix>&to=<unix>
// Long-range history beyond the in-memory ring. CloudWatch series are fetched
// from CloudWatch (finer resolution, arbitrary range); every other source
// (k8s, agent, native) is served from the persisted SQLite store.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	from, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	to, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if id == "" || err1 != nil || err2 != nil || to <= from {
		http.Error(w, "id, from, to (unix seconds) required", http.StatusBadRequest)
		return
	}
	if to-from > 90*24*3600 {
		http.Error(w, "range too large (max 90 days)", http.StatusBadRequest)
		return
	}
	labels, ok := s.store.Labels(id)
	if isAdmin, allow := s.access(r); !isAdmin {
		if !ok || !allow(serviceOf(labels)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	var pts []store.Point
	var err error
	if labels["source"] == "cloudwatch" && s.getHist() != nil {
		pts, err = s.getHist().History(r.Context(), id, time.Unix(from, 0), time.Unix(to, 0))
	} else if s.historyDB != nil {
		pts, err = s.historyDB(id, from, to, historyStep(to-from))
	} else {
		http.Error(w, "history unavailable (persistence disabled)", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if pts == nil {
		pts = []store.Point{}
	}
	writeJSON(w, pts)
}

// historyStep picks a downsample bucket (seconds) for a DB history span, so a
// wide range returns O(hundreds–thousands) of points rather than every raw
// sample. Mirrors the CloudWatch period tiers. 0/1 means "raw, no bucketing".
func historyStep(span int64) int64 {
	switch {
	case span <= 6*3600:
		return 0
	case span <= 48*3600:
		return 60
	case span <= 7*24*3600:
		return 300
	default:
		return 3600
	}
}

// GET /api/pods — served from the collectors' shared in-memory inventory
// (refreshed every poll), so page loads never wait on cluster API calls.
func (s *Server) handlePods(w http.ResponseWriter, r *http.Request) {
	if _, allow := s.access(r); !allow("EKS") { // pods belong to the EKS service
		writeJSON(w, []k8s.PodInfo{})
		return
	}
	if s.inv == nil {
		writeJSON(w, []k8s.PodInfo{})
		return
	}
	writeJSON(w, s.inv.All())
}

// GET /api/status — collector health for debugging "why is X empty".
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if s.getStatus != nil {
		out = s.getStatus()
	}
	clusters := s.getClusters()
	names := make([]string, 0, len(clusters))
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	out["clusters"] = names
	out["series"] = len(s.store.List(""))
	writeJSON(w, out)
}

// clusterFor returns the client for a named cluster (or the first one).
func (s *Server) clusterFor(name string) *k8s.Client {
	clusters := s.getClusters()
	for _, c := range clusters {
		if c.Name == name {
			return c.Client
		}
	}
	if len(clusters) > 0 && name == "" {
		return clusters[0].Client
	}
	return nil
}

func sseHeaders(w http.ResponseWriter) (http.Flusher, bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	return fl, true
}

// GET /api/stream — SSE of every new point landing in the store.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := sseHeaders(w)
	if !ok {
		return
	}
	ch, cancel := s.store.Subscribe()
	defer cancel()
	_, allow := s.access(r) // filter the live fan-out to the caller's services

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case u := <-ch:
			if !allow(serviceOf(u.Labels)) {
				continue
			}
			b, _ := json.Marshal(u)
			fmt.Fprintf(w, "event: point\ndata: %s\n\n", b)
			fl.Flush()
		}
	}
}

// GET /api/logs?namespace=default&pod=web-abc&container=app&tail=200
// Streams pod logs as SSE `log` events, one per line.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if _, allow := s.access(r); !allow("EKS") { // pod logs belong to the EKS service
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	ns, pod := q.Get("namespace"), q.Get("pod")
	kc := s.clusterFor(q.Get("cluster"))
	if kc == nil {
		// Fall back to logs shipped by the DaemonSet agent, if any.
		r.URL.RawQuery = "source=" + url.QueryEscape("pod/"+ns+"/"+pod) + "&tail=" + q.Get("tail")
		s.handleAgentLogs(w, r)
		return
	}
	if ns == "" || pod == "" {
		http.Error(w, "namespace and pod required", http.StatusBadRequest)
		return
	}
	tail, _ := strconv.Atoi(q.Get("tail"))
	if tail <= 0 {
		tail = 200
	}

	fl, ok := sseHeaders(w)
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	body, err := kc.StreamLogs(ctx, ns, pod, q.Get("container"), tail)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
		fl.Flush()
		return
	}
	defer body.Close()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", jsonString(sc.Text()))
		fl.Flush()
	}
	fmt.Fprint(w, "event: eof\ndata: {}\n\n")
	fl.Flush()
}

// ---------------------------------------------------------------- sessions

const sessionCookie = "frugal_session"

// sessionUser returns the logged-in user for the request's session cookie.
func (s *Server) sessionUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	return s.authn.SessionUser(c.Value)
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure:  r.TLS != nil,
		Expires: time.Now().Add(sessionTTL), MaxAge: int(sessionTTL.Seconds()),
	})
}

// sessionTTL mirrors auth.SessionTTL for the cookie lifetime (kept as a local
// constant so the server package doesn't import auth just for a duration).
const sessionTTL = 7 * 24 * time.Hour

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

// gate protects the dashboard and data APIs: a valid session is required, and
// a user still flagged must-change is treated as not-yet-authed (so they can't
// bypass the forced password change). No-op when auth is disabled.
func (s *Server) gate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled {
			next(w, r)
			return
		}
		user, ok := s.sessionUser(r)
		if !ok || s.authn.MustChange(user) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/login", http.StatusFound)
			}
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	b, err := s.assets.ReadFile("login.html")
	if err != nil {
		http.Error(w, "login asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

// GET /api/me — auth status for the login page and dashboard header.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	user, ok := s.sessionUser(r)
	role, isAdmin := "", false
	if ok {
		role = s.authn.Role(user)
		isAdmin = s.authn.IsAdmin(user)
	}
	writeJSON(w, map[string]any{
		"enabled":       true,
		"authenticated": ok,
		"user":          user,
		"role":          role,
		"is_admin":      isAdmin,
		"must_change":   ok && s.authn.MustChange(user),
	})
}

// POST /api/login — validate credentials and start a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		http.Error(w, "auth disabled", http.StatusNotFound)
		return
	}
	var body struct{ Username, Password string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	mustChange, ok := s.authn.Authenticate(body.Username, body.Password)
	if !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": false})
		return
	}
	token, err := s.authn.CreateSession(body.Username)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, map[string]any{"ok": true, "must_change": mustChange})
}

// POST /api/logout — end the current session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authn != nil {
		if c, err := r.Cookie(sessionCookie); err == nil {
			s.authn.DeleteSession(c.Value)
		}
	}
	clearSessionCookie(w)
	writeJSON(w, map[string]any{"ok": true})
}

// POST /api/change-password — set a new password for the logged-in user
// (also completes the forced change for a seeded account).
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		http.Error(w, "auth disabled", http.StatusNotFound)
		return
	}
	user, ok := s.sessionUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body.NewPassword) < 6 {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"ok": false, "error": "password must be at least 6 characters"})
		return
	}
	if err := s.authn.SetPassword(user, body.NewPassword); err != nil {
		http.Error(w, "could not set password", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- user mgmt

// adminOnly restricts a handler to signed-in users with the admin role.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled { // auth off: admin surfaces are open (consistent with data access)
			next(w, r)
			return
		}
		user, ok := s.sessionUser(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.authn.MustChange(user) || !s.authn.IsAdmin(user) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// badReq writes {ok:false, error} with the given status — used by the mutating
// user endpoints so the admin UI can surface the reason.
func badReq(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"ok": false, "error": msg})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.authn.ListUsers()
	if err != nil {
		http.Error(w, "list error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct{ Username, Password, Role string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Role == "" {
		body.Role = "viewer"
	}
	if err := s.authn.CreateUser(body.Username, body.Password, body.Role); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if cur, _ := s.sessionUser(r); cur == name {
		badReq(w, http.StatusBadRequest, "you cannot delete your own account")
		return
	}
	if err := s.authn.DeleteUser(name); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.authn.ResetPassword(name, body.NewPassword); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.authn.SetRole(name, body.Role); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.authn.ListRoles()
	if err != nil {
		http.Error(w, "list error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, roles)
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string   `json:"name"`
		Services []string `json:"services"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.authn.CreateRole(body.Name, body.Services); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Services []string `json:"services"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := s.authn.UpdateRole(r.PathValue("name"), body.Services); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.authn.DeleteRole(r.PathValue("name")); err != nil {
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- settings

// GET /api/settings — current runtime config with secrets stripped (write-only)
// plus flags indicating which secrets are set.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	rt, err := s.getConfig()
	if err != nil {
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"has_secret_key":   s.hasKey(),
		"aws_keys_set":     rt.AWS.SecretAccessKey != "",
		"ingest_token_set": rt.IngestToken != "",
		"kubeconfig_set":   rt.Kubernetes.Kubeconfig != "",
	}
	// strip secrets before returning
	rt.AWS.SecretAccessKey, rt.AWS.SessionToken, rt.IngestToken, rt.Kubernetes.BearerToken, rt.Kubernetes.Kubeconfig = "", "", "", "", ""
	for i := range rt.Native.Valkey {
		rt.Native.Valkey[i].Password = ""
	}
	for i := range rt.Native.OpenSearch {
		rt.Native.OpenSearch[i].Password = ""
	}
	for i := range rt.Native.RabbitMQ {
		rt.Native.RabbitMQ[i].Password = ""
	}
	for i := range rt.Kubernetes.Clusters {
		rt.Kubernetes.Clusters[i].BearerToken = ""
	}
	resp["config"] = rt
	writeJSON(w, resp)
}

// POST /api/settings — validate, merge blank secrets with existing, persist,
// and re-apply the collector service. `clear_aws_keys` reverts AWS to the
// default credential chain (IRSA/env).
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config       config.Runtime `json:"config"`
		ClearAWSKeys bool           `json:"clear_aws_keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		badReq(w, http.StatusBadRequest, "bad request")
		return
	}
	cur, err := s.getConfig()
	if err != nil {
		badReq(w, http.StatusInternalServerError, "config error")
		return
	}
	rt := mergeSecrets(body.Config, cur, body.ClearAWSKeys)
	if err := s.saveConfig(rt); err != nil { // persists (encrypts) + re-applies collectors
		badReq(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// mergeSecrets fills blank secret fields in `in` from the current config so the
// UI can omit unchanged secrets. Same-named native targets / clusters keep
// their stored password/token when the incoming one is blank.
func mergeSecrets(in, cur config.Runtime, clearAWSKeys bool) config.Runtime {
	if clearAWSKeys {
		in.AWS.AccessKeyID, in.AWS.SecretAccessKey, in.AWS.SessionToken = "", "", ""
	} else {
		if in.AWS.SecretAccessKey == "" {
			in.AWS.SecretAccessKey = cur.AWS.SecretAccessKey
		}
		if in.AWS.SessionToken == "" {
			in.AWS.SessionToken = cur.AWS.SessionToken
		}
	}
	if in.IngestToken == "" {
		in.IngestToken = cur.IngestToken
	}
	if in.Kubernetes.Kubeconfig == "" {
		in.Kubernetes.Kubeconfig = cur.Kubernetes.Kubeconfig
	}
	valkey := map[string]string{}
	for _, t := range cur.Native.Valkey {
		valkey[t.Name] = t.Password
	}
	for i := range in.Native.Valkey {
		if in.Native.Valkey[i].Password == "" {
			in.Native.Valkey[i].Password = valkey[in.Native.Valkey[i].Name]
		}
	}
	opensearch := map[string]string{}
	for _, t := range cur.Native.OpenSearch {
		opensearch[t.Name] = t.Password
	}
	for i := range in.Native.OpenSearch {
		if in.Native.OpenSearch[i].Password == "" {
			in.Native.OpenSearch[i].Password = opensearch[in.Native.OpenSearch[i].Name]
		}
	}
	rabbit := map[string]string{}
	for _, t := range cur.Native.RabbitMQ {
		rabbit[t.Name] = t.Password
	}
	for i := range in.Native.RabbitMQ {
		if in.Native.RabbitMQ[i].Password == "" {
			in.Native.RabbitMQ[i].Password = rabbit[in.Native.RabbitMQ[i].Name]
		}
	}
	clusters := map[string]string{}
	for _, c := range cur.Kubernetes.Clusters {
		clusters[c.Name] = c.BearerToken
	}
	for i := range in.Kubernetes.Clusters {
		if in.Kubernetes.Clusters[i].BearerToken == "" {
			in.Kubernetes.Clusters[i].BearerToken = clusters[in.Kubernetes.Clusters[i].Name]
		}
	}
	return in
}

// auth guards the push endpoints with the shared ingest token.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok := s.ingestToken(); tok != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

type ingestPoint struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	Point  store.Point       `json:"point"`
}

// POST /api/ingest — agents push metric points.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Batch []ingestPoint `json:"batch"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, p := range body.Batch {
		if p.ID == "" || p.Labels == nil {
			continue
		}
		s.store.Add(p.ID, p.Labels, p.Point)
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/ingest/logs — agents push log lines.
func (s *Server) handleIngestLogs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source string   `json:"source"`
		Lines  []string `json:"lines"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Source == "" || len(body.Lines) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.logs.Append(body.Source, body.Lines)
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/agentlogs?source=host/ip-10-0-1-5&tail=200 — SSE tail + follow
// of logs shipped by agents.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source == "" {
		http.Error(w, "source required", http.StatusBadRequest)
		return
	}
	// host/* logs belong to Hosts; pod/* (the k8s fallback) to EKS.
	svc := "Hosts"
	if strings.HasPrefix(source, "pod/") {
		svc = "EKS"
	}
	if _, allow := s.access(r); !allow(svc) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if tail <= 0 {
		tail = 200
	}
	fl, ok := sseHeaders(w)
	if !ok {
		return
	}
	ch, cancel := s.logs.Subscribe()
	defer cancel()
	for _, line := range s.logs.Tail(source, tail) {
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", jsonString(line))
	}
	fl.Flush()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case l := <-ch:
			if l.Source != source {
				continue
			}
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", jsonString(l.Text))
			fl.Flush()
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
