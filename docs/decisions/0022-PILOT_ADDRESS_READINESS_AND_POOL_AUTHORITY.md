# ADR 0022: Advisory pilot address readiness and one authority per pool

Status: accepted for the local offline pilot workflow, 2026-09-23. Live
customer evidence and AWS IPAM execution remain unverified.

## Context

The first customer deliverable is an address-conflict assessment and
replacement options for networks that must communicate. It does not need to
orchestrate TGW routing or workload migration. Existing `onboard assess`
classifies intended connectivity, while `onboard progress` tracks a separate
reviewed migration file. The allocator's ledger and AWS IPAM cannot both
allocate from the same range.

## Decision

Produce a separate, read-only address report from the existing assessment,
original inventory, protected ranges and an approved pool plan. Classify
address readiness as `CIDR_BLOCKED`, `UNKNOWN` or `CIDR_READY`; always keep
network readiness `NOT_ASSESSED`. Offer deterministic, distinct candidate
CIDRs for pilot-scoped VPCs in a confirmed conflict as alternatives for owner
review, never as an automatic keep/move decision or durable hold. Treat the
whole supplied inventory as occupancy even when only some accounts belong to
the pilot. Withhold
candidates when inventory or input evidence is incomplete. No plan or UI
read can allocate.

For a pilot with a reviewed `migration.yaml`, selected replacement candidates
use the target request's account, region and prefix length. They are planned
before unselected alternatives. The pilot planning gate refuses a candidate
whose size or target identity differs from the reviewed move. This does not
make the CIDR a reservation or authorize the move.

Each approved pool names one authority. Overlapping pools are refused. For
`platform-ipam` authority, show a candidate that avoids known input space,
with an explicit snapshot-only warning; the existing reservation path is
still the only way to acquire it. For `aws-ipam` authority, show the pool and
size but no locally selected CIDR. AWS IPAM execution needs a separate
contract before it can be presented as integrated allocation.

## Consequences

- The pilot asks the customer for connectivity intent, inventory access,
  protected ranges and an owner/scope. The platform operator supplies its
  own approved pool plan.
- A `CIDR_READY` result never means TGW routes, security, DNS or traffic were
  tested. Unresolved intent and incomplete coverage cannot yield readiness.
- A future reservation or AWS IPAM response must be revalidated against
  current authoritative state. No candidate may bypass ledger holds,
  quarantine or AWS IPAM pool ownership.

## Pilot gate clarification, 2026-09-23

The [first customer pilot gates](../PILOT_GATES.md) require an actual
reservation by the pool's sole authority and revalidation of its returned
CIDR before a replacement counts as verified. A read-only availability check
cannot close the allocation gate because it leaves a race before reservation.
The existing Platform-IPAM reservation path can provide its own durable hold;
AWS IPAM authority still requires a separate integration contract and
implementation. Planning results remain advisory in either mode.

## Platform authority evidence, 2026-09-23

The [read-only pilot verifier](../RESERVATION_VERIFICATION.md) now checks
Platform-IPAM's current committed allocation rows against reviewed moves,
pool policy and occupancy. Its result is separate from ADR 0015's
CIDR-free migration-progress export. It cannot verify AWS IPAM authority.
An imported verifier report may be displayed by the NetBox workspace without
giving that plugin a platform API credential; the source remains an
operator-supplied artifact.
