# Break glass

When the orchestrator itself is what's broken, these are the moves, in order of
preference. Each one leaves a trail.

## A deployment is stuck holding a lease

The lease expires on its own 60 s after the last heartbeat. If `orchd` is still
running and wedged, restart it: startup reconciliation will ask the provider
what happened and resume, fail, or mark the deployment `UNKNOWN`.

## A deployment is `UNKNOWN` or `ROLLBACK_FAILED`

Both page because production is in a state the tool can't vouch for.

1. `orch logs <id>` — read what the provider said.
2. Check the target by hand. Believe the target, not the tool.
3. Fix forward, or deploy a known-good version explicitly with `orch deploy`.
   Nothing transitions out of these states; the next deploy is a new record.

## orchd is down and you must ship

Deploy with the provider's own mechanism (the script `exec` would have run).
Then, once `orchd` is back, deploy the same version through it so its record
matches reality, and note the out-of-band deploy in your incident log. `orchd`
cannot audit what it didn't do.

## `orchd` refuses to start: the audit log doesn't verify

Do not delete the journal. Either it was edited or it is corrupt, and you need to
know which.

1. Stop and copy the journal somewhere read-only.
2. Restore the most recent backup and start from that.
3. Compare the two to find the divergence before trusting either.

## `orchd` refuses to start: journal corruption

A torn final write is truncated automatically. A bad checksum in the middle is
not, because silently dropping a committed record would lose history. Restore
from backup; keep the damaged copy for inspection.

## A token has leaked

```sh
orchd token list  --db /var/lib/orch/orch.journal
orchd token revoke --db /var/lib/orch/orch.journal <id>
```

`orchd` reads `tokens.json` at startup, so **restart it** after revoking — until
then the running server still accepts the token. Then check `orch audit log` for
anything the token did.
