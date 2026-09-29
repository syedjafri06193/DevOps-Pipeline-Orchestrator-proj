# DevOps Pipeline Orchestrator (`orch`)

CLI tool and dashboard for managing multi-environment deployments with
automated rollback, health checks, and Slack integration — for everything that
isn't Kubernetes (VMs over SSH, ECS, Lambda, static sites, or anything a script
can deploy to).

One binary, one state file, one systemd unit: `orchd` runs the API, dashboard,
executor and notification worker; `orch` is the CLI.

```
 orch CLI / Slack / API ──► orchd ──► journal (fsynced, CRC-checked, hash-chained audit)
                                │
        deploy ──► provider (exec / fake) ──► verify vs baseline ──┬─► healthy: done
                                                                    └─► unhealthy: rollback
                                                                          (refused below a migration's rollback_floor)
                                │
                     transactional outbox ──► Slack      dashboard (SSE) · Prometheus /metrics
```

## What it guarantees

- **Crash-safe:** every state change is journalled before its side effect; startup reconciliation marks what it can't explain `UNKNOWN` instead of guessing.
- **No concurrent deploys to one target:** leases with fencing tokens.
- **No double deploys:** idempotency keys on every mutating call.
- **No unsafe rollbacks:** refused when an irreversible migration makes them unsafe.
- **No self-approval:** real RBAC, the same check across Slack, CLI and API.
- **Tamper-evident audit log:** SHA-256 hash chain, verified at startup.

## Quick start

Needs Go:

```sh
cd v1
make build                                   # static binaries in bin/
make test                                    # 279 tests, with -race
bin/orch config validate examples/orch.yaml
```

The full walkthrough (issuing a token, running `orchd`, deploy / promote /
rollback) is in [`v1/README.md`](v1/README.md#quick-start).

## Repository layout

```
.
├── README.md          ← you are here
├── docs/
│   ├── design.md      ← full design guide (the spec code comments cite)
│   └── design.pdf     ← same guide, PDF
└── v1/                ← first implementation
    ├── cmd/           orchd (server) and orch (CLI)
    ├── internal/      engine, store, api, auth, config, dashboard, notify/slack,
    │                  provider, verifier, metrics, redact, ulid
    ├── examples/      orch.yaml
    └── docs/          security, migrations, break-glass, spec errata
```

Each `vN/` directory is a self-contained iteration.

## Versions and feedback

| Version | Summary | Feedback |
|---|---|---|
| [v1](v1/) | Crash-safe journalled executor, verification + guarded rollback, RBAC approvals, Slack, dashboard, chaos tests | — |

Add a row per version as new iterations land.
