# Restore runbook

Restore the platform PostgreSQL database using the managed database service's
point-in-time or snapshot procedure. Keep the allocation ledger and its audit
history together; do not restore NetBox over it as a substitute. NetBox is an
independent inventory release and should be restored from its own backup.

After restore, pause new allocations and reclamation, verify database
connectivity with the migration Job and run a read-only reconciliation. Check
that allocation keys, durable holds, NetBox identity markers, and AWS scan
coverage agree before resuming admission.

## Local platform ledger recovery rehearsal

```sh
python3 tests/e2e/run-ledger-recovery.py --out /tmp/ledger-recovery.json
```

This opt-in runner builds the real application image and uses the maintained
Compose file in a uniquely named disposable development project. It seeds
three synthetic ledger fixtures: `RESERVED`, `QUARANTINED` and `RELEASED`.
They are not reservations verified against NetBox or AWS. An authenticated
metadata PATCH creates a real application audit event and retry record.

The runner checks unmigrated startup refusal, migration completion before API
readiness, API process crash, PostgreSQL outage/readiness recovery, migration
failure and repeat, and rollback of an invalid policy configuration using the
same binary. It then quiesces the API, dumps the complete ledger and restores
into a second database. The database identity is checked explicitly.

Every recovery compares all ledger table rows against the original snapshot,
including allocation IDs/CIDRs, durable holds, permanent retired keys, audit
events and idempotency records. Replaying the PATCH must return the existing
revision without adding a second event. Foreign-tenant and unauthenticated
reads remain denied, and the read-only operator cannot PATCH an allocation.
The runner removes only its own project volumes/image and private temporary
dump; the optional output retains hashes, counts and synthetic audit evidence.

This does not prove coordinated NetBox recovery, a different binary's schema
compatibility, stage/prod Kubernetes rollout, real OIDC admission or live AWS.

The [2026-10-14 real-binary evidence](../../docs/qualification/LEDGER_RECOVERY_2026_10_14.json)
records seven passed recovery checks, three allocation/hold/key rows, one
API-generated audit event and one retry record. A 14,254-byte custom-format
dump restored into `recovery_restored`; every ledger table matched its source
snapshot, and retries retained the existing revision and audit count. This
was a separate rehearsal from the workspace backup; it does not qualify
coordinated cutover of both databases.

## Coordinated ledger and NetBox database rehearsal

```sh
python3 tests/e2e/run-coordinated-restore.py --out /tmp/coordinated-restore.json
```

This opt-in runner creates a disposable Compose stack with real NetBox,
PostgreSQL, API and worker images. AWS observations use the development fake
fixture. Two owning-tenant API reservations must commit to NetBox before the
runner performs an authenticated metadata PATCH and pauses all four writers:
the platform API/worker and NetBox server/worker.

The stopped writer set establishes one logical cutoff for the two sequential
database dumps. Before restore, record the ledger rows and complete NetBox
prefix rows. Restore into two new databases and explicitly verify both
database identities. Compare all ledger tables and prefix rows; verify that
each restored reserved allocation matches exactly one prefix ID/CIDR, allocation/key/
tenant/pool marker, permanent key and committed durable hold, with no orphan
or duplicate allocation markers.

Keep admission paused until this preflight passes. The runner deliberately
changes one restored NetBox allocation marker and requires its preflight to
refuse the pair before starting the API. It restores the correct marker and
rechecks the original row snapshot before proceeding. This is a rehearsal of
the operator's restore gate; it does not add an automatic production startup
gate to the application.

The API starts first, with the worker still paused. Exact reservation/PATCH
retries and tenant authorization must leave the recovered pair unchanged.
After the workers resume, a third reservation must commit from a range
disjoint from the two restored holds, and the existing audit events must
remain unchanged. Queue and media volumes are retained during this local
test, rather than restored; qualify their custody/recovery, real identity,
workload access and admission controls in the actual deployment separately.
Only the runner's project resources and private temporary dumps are removed.

The [2026-10-14 coordinated evidence](../../docs/qualification/COORDINATED_RESTORE_2026_10_14.json)
records six passed checks: two API-created reservations and three source
prefixes (including the pool), matching restored ledger/inventory rows, a
refused marker mismatch, unchanged retries/authorization, and successful
resumed allocation. Five original audit events survived unchanged; the third
reservation added two more. The new `10.64.8.0/22` range was disjoint from the
restored `10.64.0.0/22` and `10.64.4.0/22` holds. All are disposable local
development ranges observed through the fake AWS adapter.

