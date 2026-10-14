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
