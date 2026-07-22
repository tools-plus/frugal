// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// Package logstore keeps recent log lines shipped by agents, per source
// (e.g. "host/ip-10-0-1-5" or "pod/default/web-7f9c"), with pub/sub so the
// dashboard can live-tail them.
package logstore

import (
	"sync"
)

type Line struct {
	Source string `json:"source"`
	Text   string `json:"text"`
}

type ring struct {
	buf  []string
	head int
	full bool
}

func (r *ring) push(s string) {
	r.buf[r.head] = s
	r.head++
	if r.head == len(r.buf) {
		r.head = 0
		r.full = true
	}
}

func (r *ring) tail(n int) []string {
	size := r.head
	if r.full {
		size = len(r.buf)
	}
	if n > size {
		n = size
	}
	out := make([]string, 0, n)
	start := r.head - n
	if start < 0 {
		start += len(r.buf)
	}
	for i := 0; i < n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}

type Store struct {
	mu       sync.RWMutex
	capacity int
	rings    map[string]*ring

	subMu sync.Mutex
	subs  map[chan Line]struct{}
}

func New(capacity int) *Store {
	if capacity <= 0 {
		capacity = 2000
	}
	return &Store{
		capacity: capacity,
		rings:    map[string]*ring{},
		subs:     map[chan Line]struct{}{},
	}
}

func (s *Store) Append(source string, lines []string) {
	s.mu.Lock()
	r, ok := s.rings[source]
	if !ok {
		r = &ring{buf: make([]string, s.capacity)}
		s.rings[source] = r
	}
	for _, l := range lines {
		r.push(l)
	}
	s.mu.Unlock()

	s.subMu.Lock()
	for ch := range s.subs {
		for _, l := range lines {
			select {
			case ch <- Line{Source: source, Text: l}:
			default:
			}
		}
	}
	s.subMu.Unlock()
}

func (s *Store) Tail(source string, n int) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rings[source]
	if !ok {
		return nil
	}
	return r.tail(n)
}

// Sources lists sources that currently have logs.
func (s *Store) Sources() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.rings))
	for k := range s.rings {
		out = append(out, k)
	}
	return out
}

func (s *Store) Subscribe() (<-chan Line, func()) {
	ch := make(chan Line, 512)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch, func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}
}
