# Migrations and safe rollback

Rollback is not the inverse of deploy. Putting old code back is easy; putting an
old *schema* back usually isn't. `orch` handles this by refusing rollbacks it
can't prove are safe, and it can only prove that if your builds tell it about
their migrations.

## Expand / contract

Split every breaking schema change across releases so each release runs against
both the old and new schema:

| Release | Schema change | Safe to roll back to the previous release? |
|---|---|---|
| N   | **Expand**: add the new column, nullable; write both | Yes |
| N+1 | Backfill; read the new column | Yes |
| N+2 | **Contract**: drop the old column | **No** — N+1 reads a column that no longer exists |

Release N+2's `rollback_floor` is therefore N+2 itself: nothing older is safe.

## Release manifests

Each build writes `<manifests>/<app>/<version>.json` and `orchd serve
--manifests <dir>` reads it:

```json
{
  "version": "v2.0.0",
  "schema_version": 5,
  "rollback_floor": "v2.0.0",
  "migrations": [
    { "id": "0008_drop_legacy_email", "reversible": false, "expand_phase": false }
  ]
}
```

A rollback is refused when:

- the target is older than the current version's `rollback_floor`;
- the schema version differs and the current release ran an irreversible
  migration;
- the current version's manifest can't be read. *Not knowing is not permission.*

The refusal says what it would have done, why it won't, and what to do instead —
almost always "roll forward with a fix".

Without `--manifests` the check is skipped with a warning at startup. That keeps
the tool usable before you adopt manifests, but you should adopt them.

## Overriding

`orch rollback --force` requires `deploy:override`, is audited under the
caller's name as `rollback.forced` with the refusal text, and should be rare
enough that every use is worth a conversation.
