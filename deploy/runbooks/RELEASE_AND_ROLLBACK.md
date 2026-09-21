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