## Local queue, media and secret recovery

```sh
python3 tests/e2e/run-coordinated-restore.py --restore-auxiliary --out /tmp/auxiliary-recovery.json
```

This optional extension uses the same real API/NetBox reservation pair. With
all four writers stopped, create two synthetic files in NetBox's media volume
and one pending synthetic RQ job (`builtins.sum([20, 22])`). These fixtures are
not model attachments or customer provisioning jobs. Record the media bytes,
file permissions/ownership, job identity/arguments and pending membership.

Keep the generated local credentials in a mode-0600 file inside the runner's
private temporary directory. Simulate losing the runner's credential values
and load them from that file before creating the recovered services. No
credential values are written to the evidence report. This demonstrates a
local custody round trip; production vault, encryption/key rotation, access
audit and retention need the deployment owner's process.

Stop the queue/cache services gracefully before archiving the queue's complete
data directory, including its append-only files, and the media volume. Keep
the private mode-0600 archives together with the paired database dumps at the
quiesced cutoff. The runner refuses a deliberately corrupted archive checksum
before extraction; this is its preflight gate, not a production admission
feature or an authenticated backup manifest.

Extract only into fresh, empty queue/media volumes. The source volumes remain
untouched. Recreate the cache on a fresh volume rather than restoring it.
Check the media manifest and pending job against the source while application
writers remain paused; authenticated queue reads must work and anonymous
queue access must fail. Resume the NetBox worker only after these checks and
the paired database/marker preflight pass; keep the platform worker paused
during the job comparison so its normal fresh reconciliation cannot change
the snapshot. The recovered job must return 42 and leave the queue, with
reservation/audit rows and media unchanged. Then resume the platform worker
and require the normal disjoint third reservation to commit.

This does not qualify interrupted or scheduled jobs, external job side
effects, model-backed media attachments, production secret custody, real
identity/cloud access or a production cutover. Remove only the disposable
project and its private files after the rehearsal. Owner-defined retention,
holds, deletion authorization and backup expiry remain open.

The [2026-10-14 auxiliary recovery evidence](../../docs/qualification/AUXILIARY_RECOVERY_2026_10_14.json)
records ten passed checks, a 39,424-byte queue archive, a 4,608-byte media
archive and a three-entry media manifest. A restored pending job returned 42
without changing the paired reservation/audit evidence. After the platform
worker resumed, a third disjoint reservation committed and all five original
audit events remained unchanged. Runtime image/package identities are bound
in the report; private archives, credential files and project resources were
removed.

### Model-backed media extension

```sh
python3 tests/e2e/run-coordinated-restore.py --restore-model-media --out /tmp/model-media-recovery.json
```

This option includes auxiliary recovery and adds two synthetic NetBox site
image attachments. Sites support image attachments in the pinned NetBox image;
allocation prefixes are still checked separately by the paired restore.
The fixture uses the actual model's validation and storage, then captures full
attachment/site rows, parent identities, PNG bytes/dimensions and two readers'
object permissions. Readers have unusable passwords and no superuser
privileges. Generated API credentials stay inside the private test database.

Before backup and after restoration, real HTTP API requests must show each
reader only their own attachment: own GET succeeds, foreign GET returns 404,
the list contains one record, anonymous GET fails and PATCH returns 403. The
tokens allow writes so the PATCH refusal exercises server-side object
permissions. These probes authenticate the existing API, without browser SSO
or reservation writes. Restored metadata probes must leave the model snapshot
unchanged. With writers paused, deliberately withhold one media file and
misassign one attachment parent; the runner must reject each and restore the
original witness before admission.

This qualifies synthetic site attachments and API metadata authorization
locally. It does not qualify tenant-private raw media downloads, customer
attachments, production ingress/storage access, retention or real OIDC.
Interrupted and scheduled jobs still require separate qualification.

The [2026-10-14 model-backed media evidence](../../docs/qualification/MODEL_MEDIA_RECOVERY_2026_10_14.json)
records twelve passed checks, two 79-byte PNG attachments and a five-entry
media manifest. Both readers retained own GET 200, foreign GET 404, anonymous
GET 403, PATCH 403 and one visible attachment in their list. Missing files
and incorrect parent identities were refused by runner preflight. The
recovered pending job returned 42, all five original reservation audit events
remained unchanged, and a third disjoint reservation committed. The runner
removed its project, private backups and three unreferenced source volumes.

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
