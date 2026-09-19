# Security

`orchd` can change production, so it is itself a production credential. This is
the threat model from section 12 and what implements each control.

| Threat | Control | Where |
|---|---|---|
| Stolen CLI/CI token | Tokens are SHA-256 hashed at rest, expire (12 h default), can be scoped to apps and environments, are revocable, and record last use. Header only — a `?token=` query parameter is ignored. | `internal/auth` |
| Malicious or mistaken insider | Real RBAC per environment; `deploy:rollback`, `deploy:approve`, `deploy:override`, `deploy:freeze` are separate permissions. Denied attempts are audited. | `internal/config`, `internal/api` |
| Slack approval spoofing | HMAC-SHA256 over `v0:{ts}:{raw body}`, constant-time compare, 5-minute replay window. Slack user IDs must map to a configured identity; RBAC applies; no self-approval; approvals expire. | `internal/notify/slack` |
| Secrets in logs | Provider output passes through a streaming redactor before it is persisted; it holds back a tail so a secret split across two writes is still caught. Secrets are never rendered on the dashboard or returned by `/v1/config`. | `internal/redact` |
| Tampering with history | Hash-chained audit log covering every field, verified at startup and via `orch audit verify`. | `internal/store` |
| Supply chain | No third-party Go modules. Static `CGO_ENABLED=0` binaries. | `go.mod`, `Makefile` |

## Operating it

- Run `orchd` as its own user; the journal and `tokens.json` are created 0600.
- Keep it on `127.0.0.1` behind a TLS-terminating proxy. It serves plain HTTP.
- `--no-auth` exists for a single-user laptop and logs a warning. Never use it on
  a shared host.
- Prefer `ORCH_TOKEN_FILE` over `ORCH_TOKEN`; environment variables leak into
  child processes and crash reports.
- Issue CI tokens with `--apps` and `--environments`. A leaked pipeline variable
  should not reach production.
