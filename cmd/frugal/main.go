// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 tools-plus

// frugal — a single-binary AWS + EKS observability tool with two modes:
//
//	frugal server -config config.json   # web dashboard + data collectors
//	frugal agent  -config agent.json    # push host metrics/logs to a server
//
// In server mode the binary runs in two parts: Part 1, the web server, comes up
// immediately from bootstrap config (listen, data_dir, auth). Part 2, the data
// collection service, is supervised — it starts from the runtime config stored
// (encrypted) in the control DB and is torn down + relaunched on reconfigure,
// without restarting Part 1. Runtime config (AWS/k8s/native, credentials,
// retention, ingest token) is edited from the admin UI, not server.json.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/tools-plus/frugal/internal/agent"
	"github.com/tools-plus/frugal/internal/auth"
	"github.com/tools-plus/frugal/internal/collector"
	"github.com/tools-plus/frugal/internal/config"
	"github.com/tools-plus/frugal/internal/db"
	"github.com/tools-plus/frugal/internal/k8s"
	"github.com/tools-plus/frugal/internal/logstore"
	"github.com/tools-plus/frugal/internal/secret"
	"github.com/tools-plus/frugal/internal/server"
	"github.com/tools-plus/frugal/internal/store"
	"github.com/tools-plus/frugal/web"
)

