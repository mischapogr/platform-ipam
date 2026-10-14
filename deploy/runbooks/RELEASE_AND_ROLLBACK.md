# Release and rollback

Build one image, record its immutable digest, and use that digest in both
stage and prod values. Stage uses separate database/NetBox Secret names and
OIDC audience values. Promote the tested chart and digest together after the
stage lifecycle and sandbox checks pass.

To roll back, first disable new allocations and reclamation, then use the
previous chart version and image digest. Confirm the previous binary remains
compatible with the current database schema. Keep all durable holds and
quarantine records. Do not delete prefixes or rewind allocation identities as
part of a Helm rollback.

For the opt-in local real-binary migration and configuration-rollback
rehearsal, run `python3 tests/e2e/run-ledger-recovery.py --out /tmp/ledger-recovery.json`
from the repository root. See the [restore runbook](RESTORE.md) for its
fixture, ledger comparison and authorization checks. It verifies rollback
to the same binary after a bad policy configuration; qualifying an older
binary against a newer schema and the actual stage/prod release remains a
separate deployment gate.

## Local same-schema binary upgrade and rollback

```sh
python3 tests/e2e/run-ledger-recovery.py --upgrade-from d1807f4 --out /tmp/version-recovery.json
```

The optional baseline is a reviewed Git commit, built from a private archive
without changing the checkout. The candidate is built from the current
checkout. Record both distinct image identities, the baseline commit/archive
hash, and candidate runtime source hashes in the output. Both images use the
current development Compose configuration and fake AWS mode.

The older image migrates a new disposable database containing three seeded
allocation states. An authenticated PATCH produces the audit/retry baseline.
Pause the API, run the candidate migration, and admit the candidate only when
the complete ledger (including schema rows), owning-tenant read, exact retry,
foreign-tenant privacy and read-only operator refusal remain unchanged.

After the candidate recovery checks, restore its dump into a separate database
and explicitly verify that database identity. With the API stopped, replace
the supported version-1 marker with a synthetic version-999 marker. Neither
binary may start or mutate that database. Restore the original marker, then
run the older binary and candidate again against the restored database,
checking the same rows, retries, authorization and audit event after each.

This tests two binary versions with an unchanged version-1 schema. The marker
refusal is not structural schema validation, a forward schema migration, or
proof that an older binary can read an arbitrary future schema. Synthetic
allocations are not NetBox/AWS reservations. Qualify the exact production
image/chart/schema pair and real identity/cloud access in stage before
promotion; no production admission setting is changed by this runner.

The [2026-10-14 version recovery evidence](../../docs/qualification/VERSION_RECOVERY_2026_10_14.json)
records eleven passed checks from baseline `d1807f4` to candidate runtime
`f843252`, including rollback on the candidate-restored database. Three
allocation/hold/key rows, one API-generated audit event and its retry record
remained unchanged across both distinct images. The 14,254-byte private dump
and all disposable project resources were removed after the run.
