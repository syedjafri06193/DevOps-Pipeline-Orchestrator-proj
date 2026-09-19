# orch — a deployment orchestrator for everything that isn't Kubernetes

One binary, one state file, one systemd unit. `orchd` runs the API, the
dashboard, the deployment executor and the notification worker; `orch` is the
CLI. Copy the binary, point it at a config file, done.

It deploys to VMs over SSH, ECS, Lambda and static sites (anything a script can
deploy to, via the `exec` provider), verifies each release against its own
baseline, rolls back automatically when verification fails, **refuses** to roll
back when an irreversible migration makes that unsafe, and tells Slack about all
of it. Kubernetes is deliberately out of scope: Argo CD, Argo Rollouts and Kargo
already own that space.

## Quick start

```sh
make build                                       # CGO_ENABLED=0, static binaries in bin/
bin/orch config validate examples/orch.yaml      # works without a server
bin/orchd token issue --db /tmp/orch.journal --user sam > sam.token
bin/orchd serve --config examples/orch.yaml --db /tmp/orch.journal \
    --manifests ./manifests --allow-fake-provider

export ORCH_TOKEN_FILE=$PWD/sam.token
bin/orch doctor
bin/orch deploy  --app web --env staging --version v1.4.0 --wait
bin/orch promote --app web --env prod --wait     # ships exactly what staging ran
bin/orch rollback --app web --env prod --dry-run
bin/orch status
```

The dashboard is at `http://127.0.0.1:8080/`, Prometheus metrics at `/metrics`.

## What it guarantees

| Property | How |
|---|---|
| A crash never leaves a deploy wedged | Every state transition is journalled (CRC-checked, fsynced) **before** its side effect; startup reconciliation asks the provider what actually happened and marks anything it can't explain `UNKNOWN` rather than guessing. |
| Two deploys never touch one target at once | Leases with a 60 s TTL, 15 s heartbeat and monotonic fencing tokens. A lost lease aborts the executor. |
| A retried request doesn't deploy twice | Every mutating call needs an `Idempotency-Key`; the CLI generates one per logical deploy. |
| Notifications survive crashes | Transactional outbox: the state change, the audit row and the notification are one commit. |
| A flaky metric doesn't roll back a good deploy | Verifier errors are *inconclusive*, never *unhealthy*; criteria have a `floor_value` so 0.001 % → 0.004 % doesn't trip a ratio check. |
| An unsafe rollback doesn't happen | Release manifests carry a `rollback_floor`; below it the rollback is refused with the reason and the remedy. A missing manifest is not permission. |
| A rolled-back deploy fails the pipeline | `orch deploy --wait` exits non-zero on `ROLLED_BACK`. |
| Nobody approves their own deploy | Identity mapping, real RBAC, no self-approval, approvals expire — the same check for Slack, CLI and API. |
| The audit log can't be quietly edited | SHA-256 hash chain over every field, including the detail map; verified at every startup. |

## Layout

```
cmd/orchd            server: serve, token issue|list|revoke
cmd/orch             CLI: deploy rollback promote status history logs abort
                     approve freeze unfreeze config audit doctor
internal/config      strict YAML-subset parser and schema ("did you mean …")
internal/store       append-only journal, leases, audit chain, outbox
internal/engine      state machine, executor, verification, rollback, reconcile
internal/provider    Provider interface; exec (scripts) and fake
internal/verifier    Verifier interface; http and fake
internal/notify/slack  signature verification, Block Kit, approval handler, transport
internal/api         HTTP API with idempotency, authn/authz, audit
internal/auth        hashed, scoped, expiring tokens
internal/dashboard   server-rendered pages + SSE with Last-Event-ID replay
internal/metrics     Prometheus text exposition
```

## Testing

```sh
make test     # go test -race -count=1 ./...  — 279 tests
```

The chaos suite (`internal/engine/chaos_test.go`) crashes the executor before
and after every state's side effect, reopens the journal as a fresh process,
reconciles, and then **deploys again** to prove the system isn't wedged.

## Docs

- [`docs/notes-on-the-spec.md`](docs/notes-on-the-spec.md) — where this departs from the design document, and why
- [`docs/migrations.md`](docs/migrations.md) — expand/contract and release manifests
- [`docs/security.md`](docs/security.md) — threat model and controls
- [`docs/break-glass.md`](docs/break-glass.md) — what to do when the tool itself is the problem
