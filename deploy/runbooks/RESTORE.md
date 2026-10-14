# Restore runbook

Restore the platform PostgreSQL database using the managed database service's
point-in-time or snapshot procedure. Keep the allocation ledger and its audit
history together; do not restore NetBox over it as a substitute. NetBox is an
independent inventory release and should be restored from its own backup.

After restore, pause new allocations and reclamation, verify database
connectivity with the migration Job and run a read-only reconciliation. Check
that allocation keys, durable holds, NetBox identity markers, and AWS scan
coverage agree before resuming admission.

## Saved migration pilot evidence

`MigrationPilot` and `PilotSnapshot` are in the NetBox database. Back up the
whole database, including users, group membership and plugin migration
history. Preserve the original NetBox secret and token-pepper custody under
the deployment owner's process. The platform ledger and its reservation
audit events need their own coordinated restore; pilot snapshots are evidence
projections and do not replace that ledger.

1. Pause workspace uploads and snapshot saves, NetBox workers and other
   inventory writers. Record the backup time, exact NetBox/plugin image,
   schema migrations and an access-controlled manifest of pilot/snapshot IDs,
   creators, timestamps and report digests.
2. Restore into an isolated database with the matching image and schema.
   Keep admission and allocation writers paused until the ledger/inventory
   reconciliation above is reviewed. Do not overlay a newer live database.
3. Compare report export bytes, snapshot digests, creator identities, group
   membership and history against the manifest. Confirm older snapshots stay
   unchanged and corrupted snapshots refuse display and export with `409`.
4. Confirm expiry is evaluated at the new read time: a restored historical
   `VERIFIED` result must not regain current validity. Verify allocation IDs
   against fresh authenticated authority evidence before acting on a hold.
5. Check the deployed browser ingress: admitted operators can render/export;
   anonymous users and users outside the operator group cannot read or save
   reports. A forged identity header must not bypass the proxy. Operators
   intentionally share pilot visibility; these snapshots are not tenant-private.

The owner must specify retention duration, holds, authorized deletion,
deletion audit and backup expiry before any automatic cleanup is enabled.
Current creator/pilot foreign keys protect provenance from incidental parent
deletion; they are not a retention engine and do not prevent privileged SQL
or direct snapshot deletion. Qualify retention and restore in the actual
deployment with real identity admission before closing OPS-1.

## Local rehearsal

Run from the repository root with an existing local Compose `.env`:

```sh
python3 tests/e2e/run-workspace-restore.py --out /tmp/workspace-restore.json
```

The opt-in runner builds the current workspace image and creates a uniquely
named disposable Compose project. It seeds two synthetic evidence snapshots
and one deliberately corrupted snapshot, quiesces NetBox, makes a PostgreSQL
custom-format dump and restores it into a second database. It compares
contents/provenance and exercises the real workspace view plus the published
basic-auth proxy. Only the runner's project volumes and image are removed;
the private temporary database dump is discarded. The optional output contains
hashes, counts, identities and check results, never credentials or the dump.

This verifies local snapshot persistence and authorization, not a platform
ledger restore, live reservation, real OIDC admission or production retention.

The [2026-10-14 rehearsal evidence](../../docs/qualification/WORKSPACE_RESTORE_2026_10_14.json)
records a successful 1,256,545-byte custom-format dump/restore, three preserved
snapshots (two valid and one intentionally corrupt), creator/group metadata,
access/expiry checks and the authenticated proxy result. Product and runner
source hashes bind this local result to the tested files. The isolated project
was removed after verification.
