// Package server exposes the dashboard and its APIs. Live data flows over
// Server-Sent Events — one-directional push is all a chart needs, and it
// keeps the binary free of websocket dependencies.
package server

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/example/awsobs/internal/k8s"
	"github.com/example/awsobs/internal/store"
)

// Historian answers on-demand range queries against the metric origin
// (CloudWatch) for windows longer than the in-memory ring buffer.
type Historian interface {
	History(ctx context.Context, id string, from, to time.Time) ([]store.Point, error)
}

type Server struct {
	store  *store.Store
	k8s    *k8s.Client // nil when kubernetes collection is disabled
	hist   Historian   // nil when the AWS collector is disabled
	logger *log.Logger
	assets embed.FS
	mux    *http.ServeMux
}

func New(st *store.Store, kc *k8s.Client, hist Historian, assets embed.FS, logger *log.Logger) *Server {
	s := &Server{store: st, k8s: kc, hist: hist, logger: logger, assets: assets}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/series", s.handleSeries)
	mux.HandleFunc("GET /api/series/data", s.handleSeriesData)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("GET /api/pods", s.handlePods)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
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

// GET /api/pods
func (s *Server) handlePods(w http.ResponseWriter, r *http.Request) {
	if s.k8s == nil {
		writeJSON(w, []k8s.PodInfo{})
		return
	}
	pods, err := s.k8s.ListPods(r.Context(), r.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, pods)
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
	if s.k8s == nil {
		http.Error(w, "kubernetes collection disabled", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	ns, pod := q.Get("namespace"), q.Get("pod")
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
	body, err := s.k8s.StreamLogs(ctx, ns, pod, q.Get("container"), tail)
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

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
