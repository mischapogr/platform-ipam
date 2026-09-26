# Pilot reservation verification

Status: local read-only Platform-IPAM authority check, 2026-09-23. Tested
against synthetic reports and the development API's authenticated operator
read. No real AWS Organization or AWS IPAM pool has been verified.

The migration-progress `client evidence` export deliberately omits CIDR and
pool ID ([ADR 0015](decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md)).
It cannot prove that a proposed CIDR was reserved from the reviewed pool.
[`verify-reservations.py`](../scripts/aws/verify-reservations.py) reads the
current Platform-IPAM allocation API with a read-only operator credential
and produces a separate pilot verification report. It never reserves space.

First produce `pilot-evidence.json` with the CLI steps in the
[workspace guide](MIGRATION_WORKSPACE.md), review the selected moves, and
reserve their stable allocation keys through `client reserve` or Terraform
apply. The approved pool plan must describe the same authority, IDs and
ranges that the running allocator uses. Then run:

```sh
export PLATFORM_IPAM_TOKEN=<read-only-operator-token>
python3 scripts/aws/verify-reservations.py \
  --pilot pilot-evidence.json \
  --approved-plan approved-plan.json \
  --protected protected.json \
  --networks inventory/networks.csv --run inventory/run.json \
  --url https://ipam.example.com \
  --max-inventory-age-seconds 600 \
  --out verification.json
```

Loopback HTTP is accepted for local development; other origins require
HTTPS. The command pages through the authenticated allocation list, requires
operator-scoped rows with tenant IDs, and records the authority read instant.
It matches each reviewed move's tenant and stable key to one committed
`RESERVED` or `ACTIVE` allocation, then checks target account, region,
environment, scope and size; approved pool ID, authority, region, CIDR and
exclusions; protected and observed occupancy; and other selected
replacements. In this first version, a returned CIDR different from the
advisory candidate requires owner review and does not verify. It refuses an
inventory whose **oldest successful account/region observation** exceeds the
explicit age limit. Exit `0` means all selected
moves verified, `3` means a report with missing or mismatched allocations,
and `4` means no trustworthy report (including stale inventory or an
unreadable authority response).

The verifier supports **Platform-IPAM-owned** pools only. An AWS-IPAM-owned
pool remains `UNKNOWN` until an AWS IPAM authority handoff exists. A
successful check is evidence of durable Platform-IPAM holds at the read
instant; it does not mean the old VPC CIDRs have moved or that routes,
security controls, DNS or traffic work. The API's paginated list is not a
transactional snapshot, but committed holds remain durable; the verifier
also checks for duplicate keys and overlapping returned CIDRs.

Upload `verification.json` with the same pilot inputs in **Migration pilot
overview**, or pass it as `--verification` to `pilot-evidence.py`. The
workspace recomputes the pilot report, checks its digest and the verifier's
counts, and shows the allocation ID, returned CIDR and any mismatch reason
per move. An old verification report becomes `UNKNOWN` when the inventory
age limit expires. The browser does **not** authenticate to Platform-IPAM;
the imported report is an operator-supplied evidence artifact, not a
cryptographically signed attestation. Keep the original CLI output and
authority read context with the pilot records.
