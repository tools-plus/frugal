# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`awsobs` is a single Go binary that is both an observability **server** (CloudWatch + EKS + native-endpoint collectors feeding an embedded live dashboard) and a metrics/logs **agent** (pushes host data from EC2/EKS nodes to a server). Subcommand selects mode; `server` is the default when none is given. Everything ships as one binary with no external database and no Prometheus — recent data lives in in-memory ring buffers, optionally backed by SQLite.

## Commands

```bash
go run ./cmd/awsobs                    # run server (default mode), reads defaults
go run ./cmd/awsobs -config server.json
go run ./cmd/awsobs agent -config agent.json
go build -o awsobs ./cmd/awsobs        # CGO_ENABLED=1 by default → includes sqlite driver

go test ./...                          # all tests
go test ./internal/store/              # one package
go test ./internal/awsmetrics/ -run TestName -v   # single test
go vet ./...

docker build -t awsobs .               # multi-stage, CGO on, distroless base
```

There is no lint config beyond `go vet`; stick to `gofmt`.

## Architecture

`cmd/awsobs/main.go` wires everything. The server (`runServer`) starts each collector as a goroutine writing into two shared, concurrency-safe stores, then serves HTTP:

- **`internal/store`** — per-series ring buffers (metrics) + a pub/sub fan-out. `Store.Subscribe()` is how the SSE endpoint and any future alerting hook get live points. Slow subscribers are dropped, not blocked. Duplicate/older timestamps per series are ignored (CloudWatch re-polls the same window).
- **`internal/logstore`** — bounded log-line buffers per source.
- **`internal/k8s.Inventory`** — pod/workload inventory cache.

Collectors, all feeding the stores:
- **`internal/awsmetrics`** — CloudWatch. `ListMetrics` discovers resources per namespace; `GetMetricData` (batched ≤500 queries) fetches. Add metrics by extending the `defaults` map in `cloudwatch.go`. Also implements `server.Historian` for on-demand long-range `/api/history` queries.
- **`internal/native`** — free in-VPC endpoints instead of CloudWatch: Valkey/ElastiCache (`INFO`), OpenSearch (`_cluster/health`), RabbitMQ (mgmt API).
- **`internal/k8s`** — ~100-line REST client (no client-go). One collector per cluster. Metrics from `metrics.k8s.io`, inventory from core API, log tails via `pods/{pod}/log?follow=true`. `proxy.go` supervises a `kubectl proxy` child process per kubeconfig context.
- **`internal/agent`** — client side of the two ingest endpoints; reads `/proc`, tails log globs, buffers while server unreachable.

`internal/server` (`server.go`) is a stdlib `net/http.ServeMux` with method-prefixed routes. `/api/stream`, `/api/logs`, `/api/agentlogs` are SSE. `/api/ingest*` are the only authenticated routes (bearer token via `auth()` middleware). See the HTTP API table in README.md for the full endpoint list.

`internal/db` is optional SQLite persistence (system of record; memory is the hot path). On start it hydrates the in-memory stores from disk so the dashboard serves immediately, then persists in batched background transactions.

`web/index.html` is the entire dashboard (single file, vanilla JS), embedded via `go:embed` in `web/embed.go`.

### Key conventions

- **Series IDs are pipe-delimited and predictable**: `k8s|pod|default/web-7f9c|cpu_cores`, `cw|RDS|CPUUtilization|mydb`. The dashboard parses these; keep the scheme stable when adding metrics.
- **Config** (`internal/config/config.go`): JSON file (all fields optional, `Default()` supplies sane values) with env-var overrides applied *after* the file. `AWS_REGION`/`AWS_PROFILE` and other `AWSOBS_*` envs always win over file values. Poll intervals are floored (AWS ≥30s, k8s ≥5s) to prevent API-cost surprises.
- **CGO / sqlite driver**: the sqlite driver is behind build tags (`driver_cgo.go` / `driver_nocgo.go`). A normal build (CGO on) includes it; `CGO_ENABLED=0` cross-builds (agents) compile fine but have no persistence. For a pure-Go server, swap the `mattn/go-sqlite3` import for `modernc.org/sqlite` per the note in `driver_nocgo.go`.
- **Graceful degradation**: missing AWS creds, unreachable cluster, or disabled persistence each log a warning and continue — never fatal.
- **Kubernetes auth precedence**: `kubernetes.api_url` in config → in-cluster ServiceAccount → local `kubectl proxy` on `127.0.0.1:8001`.

## Deploying

`deploy/k8s.yaml` (server in-cluster, IRSA for CloudWatch — needs only `cloudwatch:ListMetrics` + `GetMetricData`), `deploy/agent-daemonset.yaml` (agent per EKS node), `deploy/awsobs-agent.service` (systemd EC2 agent). The dashboard has **no built-in auth** — keep it behind port-forward/VPN/authenticating ingress.
