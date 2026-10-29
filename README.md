# DevOps Pipeline Orchestrator (`orch`)

[![CI](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/actions/workflows/ci.yml/badge.svg)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/actions/workflows/ci.yml)

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
├── 1—Documentation/   ← all documentation
│   ├── README.md      ← documentation index
│   ├── design.md      ← full design guide (the spec code comments cite)
│   ├── design.pdf     ← same guide, PDF
│   └── v1/            security, migrations, break-glass, spec errata
└── v1/                ← first implementation
    ├── cmd/           orchd (server) and orch (CLI)
    ├── internal/      engine, store, api, auth, config, dashboard, notify/slack,
    │                  provider, verifier, metrics, redact, ulid
    └── examples/      orch.yaml
```

Each `vN/` directory is a self-contained iteration.

## Versions and feedback

| Version | Summary | Feedback |
|---|---|---|
| [v1](v1/) | Crash-safe journalled executor, verification + guarded rollback, RBAC approvals, Slack, dashboard, chaos tests | — |

Add a row per version as new iterations land.

## Documentation

All documentation lives in [`1—Documentation/`](1—Documentation/README.md) — start with its index.

<!-- project-metrics:start -->
## Project Metrics

[![Stars](https://img.shields.io/github/stars/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/stargazers) [![Forks](https://img.shields.io/github/forks/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/forks) [![Open issues](https://img.shields.io/github/issues/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/issues) [![Last commit](https://img.shields.io/github/last-commit/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/improvements?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/commits/improvements) [![Commits](https://img.shields.io/github/commit-activity/t/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj/commits/improvements) [![Top language](https://img.shields.io/github/languages/top/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj) [![Repo size](https://img.shields.io/github/repo-size/syedjafri06193/DevOps-Pipeline-Orchestrator-proj?style=flat-square)](https://github.com/syedjafri06193/DevOps-Pipeline-Orchestrator-proj)

### Code

| Metric | Value |
|---|---|
| Lines of code | 11,351 |
| Source files | 67 |
| Tracked files | 79 |
| Documentation | 8 Markdown files (14,455 words), 1 PDF |
| Commits | 10 |
| Contributors | 1 |
| Active | 2026-09-11 → 2026-10-28 |

| Language | Lines | Share |
|---|---:|---:|
| Go | 10,810 | 95.2% |
| HTML+Django/Jinja | 189 | 1.7% |
| YAML | 130 | 1.1% |
| Transact-SQL | 112 | 1.0% |
| CSS+Lasso | 59 | 0.5% |
| Makefile | 49 | 0.4% |

<sub>Code stats as of 2026-10-28, counted with pygount (comments and blank lines excluded). Badges update live.</sub>
<!-- project-metrics:end -->
