-- The schema from design.md section 5, verbatim where the document gives it
-- and extended only where this implementation needed a column the document's
-- prose requires but its DDL omits (noted inline).
--
-- This file is not executed by the current build: with no module proxy there
-- is no SQL driver, so internal/store implements the same contract over an
-- append-only journal. See docs/notes-on-the-spec.md. It is kept, and kept
-- correct, because it is the migration that runs on the day a driver is
-- available, and because the schema is the clearest statement of what the
-- store holds.
--
-- Everything here targets SQLite via modernc.org/sqlite (CGO_ENABLED=0). The
-- Postgres variant differs only in the timestamp type and in using BIGSERIAL
-- where SQLite uses INTEGER PRIMARY KEY.

-- +goose Up

PRAGMA journal_mode = WAL;      -- crash safety is the whole point of the file
PRAGMA synchronous = FULL;      -- an fsync per commit; see section 5's ordering
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

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
    provider         TEXT NOT NULL,             -- (added) which provider owns this target
    provider_handle  TEXT,                      -- opaque; for reconciliation
    is_rollback      INTEGER NOT NULL DEFAULT 0,
    rollback_of      TEXT REFERENCES deployments(id),
    slack_ts         TEXT,                      -- (added) for chat.update, section 6.4
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    finished_at      TIMESTAMP,
    error            TEXT
);

-- The partial index the document specifies: "is anything running on this
-- target" is the hottest query in the system, asked before every deploy.
CREATE INDEX idx_dep_active ON deployments(app, environment, state)
    WHERE state NOT IN ('SUCCEEDED','ROLLED_BACK','ROLLBACK_FAILED','ABORTED','FAILED','UNKNOWN');

-- (added) The rollback-target lookup and the history page both scan by target
-- in reverse id order.
CREATE INDEX idx_dep_history ON deployments(app, environment, id DESC);

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
    criterion     TEXT NOT NULL,                 -- (added) which criterion produced it
    at            TIMESTAMP NOT NULL,
    value         REAL NOT NULL,
    baseline      REAL,
    healthy       INTEGER NOT NULL,
    note          TEXT                           -- (added) why it was judged that way
);

CREATE INDEX idx_samples_dep ON health_samples(deployment_id, at);

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

-- The audit log is append-only by policy; these triggers make it append-only
-- in the database, so a bug (or a hand-typed UPDATE during an incident)
-- cannot quietly break the chain. An attacker with file access can still drop
-- the triggers -- which is why the chain and the external anchor exist.
-- +goose StatementBegin
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
-- +goose StatementEnd

CREATE TABLE outbox (
    id           INTEGER PRIMARY KEY,
    created_at   TIMESTAMP NOT NULL,
    notifier     TEXT NOT NULL,
    payload      TEXT NOT NULL,
    dedupe_key   TEXT,                            -- (added) idempotent consumers, 6.4
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt TIMESTAMP NOT NULL,
    delivered_at TIMESTAMP,
    last_error   TEXT                             -- (added) visible failure, 9.5
);

-- The drain query: undelivered and due. Partial so it stays small as
-- delivered rows accumulate.
CREATE INDEX idx_outbox_due ON outbox(next_attempt)
    WHERE delivered_at IS NULL;

CREATE TABLE approvals (
    deployment_id TEXT NOT NULL REFERENCES deployments(id),
    user_id       TEXT NOT NULL,
    at            TIMESTAMP NOT NULL,
    source        TEXT NOT NULL,                  -- slack | cli | dashboard
    -- One person clicking Approve twice is one approval, not two. Without
    -- this, a require_peer check that counts approvals is satisfied by one
    -- person double-clicking.
    PRIMARY KEY (deployment_id, user_id)
);

CREATE TABLE freezes (
    resource   TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    by_user    TEXT NOT NULL,
    at         TIMESTAMP NOT NULL,
    until      TIMESTAMP,
    lifted     INTEGER NOT NULL DEFAULT 0,
    lifted_by  TEXT
);

-- +goose Down

DROP TABLE IF EXISTS freezes;
DROP TABLE IF EXISTS approvals;
DROP INDEX IF EXISTS idx_outbox_due;
DROP TABLE IF EXISTS outbox;
DROP TRIGGER IF EXISTS audit_log_no_delete;
DROP TRIGGER IF EXISTS audit_log_no_update;
DROP TABLE IF EXISTS audit_log;
DROP INDEX IF EXISTS idx_samples_dep;
DROP TABLE IF EXISTS health_samples;
DROP TABLE IF EXISTS leases;
DROP TABLE IF EXISTS deployment_steps;
DROP INDEX IF EXISTS idx_dep_history;
DROP INDEX IF EXISTS idx_dep_active;
DROP TABLE IF EXISTS deployments;
