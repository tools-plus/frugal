# Security Policy

## Supported versions

frugal is pre-1.0 and under active development. Security fixes are applied to the
latest release and the `main` branch.

| Version              | Supported |
| :------------------- | :-------: |
| latest release (0.1.x) |    ✅    |
| `main`               |    ✅    |
| older releases       |    ❌    |

## Reporting a vulnerability

Please report security vulnerabilities **privately** — do **not** open a public
GitHub issue.

- **Preferred:** GitHub private vulnerability reporting — open the repository's
  **Security** tab → **Report a vulnerability**. This keeps the report private
  and lets us collaborate on a fix through a GitHub Security Advisory.
- **Alternatively:** email **security@mguptahub.com**.

Where possible, please include:

- the affected version or commit, and the platform (OS/arch, container vs binary);
- a description of the vulnerability and its impact;
- steps to reproduce, or a proof of concept;
- any suggested remediation.

## What to expect

- **Acknowledgement** within 3 business days.
- An **initial assessment** and severity classification within 7 business days.
- Progress updates as we work on a fix, and a coordinated disclosure timeline.
- With your permission, credit in the advisory and release notes.

Please give us a reasonable window to ship a fix before public disclosure — we
aim for coordinated disclosure within 90 days.

## Deployment hardening

A few things to keep in mind when running frugal, since they affect its security
posture:

- The dashboard has **no external authentication beyond its built-in login** —
  run it behind a port-forward, VPN, or an authenticating ingress. Never expose
  it directly to the public internet.
- Stored credentials (AWS keys, native-endpoint passwords, the ingest token) are
  encrypted at rest with `FRUGAL_SECRET_KEY` (AES-256-GCM). Keep that key out of
  source control and prefer an environment variable or a secret manager in
  production.
- The agent ingest endpoints (`/api/ingest*`) are authenticated with the shared
  `ingest_token` — treat it as a secret and rotate it if leaked.
- Prefer IRSA / instance roles over long-lived static AWS keys; frugal needs only
  read/list/describe permissions (see `deploy/iam-policy.json`).
