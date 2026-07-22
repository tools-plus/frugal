# awsobs

A single-binary AWS + EKS observability tool with two modes:

```bash
awsobs server   # web dashboard + data collectors (default when no subcommand)
awsobs agent    # push host metrics + logs from EC2 / EKS nodes to a server
```

It collects CloudWatch metrics for managed services (EC2, RDS, DocumentDB,
ElastiCache/Valkey, AmazonMQ ActiveMQ + RabbitMQ, OpenSearch, S3, ALB, NLB, EKS
control plane, Container Insights) and live pod/node CPU + memory from EKS
clusters, streams pod logs, and serves a live dashboard — no Prometheus, no
external database, no third-party agents required.

```
CloudWatch API ─┐
                ├─▶ collectors ─▶ in-memory ring buffers ─▶ HTTP + SSE ─▶ live dashboard
EKS APIs ───────┘     (Go)          (+ optional SQLite)                    (embedded HTML)
```

## Architecture: two parts, one binary

In `server` mode the process runs as two parts:

1. **Web server** — comes up immediately from a tiny bootstrap config
   (`listen`, `data_dir`, `secret_key`, `auth`). It always serves the login,
   dashboard, and APIs, even before anything is configured.
2. **Data-collection service** — a supervised subsystem started from the
   *runtime* config (AWS/EKS/native targets, credentials, retention, ingest
   token). It only starts when credentials are available, and it is torn down
   and relaunched when you change settings — **without restarting the web
   server**.

Runtime config is edited in the dashboard (**Admin ▸ Settings**) and stored,
encrypted, in the control database — not in `server.json`.

## Hybrid collection (why this is cheap)

The server prefers **native, free endpoints** over CloudWatch wherever they
exist, and uses CloudWatch only for what has no alternative:

| Source | How | Cost | Resolution |
|---|---|---|---|
| Valkey / ElastiCache | `INFO` over the redis protocol | free | seconds |
| OpenSearch | `_cluster/health`, `_nodes/stats` | free | seconds |
| AmazonMQ (RabbitMQ) | management HTTP API | free | seconds |
| EC2 / hosts | `awsobs agent` reading /proc, pushed | free | seconds |
| EKS pods/nodes | metrics-server + kubelet log API | free | ~15s |
| ALB / NLB / S3 / RDS / DocDB host CPU+RAM / EKS control plane | CloudWatch `GetMetricData` | ~$0.01 / 1k metrics | 300s default |

## Configuration: bootstrap vs runtime

**Bootstrap** — the few things needed before the server can start and find its
storage. These live in `server.json`, and **every key can be overridden by an
environment variable** (the env wins):

| `server.json` key | env override | meaning |
|---|---|---|
| `listen` | `AWSOBS_LISTEN` | bind address (default `:8080`) |
| `data_dir` | `AWSOBS_DATA_DIR` | directory for the SQLite databases |
| `secret_key` | `AWSOBS_SECRET_KEY` | key that encrypts stored credentials (see below) |
| `auth.enabled` | `AWSOBS_AUTH_ENABLED` | require login (default `true`) |
| `auth.db_path` | `AWSOBS_AUTH_DB_PATH` | control-db path (default `<data_dir>/auth.db`) |

```json
{
  "listen": ":8080",
  "data_dir": "./data",
  "secret_key": "CHANGE_ME_ENCRYPTS_STORED_CREDENTIALS",
  "auth": { "enabled": true, "db_path": "" }
}
```

**Runtime** — everything about *what to monitor and how*: AWS (region,
credentials, namespaces, poll intervals), Kubernetes (contexts/clusters),
native targets (Valkey/OpenSearch/RabbitMQ), retention, and the `ingest_token`.
You configure this in **Admin ▸ Settings** in the dashboard; it's stored
encrypted in the control DB and **hot-applied** (the collector service
restarts, the web server keeps serving). Saving a change takes effect live —
no process restart — except `retention_points` (in-memory ring size), which
applies on the next restart.

**`secret_key`** encrypts every credential stored in the control DB (AWS keys,
native passwords, ingest token) with AES-256-GCM. Keep it out of source control
(`server.json` is gitignored); in production prefer the `AWSOBS_SECRET_KEY` env
var or a secret manager. Without a key the server still runs and the login
works, but credentials can't be stored or used until you set one.

> **Migration:** on first boot, if the control DB has no config yet, awsobs
> seeds the runtime config from any `aws`/`kubernetes`/`native`/`ingest_token`/
> retention fields present in `server.json`. So an existing full `server.json`
> keeps working — it's imported once, then the Settings UI is the source of
> truth.

## Deployment view

### 1. Run the server

**In an EKS cluster (recommended):**

```bash
docker build -t YOUR_ECR_REPO/awsobs:latest . && docker push YOUR_ECR_REPO/awsobs:latest
# edit deploy/k8s.yaml: set the image, the IRSA role ARN (annotation on the
# ServiceAccount), and AWSOBS_SECRET_KEY (as a Secret env var).
kubectl apply -f deploy/k8s.yaml
kubectl -n awsobs port-forward svc/awsobs 8080:80
```

