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

	"github.com/example/awsobs/internal/auth"
	"github.com/example/awsobs/internal/k8s"
	"github.com/example/awsobs/internal/logstore"
	"github.com/example/awsobs/internal/store"
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
	ListUsers() ([]auth.User, error)
	CreateUser(user, pass, role string) error
	DeleteUser(user string) error
	ResetPassword(user, newPass string) error
	SetRole(user, role string) error
}

type Server struct {
	store       *store.Store
	logs        *logstore.Store
	inv         *k8s.Inventory
	clusters    []Cluster     // empty when kubernetes collection is disabled
	hist        Historian     // nil when the AWS collector is disabled
	authn       Authenticator // nil when authentication is disabled
	status      func() map[string]any
	ingestToken string
	logger      *log.Logger
	assets      embed.FS
	mux         *http.ServeMux
}

func New(st *store.Store, ls *logstore.Store, inv *k8s.Inventory, clusters []Cluster, hist Historian, status func() map[string]any, ingestToken string, authn Authenticator, assets embed.FS, logger *log.Logger) *Server {
	s := &Server{store: st, logs: ls, inv: inv, clusters: clusters, hist: hist, authn: authn, status: status, ingestToken: ingestToken, logger: logger, assets: assets}
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

// GET /api/series?filter=cw|RDS
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.List(r.URL.Query().Get("filter")))
}

// GET /api/series/data?id=<series id>
func (s *Server) handleSeriesData(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	writeJSON(w, s.store.Data(id))
}

// GET /api/history?id=<series id>&from=<unix>&to=<unix>
// Fetches straight from CloudWatch — for ranges beyond the ring buffer.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.hist == nil {
		http.Error(w, "history unavailable (aws collector disabled)", http.StatusServiceUnavailable)
		return
	}
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
	pts, err := s.hist.History(r.Context(), id, time.Unix(from, 0), time.Unix(to, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if pts == nil {
		pts = []store.Point{}
	}
	writeJSON(w, pts)
}

// GET /api/pods — served from the collectors' shared in-memory inventory
// (refreshed every poll), so page loads never wait on cluster API calls.
func (s *Server) handlePods(w http.ResponseWriter, r *http.Request) {
	if s.inv == nil {
		writeJSON(w, []k8s.PodInfo{})
		return
	}
	writeJSON(w, s.inv.All())
}

// GET /api/status — collector health for debugging "why is X empty".
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if s.status != nil {
		out = s.status()
	}
	names := make([]string, 0, len(s.clusters))
	for _, c := range s.clusters {
		names = append(names, c.Name)
	}
	out["clusters"] = names
	out["series"] = len(s.store.List(""))
	writeJSON(w, out)
}

// clusterFor returns the client for a named cluster (or the first one).
func (s *Server) clusterFor(name string) *k8s.Client {
	for _, c := range s.clusters {
		if c.Name == name {
			return c.Client
		}
	}
	if len(s.clusters) > 0 && name == "" {
		return s.clusters[0].Client
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
			b, _ := json.Marshal(u)
			fmt.Fprintf(w, "event: point\ndata: %s\n\n", b)
			fl.Flush()
		}
	}
}

// GET /api/logs?namespace=default&pod=web-abc&container=app&tail=200
// Streams pod logs as SSE `log` events, one per line.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
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

const sessionCookie = "awsobs_session"

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
		if s.authn == nil {
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
	if s.authn == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	user, ok := s.sessionUser(r)
	role := ""
	if ok {
		role = s.authn.Role(user)
	}
	writeJSON(w, map[string]any{
		"enabled":       true,
		"authenticated": ok,
		"user":          user,
		"role":          role,
		"must_change":   ok && s.authn.MustChange(user),
	})
}

// POST /api/login — validate credentials and start a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.authn == nil {
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
	if s.authn == nil {
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
		if s.authn == nil {
			http.Error(w, "auth disabled", http.StatusNotFound)
			return
		}
		user, ok := s.sessionUser(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if s.authn.MustChange(user) || s.authn.Role(user) != "admin" {
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

// auth guards the push endpoints with the shared ingest token.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.ingestToken != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.ingestToken)) != 1 {
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
