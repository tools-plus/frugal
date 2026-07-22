// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// Package db is the SQLite system of record. The in-memory store stays the
// hot read path; this package hydrates it on boot and persists everything
// (series metadata, points, logs, pod inventory) in the background, so a
// restart or page refresh serves data immediately while collectors warm up.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tools-plus/frugal/internal/k8s"
	"github.com/tools-plus/frugal/internal/logstore"
	"github.com/tools-plus/frugal/internal/store"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS series (
	id     TEXT PRIMARY KEY,
	labels TEXT NOT NULL             -- JSON
);
CREATE TABLE IF NOT EXISTS points (
	series_id TEXT    NOT NULL,
	t         INTEGER NOT NULL,      -- unix seconds
	v         REAL    NOT NULL,
	PRIMARY KEY (series_id, t)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS logs (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	source TEXT NOT NULL,
	line   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS logs_source_seq ON logs(source, seq);
CREATE TABLE IF NOT EXISTS pods (
	cluster   TEXT NOT NULL,
	namespace TEXT NOT NULL,
	name      TEXT NOT NULL,
	data      TEXT NOT NULL,         -- JSON PodInfo
	PRIMARY KEY (cluster, namespace, name)
);
`

type DB struct {
	sql    *sql.DB
	logger *log.Logger
	// RetentionHours bounds how much point history is kept (default 72).
	RetentionHours int
	// LogLinesPerSource bounds stored log lines per source (default 2000).
	LogLinesPerSource int
}

// Open creates/opens <dir>/frugal.db and ensures the schema exists.
func Open(dir string, logger *log.Logger) (*DB, error) {
	if !driverAvailable {
		return nil, fmt.Errorf("sqlite driver not compiled in (build with CGO_ENABLED=1, or swap the driver import to modernc.org/sqlite)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "frugal.db")
	sq, err := sql.Open(driverName, path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	sq.SetMaxOpenConns(1) // single writer; reads happen at boot before writers start
	if _, err := sq.Exec(schema); err != nil {
		sq.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	logger.Printf("db: sqlite ready at %s", path)
	return &DB{sql: sq, logger: logger, RetentionHours: 72, LogLinesPerSource: 2000}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// ---------------------------------------------------------------- hydrate

// Hydrate loads persisted state into the hot stores. Called once on boot,
// before collectors start, so the dashboard has data immediately.
func (d *DB) Hydrate(st *store.Store, ls *logstore.Store, inv *k8s.Inventory) {
	// series labels
	labels := map[string]map[string]string{}
	rows, err := d.sql.Query(`SELECT id, labels FROM series`)
	if err == nil {
		for rows.Next() {
			var id, lj string
			if rows.Scan(&id, &lj) == nil {
				m := map[string]string{}
				json.Unmarshal([]byte(lj), &m)
				labels[id] = m
			}
		}
		rows.Close()
	}

	// points within retention, chronological so ring buffers fill correctly
	since := time.Now().Add(-time.Duration(d.RetentionHours) * time.Hour).Unix()
	rows, err = d.sql.Query(`SELECT series_id, t, v FROM points WHERE t >= ? ORDER BY series_id, t`, since)
	n := 0
	if err == nil {
		for rows.Next() {
			var id string
			var t int64
			var v float64
			if rows.Scan(&id, &t, &v) == nil {
				st.Add(id, labels[id], store.Point{T: t, V: v})
				n++
			}
		}
		rows.Close()
	}

	// last K log lines per source
	nl := 0
	rows, err = d.sql.Query(`
		SELECT source, line FROM (
			SELECT source, line, seq,
			       ROW_NUMBER() OVER (PARTITION BY source ORDER BY seq DESC) AS rn
			FROM logs
		) WHERE rn <= ? ORDER BY seq ASC`, d.LogLinesPerSource)
	if err == nil {
		for rows.Next() {
			var src, line string
			if rows.Scan(&src, &line) == nil {
				ls.Append(src, []string{line})
				nl++
			}
		}
		rows.Close()
	}

	// pod inventory
	byCluster := map[string][]k8s.PodInfo{}
	rows, err = d.sql.Query(`SELECT cluster, data FROM pods`)
	if err == nil {
		for rows.Next() {
			var cl, dj string
			if rows.Scan(&cl, &dj) == nil {
				var p k8s.PodInfo
				if json.Unmarshal([]byte(dj), &p) == nil {
					byCluster[cl] = append(byCluster[cl], p)
				}
			}
		}
		rows.Close()
	}
	np := 0
	for cl, pods := range byCluster {
		inv.Set(cl, pods)
		np += len(pods)
	}
	d.logger.Printf("db: hydrated %d points, %d log lines, %d pods (%d series known)", n, nl, np, len(labels))
}

// ---------------------------------------------------------------- persist

// StartPersist subscribes to live updates synchronously (so no point is
// missed between hydration and collector start) and writes them in batched
// transactions on a background goroutine. The returned channel closes when
// the final flush after ctx cancellation is done.
func (d *DB) StartPersist(ctx context.Context, st *store.Store, ls *logstore.Store, inv *k8s.Inventory) <-chan struct{} {
	ptCh, cancelPt := st.Subscribe()
	logCh, cancelLog := ls.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancelPt()
		defer cancelLog()
		d.persistLoop(ctx, ptCh, logCh, inv)
	}()
	return done
}

func (d *DB) persistLoop(ctx context.Context, ptCh <-chan store.Update, logCh <-chan logstore.Line, inv *k8s.Inventory) {

	flush := time.NewTicker(2 * time.Second)
	podT := time.NewTicker(30 * time.Second)
	pruneT := time.NewTicker(10 * time.Minute)
	defer flush.Stop()
	defer podT.Stop()
	defer pruneT.Stop()

	var (
		pts       []store.Update
		lines     []logstore.Line
		seenSer   = map[string]bool{}
		newSeries []store.Update
	)
	for {
		select {
		case <-ctx.Done():
			d.flushBatch(pts, newSeries, lines)
			d.savePods(inv)
			return
		case u := <-ptCh:
			if !seenSer[u.ID] {
				seenSer[u.ID] = true
				newSeries = append(newSeries, u)
			}
			pts = append(pts, u)
			if len(pts) >= 2000 {
				d.flushBatch(pts, newSeries, lines)
				pts, newSeries, lines = pts[:0], newSeries[:0], lines[:0]
			}
		case l := <-logCh:
			lines = append(lines, l)
		case <-flush.C:
			d.flushBatch(pts, newSeries, lines)
			pts, newSeries, lines = pts[:0], newSeries[:0], lines[:0]
		case <-podT.C:
			d.savePods(inv)
		case <-pruneT.C:
			d.prune()
		}
	}
}

func (d *DB) flushBatch(pts, newSeries []store.Update, lines []logstore.Line) {
	if len(pts) == 0 && len(lines) == 0 && len(newSeries) == 0 {
		return
	}
	tx, err := d.sql.Begin()
	if err != nil {
		d.logger.Printf("db: begin: %v", err)
		return
	}
	defer tx.Rollback()

	if len(newSeries) > 0 {
		stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO series(id, labels) VALUES(?, ?)`)
		for _, u := range newSeries {
			lj, _ := json.Marshal(u.Labels)
			stmt.Exec(u.ID, string(lj))
		}
		stmt.Close()
	}
	if len(pts) > 0 {
		stmt, _ := tx.Prepare(`INSERT OR IGNORE INTO points(series_id, t, v) VALUES(?, ?, ?)`)
		for _, u := range pts {
			stmt.Exec(u.ID, u.Point.T, u.Point.V)
		}
		stmt.Close()
	}
	if len(lines) > 0 {
		stmt, _ := tx.Prepare(`INSERT INTO logs(source, line) VALUES(?, ?)`)
		for _, l := range lines {
			stmt.Exec(l.Source, l.Text)
		}
		stmt.Close()
	}
	if err := tx.Commit(); err != nil {
		d.logger.Printf("db: commit: %v", err)
	}
}

func (d *DB) savePods(inv *k8s.Inventory) {
	pods := inv.All()
	tx, err := d.sql.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM pods`)
	stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO pods(cluster, namespace, name, data) VALUES(?, ?, ?, ?)`)
	for _, p := range pods {
		dj, _ := json.Marshal(p)
		stmt.Exec(p.Cluster, p.Namespace, p.Name, string(dj))
	}
	stmt.Close()
	tx.Commit()
}

func (d *DB) prune() {
	cutoff := time.Now().Add(-time.Duration(d.RetentionHours) * time.Hour).Unix()
	if _, err := d.sql.Exec(`DELETE FROM points WHERE t < ?`, cutoff); err != nil {
		d.logger.Printf("db: prune points: %v", err)
	}
	// keep only the newest K lines per source
	if _, err := d.sql.Exec(`
		DELETE FROM logs WHERE seq IN (
			SELECT seq FROM (
				SELECT seq, ROW_NUMBER() OVER (PARTITION BY source ORDER BY seq DESC) AS rn
				FROM logs
			) WHERE rn > ?
		)`, d.LogLinesPerSource); err != nil {
		d.logger.Printf("db: prune logs: %v", err)
	}
	d.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
}