- **IRSA** gives the pod CloudWatch access with no long-lived keys — leave AWS
  keys blank in Settings and the default credential chain is used. The IAM
  policy needs only `cloudwatch:ListMetrics` and `cloudwatch:GetMetricData`.
- The Service is ClusterIP on purpose. There's a built-in login, but keep the
  dashboard behind port-forward / VPN / an authenticating ingress anyway.
- Mount a volume at `data_dir` so the control DB (users, roles, encrypted
  config) and metric history survive restarts.

**On an EC2 host / VM / locally:** run the binary with a `server.json` (or the
env overrides), providing at least `data_dir` and `secret_key`.

### 2. First login and configure

1. Open the dashboard → you're prompted to log in. First-time setup seeds
   **`admin` / `admin`** and forces you to set a new password.
2. Go to **Admin ▸ Settings** and configure what to collect:
   - **AWS** — region, namespaces, poll intervals, and either static keys
     (encrypted at rest) or leave blank to use IRSA/instance role.
   - **Kubernetes** — kubeconfig contexts, or direct clusters (`api_url` +
     bearer token). In-cluster, the pod's ServiceAccount is used automatically.
   - **Native targets** — Valkey/ElastiCache, OpenSearch, RabbitMQ endpoints.
   - **Retention** and the **ingest token** (used by agents; generate one here).
3. Save — collectors start immediately.

Requirement for EKS metrics: **metrics-server** installed in the cluster (the
EKS `metrics-server` addon — if `kubectl top pods` works, you already have it).

### 3. Deploy agents (optional — host metrics + logs)

Agents push host CPU/memory/disk/network/load (from `/proc`) and tail log globs
to the server, using the **ingest token** you set in Settings.

- **EC2**: `deploy/awsobs-agent.service` (systemd unit)
- **EKS**: `deploy/agent-daemonset.yaml` (DaemonSet on every node; with
  `kube_logs: true` it ships each container's logs from `/var/log/containers`,
  so pod logs work even when the server runs outside the cluster)
- Agent config: `agent.example.json`, or env `AWSOBS_SERVER_URL` +
  `AWSOBS_TOKEN` (the token must match the server's ingest token).

## Developer view

Prerequisites: **Go ≥ 1.24** (required by the AWS SDK service clients), and for EKS work `kubectl` access plus
metrics-server in the cluster.

```bash
git clone https://github.com/tools-plus/awsobs && cd awsobs
go mod download

# minimal local bootstrap config
cat > server.json <<'JSON'
{
  "listen": ":8080",
  "data_dir": "./data",
  "secret_key": "dev-only-key",
  "auth": { "enabled": true }
}
JSON

go run ./cmd/awsobs -config server.json
# open http://localhost:8080  → log in as admin/admin, set a password,
# then configure AWS/EKS/native under Admin ▸ Settings.
```

Tips for local development:

- **Skip the login** while iterating with `"auth": { "enabled": false }` (or
  `AWSOBS_AUTH_ENABLED=false`). The Settings UI is then open without a login.
- **AWS**: set static keys in Settings, or export `AWS_REGION` / `AWS_PROFILE`
  and leave keys blank to use your local credential chain.
- **Kubernetes**: put your kubeconfig context name(s) in Settings ▸ Kubernetes
  (`*` = every context). awsobs runs a supervised `kubectl proxy` per context
  for you; a manually-run proxy + a direct `api_url` cluster entry also works.
- **Seed instead of clicking**: for a repeatable dev setup, put `aws` /
  `kubernetes` / `native` blocks in `server.json` — they seed the control DB on
  first boot (delete `<data_dir>/auth.db` to re-seed).
- **CGO**: the SQLite driver (control DB + metric persistence) needs
  `CGO_ENABLED=1` (the default). Agent cross-builds with `CGO_ENABLED=0` still
  compile — they just omit the driver they don't use.

Build and test:

```bash
go build ./...                 # builds server + agent (CGO on)
go test ./...                  # all tests
go test ./internal/store/      # one package
go vet ./...
docker build -t awsobs .       # multi-stage, distroless
```

Layout: `cmd/awsobs` (entrypoint / two-part wiring), `internal/collector`
(supervised collector service), `internal/awsmetrics` · `internal/native` ·
`internal/k8s` (collectors), `internal/auth` (users/roles/sessions + encrypted
config), `internal/secret` (AES-GCM), `internal/server` (HTTP + SSE + access
control), `internal/store` · `internal/logstore` · `internal/db` (hot stores +
SQLite), `web/` (embedded dashboard: `index.html` + `js/` ES modules).

## Authentication, users & roles

The login is **enabled by default**. Users, roles, sessions, and the encrypted
runtime config all live in the control DB (`<data_dir>/auth.db`).

- Passwords are bcrypt-hashed; login issues an `HttpOnly` session cookie
  (7-day). Change your own password from the **Profile** menu.
- **Roles** are a name + a set of services (EKS, RDS, S3, …). A scoped role
  sees **only those services**, read-only. Built-ins: `admin` (manage
  users/roles + everything) and `viewer` (all services, read-only). Create
  scoped roles (e.g. `db-team` → RDS, DocDB, ElastiCache) under **Admin ▸
  Roles** and assign them under **Admin ▸ Users**.
- New users get a temporary password and must change it at first login. The
  built-in `admin` user's role is locked; the last admin can't be deleted or
  demoted.
- Service access is enforced **server-side** on every data path — series list,
  data/history, pods, logs, and the live SSE stream — so a scoped user can't
  reach another team's services even via the raw API. Admins (and
  auth-disabled mode) bypass the filter.
