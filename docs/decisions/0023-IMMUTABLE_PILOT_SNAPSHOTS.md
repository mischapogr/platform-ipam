# ADR 0023: Explicit immutable pilot snapshots

Status: accepted for the local NetBox workspace, 2026-09-24. Stage and
production deployment still require migration, access-control, retention and
load qualification.

## Decision

An authenticated operator may opt in to saving a named **derived pilot report**
after a successful assessment. A `MigrationPilot` groups successive immutable
`PilotSnapshot` reports for one pilot owner. The first save creates the pilot;
later saves explicitly select it and require the same owner. The snapshot table
stores the report JSON, its SHA-256, creator and creation time. Opening a snapshot rerenders its
evidence against the current NetBox Prefix view; the saved report itself is
not edited. A new upload creates a new snapshot. The browser never retains
the uploaded inventory, matrix, plan, credentials or Terraform state files.

This amends ADR 0020's no-storage rule only for this explicit derived-report
snapshot. Uploading without the save option remains request-scoped and
read-only. The snapshot is an operator-provided artifact, not independent
proof that AWS, the allocation authority or Terraform produced its contents.
Its input hashes support comparison with retained customer files; they do not
replace signed collection provenance. All workspace operators can read saved
pilot snapshots in this local version, so a production rollout must validate
that shared visibility and set a retention policy.

Terraform execution references can be joined to a pilot by matching the
report's input hashes and stable allocation keys. They are labelled as
customer-supplied and never change address, allocation or network readiness.
The plugin does not execute Terraform or reserve addresses. Platform-IPAM
authority verification remains the separate CLI report; AWS IPAM authority
remains unverified until a dedicated handoff exists.
