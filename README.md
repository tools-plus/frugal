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
# Terminal 1 — proxy your cluster API locally (zero-config k8s auth):
kubectl proxy

# Terminal 2 — AWS creds from your normal profile/env:
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

- pod + node CPU/memory: `metrics.k8s.io` (metrics-server, ~15s resolution)
- pod inventory (phase, restarts, containers): core API
- live log tails: `GET .../pods/{pod}/log?follow=true` — the same call
  `kubectl logs -f` makes, streamed to the browser over SSE

**Storage** — fixed-size ring buffer per series (default 720 points ≈ 6h at
30s). Older data lives in CloudWatch itself; query it there when you need
history. This keeps the tool stateless — restart it and charts refill within
one poll. For time ranges beyond the buffer (24h/3d/7d in the UI)
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

The Service is ClusterIP on purpose — the dashboard has **no auth built in**.
Keep it behind port-forward, your VPN, or an authenticating ingress.

## HTTP API

| Endpoint | What |
|---|---|
| `GET /` | dashboard |
| `GET /api/series?filter=` | all known series (id, labels, last value) |
| `GET /api/series/data?id=` | full ring buffer for one series |
| `GET /api/history?id=&from=&to=` | on-demand CloudWatch fetch for long ranges (unix seconds) |
| `GET /api/stream` | SSE: every new point, as it lands |
| `GET /api/pods` | pod inventory |
| `GET /api/logs?namespace=&pod=&container=&tail=` | SSE: live pod log tail (k8s API, or agent-shipped fallback) |
| `GET /api/agentlogs?source=host/<name>&tail=` | SSE: live tail of agent-shipped logs |
| `POST /api/ingest` | agent metric push (bearer token) |
| `POST /api/ingest/logs` | agent log push (bearer token) |

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
