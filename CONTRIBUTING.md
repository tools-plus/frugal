# Contributing to frugal

Thanks for your interest in contributing! frugal is a single-binary AWS + EKS
observability tool written in Go. Contributions of all kinds are welcome — bug
reports, features, docs, and collectors.

By contributing, you agree that your contributions are licensed under the
project's [GNU AGPL-3.0](LICENSE).

## Getting set up

Prerequisites: **Go ≥ 1.24** (required by the AWS SDK service clients). For EKS
work you'll also want `kubectl` access and metrics-server in the cluster.

```bash
git clone https://github.com/tools-plus/frugal && cd frugal
go mod download
go run ./cmd/frugal -config server.json   # see the README for a minimal server.json
```

Or run it via Docker Compose — see **Quickest local run** in the [README](README.md).

## Development workflow

1. Branch from `main`: `git checkout -b feat/short-description`.
2. Make your change, keeping the layered architecture intact (see the README's
   *How it collects* section and `CLAUDE.md`).
3. Format, vet, build, and test before you push:

   ```bash
   gofmt -w .
   go vet ./...
   go build ./...
   go test ./...
   ```

4. Open a pull request against `main` with a clear description of *what* changed
   and *why*.

There is no linter beyond `go vet` — stick to `gofmt` defaults.

## Commit & PR conventions

- Write clear, imperative commit messages (e.g. `feat: add S3 request-metrics
  collector`, `fix: handle empty CloudWatch dimension`).
- Keep pull requests focused — one logical change per PR where practical.
- Reference any related issue in the PR description.
- Make sure `go build ./...`, `go vet ./...`, and `go test ./...` pass locally.

## Adding collectors or metrics

- **More CloudWatch metrics** — extend the `defaults` map in
  `internal/awsmetrics/cloudwatch.go` (daily-granularity namespaces go in
  `dailyNamespaces`).
- **A new native poller** — add it under `internal/native` and wire it into
  `internal/collector`.
- **A new dashboard rail service** — keep `web/js/state.js` and
  `internal/server/access.go` classification in sync (see *Extending it* in the
  README).
- Keep series IDs **pipe-delimited and predictable** (`cw|…`, `nv|…`, `k8s|…`,
  `pi|…`, `ag|…`) — the dashboard parses them.

## Reporting issues

- **Bugs and feature requests** — open a GitHub issue.
- **Security vulnerabilities** — do **not** open a public issue; follow
  [SECURITY.md](SECURITY.md).