- Agent push endpoints (`/api/ingest*`) use the shared `ingest_token` bearer
  token, not the login (agents can't do interactive auth).

## HTTP API

| Endpoint | What |
|---|---|
| `GET /` · `GET /login` | dashboard (session required) · login page |
| `POST /api/login` · `POST /api/logout` · `POST /api/change-password` | session lifecycle |
| `GET /api/me` | auth status: enabled, authenticated, user, role, is_admin, must_change |
| `GET/POST /api/settings` | runtime config (admin only; secrets are write-only) |
| `GET/POST /api/users`, `DELETE /api/users/{name}`, `POST /api/users/{name}/password`, `.../role` | user management (admin only) |
| `GET/POST /api/roles`, `POST /api/roles/{name}`, `DELETE /api/roles/{name}` | role management (admin only) |
| `GET /api/series?filter=` | known series, filtered to the caller's allowed services |
| `GET /api/series/data?id=` | full ring buffer for one series |
| `GET /api/history?id=&from=&to=` | on-demand CloudWatch fetch for long ranges (unix seconds) |
| `GET /api/stream` | SSE: every new point, filtered to allowed services |
| `GET /api/pods` | pod inventory (in-memory cache — instant) |
| `GET /api/status` | collector health: targets, last error/poll, clusters |
| `GET /api/logs?namespace=&pod=&container=&tail=` | SSE: live pod log tail |
| `GET /api/agentlogs?source=host/<name>&tail=` | SSE: live agent-shipped log tail |
| `POST /api/ingest` · `POST /api/ingest/logs` | agent metric/log push (bearer token) |

`/login`, `/api/login`, `/api/me`, and static assets are public; everything
else requires a session (and `/api/users*`, `/api/roles*`, `/api/settings`
require an admin) when auth is enabled.

Series IDs are pipe-delimited and predictable:
`k8s|pod|default/web-7f9c|cpu_cores`, `cw|RDS|CPUUtilization|mydb`.

## How it collects

**AWS** — one collector, one API. `ListMetrics` per namespace discovers
resources (a new RDS instance appears within the discovery interval);
`GetMetricData` (batched ≤500 queries/call) fetches. Cost is ~$0.01 per 1,000
metrics; slow `poll_interval_seconds` to cut it. S3 storage metrics are emitted
once per day by AWS, so awsobs polls those over a multi-day window at daily
resolution and their charts show one point per day.

**EKS** — a ~100-line REST client (no client-go) hitting the cluster APIs:
multiple clusters at once (each labeled), drill-down cluster → control plane /
nodes / workloads (from pod ownerReferences) → pods, pod/node CPU+memory from
`metrics.k8s.io`, inventory from the core API, and live log tails via
`pods/{pod}/log?follow=true` streamed to the browser over SSE.

**Storage** — SQLite is the system of record, memory is the hot path. With
`data_dir` set, the server ensures `<data_dir>/awsobs.db` on start, hydrates the
in-memory stores from it so the dashboard serves data *immediately* on restart,
and persists collected data in batched background transactions. History is
pruned to `db_retention_hours`, logs to `log_retention_lines` per source. For
ranges beyond the ring buffer (24h/3d/7d) the dashboard fetches from CloudWatch
via `/api/history`.

**Live updates** — every new point fans out to connected dashboards over SSE
(`/api/stream`), filtered per connection to the viewer's allowed services.

## Extending it

- **More CloudWatch metrics**: add entries to the `defaults` map in
  `internal/awsmetrics/cloudwatch.go`. Daily-granularity namespaces go in
  `dailyNamespaces`.
- **Container-level metrics**: scrape kubelet's cAdvisor endpoint
  (`/api/v1/nodes/{node}/proxy/metrics/cadvisor`) — the REST client already has
  `GetJSON`; add a Prometheus-text parser.
- **Alerting**: subscribe to the store (`store.Subscribe()`) and evaluate
  thresholds — the fan-out layer is already there.
- **New rail service**: keep `NS2SVC`/`svcOf` in `web/js/state.js` and
  `ns2svc`/`serviceOf` in `internal/server/access.go` in sync (they classify
  series for the rail and for role-based access).

## Notes

- go.mod targets Go 1.24 (the AWS SDK service clients used for discovery
  require it); the Dockerfile builds on `golang:1.24`.
- OpenSearch metrics live under the legacy `AWS/ES` namespace.
- ALB/NLB metrics are per-load-balancer and per-target-group; both dimension
  sets are discovered automatically.
