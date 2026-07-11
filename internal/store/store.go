// Package store keeps recent time-series data in per-series ring buffers
// and fans out live updates to subscribers (the SSE endpoint).
package store

import (
	"sort"
	"strings"
	"sync"
)

// Point is a single sample.
type Point struct {
	T int64   `json:"t"` // unix seconds
	V float64 `json:"v"`
}

// Update is what subscribers receive when a new point lands.
type Update struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels,omitempty"`
	Point  Point             `json:"point"`
}

// SeriesMeta describes a series without its data.
type SeriesMeta struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels"`
	Last   Point             `json:"last"`
	Count  int               `json:"count"`
}

type series struct {
	labels map[string]string
	buf    []Point // ring
	head   int     // next write index
	full   bool
	lastT  int64
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	capacity int
	series   map[string]*series

	subMu sync.Mutex
	subs  map[chan Update]struct{}
}

func New(capacity int) *Store {
	if capacity <= 0 {
		capacity = 720 // 6h at 30s
	}
	return &Store{
		capacity: capacity,
		series:   make(map[string]*series),
		subs:     make(map[chan Update]struct{}),
	}
}

// Add appends a point to a series (creating it if needed). Duplicate
// timestamps for the same series are ignored so CloudWatch re-polls of the
// same window don't produce sawtooth duplicates.
func (s *Store) Add(id string, labels map[string]string, p Point) {
	s.mu.Lock()
	sr, ok := s.series[id]
	if !ok {
		sr = &series{labels: labels, buf: make([]Point, s.capacity)}
		s.series[id] = sr
	}
	if p.T <= sr.lastT {
		s.mu.Unlock()
		return
	}
	sr.buf[sr.head] = p
	sr.head++
	if sr.head == len(sr.buf) {
		sr.head = 0
		sr.full = true
	}
	sr.lastT = p.T
	s.mu.Unlock()

	s.publish(Update{ID: id, Labels: labels, Point: p})
}

// Labels returns a series' labels (used to classify it for access control).
func (s *Store) Labels(id string) (map[string]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sr, ok := s.series[id]
	if !ok {
		return nil, false
	}
	return sr.labels, true
}

// Data returns the points of a series in chronological order.
func (s *Store) Data(id string) []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sr, ok := s.series[id]
	if !ok {
		return nil
	}
	return sr.snapshot()
}

func (sr *series) snapshot() []Point {
	if !sr.full {
		out := make([]Point, sr.head)
		copy(out, sr.buf[:sr.head])
		return out
	}
	out := make([]Point, 0, len(sr.buf))
	out = append(out, sr.buf[sr.head:]...)
	out = append(out, sr.buf[:sr.head]...)
	return out
}

// List returns metadata for all series whose ID contains the filter
// (empty filter matches everything), sorted by ID.
func (s *Store) List(filter string) []SeriesMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SeriesMeta, 0, len(s.series))
	for id, sr := range s.series {
		if filter != "" && !strings.Contains(id, filter) {
			continue
		}
		n := sr.head
		if sr.full {
			n = len(sr.buf)
		}
		last := Point{}
		if n > 0 {
			idx := sr.head - 1
			if idx < 0 {
				idx = len(sr.buf) - 1
			}
			last = sr.buf[idx]
		}
		out = append(out, SeriesMeta{ID: id, Labels: sr.labels, Last: last, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Subscribe registers a live-update channel. The returned cancel func must
// be called when the subscriber goes away.
func (s *Store) Subscribe() (<-chan Update, func()) {
	ch := make(chan Update, 256)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	cancel := func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}
	return ch, cancel
}

func (s *Store) publish(u Update) {
	s.subMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- u:
		default: // slow subscriber: drop rather than block collectors
		}
	}
	s.subMu.Unlock()
}
