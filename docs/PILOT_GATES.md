# First customer pilot gates

Status: acceptance contract, 2026-09-23. The local workflow has synthetic
evidence only. No customer pilot has passed these gates.

## Goal and boundary

Demonstrate that Platform-IPAM can discover the customer's AWS address space
with measurable coverage, identify address conflicts relevant to intended
private connectivity, and produce replacement CIDR proposals that can be
safely handed to the customer's sole allocation authority.

The result is **address readiness**. Network readiness remains
`NOT_ASSESSED`: TGW routes and attachments, security controls, DNS and tested
traffic are separate evidence. The customer owns workload migration,
application validation, cutover and rollback.

Each gate is `PASS`, `FAIL` or `UNKNOWN`, with a dated evidence reference and
an owner. A missing artifact, unreadable account or unresolved assumption is
`UNKNOWN`, never an implicit pass. The pilot passes only when all four gates
pass for the agreed scope. A narrower scope requires the pilot owner's
recorded approval and a new assessment; it cannot silently remove a failed
account or region.

## 1. Input gate

**Pass evidence:** the pilot owner has approved the account and region scope,
connectivity matrix (`must_communicate`, `must_stay_isolated`, unresolved),
protected external CIDRs, and pool plan with exactly one authority per pool.
Read-only AWS access is available for every requested account and region.
Record the owner, approval dates, input hashes, and any explicit unknowns.

An unresolved connectivity relationship may remain in the input, but it makes
the affected address result `UNKNOWN` rather than `CIDR_READY`. Missing AWS
access is recorded as missing coverage. The approved plan is supplied by the
platform operator and reviewed with the customer; it is not inferred from
apparently free address space.

## 2. Discovery gate

**Pass evidence:** keep the original `accounts.json`, `run.json`,
`failures.csv` and `networks.csv` from a live read-only
[`org-inventory.sh`](../scripts/aws/org-inventory.sh) run. Report expected and
scanned account/region cells, inaccessible or partial cells and their reasons,
scan start/end and duration, newest and oldest successful observation, and
VPC/CIDR counts. Reconcile a representative sample against AWS. The pilot
owner must explicitly accept any missing cell; acceptance records the limit
and does not turn incomplete coverage into complete coverage.

`run.json` and `accounts.json` are required to distinguish empty cells from
cells never scanned. A missing or interrupted run is `UNKNOWN`. Do not claim
100% requested coverage unless every requested account/region cell succeeded.
The collector has only been exercised with a stubbed AWS CLI so far; live
read-only collection and a representative scale measurement remain open.

## 3. Planning gate

**Pass evidence:** retain the reviewed inputs and the deterministic
[`onboard assess`](OVERLAP_ASSESSMENT.md) and
[`address-plan.py`](ADDRESS_READINESS.md) reports with their hashes. Every
confirmed `must_communicate` overlap in the agreed scope has resource IDs,
CIDRs and an owner review. Unresolved intent and incomplete coverage appear
as `UNKNOWN`. Proposed CIDRs fit an approved pool and avoid the supplied
inventory, protected ranges, exclusions and other proposals. Rerunning over
unchanged inputs yields the same result.

A proposed CIDR is advisory. `ADVISORY_CANDIDATE` means free relative to the
report's inputs, not `AVAILABLE` at the allocation authority. An
`aws-ipam`-owned pool has a pool and size recommendation, not a locally chosen
CIDR. The owner chooses which VPCs to keep or move; the report presents
alternatives without making that decision.

## 4. Allocation gate

**Pass evidence:** immediately before provisioning, the sole authority for
each selected pool accepts a reservation/allocation and returns a committed
CIDR. Record the authority, pool,
request identity, operation/allocation ID, returned CIDR, time and result.
Revalidate the *returned* CIDR against the approved plan, protected ranges,
current complete occupancy evidence and the selected migration intent. A
pending operation, stale/incomplete evidence, rejection or conflicting result
is `UNKNOWN` or `FAIL`; it is never counted as a verified replacement.

For a `platform-ipam`-owned pool, use the existing authenticated reservation
API and its durable ledger hold. The returned CIDR can differ from the
offline candidate; the returned allocation is the execution evidence. A
read-only availability query followed by a later write does not satisfy this
gate, because another writer can allocate the prefix between those steps.
The [pilot verifier](RESERVATION_VERIFICATION.md) checks the durable hold
through a current authenticated read. Its first version requires an exact
match to the reviewed advisory CIDR; a different safe CIDR needs explicit
owner review before it can count as verified.

For an `aws-ipam`-owned pool, this gate is **currently open**. The offline
planner does not read AWS IPAM state or reserve from AWS IPAM, and the local
allocator must not allocate in that pool. Implement and validate a dedicated
authority handoff with stable request identity, retries, response
revalidation, failure recovery and NetBox projection before claiming a pass.
Until then, the report may recommend a pool and size but cannot claim that an
AWS IPAM prefix is available or reserved.

## Pilot evidence summary

Publish one dated summary with counts and links to the retained evidence:

| Measure | Required interpretation |
| --- | --- |
| Requested / scanned / inaccessible account-region cells | Counts and reasons; no silent omissions |
| Inventory age and duration | Start, finish, oldest and newest successful observation |
| VPCs and VPC CIDR associations | Separate counts; secondary CIDRs remain visible |
| Confirmed blockers and affected VPCs/accounts | Only conflicts relevant to reviewed connectivity intent |
| Unresolved intent and coverage | Explicit `UNKNOWN`, including accepted limitations |
| Reviewed moves and proposed replacements | Alternatives; the owner selects actual moves |
| Authority-committed replacements | Count only completed and revalidated reservations |
| Network readiness | `NOT_ASSESSED` |

This is a pilot acceptance artifact, not evidence that applications migrated
or that private traffic works. The next engineering priority is the authority
handoff for customers whose pools are owned by AWS IPAM, followed by live
discovery and timing at representative account scale. TGW/security/traffic
correlation is outside this pilot.

The supported pilot deployment also needs an operational qualification with
real users: persistence, authentication and authorization on the chosen
NetBox release. This checks that the delivered workflow can be used by the
pilot team; it does not expand the address-readiness claim or require every
NetBox deployment topology to be supported.