// version is overridden at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	args := os.Args[1:]

	// Top-level help / version, before mode + flag parsing.
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version", "-v":
			fmt.Printf("frugal %s\n", version)
			return
		case "help", "-help", "--help", "-h":
			usage(os.Stdout)
			return
		}
	}

	mode := "server"
	if len(args) > 0 && (args[0] == "server" || args[0] == "agent") {
		mode = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("frugal "+mode, flag.ExitOnError)
	configPath := fs.String("config", "", "path to a JSON config file (optional; env vars override)")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() { modeUsage(os.Stderr, mode, fs) }
	fs.Parse(args)
	if *showVersion {
		fmt.Printf("frugal %s\n", version)
		return
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch mode {
	case "agent":
		if err := agent.Run(ctx, cfg.Agent, logger); err != nil {
			logger.Fatal(err)
		}
	default:
		runServer(ctx, cfg, logger)
	}
}

// usage prints the top-level help (frugal help / frugal --help).
func usage(w *os.File) {
	fmt.Fprintf(w, `frugal %s — cheap, single-binary AWS + EKS observability

Usage:
  frugal [server] [-config FILE]   Web dashboard + collectors (default mode)
  frugal agent    [-config FILE]   Agent: push host metrics + logs to a server
  frugal version                   Print the version
  frugal help                      Show this help

Server mode serves the dashboard immediately from bootstrap config (listen,
data_dir, secret_key, auth). What to collect (AWS/EKS/native targets and
credentials) is configured live in Admin ▸ Settings, not in the config file.
The config file is optional — every key can be set via an environment variable,
and the env value wins.

Key environment variables:
  FRUGAL_LISTEN         bind address (default :8080)
  FRUGAL_DATA_DIR       directory for the SQLite databases (enables persistence)
  FRUGAL_SECRET_KEY     encrypts stored credentials (required to save secrets)
  FRUGAL_AUTH_ENABLED   require login (default true)
  AWS_REGION, AWS_PROFILE
  FRUGAL_SERVER_URL     agent: the frugal server to push to
  FRUGAL_TOKEN          agent: shared ingest token (matches the server's)

Examples:
  frugal                                         # server on :8080, config from env
  FRUGAL_SECRET_KEY=$(openssl rand -hex 32) frugal
  frugal server -config server.json
  frugal agent  -config agent.json
  docker run -p 8080:8080 -e FRUGAL_SECRET_KEY=... ghcr.io/tools-plus/frugal

Docs: https://github.com/tools-plus/frugal
`, version)
}

// modeUsage prints per-mode help (frugal server -h / frugal agent -h, or on a
// bad flag).
func modeUsage(w *os.File, mode string, fs *flag.FlagSet) {
	if mode == "agent" {
		fmt.Fprint(w, "frugal agent — push host metrics + logs to a frugal server\n\nUsage: frugal agent [-config FILE]\n\nFlags:\n")
	} else {
		fmt.Fprint(w, "frugal server — web dashboard + data collectors (default mode)\n\nUsage: frugal server [-config FILE]\n\nFlags:\n")
	}
	fs.PrintDefaults()
	fmt.Fprint(w, "\nRun `frugal help` for environment variables and examples.\n")
}

func runServer(ctx context.Context, cfg config.Config, logger *log.Logger) {
	// Control DB (always present): users, roles, sessions, and the encrypted
	// runtime config. The master key comes from the environment.
	cipher := secret.New(cfg.SecretKey)
	if !cipher.Available() {
		logger.Printf("WARNING: no secret_key set (server.json secret_key or FRUGAL_SECRET_KEY) — credentials can't be stored or used until it is")
	}
	ctrl, err := auth.Open(cfg.AuthDBPath(), cipher, logger)
	if err != nil {
		logger.Fatalf("control db: %v", err)
	}
	defer ctrl.Close()

	// Runtime config: seed from server.json on first boot (migration), then the
	// control DB is the source of truth.
	rt, seeded, err := ctrl.GetConfig()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	if !seeded {
		if err := ctrl.SaveConfig(cfg.ToRuntime()); err != nil {
			logger.Printf("config: could not seed from server.json (%v) — starting empty; configure in Admin ▸ Settings", err)
		} else {
			logger.Printf("config: seeded control db from server.json")
		}
		rt, _, _ = ctrl.GetConfig()
	}

	// Part 1: web server stores (always up).
	st := store.New(rt.RetentionCap)
	ls := logstore.New(rt.LogRetentionLines)
	inv := k8s.NewInventory()

	// Optional SQLite persistence of metrics/logs (needs data_dir).
	var sdb *db.DB
	var persistDone <-chan struct{}
	if cfg.DataDir != "" {
		if sdb, err = db.Open(cfg.DataDir, logger); err != nil {
			logger.Printf("db: disabled: %v", err)
			sdb = nil
		} else {
			sdb.Hydrate(st, ls, inv)
			persistDone = sdb.StartPersist(ctx, st, ls, inv)
		}
	}

	// Part 2: supervised data-collection service.
	sup := collector.New(ctx, st, ls, inv, logger)
	defer sup.Close()

	var tokMu sync.RWMutex
	var ingestTok string
	apply := func(r config.Runtime) {
		if sdb != nil {
			if r.DBRetentionHours > 0 {
				sdb.RetentionHours = r.DBRetentionHours
			}
			if r.LogRetentionLines > 0 {
				sdb.LogLinesPerSource = r.LogRetentionLines
			}
		}
		tokMu.Lock()
		ingestTok = r.IngestToken
		tokMu.Unlock()
		if r.IngestToken == "" {
			logger.Printf("note: ingest_token is empty — /api/ingest is unauthenticated")
		}
		sup.Apply(r)
	}
	go apply(rt) // initial — async so the web server (Part 1) serves immediately

	authEnabled := cfg.Auth.On()
	if !authEnabled {
		logger.Printf("auth: disabled — dashboard served without a login (set auth.enabled=true to require login)")
	}

	// saveConfig persists (encrypting secrets), reloads (decrypts / normalizes),
	// then hot-applies to the collector service.
	saveConfig := func(r config.Runtime) error {
		if err := ctrl.SaveConfig(r); err != nil {
			return err
		}
		nr, _, err := ctrl.GetConfig()
		if err != nil {
			return err
		}
		apply(nr)
		return nil
	}

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: server.New(server.Deps{
			Store: st, Logs: ls, Inv: inv,
			Clusters: func() []server.Cluster {
				cs := sup.Clusters()
				out := make([]server.Cluster, len(cs))
				for i, c := range cs {
					out[i] = server.Cluster{Name: c.Name, Client: c.Client}
				}
				return out
			},
			Hist: func() server.Historian {
				if c := sup.AWSCollector(); c != nil {
					return c
				}
				return nil
			},
			HistoryDB: func() func(id string, from, to, step int64) ([]store.Point, error) {
				if sdb == nil {
					return nil
				}
				return sdb.History
			}(),
			Status:       sup.Status,
			IngestToken:  func() string { tokMu.RLock(); defer tokMu.RUnlock(); return ingestTok },
			Authn:        ctrl,
			AuthEnabled:  authEnabled,
			GetConfig:    func() (config.Runtime, error) { r, _, e := ctrl.GetConfig(); return r, e },
			SaveConfig:   saveConfig,
			HasSecretKey: ctrl.HasSecretKey,
			Assets:       web.FS,
			Logger:       logger,
		}),
	}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	logger.Printf("dashboard listening on http://localhost%s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal(err)
	}
	sup.Close()
	if sdb != nil {
		select {
		case <-persistDone:
		case <-time.After(3 * time.Second):
		}
		sdb.Close()
	}
	fmt.Fprintln(os.Stderr, "bye")
}
