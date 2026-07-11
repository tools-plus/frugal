# awsobs

A single-binary AWS + EKS observability tool with two modes:

```
awsobs server   # collectors + dashboard (default when no subcommand given)
awsobs agent    # push host metrics + logs from EC2 / EKS nodes to a server
``` Collects CloudWatch metrics for
managed services (EC2, RDS, DocumentDB, ElastiCache/Valkey, AmazonMQ ActiveMQ +
RabbitMQ, OpenSearch, S3, ALB, NLB, EKS control plane, Container Insights) and live pod/node CPU + memory from an EKS cluster, streams pod logs, and
serves a live dashboard — no Prometheus, no agents, no external database.

```
CloudWatch API ─┐
                ├─▶ collector ─▶ in-memory ring buffers ─▶ HTTP + SSE ─▶ live dashboard
EKS APIs ───────┘     (Go)         (recent history)                       (embedded HTML)
```

## Hybrid collection (why this is cheap)

The server prefers **native, free endpoints** over CloudWatch wherever they
exist, and uses CloudWatch (default 300s poll) only for what has no
alternative:

| Source | How | Cost | Resolution |
|---|---|---|---|
| Valkey / ElastiCache | `INFO` over the redis protocol | free | seconds |
| OpenSearch | `_cluster/health`, `_nodes/stats` | free | seconds |
| AmazonMQ (RabbitMQ) | management HTTP API | free | seconds |
| EC2 / hosts | `awsobs agent` reading /proc, pushed | free | seconds |
| EKS pods/nodes | metrics-server + kubelet log API | free | ~15s |
| ALB / NLB / S3 / RDS / DocDB host CPU+RAM / EKS control plane | CloudWatch `GetMetricData` | $0.01 / 1k metrics | 300s default |

RDS/DocDB stay on CloudWatch because instance CPU/memory isn't reachable
any other way; SQL-level pollers (connections, cache hit ratios) are a
planned add-on.

## Agent mode

Agents push to `POST /api/ingest` (metrics) and `/api/ingest/logs` (log
lines) with a shared bearer token (`ingest_token` on the server, `token` on
the agent). They collect host CPU / memory / disk / network / load from
/proc, tail configured log globs, buffer while the server is unreachable,
and on Kubernetes nodes (`kube_logs: true`) ship every container's logs
from `/var/log/containers` — attributed to `pod/<namespace>/<pod>` so the
dashboard's pod log view works even when the server runs outside the
cluster.

- **EC2**: `deploy/awsobs-agent.service` (systemd unit)
- **EKS**: `deploy/agent-daemonset.yaml` (DaemonSet, every node)
- Config: `agent.example.json`, or env `AWSOBS_SERVER_URL` + `AWSOBS_TOKEN`

Traces are out of scope for now; the natural path is an OTLP ingest
endpoint on the server — planned.

## Quick start (local, 2 minutes)

```bash
# Point config at your kubeconfig contexts — awsobs runs kubectl proxy for
# you (one supervised child process per context, restarted if it dies):
#   "kubernetes": { "contexts": ["plane-eks-dev", "plane-eks-atc"] }
# or "contexts": ["*"] for every context in `kubectl config get-contexts`.
# (Running kubectl proxy manually + "clusters" api_url entries still works.)

# AWS creds from your normal profile/env (env vars AWS_REGION / AWS_PROFILE
# always override values in the config file):
export AWS_REGION=ap-south-1
export AWS_PROFILE=myprofile   # or set "profile" in config.json
go run ./cmd/awsobs

# open http://localhost:8080
```

The k8s collector talks to `http://127.0.0.1:8001` (kubectl proxy) by default
when it isn't running in-cluster. The AWS collector uses the standard
credential chain (env vars, `~/.aws/config`, SSO, instance role).

Requirements: Go ≥ 1.22, `kubectl` access to the cluster, metrics-server
installed in the cluster (`aws eks` clusters: enable the metrics-server addon,
or `kubectl top pods` working means you already have it).

## How it collects

**AWS managed services** — one collector, one API. Resource discovery uses
`ListMetrics` per namespace (a new RDS instance appears automatically within
the discovery interval), and data collection uses `GetMetricData` batched up
to 500 queries per call. Cost is ~$0.01 per 1,000 metrics requested; with the
default 60s poll a few dozen resources costs a few dollars a month. Slow the
poll (`poll_interval_seconds`) to cut it further. S3 storage metrics are
emitted daily by AWS, so those charts get one point per day — that's AWS, not
a bug.

**EKS** — talks straight to the cluster APIs with a ~100-line REST client
(no client-go):

- multiple clusters at once — each gets its own API endpoint (a `kubectl
  proxy` port locally, or in-cluster ServiceAccount when deployed inside);
  every series is labeled with its cluster
- the dashboard drills down: cluster → control plane / nodes / namespaces →
  workloads (derived from pod ownerReferences: Deployments, StatefulSets,
  DaemonSets, Jobs) → pods; selecting a workload overlays its pods' CPU and
  memory on shared charts
- pod + node CPU/memory: `metrics.k8s.io` (metrics-server, ~15s resolution)
- pod inventory (phase, restarts, containers): core API
- live log tails: `GET .../pods/{pod}/log?follow=true` — the same call
  `kubectl logs -f` makes, streamed to the browser over SSE

