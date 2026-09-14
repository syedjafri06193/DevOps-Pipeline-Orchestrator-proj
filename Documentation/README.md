# DevOps Pipeline Orchestrator — Design & Build Guide

**Project:** CLI + dashboard for multi-environment deployments with automated rollback, health checks, and Slack integration
**Language:** Go
**Status of this document:** planning + reference

---

## Table of contents

1. [Executive summary and scope](#1-executive-summary-and-scope)
2. [Reality check](#2-reality-check)
3. [The architecture fork: where does state live](#3-the-architecture-fork-where-does-state-live)
4. [System architecture](#4-system-architecture)
5. [The deployment state machine](#5-the-deployment-state-machine)
6. [Concurrency, locking, and idempotency](#6-concurrency-locking-and-idempotency)
7. [Health checks and verification](#7-health-checks-and-verification)
8. [Rollback and the migration problem](#8-rollback-and-the-migration-problem)
9. [Deployment strategies](#9-deployment-strategies)
10. [Slack integration](#10-slack-integration)
11. [The dashboard](#11-the-dashboard)
12. [Security model](#12-security-model)
13. [Configuration schema](#13-configuration-schema)
14. [Tech stack and setup](#14-tech-stack-and-setup)
15. [Repository layout](#15-repository-layout)
16. [Milestone ladder](#16-milestone-ladder)
17. [Reference implementations](#17-reference-implementations)
18. [Testing strategy](#18-testing-strategy)
19. [Operational concerns](#19-operational-concerns)
20. [Stretch goals](#20-stretch-goals)
21. [References](#21-references)

---

## 1. Executive summary and scope

### The original statement

> CLI tool and dashboard for managing multi-environment deployments with automated rollback, health checks, and Slack integration.

Everything here is buildable. Unlike a hardware project, there's no physical wall. But this project has a different failure mode: **it's easy to build something that demos beautifully and is dangerous in production.** A deployment orchestrator that works on the happy path and corrupts state when the process is killed mid-deploy is worse than no tool at all, because people will trust it.

Four things will shape this project more than the feature list:

1. **If you target Kubernetes, you are rebuilding ArgoCD + Argo Rollouts + Kargo.** That stack already does multi-environment promotion, metric-driven automated rollback, canary and blue-green, health assessment, and notifications. You need a differentiator that isn't "but mine is newer." See section 2.1.
2. **Rollback is not the inverse of deploy.** Database migrations, message schema changes, and cached client assets all break the assumption that redeploying the previous artifact restores the previous state. A rollback button that doesn't account for this is a loaded gun. See section 8.
3. **The hard part of "automated rollback" is the signal, not the mechanism.** Flipping traffic back is twenty lines of code. Deciding *correctly* that a deploy is bad — without flapping on a transient blip, and without waiting so long the damage is done — is the actual engineering. See section 7.
4. **This tool holds production credentials.** It is, by construction, the highest-value target in the infrastructure it manages. Security is not a milestone you add at the end. See section 12.

### Revised project statement

> A single-binary deployment orchestrator for **heterogeneous, non-Kubernetes-native targets** (VMs over SSH, ECS, Lambda, static sites, bare metal), providing crash-safe deployment state, lease-based concurrency control, SLI-based automated rollback with baseline comparison, and Slack-driven approval gates — with an embedded dashboard and a full audit trail.

The pivot to non-Kubernetes targets is the important word in that sentence. See 2.1.

### Explicit non-goals

- **Not a CI system.** You do not build artifacts, run tests, or manage build caches. You consume artifacts that already exist. GitHub Actions / GitLab CI / Buildkite handle CI; you handle CD. Conflating them is how this project balloons to 18 months.
- **Not a Kubernetes GitOps controller.** Argo CD and Flux own that space completely.
- **Not an infrastructure provisioner.** Terraform/OpenTofu/Pulumi provision; you deploy application versions onto already-provisioned infrastructure.
- **Not a secrets manager.** You *read* from Vault / AWS Secrets Manager / SOPS. You never store secrets.
- **Not a monitoring system.** You *query* Prometheus/Datadog/CloudWatch for verification signals. You don't collect them.

Each of these is a tool that exists, is mature, and is not worth reimplementing. The value of a deployment orchestrator is in being the *connective tissue* between them with correct state handling.

---

## 2. Reality check

### 2.1 The competitive landscape

| Tool | What it does | Overlap with your statement |
|---|---|---|
| **Argo CD** | Kubernetes GitOps CD. Git is the source of truth; rollback is a git revert; the audit trail is the git log. | Very high, for K8s |
| **Argo Rollouts** | Progressive delivery: canary, blue-green, analysis-driven promotion, **automatic rollback on metrics**. Argo CD has had built-in health support for Rollout resources since 2.0. | This *is* your "automated rollback + health checks" feature, already built |
| **Kargo** (Akuity) | Multi-stage promotion pipelines across environments, built on Argo CD | This *is* your "multi-environment" feature |
| **Flux** | Modular Kubernetes GitOps | High, for K8s |
| **Spinnaker** | Multi-cloud, heavyweight, deployment-strategy-rich, its own backing store for pipeline state | High, but Spinnaker is widely regarded as too heavy to operate |
| **Harness / Octopus Deploy / Codefresh** | Commercial CD platforms with exactly your feature list | Very high |
| **GitHub Actions Environments** | Environment protection rules, required reviewers, deployment history | Medium — covers approvals and history but not rollback or health verification |

**The uncomfortable conclusion:** the project statement as written describes the Argo stack almost exactly. If you build a Kubernetes deployment orchestrator with canary rollback and Slack notifications, an experienced reviewer's first question will be "why not Argo Rollouts?" and you need a better answer than "I wanted to learn."

Three defensible positions:

**1. Target what Argo doesn't (recommended).** The Argo ecosystem is Kubernetes-native by design. An enormous amount of software still runs on EC2 instances, ECS, Lambda, Cloud Run, Fly.io, bare metal, and static CDNs — often several of these in one company. There is no good lightweight tool for "deploy this version to these fifteen VMs, watch the error rate, roll back if it spikes, and tell Slack." That's a real gap.

**2. Radical operational simplicity.** Argo CD is a set of controllers in a cluster with CRDs and an RBAC model. A single static binary with a SQLite file that a five-person team can run on one box, understand completely, and audit in an afternoon is a genuinely different product.

**3. Be honest that it's a learning project.** Legitimate, but then optimize for demonstrating hard things well — crash-safe state machines, lease-based distributed locking, statistical verification — rather than for feature count.

**Pick 1, build it so 2 is also true, and say 3 in the README without apologizing.**

### 2.2 The correctness problems nobody demos

A deployment tool's happy path is easy. These are the cases that separate a real tool from a script:

| Scenario | Naive behavior | What's required |
|---|---|---|
| Process killed mid-deploy | State says `deploying` forever; the lock never releases; nobody can deploy again | Persisted state machine + lease with TTL + reconciliation on startup |
| Two engineers deploy prod simultaneously | Interleaved, nondeterministic result | Lease-based lock per `(app, environment)` |
| CI retries a failed deploy step | Double deployment, or double Slack notification | Idempotency keys on every mutating operation |
| Network dies between "deploy" and "record success" | State and reality disagree | Reconcile actual state from the provider on startup; never trust the DB alone |
| Slack is down when a deploy finishes | Notification lost silently | Transactional outbox + retry worker |
| Health check flaps once during bake | Spurious automatic rollback, possibly mid-traffic-shift | Consecutive-failure thresholds and baseline comparison |
| Rollback itself fails health checks | Infinite rollback loop | Rollbacks are never auto-rolled-back; escalate to a human |
| Deploy succeeded but the DB migration didn't | Rollback to old code against a new schema → outage | Migration discipline enforced by the tool (section 8) |

**Design for these from day one.** Retrofitting crash safety onto a tool that assumed the happy path means rewriting the core.

### 2.3 The Slack timing constraint

Slack interactive components (buttons, modals) require your endpoint to **acknowledge within 3 seconds**. A deployment takes minutes. This is not a detail — it forces the whole architecture to be asynchronous:

```
Slack button click → verify signature → enqueue job → return 200  (< 100 ms)
                                             ↓
                                    worker picks up job
                                             ↓
                                    long-running deploy
                                             ↓
                            chat.update the original message with progress
```

There's a second constraint: Slack interactivity normally requires a **public HTTPS endpoint** Slack can POST to. For a self-hosted tool behind a corporate firewall, that's often a non-starter. **Socket Mode** solves it — your app opens an outbound WebSocket to Slack and receives events over it, no inbound ports, no public DNS, no TLS certificate. For this project, Socket Mode should be the default and HTTP endpoints the option, not the reverse.

### 2.4 The trust problem

A tool that can deploy to production can, by definition, deploy *anything* to production. Consider what an attacker gets by compromising it:

- Cloud credentials for every environment
- The ability to deploy arbitrary artifacts
- A Slack bot token that can post as a trusted identity
- The audit log (which they can tamper with if it's just a table)

This means: no secrets in the config file or the database, short-lived credentials wherever possible, an append-only audit log, signature verification on every Slack request, and a permission model where "can trigger a deploy" is separate from "can approve a prod deploy" and separate from "can bypass a gate."

Treat section 12 as a requirements document, not a chapter.

---

## 3. The architecture fork: where does state live

"CLI tool **and** dashboard" quietly implies a server, and where you put state is the decision everything else follows from. Make it consciously.

### Option A — Stateless CLI, state in Git

The desired state lives in a Git repo. The CLI commits and pushes. A dashboard reads the repo and the provider. No database.

| | |
|---|---|
| **Pros** | Free audit trail (git log). Free rollback (git revert). No server to operate. No state to corrupt. Reviewable via PRs. |
| **Cons** | Git is a terrible place for *runtime* state (a deploy in progress, health-check samples, lease ownership). Concurrent writes become push conflicts. Rollback-as-git-revert is a fiction when migrations are involved. |
| **Verdict** | This is what Argo CD does, and it does it better than you will. |

### Option B — Central server, thin CLI (recommended)

A single binary runs as a long-lived server owning a database. The CLI is an API client. The dashboard is served by the same binary.

| | |
|---|---|
| **Pros** | Real state machine with real transactions. Leases and idempotency are natural. Long-running deploys survive CLI disconnection. One place for Slack Socket Mode to live. Dashboard is trivial. |
| **Cons** | Something to operate. A single point of failure — you need a break-glass path (section 19.3). |
| **Verdict** | **Correct for this project.** One binary, one SQLite file, one systemd unit. |

### Option C — Serverless / CI-native

No server; deploys run inside CI jobs, state in DynamoDB or S3.

| | |
|---|---|
| **Pros** | Nothing to operate. Inherits CI's auth. |
| **Cons** | Slack Socket Mode needs a persistent connection. Long deploys hit CI job timeouts. Lease handling across ephemeral runners is painful. |
| **Verdict** | Viable but harder, for no benefit at this scale. |

### The decision

**Option B.** One Go binary that runs in three modes:

```
orchd serve              # long-lived: API, dashboard, workers, Slack socket
orch  deploy web prod    # CLI: thin API client
orchd worker             # optional: separate worker process for scale-out
```

The dashboard is compiled into the binary with `embed.FS`. Deployment is: copy one file, run one systemd unit. That simplicity is a feature and should be in the README's first paragraph.

---

## 4. System architecture

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│   CLI        │   │  Dashboard   │   │    Slack     │
│   (orch)     │   │  (browser)   │   │  (Socket     │
│              │   │              │   │   Mode WS)   │
└──────┬───────┘   └──────┬───────┘   └──────┬───────┘
       │ HTTP/JSON        │ HTTP + SSE       │ outbound WS
       └──────────────────┴──────────────────┘
                          │
       ┌──────────────────▼──────────────────────────────┐
       │  API layer                                       │
       │   authn (token / OIDC) → authz (RBAC) → audit    │
       └──────────────────┬──────────────────────────────┘
                          │
       ┌──────────────────▼──────────────────────────────┐
       │  Orchestrator core                               │
       │                                                  │
       │  ┌────────────┐  ┌──────────┐  ┌─────────────┐  │
       │  │   Lease    │  │  State   │  │ Idempotency │  │
       │  │  manager   │  │ machine  │  │    store    │  │
       │  └────────────┘  └────┬─────┘  └─────────────┘  │
       │                       │                          │
       │  ┌────────────────────▼──────────────────────┐   │
       │  │  Executor: runs one deployment plan       │   │
       │  │   preflight → gate → deploy → verify      │   │
       │  │            → promote | rollback           │   │
       │  └───┬──────────────┬──────────────┬─────────┘   │
       └──────┼──────────────┼──────────────┼─────────────┘
              │              │              │
   ┌──────────▼───┐ ┌────────▼──────┐ ┌────▼──────────────┐
   │  Providers   │ │  Verifiers    │ │  Notifiers        │
   │  ─────────── │ │  ──────────── │ │  ──────────────   │
   │  ssh         │ │  http         │ │  slack (outbox)   │
   │  ecs         │ │  prometheus   │ │  webhook          │
   │  lambda      │ │  datadog      │ │  stdout           │
   │  s3-static   │ │  cloudwatch   │ │                   │
   │  k8s         │ │  exec         │ │                   │
   │  exec        │ │               │ │                   │
   └──────────────┘ └───────────────┘ └───────────────────┘
              │
   ┌──────────▼──────────────────────────────────────────┐
   │  Store (SQLite / Postgres)                           │
   │   deployments · steps · leases · audit_log ·         │
   │   outbox · artifacts · approvals · health_samples    │
   └──────────────────────────────────────────────────────┘
```

### Core interfaces

Three plugin points. Keep them small — a small interface is the difference between "anyone can add a provider" and "only you understand this."

```go
// Provider knows how to put a version onto an environment.
type Provider interface {
    Name() string

    // Validate checks config at load time, before any deploy runs.
    Validate(cfg Config) error

    // Current returns the version currently deployed, read from the
    // real environment — never from our database.
    Current(ctx context.Context, target Target) (Version, error)

    // Deploy is idempotent: calling it twice with the same
    // (target, version, idempotencyKey) must not deploy twice.
    Deploy(ctx context.Context, req DeployRequest, out io.Writer) (DeployResult, error)

    // Shift moves traffic for strategies that support it.
    // Providers without traffic control return ErrUnsupported.
    Shift(ctx context.Context, target Target, weight int) error

    // Abort stops an in-flight deploy as cleanly as the platform allows.
    Abort(ctx context.Context, target Target, handle string) error
}

// Verifier answers: is the thing we just deployed healthy?
type Verifier interface {
    Name() string
    // Sample returns one observation. The engine decides what to do
    // with a series of them — Verifiers never decide to roll back.
    Sample(ctx context.Context, target Target, window time.Duration) (Sample, error)
}

// Notifier delivers an event. Must be safe to retry.
type Notifier interface {
    Name() string
    Notify(ctx context.Context, ev Event) error
}
```

Note the comment on `Verifier`: **a verifier never decides to roll back.** It produces observations. The rollback decision lives in one place in the engine, where it can be tested, tuned, and reasoned about. Scattering rollback logic into provider-specific health checks is how these tools become unpredictable.

---

## 5. The deployment state machine

Every deployment is a row whose state transitions are persisted **before** the corresponding side effect, so a crash always leaves a recoverable state.

```
                         ┌─────────┐
                         │ PENDING │
                         └────┬────┘
                              │ lease acquired
                         ┌────▼─────────┐
                         │  PREFLIGHT   │  config valid, artifact exists,
                         └────┬─────────┘  provider reachable, prior deploy clean
                              │
                    ┌─────────┴──────────┐
                    │                    │ gate required
                    │             ┌──────▼──────┐
                    │             │ AWAIT_APPROVAL│◀── Slack / CLI / dashboard
                    │             └──────┬──────┘
                    │                    │ approved
                    └─────────┬──────────┘
                              │
                         ┌────▼─────┐
                         │ DEPLOYING│  provider.Deploy()
                         └────┬─────┘
                              │
                         ┌────▼─────┐
                         │ VERIFYING│  bake window, sample SLIs
                         └────┬─────┘
                    ┌─────────┼─────────┐
            healthy │         │         │ unhealthy
              ┌─────▼─────┐   │   ┌─────▼────────┐
              │ PROMOTING │   │   │ ROLLING_BACK │
              └─────┬─────┘   │   └─────┬────────┘
                    │         │         │
              ┌─────▼─────┐   │   ┌─────┴─────────┬──────────────┐
              │ SUCCEEDED │   │   │               │              │
              └───────────┘   │ ┌─▼──────────┐ ┌─▼────────────┐ │
                              │ │ ROLLED_BACK│ │ROLLBACK_FAILED│ │
                              │ └────────────┘ └──────┬───────┘ │
                              │                       │ PAGE A HUMAN
                         ┌────▼─────┐                 │
                         │ ABORTED  │◀── manual       │
                         └──────────┘                  │
```

### Rules

- **Terminal states:** `SUCCEEDED`, `ROLLED_BACK`, `ROLLBACK_FAILED`, `ABORTED`. Nothing transitions out of them.
- **`ROLLBACK_FAILED` is the only state that pages.** Everything else is recoverable by the system; this one means production is in an unknown state and a human must look.
- **A rollback is never automatically rolled back.** If the rollback's own verification fails, go to `ROLLBACK_FAILED`. The alternative is an infinite loop that thrashes production.
- **Persist the transition, then do the thing.** Write `DEPLOYING` and commit *before* calling `provider.Deploy()`. On restart, a deployment found in `DEPLOYING` with an expired lease is reconciled by asking the provider what actually happened — never by assuming.
- **Every transition writes an audit row** in the same transaction.

### Schema

```sql
CREATE TABLE deployments (
    id               TEXT PRIMARY KEY,          -- ULID: sortable by time
    app              TEXT NOT NULL,
    environment      TEXT NOT NULL,
    version          TEXT NOT NULL,             -- artifact identity
    previous_version TEXT,                      -- captured at PREFLIGHT
    strategy         TEXT NOT NULL,
    state            TEXT NOT NULL,
    idempotency_key  TEXT UNIQUE,
    triggered_by     TEXT NOT NULL,
    trigger_source   TEXT NOT NULL,             -- cli | slack | api | schedule
    provider_handle  TEXT,                      -- opaque; for reconciliation
    is_rollback      INTEGER NOT NULL DEFAULT 0,
    rollback_of      TEXT REFERENCES deployments(id),
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    finished_at      TIMESTAMP,
    error            TEXT
);

CREATE INDEX idx_dep_active ON deployments(app, environment, state)
    WHERE state NOT IN ('SUCCEEDED','ROLLED_BACK','ROLLBACK_FAILED','ABORTED');

CREATE TABLE deployment_steps (
    id            INTEGER PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    seq           INTEGER NOT NULL,
    name          TEXT NOT NULL,
    state         TEXT NOT NULL,
    started_at    TIMESTAMP,
    finished_at   TIMESTAMP,
    output_ref    TEXT,                          -- log blob key
    error         TEXT,
    UNIQUE(deployment_id, seq)
);

CREATE TABLE leases (
    resource    TEXT PRIMARY KEY,                -- "app:web/env:prod"
    holder      TEXT NOT NULL,                   -- deployment id
    owner_node  TEXT NOT NULL,
    acquired_at TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    fence_token INTEGER NOT NULL                 -- monotonic, see 6.2
);

CREATE TABLE health_samples (
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    verifier      TEXT NOT NULL,
    at            TIMESTAMP NOT NULL,
    value         REAL NOT NULL,
    baseline      REAL,
    healthy       INTEGER NOT NULL
);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY,
    at         TIMESTAMP NOT NULL,
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    resource   TEXT NOT NULL,
    detail     TEXT,                              -- JSON
    prev_hash  TEXT NOT NULL,                     -- hash chain, see 12.4
    hash       TEXT NOT NULL
);

CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY,
    created_at   TIMESTAMP NOT NULL,
    notifier     TEXT NOT NULL,
    payload      TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt TIMESTAMP NOT NULL,
    delivered_at TIMESTAMP
);
```

Use **ULIDs**, not UUIDv4, for deployment IDs — they sort lexicographically by creation time, which makes "show me recent deploys" a primary-key range scan instead of a sort.

---

## 6. Concurrency, locking, and idempotency

### 6.1 Lease-based locking

A plain "is there an active deploy?" check is a race. A plain lock is worse — a crashed process holds it forever. Use a **lease**: a lock with an expiry, renewed by a heartbeat while work is in progress.

```go
const (
    leaseTTL       = 60 * time.Second
    leaseHeartbeat = 15 * time.Second
)

// Acquire takes the lease for a resource, or fails if held and unexpired.
func (s *Store) AcquireLease(ctx context.Context, resource, holder, node string) (int64, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return 0, err
    }
    defer tx.Rollback()

    now := time.Now().UTC()

    var (
        curHolder  string
        curExpires time.Time
        curFence   int64
    )
    err = tx.QueryRowContext(ctx,
        `SELECT holder, expires_at, fence_token FROM leases WHERE resource = ?`,
        resource,
    ).Scan(&curHolder, &curExpires, &curFence)

    switch {
    case errors.Is(err, sql.ErrNoRows):
        curFence = 0
    case err != nil:
        return 0, err
    case curExpires.After(now):
        return 0, &LeaseHeldError{Resource: resource, Holder: curHolder, Until: curExpires}
    }

    fence := curFence + 1
    if _, err := tx.ExecContext(ctx, `
        INSERT INTO leases (resource, holder, owner_node, acquired_at, expires_at, fence_token)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(resource) DO UPDATE SET
            holder = excluded.holder, owner_node = excluded.owner_node,
            acquired_at = excluded.acquired_at, expires_at = excluded.expires_at,
            fence_token = excluded.fence_token`,
        resource, holder, node, now, now.Add(leaseTTL), fence,
    ); err != nil {
        return 0, err
    }
    return fence, tx.Commit()
}
```

A background goroutine renews while the deploy runs. If renewal fails (the process was partitioned, or someone force-broke the lease), **the executor must abort** rather than keep operating on a resource it no longer owns.

### 6.2 Fencing tokens

A lease expiring doesn't stop the old holder — it may be paused, or partitioned, and will resume believing it still holds the lease. This is the classic distributed-lock failure and it's how you get two deploys running simultaneously.

The fix is a **fencing token**: a monotonically increasing number issued with each lease. Every mutating operation carries it, and the store rejects operations with a stale token.

```go
func (s *Store) TransitionState(ctx context.Context, depID string, fence int64,
    from, to State) error {

    res, err := s.db.ExecContext(ctx, `
        UPDATE deployments SET state = ?, updated_at = ?
        WHERE id = ? AND state = ?
          AND EXISTS (
            SELECT 1 FROM leases
            WHERE holder = ? AND fence_token >= ?
          )`,
        to, time.Now().UTC(), depID, from, depID, fence)
    if err != nil {
        return err
    }
    if n, _ := res.RowsAffected(); n == 0 {
        return ErrStaleFence   // lost the lease, or state moved under us
    }
    return nil
}
```

Providers that support it should carry the fence into the target system too — an ECS task definition tag, a Kubernetes annotation, an SSH lock file. Then even a resumed zombie process can't clobber a newer deploy.

### 6.3 Idempotency

Every mutating API call accepts an `Idempotency-Key` header. The CLI generates one per logical deploy and reuses it across retries.

```go
func (a *API) CreateDeployment(w http.ResponseWriter, r *http.Request) {
    key := r.Header.Get("Idempotency-Key")
    if key == "" {
        http.Error(w, "Idempotency-Key required", http.StatusBadRequest)
        return
    }
    existing, err := a.store.DeploymentByIdempotencyKey(r.Context(), key)
    if err == nil {
        // Same key, same request → return the original result, don't re-run.
        writeJSON(w, http.StatusOK, existing)
        return
    }
    // ... create new, with idempotency_key set under the UNIQUE constraint
}
```

The `UNIQUE` constraint on `idempotency_key` makes this correct even under concurrent duplicate requests — the second insert fails and you return the first result.

### 6.4 The transactional outbox

Never call Slack inside the transaction that updates deployment state. If Slack is slow you hold locks; if it fails after commit you lose the notification; if you commit after the call you can double-send.

Write the notification into the `outbox` table **in the same transaction** as the state change. A worker drains it with exponential backoff.

```go
func (e *Executor) transitionAndNotify(ctx context.Context, dep *Deployment,
    to State, ev Event) error {

    return e.store.InTx(ctx, func(tx *sql.Tx) error {
        if err := tx.Transition(dep.ID, dep.Fence, dep.State, to); err != nil {
            return err
        }
        if err := tx.AppendAudit(actorOf(ctx), string(to), dep.Resource(), ev); err != nil {
            return err
        }
        return tx.EnqueueOutbox("slack", ev)   // same transaction
    })
}
```

Exactly-once delivery is impossible; **at-least-once plus idempotent consumers** is the achievable goal. For Slack, store the `ts` of the message you posted and use `chat.update` on retry rather than posting again.

---

## 7. Health checks and verification

This is the intellectual core of the project, and where most homegrown deployment tools are weakest.

### 7.1 Three different things called "health check"

Conflating these is the root cause of bad rollback behavior.

| Type | Question | Timescale | Good for |
|---|---|---|---|
| **Liveness** | Is the process running? | seconds | Detecting a crash loop |
| **Readiness** | Can this instance serve traffic? | seconds | Gating traffic during rollout |
| **Verification** | Is this *version* good? | minutes | **Deciding whether to roll back** |

An HTTP 200 from `/health` tells you a process is up. It tells you nothing about whether the new version introduced a 3% error rate, doubled p99 latency, or broke a code path that only 5% of requests hit. **Rolling back on liveness alone misses most real regressions and fires on transient blips.**

Verification must be SLI-based and compared against a baseline.

### 7.2 Baseline comparison

Absolute thresholds ("roll back if error rate > 1%") fail in both directions: a service that normally runs at 2% errors rolls back constantly, and a service that normally runs at 0.01% doesn't roll back when it jumps to 0.9%.

Compare the canary against the **baseline over the same window**:

```go
type Sample struct {
    Value    float64
    Baseline float64
    At       time.Time
}

type Criterion struct {
    Name      string
    Verifier  string
    Direction Direction     // LowerIsBetter | HigherIsBetter
    // Absolute ceiling, applied regardless of baseline.
    Max       *float64
    // Relative tolerance, e.g. 1.20 = canary may be 20% worse than baseline.
    MaxRatio  *float64
    // Ignore the criterion below this absolute value, so that
    // 0.001% → 0.004% doesn't look like a 4x regression.
    FloorValue float64
}

func (c Criterion) Evaluate(s Sample) bool {
    if c.Direction == LowerIsBetter && s.Value <= c.FloorValue {
        return true      // too small to matter
    }
    if c.Max != nil && s.Value > *c.Max {
        return false
    }
    if c.MaxRatio != nil && s.Baseline > 0 {
        ratio := s.Value / s.Baseline
        if c.Direction == HigherIsBetter {
            ratio = s.Baseline / s.Value
        }
        if ratio > *c.MaxRatio {
            return false
        }
    }
    return true
}
```

That `FloorValue` field looks like a detail and is not. Without it, a service whose error rate moves from 0.001% to 0.004% trips a 2× ratio check and rolls back a perfectly good deploy. This single field prevents a large fraction of spurious rollbacks.

Where the baseline comes from, in descending order of quality:

1. **A parallel baseline deployment** — run the old version alongside the canary, both receiving live traffic. Highest quality, most infrastructure.
2. **The same metric from the pre-deploy window** — cheap, but confounded by time-of-day and traffic shape.
3. **The non-canary portion of the fleet** — good middle ground when you're doing partial rollouts.

### 7.3 The decision engine

```go
type VerifyPolicy struct {
    BakeDuration      time.Duration  // total observation window
    SampleInterval    time.Duration
    MinSamples        int            // never decide on fewer than this
    FailureThreshold  int            // consecutive failures before rollback
    SuccessThreshold  int            // consecutive passes before promote
    MaxInconclusive   int            // verifier errors tolerated
    Criteria          []Criterion
}

func (v *Verifier) Run(ctx context.Context, dep *Deployment, p VerifyPolicy) (Verdict, error) {
    var consecutiveFail, consecutivePass, inconclusive, total int
    ticker := time.NewTicker(p.SampleInterval)
    defer ticker.Stop()

    deadline := time.Now().Add(p.BakeDuration)

    for time.Now().Before(deadline) {
        select {
        case <-ctx.Done():
            return VerdictAborted, ctx.Err()
        case <-ticker.C:
        }

        ok, err := v.sampleAll(ctx, dep, p.Criteria)
        if err != nil {
            // A broken Prometheus is NOT a failing deploy.
            inconclusive++
            if inconclusive > p.MaxInconclusive {
                return VerdictInconclusive, err
            }
            continue
        }
        total++

        if ok {
            consecutivePass++
            consecutiveFail = 0
            if consecutivePass >= p.SuccessThreshold && total >= p.MinSamples {
                return VerdictHealthy, nil
            }
        } else {
            consecutiveFail++
            consecutivePass = 0
            if consecutiveFail >= p.FailureThreshold {
                return VerdictUnhealthy, nil
            }
        }
    }

    if total < p.MinSamples {
        return VerdictInconclusive, nil
    }
    return VerdictHealthy, nil   // survived the bake without tripping
}
```

Three things in there matter more than they look:

**A verifier error is not a deploy failure.** If Prometheus is down, you do not know whether the deploy is bad. Rolling back on missing data is how a monitoring outage becomes a deployment outage. Return `Inconclusive` and let policy decide — for prod, that should mean "hold and ask a human," not "roll back."

**Consecutive failures, not cumulative.** One bad sample in a ten-minute bake is noise. Three in a row is a signal.

**`MinSamples` guards against a fast early verdict** on a metric that hasn't had time to move.

### 7.4 The rollback circuit breaker

If a version has been deployed and rolled back twice, stop trying. Automatic retry of a known-bad deploy wastes error budget and confuses everyone watching Slack.

```go
func (e *Executor) checkRollbackBudget(ctx context.Context, app, env, version string) error {
    n, err := e.store.CountRollbacks(ctx, app, env, version, 24*time.Hour)
    if err != nil {
        return err
    }
    if n >= 2 {
        return &BlockedError{
            Reason: fmt.Sprintf(
                "version %s has been rolled back %d times in 24h; "+
                "deploy blocked. Override with --force (requires deploy:override)",
                version, n),
        }
    }
    return nil
}
```

---

## 8. Rollback and the migration problem

**Read this section before writing the rollback code.** It is the part of the project most likely to cause a real incident if done naively.

### 8.1 Rollback is not the inverse of deploy

"Rollback" usually means "redeploy the previous artifact." That restores the *code*. It does not restore:

| Thing | Why rollback breaks it |
|---|---|
| **Database schema** | v2 ran a migration that dropped `users.legacy_id`. v1 selects that column. Rolling back the code produces instant 500s on every request. |
| **Data written in the new format** | v2 wrote JSON into a column v1 expects to be CSV. The rows are still there after rollback. |
| **Message queue payloads** | v2 enqueued messages with a new schema. v1 consumers can't parse them and the queue poisons. |
| **Cached client assets** | Browsers hold v2's JS bundle, which calls a v2-only API endpoint that no longer exists. |
| **External side effects** | v2 sent 40,000 emails. There is no rollback for that. |
| **Feature flag state** | v2 turned on a flag that v1 doesn't understand. |

The dangerous property is that **rollback appears to succeed**. The deploy completes, the process starts, liveness passes — and the application is broken in a way that only shows up under real traffic.

### 8.2 The discipline the tool must enforce: expand/contract

The only way to make rollback safe is to require that **every version is compatible with its immediate predecessor's data**. That means schema changes happen in separate, ordered releases:

```
Release N   (expand)    Add the new column, nullable. Deploy code that
                        writes BOTH old and new, reads old.
                        → safe to roll back: old code ignores the new column

Release N+1 (migrate)   Backfill. Deploy code that writes both, reads new.
                        → safe to roll back: both columns are populated

Release N+2 (contract)  Deploy code that writes and reads only new.
                        → safe to roll back to N+1

Release N+3 (cleanup)   Drop the old column.
                        → NOT safe to roll back past N+2
```

Four releases to rename a column. That is the actual cost of safe rollback, and a deployment orchestrator's job is to make the cost visible rather than pretend it doesn't exist.

### 8.3 How the tool encodes this

Every artifact declares a **compatibility floor** — the oldest version it can safely roll back to:

```yaml
# .orch/manifest.yaml, built into the artifact at CI time
version: "2026.09.14-a3f9c21"
schema_version: 47
rollback_floor: "2026.09.02-8b1de40"   # cannot safely go below this
migrations:
  - id: 0047_add_users_uuid
    reversible: true
    expand_phase: true
  - id: 0046_backfill_uuid
    reversible: false                   # ← the thing that matters
```

Then rollback becomes a checked operation, not a button:

```go
func (e *Executor) PlanRollback(ctx context.Context, dep *Deployment) (*Plan, error) {
    target, err := e.store.LastSuccessfulBefore(ctx, dep.App, dep.Environment, dep.ID)
    if err != nil {
        return nil, err
    }

    cur, err := e.artifacts.Manifest(ctx, dep.Version)
    if err != nil {
        return nil, err
    }

    if semverLess(target.Version, cur.RollbackFloor) {
        return nil, &UnsafeRollbackError{
            From: dep.Version, To: target.Version, Floor: cur.RollbackFloor,
            Reason: "target predates the current version's rollback floor; " +
                    "an irreversible migration ran between them",
            Remedy: "roll forward with a fix, or run the documented manual " +
                    "recovery procedure for this migration",
        }
    }

    if target.SchemaVersion != cur.SchemaVersion {
        for _, m := range cur.Migrations {
            if !m.Reversible {
                return nil, &UnsafeRollbackError{
                    Reason: fmt.Sprintf("migration %s is irreversible", m.ID),
                    Remedy: "roll forward",
                }
            }
        }
    }
    return e.buildPlan(target, WithFlag("is_rollback")), nil
}
```

**When rollback is unsafe, the tool must refuse and say why.** Not warn — refuse. The correct action in that situation is roll-forward with a fix, and a tool that makes the unsafe path easy will get someone paged at 3 a.m.

This one feature is probably the most defensible thing in the whole project. Most deployment tools, commercial ones included, treat rollback as unconditional.

### 8.4 Roll forward as a first-class operation

Because rollback is often unsafe, make roll-forward equally ergonomic:

```bash
orch rollback web prod              # refuses if unsafe, explains why
orch deploy web prod --version=...  # the roll-forward path
orch freeze web prod --reason="..."  # stop all deploys while you think
```

---

## 9. Deployment strategies

| Strategy | Mechanism | Rollback | Requires | Use for |
|---|---|---|---|---|
| **Recreate** | Stop old, start new | Redeploy old | nothing | Dev, batch jobs, anything with downtime tolerance |
| **Rolling** | Replace instances in batches | Roll the previous version back through | Multiple instances, readiness checks | Default for most services |
| **Blue-green** | Deploy to idle env, flip traffic atomically | Flip back — **fast and clean** | 2× capacity, a traffic switch (LB, DNS, alias) | Anything where fast rollback matters most |
| **Canary** | Shift a % of traffic, verify, increase | Shift back to 0% | Traffic splitting + per-version metrics | High-traffic services where you can measure |

### Implementation notes

**Blue-green has the best rollback story** and is the easiest to implement correctly, because the flip is a single atomic operation on a load balancer target group or a Lambda alias. If you implement one non-trivial strategy first, make it this one.

**Canary needs per-version metrics**, and this is where it usually falls apart. If your metrics aren't tagged by version, your "canary error rate" is actually the whole fleet's error rate diluted by 90% of healthy traffic, and you will never detect anything. Verify that the tagging works *before* building canary support.

Canary step definition:

```yaml
strategy:
  type: canary
  steps:
    - setWeight: 5
    - verify: { bake: 5m }
    - setWeight: 25
    - verify: { bake: 10m }
    - setWeight: 50
    - verify: { bake: 10m }
    - setWeight: 100
```

**Analysis at 5% traffic is statistically weak.** For a service doing 100 req/s, 5% for 5 minutes is 1,500 requests. Detecting a change in error rate from 0.1% to 0.3% in 1,500 samples is not reliable. Either start at a higher weight, bake longer, or accept that the early steps catch only catastrophic failures. Say this in your docs — it's the kind of honesty that distinguishes a considered tool.

---

## 10. Slack integration

### 10.1 Socket Mode first

```go
import (
    "github.com/slack-go/slack"
    "github.com/slack-go/slack/socketmode"
)

func (s *SlackBot) Run(ctx context.Context) error {
    api := slack.New(
        s.botToken,                             // xoxb-...
        slack.OptionAppLevelToken(s.appToken),  // xapp-...
    )
    client := socketmode.New(api)

    go func() {
        for evt := range client.Events {
            switch evt.Type {
            case socketmode.EventTypeInteractive:
                cb, ok := evt.Data.(slack.InteractionCallback)
                if !ok {
                    continue
                }
                // ACK IMMEDIATELY. Slack's deadline is 3 seconds.
                client.Ack(*evt.Request)
                // Do the real work asynchronously.
                go s.handleInteraction(ctx, cb)

            case socketmode.EventTypeSlashCommand:
                cmd, _ := evt.Data.(slack.SlashCommand)
                client.Ack(*evt.Request)
                go s.handleCommand(ctx, cmd)
            }
        }
    }()

    return client.RunContext(ctx)
}
```

Socket Mode means no public endpoint, no inbound firewall rule, no TLS certificate, no ngrok during development. For a self-hosted tool this is decisively the right default.

### 10.2 If you do expose an HTTP endpoint, verify signatures

Unverified Slack webhook handlers are a straightforward path to "anyone on the internet can deploy to your production."

```go
func VerifySlackSignature(signingSecret string, h http.Header, body []byte) error {
    tsStr := h.Get("X-Slack-Request-Timestamp")
    ts, err := strconv.ParseInt(tsStr, 10, 64)
    if err != nil {
        return errors.New("bad timestamp")
    }
    // Replay protection: Slack recommends rejecting anything older than 5 min.
    if math.Abs(float64(time.Now().Unix()-ts)) > 300 {
        return errors.New("stale request")
    }

    base := "v0:" + tsStr + ":" + string(body)
    mac := hmac.New(sha256.New, []byte(signingSecret))
    mac.Write([]byte(base))
    expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

    // Constant-time comparison — a naive == is a timing oracle.
    if !hmac.Equal([]byte(expected), []byte(h.Get("X-Slack-Signature"))) {
        return errors.New("signature mismatch")
    }
    return nil
}
```

Read the raw body **before** any JSON decoding — the signature covers exact bytes, and a decode-then-re-encode round trip will not match.

### 10.3 Approval gates: the authorization trap

A Slack "Approve" button carries a Slack user ID. It does **not** carry your system's authorization. If you map "clicked the button" to "approved," then anyone who can be invited to that channel can approve a production deploy.

Rules:

```go
func (s *SlackBot) handleApproval(ctx context.Context, cb slack.InteractionCallback) {
    depID := cb.ActionCallback.BlockActions[0].Value

    // 1. Map Slack identity → internal identity. Never trust the display name.
    user, err := s.identity.BySlackUserID(ctx, cb.User.ID)
    if err != nil {
        s.ephemeral(cb, "Your Slack account isn't linked to an orchestrator user.")
        return
    }

    dep, err := s.store.Deployment(ctx, depID)
    if err != nil { /* ... */ }

    // 2. Check the real permission, in the real RBAC system.
    if !s.authz.Can(user, "deploy:approve", dep.App, dep.Environment) {
        s.ephemeral(cb, "You don't have approval rights for "+dep.Environment+".")
        s.audit(ctx, user, "approval.denied", dep.Resource(), nil)
        return
    }

    // 3. No self-approval on protected environments.
    if dep.TriggeredBy == user.ID && s.cfg.Env(dep.Environment).RequirePeerApproval {
        s.ephemeral(cb, "This environment requires approval from someone else.")
        return
    }

    // 4. Approvals expire. A stale button click is not consent.
    if time.Since(dep.CreatedAt) > s.cfg.ApprovalTTL {
        s.ephemeral(cb, "This approval request has expired.")
        return
    }

    if err := s.store.RecordApproval(ctx, depID, user.ID); err != nil { /* ... */ }
    s.audit(ctx, user, "approval.granted", dep.Resource(), nil)
    s.updateMessage(ctx, cb, approvedBlocks(dep, user))
}
```

All four checks matter. The fourth — approval expiry — is the one people forget, and it means a button posted at 2pm can still be clicked at 11pm against a deployment whose context has entirely changed.

### 10.4 Message design

Update one message in place rather than posting a stream. A deploy that posts eight messages to `#deploys` trains people to mute the channel.

```
┌──────────────────────────────────────────────┐
│ 🚀  web → production                          │
│                                              │
│  Version    2026.09.14-a3f9c21               │
│  From       2026.09.12-8b1de40               │
│  By         @jordan  ·  via CLI              │
│  Strategy   canary                           │
│                                              │
│  ▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░  50%  verifying         │
│                                              │
│  error rate   0.04%  (baseline 0.03%)  ✓     │
│  p99 latency   181ms  (baseline 174ms)  ✓    │
│                                              │
│  [ View dashboard ]   [ Abort ]              │
└──────────────────────────────────────────────┘
```

Store the message `ts` on the deployment row and `chat.update` it. Post a *new* message only for terminal failure states, so failures break through the noise.

**Rate limits:** `chat.postMessage` is tightly limited per channel (roughly one message per second, with small bursts). A canary with a 15-second sample interval updating a message on every sample will get throttled. Coalesce updates — at most one every 5–10 seconds, or only on meaningful state change.

---

## 11. The dashboard

### 11.1 Server-rendered + SSE, not an SPA

The dashboard's primary job is showing live deployment progress and streaming logs. That is a server-push problem, and a React SPA with polling is more code for a worse result.

```
Go binary
  ├── embed.FS with templates + a little CSS/JS
  ├── html/template server-rendered pages
  ├── htmx for interactions (no build step)
  └── Server-Sent Events for live updates
```

SSE over WebSockets because it's unidirectional (which is all you need), it reconnects automatically, it's plain HTTP, and it survives proxies that mangle WebSocket upgrades.

```go
func (h *Handler) StreamDeployment(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")   // disable nginx buffering

    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming unsupported", http.StatusInternalServerError)
        return
    }

    depID := chi.URLParam(r, "id")
    sub := h.bus.Subscribe(depID)
    defer h.bus.Unsubscribe(depID, sub)

    // Replay from the caller's last seen event so reconnects don't lose output.
    lastID := r.Header.Get("Last-Event-ID")
    for _, ev := range h.store.EventsSince(r.Context(), depID, lastID) {
        writeSSE(w, ev)
    }
    flusher.Flush()

    keepalive := time.NewTicker(20 * time.Second)
    defer keepalive.Stop()

    for {
        select {
        case <-r.Context().Done():
            return
        case ev := <-sub:
            writeSSE(w, ev)
            flusher.Flush()
        case <-keepalive.C:
            fmt.Fprint(w, ": keepalive\n\n")   // stop proxies timing out
            flusher.Flush()
        }
    }
}
```

The `Last-Event-ID` replay and the keepalive comment are both load-bearing. Without replay, a reconnect silently drops log lines. Without the keepalive, proxies close idle SSE connections after 30–60 seconds and the UI appears to freeze.

### 11.2 Pages

| Page | Content |
|---|---|
| **Overview** | Grid of apps × environments, current version, health, time since last deploy |
| **Deployment detail** | Live state machine, step timeline, streaming logs, health-sample chart, abort/approve buttons |
| **History** | Filterable list, diff between versions, who and when |
| **Audit log** | Append-only, searchable, chain-verifiable |
| **Config** | Rendered current config with the source file and commit that produced it |

The overview page answering "what version is in prod right now, and is it healthy" in under a second is the thing people will actually use it for. Optimize that one.

---

## 12. Security model

### 12.1 Threat model

| Threat | Mitigation |
|---|---|
| Stolen CLI token | Short TTL, scoped to app+env, revocable, audit every use |
| Compromised orchestrator host | No long-lived cloud creds on disk; OIDC federation for short-lived tokens |
| Malicious Slack message | Signature verification + replay window + identity mapping |
| Privilege escalation via approval button | Approval checked in RBAC, not inferred from Slack membership |
| Tampered audit log | Hash-chained entries, periodic external anchoring |
| Secret leaked into deploy logs | Redaction pass on all provider output before persistence |
| Malicious artifact | Signature verification (cosign/sigstore) before deploy |
| Insider deploying unreviewed code | Require artifact provenance linking to a merged commit |

### 12.2 Credentials

**Never store long-lived cloud credentials.** Use OIDC federation: the orchestrator presents its own identity to AWS/GCP and receives a short-lived token scoped to the specific environment.

```go
type CredentialProvider interface {
    // ForEnvironment returns credentials valid only for this environment,
    // expiring shortly after the deploy should complete.
    ForEnvironment(ctx context.Context, env string, ttl time.Duration) (Credentials, error)
}
```

Environment separation matters: the credential used to deploy to staging must not be able to touch prod. If one IAM role can do both, a staging-scoped compromise is a prod compromise.

### 12.3 Log redaction

Provider output goes into logs that end up in a database and on a dashboard. Anything the deploy target prints is a potential leak.

```go
type Redactor struct {
    literals []string          // known secret values, from the secret store
    patterns []*regexp.Regexp  // AKIA[0-9A-Z]{16}, xox[baprs]-..., -----BEGIN.*KEY, etc.
}

func (r *Redactor) Write(p []byte) (int, error) {
    out := p
    for _, lit := range r.literals {
        out = bytes.ReplaceAll(out, []byte(lit), []byte("[REDACTED]"))
    }
    for _, re := range r.patterns {
        out = re.ReplaceAll(out, []byte("[REDACTED]"))
    }
    n, err := r.inner.Write(out)
    if err != nil {
        return 0, err
    }
    _ = n
    return len(p), nil   // report the original length to the caller
}
```

Wrap every provider's output writer in this. Redact **before** persistence, not on display — once a secret is in the database it's leaked.

Note the return value: report `len(p)`, not the redacted length, or callers that check `n != len(p)` will see spurious short-write errors.

### 12.4 Hash-chained audit log

Each entry includes the hash of the previous one, so any deletion or modification breaks the chain.

```go
func (s *Store) AppendAudit(ctx context.Context, tx *sql.Tx, e AuditEntry) error {
    var prevHash string
    err := tx.QueryRowContext(ctx,
        `SELECT hash FROM audit_log ORDER BY id DESC LIMIT 1`).Scan(&prevHash)
    if errors.Is(err, sql.ErrNoRows) {
        prevHash = strings.Repeat("0", 64)   // genesis
    } else if err != nil {
        return err
    }

    payload := fmt.Sprintf("%s|%s|%s|%s|%s",
        prevHash, e.At.UTC().Format(time.RFC3339Nano), e.Actor, e.Action, e.Resource)
    sum := sha256.Sum256([]byte(payload))

    _, err = tx.ExecContext(ctx, `
        INSERT INTO audit_log (at, actor, action, resource, detail, prev_hash, hash)
        VALUES (?,?,?,?,?,?,?)`,
        e.At, e.Actor, e.Action, e.Resource, e.Detail, prevHash, hex.EncodeToString(sum[:]))
    return err
}
```

Add `orch audit verify` to walk the chain. Periodically write the head hash somewhere the orchestrator can't modify (an append-only S3 bucket, a Slack message, a log aggregator) so an attacker with database access can't rewrite history and recompute the whole chain.

### 12.5 RBAC

Separate permissions, not a single "deployer" role:

```yaml
roles:
  developer:
    - deploy:trigger    on: [dev, staging]
    - deploy:view       on: ["*"]
  release-manager:
    - deploy:trigger    on: ["*"]
    - deploy:approve    on: [prod]
    - deploy:rollback   on: ["*"]
  sre:
    - deploy:override   on: ["*"]     # bypass gates — always alerts
    - deploy:freeze     on: ["*"]
```

`deploy:override` should post to Slack every single time it's used, naming the person. Not as punishment — as the thing that makes the override safe to have.

---

## 13. Configuration schema

```yaml
# orch.yaml
version: 1

apps:
  web:
    artifact:
      type: ecr
      repository: 123456789.dkr.ecr.us-east-1.amazonaws.com/web
      require_signature: true          # cosign verification before deploy

    environments:
      dev:
        provider: ecs
        config:
          cluster: dev
          service: web
        strategy: { type: rolling, batch_size: 50% }
        verify:
          bake: 2m
          criteria:
            - name: http-ok
              verifier: http
              url: https://dev.example.com/healthz
              expect_status: 200

      staging:
        provider: ecs
        config: { cluster: staging, service: web }
        strategy: { type: blue-green }
        promote_from: dev                # artifact must have passed dev
        verify:
          bake: 10m
          sample_interval: 30s
          min_samples: 10
          failure_threshold: 3
          criteria:
            - name: error-rate
              verifier: prometheus
              query: |
                sum(rate(http_requests_total{job="web",env="staging",status=~"5.."}[2m]))
                / sum(rate(http_requests_total{job="web",env="staging"}[2m]))
              direction: lower-is-better
              max: 0.01
              max_ratio: 1.5
              floor_value: 0.001         # see section 7.2 — prevents false trips

      prod:
        provider: ecs
        config: { cluster: prod, service: web }
        strategy:
          type: canary
          steps:
            - { setWeight: 5 }
            - { verify: { bake: 5m } }
            - { setWeight: 25 }
            - { verify: { bake: 10m } }
            - { setWeight: 100 }
        promote_from: staging
        gates:
          - type: approval
            require_peer: true           # no self-approval
            roles: [release-manager, sre]
            ttl: 1h
          - type: schedule
            deny: ["Fri 16:00-23:59", "Sat", "Sun"]
            timezone: America/Los_Angeles
          - type: freeze_window
            source: calendar
        rollback:
          automatic: true
          check_migration_floor: true    # refuse unsafe rollbacks
          max_attempts: 2
        notify:
          slack:
            channel: "#deploys"
            on: [started, awaiting_approval, succeeded, failed, rolled_back]
            mention_on_failure: ["@oncall-web"]

environments:
  prod:
    require_peer_approval: true
    protected: true

notifiers:
  slack:
    mode: socket
    bot_token:  ${SLACK_BOT_TOKEN}
    app_token:  ${SLACK_APP_TOKEN}
```

Two schema decisions worth defending:

**`promote_from` enforces build-once-deploy-many.** A version can only reach prod if the identical artifact succeeded in staging. Rebuilding per environment means prod runs a binary that was never tested, and it's a shockingly common mistake.

**Validate the config exhaustively at load time.** Unknown keys are errors, not warnings — a typo'd `failure_threshhold` silently falling back to a default is exactly the bug that causes a spurious 3am rollback. Ship `orch config validate` and run it in CI on the config repo.

---

## 14. Tech stack and setup

### 14.1 Why Go

| Option | Verdict |
|---|---|
| **Go** | **Recommended.** Single static binary (the CLI must be trivially distributable), `embed.FS` for the dashboard, excellent concurrency for the executor, and the entire ecosystem you'll integrate with — Docker, Kubernetes, Terraform, AWS SDK, slack-go — is Go. Cross-compiles to every platform from one machine. |
| Rust | Better type safety for the state machine, but a thinner ops-integration ecosystem and slower iteration on a project that's mostly glue |
| TypeScript/Node | One language for CLI + dashboard, but `node_modules` for a CLI that must run on locked-down CI runners is a real drawback |
| Python | Great for scripting, poor for a distributable single-binary CLI |

### 14.2 Dependencies

Keep it small. Every dependency in a tool that holds prod credentials is supply-chain surface.

```
CLI           spf13/cobra
HTTP          net/http + go-chi/chi
Database      modernc.org/sqlite   (pure Go — no cgo, static binary)
              jackc/pgx/v5         (optional, multi-node)
Migrations    pressly/goose
Slack         slack-go/slack       (Socket Mode support)
Config        goccy/go-yaml + a strict decoder
Logging       log/slog             (stdlib)
Metrics       prometheus/client_golang
IDs           oklog/ulid
Testing       testify, testcontainers-go
Cloud         aws-sdk-go-v2 (only the services you use)
```

**`modernc.org/sqlite` over `mattn/go-sqlite3`** — the pure-Go implementation means `CGO_ENABLED=0` and a genuinely static binary. Worth a small performance cost for a tool whose whole pitch is "copy one file."

### 14.3 Setup

```bash
# Toolchain
go install github.com/spf13/cobra-cli@latest
go install github.com/pressly/goose/v3/cmd/goose@latest
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
go install gotest.tools/gotestsum@latest

# Local dev targets
docker compose up -d        # localstack (ECS/Lambda), prometheus, a demo app
k3d cluster create orch-dev # only if you build the k8s provider

# Slack app, for local development
#   api.slack.com/apps → Create New App → From scratch
#   Socket Mode: enable → generate an app-level token (xapp-) with connections:write
#   OAuth scopes (bot): chat:write, commands, users:read, users:read.email
#   Interactivity: on (Socket Mode needs no Request URL)
#   Install to workspace → copy the bot token (xoxb-)
export SLACK_APP_TOKEN=xapp-...
export SLACK_BOT_TOKEN=xoxb-...
```

Socket Mode means **no ngrok, no public URL, no TLS cert** during development. This alone saves days of setup friction versus HTTP endpoints.

### 14.4 Build

```makefile
VERSION := $(shell git describe --tags --always --dirty)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/orch  ./cmd/orch
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/orchd ./cmd/orchd

.PHONY: test
test:
	gotestsum -- -race -count=1 ./...

.PHONY: test-integration
test-integration:
	gotestsum -- -race -count=1 -tags=integration ./...

.PHONY: release
release:
	goreleaser release --clean
```

`-race` on every test run, not just occasionally. This codebase is concurrent by nature and race conditions in a deployment tool surface as data corruption in production.

---

## 15. Repository layout

```
DevOps-Pipeline-Orchestrator/
├── README.md
├── docs/
│   ├── design.md                ← this document
│   ├── security.md
│   ├── migrations.md            ← the expand/contract guide for users
│   └── providers/
├── cmd/
│   ├── orch/                    ← CLI
│   │   └── main.go
│   └── orchd/                   ← server
│       └── main.go
├── internal/
│   ├── api/                     ← HTTP handlers, authn, authz middleware
│   ├── auth/
│   │   ├── rbac.go
│   │   ├── token.go
│   │   └── oidc.go
│   ├── config/
│   │   ├── schema.go
│   │   ├── load.go
│   │   └── validate.go          ← strict decode, unknown keys are errors
│   ├── store/
│   │   ├── store.go
│   │   ├── lease.go
│   │   ├── outbox.go
│   │   ├── audit.go
│   │   └── migrations/
│   ├── engine/
│   │   ├── executor.go          ← the state machine driver
│   │   ├── plan.go
│   │   ├── verify.go            ← the decision engine, section 7.3
│   │   ├── rollback.go          ← including the migration floor check
│   │   ├── strategy/
│   │   │   ├── recreate.go
│   │   │   ├── rolling.go
│   │   │   ├── bluegreen.go
│   │   │   └── canary.go
│   │   └── reconcile.go         ← startup recovery of orphaned deploys
│   ├── provider/
│   │   ├── provider.go          ← the interface
│   │   ├── ssh/
│   │   ├── ecs/
│   │   ├── lambda/
│   │   ├── s3static/
│   │   ├── exec/
│   │   └── fake/                ← the most-used one, in tests
│   ├── verifier/
│   │   ├── http/
│   │   ├── prometheus/
│   │   ├── cloudwatch/
│   │   └── fake/
│   ├── notify/
│   │   ├── slack/
│   │   │   ├── bot.go
│   │   │   ├── blocks.go
│   │   │   ├── verify.go        ← signature verification
│   │   │   └── interactions.go
│   │   └── webhook/
│   ├── redact/
│   └── dashboard/
│       ├── server.go
│       ├── sse.go
│       ├── templates/           ← embed.FS
│       └── static/
├── test/
│   ├── integration/
│   ├── chaos/                   ← crash-injection suite, section 18.3
│   └── fixtures/
└── examples/
    ├── orch.yaml
    └── docker-compose.yaml
```

---

## 16. Milestone ladder

Deliberately front-loaded with correctness. The first four milestones produce something that looks less impressive than a weekend hack but is the part that makes the project real.

---

### M0 — CLI skeleton and config
**Est. 4–5 days**

Cobra CLI, strict YAML config loading, `orch config validate`, `orch version`. No deployment yet.

**You are learning:** Cobra patterns, strict decoding (unknown keys → error), and how to produce error messages that name the file, line, and fix.

**Done when:** a malformed config produces a message a stranger could act on.

---

### M1 — Provider interface and the fake provider
**Est. 1 week**

Define `Provider`. Implement `fake` (in-memory, configurable to fail, hang, or partially succeed) and `exec` (shells out to a script). Deploy end-to-end with no state persistence.

**Why the fake first:** it's the provider you'll use in 90% of your tests, and writing it first forces the interface to be testable.

**Done when:** `orch deploy demo dev` runs a script and reports success or failure.

---

### M2 — Persistent state machine ★ **the foundation**
**Est. 2 weeks**

SQLite store, migrations, the full state machine, the audit log with hash chaining, and `reconcile.go` — startup recovery of deployments left in non-terminal states.

**Test by killing the process mid-deploy.** Literally `kill -9` during `DEPLOYING` and verify the next start recovers correctly. Do this before moving on.

**Done when:** no sequence of crashes leaves the system unable to deploy, and `orch audit verify` passes.

---

### M3 — Leases, fencing, and idempotency
**Est. 1 week**

Lease acquisition with TTL, heartbeat renewal, fencing tokens, idempotency keys, and the abort-on-lost-lease path.

**Done when:** a test spawns 20 concurrent deploys of the same app+env and exactly one runs; and a paused-then-resumed executor with a stale fence cannot mutate state.

---

### M4 — A real provider
**Est. 1.5–2 weeks**

Pick one: SSH-to-VMs (most broadly useful, hardest to make idempotent) or ECS (cleanest API, best rollback semantics). Do one properly rather than three badly.

**Done when:** you deploy a real application to real infrastructure and it works twice in a row from a clean state.

---

### M5 — Health checks and verification
**Est. 2 weeks**

HTTP verifier, Prometheus verifier, the criteria model with baseline comparison and `floor_value`, and the decision engine with consecutive-failure thresholds and inconclusive handling.

**Done when:** a deliberately broken deploy is detected, a deliberate single-sample blip is *not*, and killing Prometheus produces `Inconclusive` rather than a rollback.

That middle case is the one worth writing a test for.

---

### M6 — Rollback and the migration floor
**Est. 1.5 weeks**

Automatic rollback on `VerdictUnhealthy`, the artifact manifest with `rollback_floor`, the safety check, the circuit breaker, and `ROLLBACK_FAILED` escalation.

**Done when:** an unsafe rollback is refused with an explanation naming the offending migration, and a rollback that fails verification lands in `ROLLBACK_FAILED` without looping.

---

### M7 — Slack notifications (one-way)
**Est. 1 week**

Socket Mode connection, Block Kit messages, `chat.update` for progress, the outbox worker with backoff, and update coalescing for rate limits.

**Done when:** killing the process mid-deploy and restarting still delivers the notification exactly once, and a canary with 30s sampling doesn't get rate-limited.

---

### M8 — Slack approvals (two-way) ★ **the security milestone**
**Est. 1.5 weeks**

Interactive buttons, signature verification (even under Socket Mode, for the HTTP fallback), Slack→internal identity mapping, RBAC checks, peer-approval enforcement, approval TTL.

**Write the abuse tests first:** unlinked Slack user, user without the role, self-approval, expired request, replayed request. All five must be rejected and audited.

**Done when:** those five tests pass and every rejection appears in the audit log.

---

### M9 — Dashboard
**Est. 2 weeks**

Embedded templates, overview grid, deployment detail with SSE log streaming and `Last-Event-ID` replay, history, audit view.

**Done when:** you can watch a deploy progress live, reconnect mid-stream without losing output, and answer "what's in prod right now" in one page load.

---

### M10 — Multi-environment promotion
**Est. 1 week**

`promote_from`, artifact provenance tracking, `orch promote web staging→prod`, schedule and freeze-window gates.

**Done when:** an artifact that hasn't passed staging cannot reach prod, and a Friday-evening prod deploy is blocked with a clear message.

---

### M11 — Advanced strategies
**Est. 2 weeks**

Blue-green first (best rollback semantics, atomic flip), then canary with weighted traffic shifting and per-step verification.

**Verify per-version metric tagging before building canary.** If you can't distinguish canary traffic from baseline traffic in your metrics, canary analysis is theater.

---

### M12 — Hardening
**Est. 2 weeks**

OIDC credential federation, log redaction, break-glass path, Prometheus metrics for the orchestrator itself, `orch doctor`, docs.

---

## 17. Reference implementations

### 17.1 The executor loop

```go
func (e *Executor) Run(ctx context.Context, depID string) error {
    dep, err := e.store.Deployment(ctx, depID)
    if err != nil {
        return err
    }

    fence, err := e.store.AcquireLease(ctx, dep.Resource(), dep.ID, e.nodeID)
    if err != nil {
        return err
    }
    dep.Fence = fence

    // Renew in the background; cancel the work if we lose the lease.
    ctx, cancel := context.WithCancel(ctx)
    defer cancel()
    go e.heartbeat(ctx, dep, cancel)
    defer e.store.ReleaseLease(context.WithoutCancel(ctx), dep.Resource(), dep.ID)

    steps := []struct {
        state State
        fn    func(context.Context, *Deployment) error
    }{
        {StatePreflight, e.preflight},
        {StateAwaitApproval, e.awaitGates},
        {StateDeploying, e.deploy},
        {StateVerifying, e.verify},
        {StatePromoting, e.promote},
    }

    for _, s := range steps {
        // Persist the transition BEFORE performing the side effect.
        if err := e.transitionAndNotify(ctx, dep, s.state, eventFor(dep, s.state)); err != nil {
            return err
        }
        if err := s.fn(ctx, dep); err != nil {
            return e.handleFailure(context.WithoutCancel(ctx), dep, err)
        }
    }

    return e.transitionAndNotify(ctx, dep, StateSucceeded, eventSucceeded(dep))
}

func (e *Executor) handleFailure(ctx context.Context, dep *Deployment, cause error) error {
    // A failed rollback never triggers another rollback.
    if dep.IsRollback {
        _ = e.transitionAndNotify(ctx, dep, StateRollbackFailed, eventPage(dep, cause))
        return cause
    }

    policy := e.cfg.RollbackPolicy(dep.App, dep.Environment)
    if !policy.Automatic {
        return e.transitionAndNotify(ctx, dep, StateFailed, eventFailed(dep, cause))
    }

    plan, err := e.PlanRollback(ctx, dep)
    if err != nil {
        // Unsafe rollback → do NOT attempt it. Escalate.
        return e.transitionAndNotify(ctx, dep, StateRollbackFailed,
            eventUnsafeRollback(dep, cause, err))
    }
    return e.executeRollback(ctx, dep, plan)
}
```

The `context.WithoutCancel` calls matter: when the deploy context is cancelled (timeout, lost lease), you still need to record the failure and release the lease. Using the cancelled context there means your failure handling silently does nothing.

### 17.2 Startup reconciliation

```go
// Reconcile recovers deployments left in non-terminal states by a crash.
// It never assumes the database is right — it asks the provider what happened.
func (e *Executor) Reconcile(ctx context.Context) error {
    orphans, err := e.store.NonTerminalDeployments(ctx)
    if err != nil {
        return err
    }

    for _, dep := range orphans {
        held, err := e.store.LeaseHeld(ctx, dep.Resource())
        if err != nil {
            return err
        }
        if held {
            continue   // another node owns it and is alive
        }

        slog.Warn("recovering orphaned deployment",
            "id", dep.ID, "state", dep.State, "app", dep.App, "env", dep.Environment)

        prov := e.providers[dep.Provider]
        actual, err := prov.Current(ctx, dep.Target())
        if err != nil {
            // Can't tell what's real. Don't guess about production.
            _ = e.markUnknown(ctx, dep, err)
            continue
        }

        switch {
        case actual.Version == dep.Version:
            // The deploy landed; we crashed before recording it.
            // Resume at verification rather than assuming success.
            _ = e.resumeAt(ctx, dep, StateVerifying)

        case actual.Version == dep.PreviousVersion:
            // Never took effect. Safe to fail cleanly.
            _ = e.transitionAndNotify(ctx, dep, StateFailed,
                eventFailed(dep, errors.New("interrupted before deploy took effect")))

        default:
            // Something else is deployed. Do not touch it.
            _ = e.markUnknown(ctx, dep,
                fmt.Errorf("unexpected version %s in %s", actual.Version, dep.Environment))
        }
    }
    return nil
}
```

The `default` branch is the important one. When reality doesn't match either expected state, the correct action is to stop and tell a human. A tool that "helpfully" reconciles toward what it thinks should be true is how automation causes outages.

### 17.3 CLI surface

```
orch deploy <app> <env> [--version=X] [--strategy=Y] [--wait] [--dry-run]
orch rollback <app> <env> [--to=VERSION] [--force]
orch promote <app> <from-env> <to-env>
orch status [<app>] [<env>]
orch history <app> <env> [--limit=N]
orch logs <deployment-id> [--follow]
orch abort <deployment-id>
orch approve <deployment-id>
orch freeze <app> <env> --reason="..." [--until=...]
orch unfreeze <app> <env>
orch config validate [--file=orch.yaml]
orch audit verify
orch doctor
```

Details that make a CLI feel finished:

- **`--wait` streams live progress and exits non-zero on failure.** This is what CI will use. Without it, a CI job reports success the moment the deploy is *queued*.
- **`--dry-run` prints the plan without executing.** Should be safe enough that people run it reflexively.
- **`orch status` is the most-used command.** It should be fast and readable at a glance.
- **`orch doctor`** checks connectivity to every provider, verifier, and notifier, plus config validity and clock skew. First thing you ask someone to run when they file a bug.
- Respect `NO_COLOR`, detect non-TTY and drop progress animations, support `--output=json` on everything.

---

## 18. Testing strategy

A deployment tool that isn't tested against failure is a deployment tool that hasn't been tested.

### 18.1 The fake provider is your most important test fixture

```go
type FakeProvider struct {
    mu       sync.Mutex
    versions map[string]string

    // Failure injection
    FailOn        map[string]error       // step name → error
    HangOn        map[string]time.Duration
    PartialDeploy bool                   // succeed on some instances, fail on others
    DeployCount   map[string]int         // for asserting idempotency
}
```

Being able to say "fail on the third instance of a rolling deploy, then hang for 90 seconds on abort" in a unit test is worth more than any number of integration tests against real cloud APIs.

### 18.2 State machine property tests

```go
func TestStateMachineInvariants(t *testing.T) {
    rapid.Check(t, func(t *rapid.T) {
        sm := NewStateMachine()
        events := rapid.SliceOf(genEvent()).Draw(t, "events")

        for _, ev := range events {
            _ = sm.Apply(ev)

            // Invariant 1: terminal states are absorbing.
            if sm.Previous.IsTerminal() {
                require.Equal(t, sm.Previous, sm.Current)
            }
            // Invariant 2: a rollback never enters ROLLING_BACK again.
            if sm.IsRollback {
                require.NotEqual(t, StateRollingBack, sm.Current)
            }
            // Invariant 3: nothing runs without a lease.
            if sm.Current.RequiresLease() {
                require.True(t, sm.HoldsLease)
            }
        }
    })
}
```

### 18.3 Chaos tests ★ **the ones that find real bugs**

```go
func TestCrashDuringEveryState(t *testing.T) {
    for _, killAt := range []State{
        StatePreflight, StateDeploying, StateVerifying,
        StatePromoting, StateRollingBack,
    } {
        t.Run(string(killAt), func(t *testing.T) {
            env := newTestEnv(t)
            env.KillProcessWhenStateReached(killAt)

            env.Deploy("web", "staging", "v2")
            env.WaitForProcessDeath()

            env2 := env.Restart()               // same DB, fresh process
            require.NoError(t, env2.Reconcile())

            dep := env2.LatestDeployment("web", "staging")
            require.True(t, dep.State.IsTerminal() || dep.State == StateVerifying,
                "left in unrecoverable state %s", dep.State)

            // The critical assertion: the system still works afterward.
            require.NoError(t, env2.Deploy("web", "staging", "v3"))
        })
    }
}
```

That last assertion is the one that matters. Recovering the *record* is easy; the real requirement is that a crash never permanently wedges the system by leaking a lease or leaving a row that blocks all future deploys.

Also worth a test: **concurrent deploys.** Fire 20 goroutines at the same app+env and assert exactly one ran, the other 19 got a clean `LeaseHeldError`, and the provider's `DeployCount` is exactly 1.

### 18.4 Integration tests

Use `testcontainers-go` for Postgres, Prometheus, and LocalStack. Tag them `//go:build integration` so the fast unit suite stays fast. Run both in CI, unit tests on every push and integration nightly plus on `main`.

### 18.5 Slack testing

Mock the Slack API at the HTTP layer. Test specifically:

- Valid signature accepted
- Invalid signature rejected
- Timestamp older than 5 minutes rejected (replay)
- Body modified after signing rejected
- Unknown Slack user rejected
- Known user without the role rejected and audited
- Self-approval rejected when `require_peer` is set
- Expired approval rejected
- Rate-limit response (`429` + `Retry-After`) handled with backoff, not dropped

---

## 19. Operational concerns

### 19.1 Observe the orchestrator itself

```go
var (
    deploymentsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
        Name: "orch_deployments_total",
    }, []string{"app", "environment", "result"})

    deploymentDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name:    "orch_deployment_duration_seconds",
        Buckets: prometheus.ExponentialBuckets(10, 2, 10),
    }, []string{"app", "environment", "strategy"})

    leaseWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
        Name: "orch_lease_wait_seconds",
    }, []string{"resource"})

    outboxDepth = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "orch_outbox_pending",
    })

    orphansRecovered = promauto.NewCounter(prometheus.CounterOpts{
        Name: "orch_orphans_recovered_total",
    })
)
```

`orch_outbox_pending` growing means Slack delivery is broken and people are not being told about deploys. Alert on it.

You now have the data for **DORA metrics** essentially for free: deployment frequency, lead time (commit → prod), change failure rate (rollbacks ÷ deploys), and MTTR (failure → recovery). Put them on the dashboard. It's a small amount of work for something teams genuinely want.

### 19.2 Backup

The SQLite file is the entire system of record. Use `VACUUM INTO` for a consistent snapshot without stopping writes, or Litestream for continuous replication to S3. Test the restore path — an untested backup is a hope.

### 19.3 Break-glass

**If the orchestrator is down, can you still deploy?** If the answer is no, you've created a single point of failure for incident response, which is exactly when you most need to ship a fix.

Ship a documented manual path:

```bash
orch export-plan web prod --version=X > plan.json
orch-exec plan.json      # standalone binary, no server, no DB, no lease
```

`orch-exec` runs the provider steps directly and writes a signed record to be reconciled when the server returns. It skips gates by design. Every use should alert loudly.

Document this in `docs/break-glass.md` and **test it quarterly**. A break-glass procedure nobody has ever run is not a procedure.

### 19.4 Clock skew

Leases, approval TTLs, replay windows, and schedule gates all depend on time. In a multi-node deployment, clock skew breaks all of them, and the failure mode is subtle — two nodes both believing they hold a lease.

Have `orch doctor` check skew against the database server's clock and refuse to start if it exceeds a few seconds.

---

## 20. Stretch goals

| Feature | Effort | Value |
|---|---|---|
| **DORA metrics dashboard** | Small | You already have the data. High perceived value. |
| **Deployment windows from a calendar** | Small | Reads freeze windows from Google Calendar / PagerDuty |
| **Terraform provider** | Medium | Declare apps and environments as code |
| **GitHub Deployments API integration** | Small | Status appears on the PR that triggered it. Very satisfying. |
| **Automated canary analysis (statistical)** | Large | Mann-Whitney U test on metric distributions instead of threshold comparison. Real Spinnaker-style ACA. |
| **Feature-flag coordination** | Medium | Tie flag state to deployment state via OpenFeature |
| **Multi-region orchestration** | Large | Region-by-region rollout with per-region verification |
| **Cost attribution** | Medium | Tag deployments with cost impact |
| **Incident linking** | Small | Correlate deploys with PagerDuty incidents; "deploys in the hour before this incident" |
| **Kubernetes provider** | Medium | Completes the story, but be clear it's for heterogeneous fleets, not competing with Argo |
| **Artifact provenance / SLSA** | Medium | Verify the artifact came from a signed build of a merged commit |

---

## 21. References

### Books and papers

- Humble & Farley, *Continuous Delivery* — the canonical text; the deployment pipeline model comes from here
- Forsgren, Humble & Kim, *Accelerate* — DORA metrics and why they matter
- *Site Reliability Engineering* (Google), chapters on release engineering and error budgets
- Kleppmann, *Designing Data-Intensive Applications*, ch. 8–9 — the fencing-token argument in section 6.2 is from here
- Martin Kleppmann, "How to do distributed locking" — read before implementing leases

### Specifications and docs

| Document | For |
|---|---|
| Slack: verifying requests | Signature scheme, replay window |
| Slack: Socket Mode | Connection lifecycle, ack deadlines |
| Slack: Block Kit Builder | Interactive message design |
| Slack: rate limits | Per-method tiers, `Retry-After` handling |
| OpenFeature spec | If you build flag coordination |
| SLSA framework | Artifact provenance levels |
| Sigstore / cosign | Artifact signature verification |

### Code worth reading

- **Argo Rollouts** — read `AnalysisRun` and the metric-provider interfaces. This is the reference implementation of section 7, and reading it will improve your design.
- **Kargo** — promotion pipelines across stages; directly relevant to your `promote_from` model
- **Flagger** — progressive delivery operator; smaller and more readable than Argo Rollouts
- **Terraform's state locking** (DynamoDB backend) — a well-tested real-world lease implementation
- **Atlantis** — a Go tool that orchestrates a risky operation from a chat/PR interface; similar shape to yours
- **Temporal** — if your state machine grows beyond what hand-rolled code handles well, this is the durable-execution engine you'd reach for. Reading its model is educational even if you don't adopt it.

---

## Appendix A — Decision record

| Decision | Rationale |
|---|---|
| Target non-Kubernetes environments | Argo CD + Argo Rollouts + Kargo already own K8s multi-env promotion with metric-driven rollback. VMs, ECS, Lambda, and static sites are an underserved gap. |
| Central server, thin CLI (Option B) | Long-running deploys need state that survives CLI disconnection; leases and idempotency need transactions. Git is the wrong store for runtime state. |
| Single static binary, SQLite, embedded dashboard | Operational simplicity is the differentiator against Argo. One file, one systemd unit. |
| Go | Static binary, `embed.FS`, and the entire integration ecosystem is Go |
| `modernc.org/sqlite` over cgo SQLite | `CGO_ENABLED=0` → genuinely static binary. Worth the small perf cost. |
| Persist state transition *before* the side effect | The only ordering that makes crash recovery possible |
| Lease with TTL + fencing token, not a plain lock | A plain lock outlives a crashed holder; a lease without fencing doesn't stop a resumed zombie |
| Transactional outbox for notifications | Prevents lost and duplicated Slack messages across crashes without holding locks during network calls |
| Verifiers observe, the engine decides | One testable rollback decision instead of logic scattered across providers |
| Baseline-relative criteria with a `floor_value` | Absolute thresholds fire constantly on noisy services and never on quiet ones; the floor prevents 0.001%→0.004% tripping a ratio check |
| Verifier error → `Inconclusive`, not rollback | A monitoring outage must not become a deployment outage |
| Consecutive-failure thresholds | One bad sample is noise; three in a row is signal |
| Rollbacks are never auto-rolled-back | Prevents an infinite loop thrashing production |
| Rollback checks a migration floor and *refuses* when unsafe | Rollback is not the inverse of deploy. An irreversible migration makes redeploying old code an outage. Refusing, not warning, is the only safe behavior. |
| Roll-forward is a first-class path | Because rollback is often correctly refused |
| Socket Mode as the Slack default | No public endpoint, no inbound firewall rule, no TLS cert, no ngrok in dev |
| Slack approvals checked against real RBAC | Channel membership is not authorization; a button click is not consent |
| Approvals expire | A button posted at 2pm should not be actionable at 11pm |
| Hash-chained audit log | Tamper evidence for the highest-value system in the infrastructure |
| Log redaction before persistence | Once a secret reaches the database it is leaked, regardless of display filtering |
| `promote_from` enforces build-once-deploy-many | Rebuilding per environment means prod runs a binary that was never tested |
| Strict config decoding | A typo'd `failure_threshhold` silently defaulting is exactly the bug that causes a 3am spurious rollback |
| Documented, tested break-glass path | The orchestrator being down must not block incident response |

---

## Appendix B — Quick reference card

```
States:   PENDING → PREFLIGHT → [AWAIT_APPROVAL] → DEPLOYING
                  → VERIFYING → PROMOTING → SUCCEEDED
                              ↘ ROLLING_BACK → ROLLED_BACK
                                             ↘ ROLLBACK_FAILED  ← pages
Terminal: SUCCEEDED · ROLLED_BACK · ROLLBACK_FAILED · ABORTED

Invariants
  persist transition BEFORE the side effect
  a rollback is never auto-rolled-back
  a verifier error is Inconclusive, not Unhealthy
  reconcile reads truth from the provider, never the DB
  when reality matches neither expected state → stop, page a human

Lease:    TTL 60s · heartbeat 15s · monotonic fence token
          lost lease → executor aborts immediately

Verify:   bake window · sample interval · MinSamples
          FailureThreshold = N consecutive (not cumulative)
          criterion: max (absolute) + max_ratio (vs baseline)
                     + floor_value (ignore below this)

Rollback: check rollback_floor from the artifact manifest
          irreversible migration → REFUSE, don't warn
          2 rollbacks in 24h → circuit breaker

Slack:    ack interactive callbacks in < 3 seconds
          signature: HMAC-SHA256 over "v0:{ts}:{raw body}"
                     compare with hmac.Equal, reject ts older than 300s
          chat.postMessage ≈ 1/sec per channel → coalesce updates
          Socket Mode: xapp- (app token) + xoxb- (bot token)

SSE:      Last-Event-ID replay · 20s keepalive · X-Accel-Buffering: no

Build:    CGO_ENABLED=0 go build -ldflags "-s -w"
          go test -race -count=1 ./...
```
