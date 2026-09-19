# Notes on the spec

Where this implementation departs from `Documentation/README.md`, and why. Each
entry is either a deliberate choice or a place where the document's own
reference material would have produced a bug.

## Deliberate choices

**Standard library only.** The document picks chi, slack-go, modernc SQLite and
friends. This build has zero third-party modules. Section 14.2 argues that a
tool holding production credentials pays for every dependency in supply-chain
surface; Go 1.22's `ServeMux` covers routing, and the Slack calls used are three
POSTs. The build environment also had no module proxy, which settled it.

**An append-only journal instead of SQLite.** Without a driver, state lives in a
CRC-checked, group-committed, fsynced journal replayed into memory at startup.
The schema the document specifies is kept verbatim in
`internal/store/migrations/0001_init.sql` (with the additions listed there), so a
move to SQLite is a storage swap, not a redesign. A torn trailing write is
truncated; a bad checksum inside a complete record is reported as corruption
rather than silently dropped.

**A YAML subset.** Anchors, aliases, merge keys, multiple documents and tabs are
rejected *by name*. A deployment config is the wrong place for YAML's clever
features, and every rejection says what to write instead.

**Tokens live beside the journal, not in it.** `tokens.json` holds SHA-256
hashes only, written atomically with mode 0600, so a backup of deployment
history is not a backup of credentials.

## Bugs the document's reference code would have shipped

1. **A capability probe with a side effect.** Asking a provider "can you shift
   traffic?" by calling `Shift(100)` and checking for `ErrUnsupported` sends
   100 % of traffic to an undeployed version. Providers now expose a pure
   `Capabilities()` method.
2. **The audit hash didn't cover `Detail`.** Someone with file access could
   rewrite which version was deployed without breaking the chain. The hash now
   covers every field.
3. **`ROLLBACK_FAILED` was only reachable from `ROLLING_BACK`.** A rollback
   deployment that fails while `DEPLOYING` had no legal terminal state. Edges
   were added from PREFLIGHT/DEPLOYING/VERIFYING/PROMOTING; `handleFailure` uses
   them only for rollback deployments.
4. **`semverLess` was unspecified.** Lexicographic comparison gets `v9` vs `v10`
   wrong, which decides whether a 3 am rollback is allowed. `versionLess`
   compares component-wise with numeric awareness and returns false when two
   versions can't be compared, so it never blocks on an unparseable pair.
5. **A rolled-back deploy returned success.** `orch deploy --wait` would exit 0
   and CI would promote the bad artifact onward. It now returns an error.
6. **Failure reasons weren't persisted.** The dashboard showed `FAILED` with no
   reason. They are now written before the terminal transition.
7. **`notify.on: [started]` matched nothing.** Config event names and state
   names are different vocabularies; `engine.EventType` is now the one place
   they meet.
8. **Reconciliation's `default` case.** When the provider reports neither the old
   nor the new version, the deployment goes to `UNKNOWN`, which pages, rather
   than being "helpfully" reconciled toward either.

## Bugs found while building this one

- **SSE `send on closed channel`.** `Publish` copied the subscriber list, released
  the lock, then sent — a browser closing its tab in that window panicked the
  server. Sends now happen under the lock (they're non-blocking), and
  unsubscribe is idempotent.
- **Every event was addressed to `slack` only.** With Slack unconfigured, the
  dashboard's live stream received nothing. Events now fan out to each
  configured notifier with a per-notifier dedupe key.
- **`orch deploy --wait` could spin forever silently** on an unexpected response
  shape. It now fails loudly on a reply with no state, is bounded to an hour,
  and Ctrl-C stops watching without touching the deployment.
- **An empty `--manifests` refused every rollback.** No manifest directory now
  means no manifest source, which takes the documented warn-and-proceed path.
- **Labelled metrics with no observations emitted an unlabelled `0`**, giving one
  metric name two label shapes. Only unlabelled metrics get the zero now.
- **No deployment metrics were recorded at all.** The server now records
  outcome, duration and rollbacks — including manual ones, which are change
  failures too.

## Additions not in the document

- `UNKNOWN` pages alongside `ROLLBACK_FAILED`.
- `orch rollback` and `orch promote` resolve their version **server-side**, so a
  stale CLI can't skip the migration-floor check or promote a rebuild instead of
  the tested artifact.
- The audit chain is verified at every startup; `orchd` refuses to start if it
  doesn't verify.