**Storage** — SQLite is the system of record, memory is the hot path. With
`data_dir` set, the server ensures `<data_dir>/awsobs.db` and its schema on
start (tables: series, points, logs, pods; WAL mode), hydrates the
in-memory stores from it so the dashboard serves data *immediately* on
restart, and persists everything collectors produce in batched background
transactions (points flush every 2s, pod inventory every 30s). Point
history is pruned to `db_retention_hours` (default 72) and logs to
`log_retention_lines` per source. The sqlite driver needs CGO
(`CGO_ENABLED=1`, the default on a normal build); agents cross-compiled
with `CGO_ENABLED=0` still build — they just don't include the driver they
never use. For a pure-Go server binary swap the driver import in
`internal/db/driver_cgo.go` to `modernc.org/sqlite`. For time ranges beyond the buffer (24h/3d/7d in the UI)
the dashboard queries CloudWatch on demand through `/api/history`.

**Live updates** — every new point fans out to connected dashboards over
Server-Sent Events (`/api/stream`). No websockets, no polling from the
browser.

## Auth modes for Kubernetes

Picked automatically in this order:

1. `kubernetes.api_url` set in config (with optional `bearer_token`) —
   point at any reachable API server
2. in-cluster ServiceAccount — when deployed inside EKS (see `deploy/`)
3. `kubectl proxy` at `127.0.0.1:8001` — local development default

## Deploying in the cluster

```bash
docker build -t YOUR_ECR_REPO/awsobs:latest . && docker push YOUR_ECR_REPO/awsobs:latest
# edit deploy/k8s.yaml: image, region, IRSA role ARN
kubectl apply -f deploy/k8s.yaml
kubectl -n awsobs port-forward svc/awsobs 8080:80
```

IRSA gives the pod CloudWatch access without long-lived keys. The IAM policy
needs only `cloudwatch:ListMetrics` and `cloudwatch:GetMetricData`.

The Service is ClusterIP on purpose. The dashboard has a built-in login (see
**Authentication** below), but keep it behind port-forward, your VPN, or an
authenticating ingress as defense in depth.

## Authentication

The dashboard is gated by a username/password login, **enabled by default**.
Users and sessions live in their own SQLite file (`<data_dir>/auth.db`,
separate from the metrics db), so this is independent of `data_dir` metric
persistence.

- **First setup** seeds a default user `admin` / `admin`, flagged
  must-change — the first login forces you to set a real password before the
  dashboard is reachable.
- Passwords are bcrypt-hashed; login issues an `HttpOnly` session cookie
  (7-day lifetime). Change your password any time from the dashboard header.
- **Disable it** (e.g. behind a trusted VPN, or for local dev) with
  `"auth": { "enabled": false }` — this restores the original open dashboard.
- `"auth": { "db_path": "..." }` overrides the auth db location (defaults to
  `<data_dir>/auth.db`, or `./awsobs-auth.db` when `data_dir` is unset).

Agent push endpoints (`/api/ingest*`) are **not** behind the login — they
authenticate with the shared `ingest_token` bearer token, since agents can't
do an interactive login.

The auth db needs the sqlite driver, so a login-enabled server must be built
with `CGO_ENABLED=1` (the default), same as `data_dir` persistence.

## HTTP API

| Endpoint | What |
|---|---|
| `GET /` | dashboard (requires a session when auth is enabled) |
| `GET /login` | login page |
| `POST /api/login` | authenticate, start a session (sets cookie) |
| `POST /api/logout` | end the current session |
| `POST /api/change-password` | set a new password for the logged-in user |
| `GET /api/me` | auth status: enabled, authenticated, user, must_change |
| `GET /api/series?filter=` | all known series (id, labels, last value) |
| `GET /api/series/data?id=` | full ring buffer for one series |
| `GET /api/history?id=&from=&to=` | on-demand CloudWatch fetch for long ranges (unix seconds) |
| `GET /api/stream` | SSE: every new point, as it lands |
| `GET /api/pods` | pod inventory (served from the collectors' in-memory cache — instant) |
| `GET /api/status` | collector health: CloudWatch target count, last error, last poll, clusters |
| `GET /api/logs?namespace=&pod=&container=&tail=` | SSE: live pod log tail (k8s API, or agent-shipped fallback) |
| `GET /api/agentlogs?source=host/<name>&tail=` | SSE: live tail of agent-shipped logs |
| `POST /api/ingest` | agent metric push (bearer token) |
| `POST /api/ingest/logs` | agent log push (bearer token) |

The read/dashboard endpoints require a valid session cookie when auth is on;
`/login`, the auth endpoints, and static assets are public.

Series IDs are pipe-delimited and predictable:
`k8s|pod|default/web-7f9c|cpu_cores`, `cw|RDS|CPUUtilization|mydb`.

## Extending it

- **More CloudWatch metrics**: add entries to the `defaults` map in
  `internal/awsmetrics/cloudwatch.go`. Anything `ListMetrics` can see,
  `GetMetricData` can fetch — including custom namespaces.
- **Container-level (not pod-level) metrics, throttling, network**: scrape
  kubelet's cAdvisor endpoint
  (`/api/v1/nodes/{node}/proxy/metrics/cadvisor`) — the REST client already
  supports it via `GetJSON`; you'd add a small Prometheus-text parser.
- **Alerting**: subscribe to the store (`store.Subscribe()`) in a new
  goroutine and evaluate thresholds — the fan-out layer is already there.
- **Longer retention**: swap the ring buffer for SQLite behind the same
  `store` interface, or remote-write to Prometheus/Mimir.

## Notes

- go.mod pins AWS SDK versions compatible with Go 1.22; run
  `go get -u ./... && go mod tidy` on a machine with a current Go toolchain
  to move to latest.
- OpenSearch metrics live under the legacy `AWS/ES` namespace — that's the
  namespace AWS still publishes to.
- ALB/NLB metrics are per-load-balancer and per-target-group; both dimension
  sets are discovered automatically.
