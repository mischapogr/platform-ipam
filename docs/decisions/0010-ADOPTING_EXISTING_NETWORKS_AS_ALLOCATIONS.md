# ADR 0010: Existing networks are adopted by a pinned reservation

Status: accepted, 2026-09-18, as a design; built by packages F1 to F7 of the
[work plan](../WORK_PLAN.md) and demonstrated end to end on 2026-09-20
(`tests/e2e/test_e2e_adopt.py`), including a VPC and its subnet adopted in two
runs. AMENDED on 2026-09-20 with the maintainer's approval: the body below says
adoption adds two overlap exemptions and no more, and building it showed that
rule refuses every VPC that has subnets; there is now a third, for a reviewed
VPC's own observed subnets, stated precisely in the last dated paragraph of this
record. The dated paragraphs at the end correct the body wherever building it
proved the body wrong, and they win over it. Still not shown: reclamation after
release, a real interruption between the adapter write and the commit, a VPC
whose subnets are only partly imported end to end, and anything against live
AWS.

## Context

[ADR 0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md) made an imported
network a NetBox prefix that blocks address space and owns nothing, and
deferred adoption to this record. The asymmetry it accepted is why we are
back: imported space has no owner, no key, no audit event and no finding, and
an existing VPC cannot be brought under Terraform management at all, because
provider import takes a platform allocation id (`docs/CLIENTS.md` section 5)
and imported occupancy has none. `docs/AWS_INTEGRATION.md` section 9 names the
intended shape already: classify every occupying range, "register intended
managed allocations through an audited operator onboarding procedure,
validate/tag their cloud bindings, and then import their platform allocation
IDs into Terraform", with writers paused so two workflows cannot claim the
same range.

ADR 0007 listed why minting an allocation is the dangerous option. Each item
is a specific piece of code, and this design is constrained by what that code
does rather than by what would be convenient.

There is exactly one place a `domain.Allocation` is constructed
(`internal/service/service.go:213`) and exactly two where `Committed` becomes
true: the commit after a successful `Ensure` (`:253`) and the worker's
recovery of a persisted reservation (`internal/service/worker.go:66`).
Everything the ledger, the API and the tests assume is a property of that one
path.

Identity is a hash. `requestHash` covers the request that comes out of
`validateRequest` with `Description` and `Labels` zeroed
(`internal/service/service.go:58`, `1356-1365`) — allocation key, scope,
environment, region, account, address family, prefix length, parent and
availability zone (`internal/domain/types.go:17-29`). A request under an
existing tenant and key is compared against that hash and either replays or is
refused with `409 allocation_key_conflict`
(`internal/service/service.go:162-183`). Normalization counts as much as the
fields: an omitted account is resolved from the principal and an empty address
family becomes `ipv4` before hashing (`887-889`, `914-921`).

`Reserve` is then a sequence of gates. A complete inventory snapshot and a
complete, current, fresh observation are required before anything is selected
(`112-144`, `206-208`); a pending operation anywhere in the overlap domain
refuses the request (`184-186`); quota and parent are rechecked inside the
transaction (`187-198`); `chooseCIDR` rejects a candidate overlapping the
snapshot, the observation or any ledger hold (`992-1070`); and the exact
candidate, a pending operation and an audit event are persisted before NetBox
is touched (`213-218`, `239-241`).

The adapter is hostile to adoption from both sides. `Ensure` refuses to create
over an existing prefix: failing to find its own marker it looks the CIDR up
in the pool's VRF and returns "CIDR already occupied in managed VRF"
(`internal/netbox/client.go:664-694`) — exactly the state an imported network
is in. `EnsureOccupancy` refuses any object carrying `platform_allocation_id`
and refuses to write the ownership fields at all
(`internal/netbox/occupancy.go:270-278`, `308-315`). One will not create
ownership where occupancy exists, the other will not add ownership to
occupancy it created, and nothing converts one into the other; `Sync` and
`Delete` then require the prefix to carry the allocation's id and CIDR already
(`internal/netbox/client.go:758-760`, `797-799`).

The worker is not fooled by a record that merely claims to be bound.
`bindingObserved` demands a resource whose id, type, account and region match
the binding, whose `platform-ipam:allocation-id` and
`platform-ipam:allocation-key` tags match the allocation, whose primary CIDR
equals the issued CIDR, and whose parent and AZ match where expected
(`internal/service/service.go:1164-1194`); an `ACTIVE` allocation whose
binding is not observed raises a `CRITICAL` finding on every pass
(`internal/service/worker.go:160-168`). The platform cannot satisfy that by
tagging: its AWS role grants `ec2:DescribeVpcs`, `ec2:DescribeSubnets`,
`ec2:DescribeAvailabilityZones` and optionally `ec2:DescribeRegions` and
nothing else (`deploy/aws/platform-ipam-readonly-role.yaml:133-155`), and
`docs/AWS_INTEGRATION.md` section 6 puts tagging with the provisioner on
separate credentials by design.

The ledger offers no shortcut. One global advisory lock guards every read and
write, and every write deletes and rewrites nine tables
(`internal/storage/postgres.go:108-146`, `322-327`). The key tombstone is not
an independent record: `allocation_keys` is regenerated from the allocations
map on each write with `retired` derived from the state (`:336`), as are holds
and the domain operation barrier (`349-369`). And the contract forbids the
obvious interface — "No consumer endpoint directly sets lifecycle, selects a
CIDR, mutates pools, resets keys, or forces reclamation" (`docs/API_V1.md`
section 2) — of which an adoption endpoint would do two in one call.

## Decision

Adoption is an operator process mode of the service binary, `platform-ipam
adopt`, with `plan` and `apply` steps mirroring `onboard`. It is not an
endpoint: an endpoint would select a CIDR for its caller and would need a
principal able to act for another tenant, which the identity model cannot
express — `domain.Principal` carries a tenant and eligible targets and no role
(`internal/domain/types.go:31-37`, a gap
[ADR 0008](0008-PLATFORM_OWNED_OPERATOR_UI.md) already named). It is not a
`client` verb, because the CLI holds no policy and would have nothing to call.
It is a sibling of `onboard` rather than a subcommand of it: `onboard` is
dispatched before the ledger exists (`cmd/platform-ipam/main.go:51-55`) and
both its package comment and its help text promise that it never opens the
ledger database (`internal/onboardcmd/onboardcmd.go:6`, `75`), while adoption
writes the ledger and needs the observer `onboard` has never built. The
operator sequence stays "import, then adopt"; only the process changes.

The operator supplies the tenant, the allocation key exactly as the owning
team will use it, the scope, environment, region and account, the exact CIDR,
the AWS resource id, the NetBox prefix id of the imported occupancy, and for a
subnet the platform allocation id of its already-adopted parent. The rest is
derived and may not be supplied: pool and overlap domain from
`validateRequest`'s matching (`internal/service/service.go:948-990`), prefix
length from the CIDR, `ipv4` as the address family, a subnet's availability
zone from the observed resource's zone id, the policy version from
configuration, and allocation id, request hash, revision, timestamps and
inventory id from the code that mints them for a reservation. Supplied account
and region are not trusted either: they are compared against the observed
resource and the pool's eligibility, and a disagreement is a refusal.

The request hash must be the one the owning team's first
`POST /v1/allocations` produces, or that request is refused forever with
`409 allocation_key_conflict`. This is achieved by construction rather than
reimplementation — the operator's fields go through the same `validateRequest`
and the same `requestHash` — and two consequences follow. The recorded prefix
length must equal the mask length of the adopted CIDR, which `chooseCIDR`
guarantees for a reservation and the pinned path must assert explicitly, or a
team asking for a `/20` collides with an adopted `/16` under its own key.
Description and labels need not match, because `requestHash` zeroes them and
mutable POST fields never overwrite existing values (`docs/API_V1.md` section
5). The team's first request replays as a `200` even though adoption wrote no
HTTP idempotency record under its key, because the tenant-and-key scan is the
branch that answers it.

Adoption goes through `Reserve` with a pinned candidate, not through a second
constructor. `Reserve`'s body becomes an unexported function taking an
optional pin; exported `Reserve` passes none and a new exported `Adopt` passes
one. The pin replaces three things and nothing else: `chooseCIDR` becomes a
verification of the pinned CIDR, `Ensure` becomes the new adapter operation
below, and the operation type and audit action say `ADOPT`. The key scan, the
retirement check, the domain barrier, quota, parent validation, the trust
rules, the persist-then-mutate ordering and the commit shape are the same
code, written once. That is what keeps
`internal/service/safety_contract_test.go` meaningful: those tests sit at the
service boundary and assert properties of that shared body — a retired key
never replays, a complete but untrustworthy observation cannot admit a
reservation, a secondary CIDR or a wrong parent or AZ cannot activate a
binding. A second constructor would leave every one of them unproven on the
adoption path while the suite kept passing, which is worse than having no
adoption at all. The price is a branch in `Reserve`; the mitigation is that
the pin is a pointer parameter on an unexported function with no transport
binding, so the consumer path cannot pin by accident — a compile-time fact
rather than a test.

The pinned path repeats every validation rather than skipping it: pool
membership and eligibility, allowed prefix length, subnet policy and parent
compatibility from `validateRequest`; a complete snapshot and a complete,
current, fresh observation, on which `plan` refuses as `Reserve` does;
containment in the pool and, for a subnet, in its parent's CIDR; and the
ledger overlap scan with no exemption beyond the existing
subnet-inside-its-own-parent case (`internal/service/service.go:1046-1064`).
Adoption adds two exemptions and no more. In the snapshot the candidate may
overlap exactly one prefix: the one whose CIDR equals the candidate, in the
pool's VRF, carrying the `platform-ipam-imported` tag
(`internal/netbox/occupancy.go:27`) and no ownership fields, whose NetBox id
the operator named. In the observation it may overlap exactly one resource:
the one whose id the operator named, whose type equals the scope, whose
primary CIDR equals the candidate, whose account and region match, whose
parent and zone match for a subnet, and which carries no
`platform-ipam:allocation-id` belonging to anything else. A second overlapping
prefix or resource in either set is a refusal. This is the shape of
`pendingReservationObservationSafe` (`internal/service/worker.go:87-108`),
with reviewed input standing in for the tag that does not exist yet.

The missing adapter operation is an `Adopt` beside `Ensure` that converts an
existing unmanaged prefix into a managed one. It resolves domain and pool as
`Ensure` does and checks the CIDR lies inside the pool
(`internal/netbox/client.go:661-663`); reads the prefixes at exactly that CIDR
in the pool's VRF as `prefixesAt` does
(`internal/netbox/occupancy.go:394-417`) and refuses unless there is exactly
one; refuses unless that prefix carries the import tag and none of
`platform_allocation_id`, `platform_allocation_key`, `platform_operation_id`
or `platform_state`; and refuses unless `findByMarker` finds no other prefix
claiming this allocation id (`internal/netbox/client.go:535-598`). It then
patches `ownedFields` on while preserving every unowned field as `Sync` does
(`632-644`, `762-766`), keeping the import tag, batch and source so the
provenance survives. This paragraph was written believing that NetBox offers
no conditional update; package F1 measured the opposite (see the dated paragraph
at the end of this record), so the patch carries the read's ETag as `If-Match`.
The write is still read, verify, patch, re-read, with `validateExisting`
applied to the re-read
(`613-629`); a re-read showing a different owner is a refusal that leaves the
operation pending for a person, never an overwrite. It belongs on the
`domain.Inventory` port rather than on the concrete client, because the
worker's recovery path holds only the port.

The order of effects follows `Reserve`, and what is durable at each step is
what makes a crash survivable. The first ledger transaction persists the
uncommitted allocation, the pending `ADOPT` operation, the audit event and the
adoption idempotency record; once it commits, the CIDR is held against every
other reservation and the overlap domain is fenced
(`internal/storage/postgres.go:349-369`). The adapter then converts the
prefix; a crash here leaves the ledger intent and a prefix already carrying
our markers, which the re-run recognises through `validateExisting` exactly as
`recoverAfterCreateError` does for a create
(`internal/netbox/client.go:719-732`). A second transaction sets `Committed`,
the inventory id and `InventorySync`, closes the operation and writes the
commit event; only then is the allocation visible to `GET` and `LIST`, which
both require `Committed` (`internal/service/service.go:282`, `295`).

A crashed adoption must not be recovered by `recoverReservations`, which
filters on the reserve operation type and calls `Ensure`
(`internal/service/worker.go:24`, `56`) — permanently refused for an adopted
CIDR, and rejected by its observation guard anyway because the resource is
untagged. The operation would stay pending and every reservation in that
domain would be answered `503 domain_busy` for as long as it did. The distinct
`ADOPT` type keeps it out of that loop and a sibling recovery path calls
`Adopt` under the same guards; without both, a crash at the wrong moment takes
the domain offline until someone edits the database by hand.

An adopted allocation is born `RESERVED` and committed, with no binding. It
cannot be born `ACTIVE`: `bindingObserved` requires the two platform tags, the
platform cannot write them, and the contradiction would be a `CRITICAL`
finding on every pass. Born `RESERVED`, the existing reconciler does the right
thing unprompted — while the resource is untagged it contributes a
`WARNING`-level `unmanaged_occupancy` finding to the pool's tenants
(`internal/service/worker.go:196-228`) and, after the configured interval, a
`WARNING`-level `reservation_aged` finding against the allocation
(`170-175`); when the owning team tags the resource with its own provisioning
credentials, the next observation claims it, verifies it and promotes it to
`ACTIVE` with a `binding_verified` event (`176-194`). Adoption therefore needs
no AWS write access, and the gap between ledger ownership and cloud tagging
appears as warnings that age rather than criticals to triage.

The audit record is two append-only events, `ADOPT_PLANNED` at the hold and
`ADOPT_COMMITTED` at the commit, with the actor set to the operator's own
subject rather than the tenant principal the process acts as. `domain.Event`
carries no structured details (`internal/domain/types.go:98-102`), so the
evidence — import batch, NetBox prefix id, AWS resource id, observation
generation and finish time — goes in the reason string; events insert with
`ON CONFLICT DO NOTHING`, so a re-run adds nothing
(`internal/storage/postgres.go:411-413`). Re-running `adopt apply` on the same
reviewed input is a replay through the tenant-and-key scan the consumer path
uses, plus an idempotency record under a method and path of its own, so a
re-run whose mutable fields changed is still a replay and can never occupy the
record the owning team's `Idempotency-Key` will need. Terraform import then
follows unchanged: the allocation id goes into `terraform import`, the AWS VPC
is imported into its own resource separately, and the configuration must
describe the same immutable request (`docs/CLIENTS.md` section 5).

There is no undo. Release moves the allocation to `QUARANTINED` and retires
the key permanently (`internal/service/service.go:86-88`, `169-171`), and
reclamation will not finish either, because the AWS resource still exists and
`observationConflict` keeps failing the post-delete verification (`760-762`) —
so a wrongly adopted network parks in `QUARANTINED` with its CIDR held and its
key dead. The only mechanism that could return the key would delete the
allocation row, and since `allocation_keys` is regenerated from the
allocations map on every write, that would erase the tombstone itself: the one
operation this project must not own. Adoption is corrected forward, never
reversed, and the protection against a wrong key or a wrong CIDR is the `plan`
step and the review before it.

Bulk adoption is serial by construction. The global lock and the full-table
rewrite make every adoption a stop-the-world ledger write, and the domain
barrier refuses every consumer reservation in that domain while one adoption
is pending. It belongs inside the paused window `docs/ONBOARDING_IMPORT.md`
section 8 already prescribes for `onboard apply`, one network at a time,
parents before subnets, with the cost per adoption growing with the size of
the whole ledger.

The evidence required before this record can be accepted is mostly tests. At
the service boundary: a pinned candidate overlapping a second unmanaged
prefix, a second resource, or a resource whose primary CIDR differs while a
secondary association matches, must each be refused; an incomplete snapshot or
untrustworthy observation must refuse adoption as it refuses reservation; an
adopted allocation must replay under the owning team's `POST` with `200` and
the same CIDR, and must conflict when the prefix length differs; an
adopted-then-released key must return `409 allocation_key_retired`. In the
adapter, `httptest` cases for a conversion, an idempotent second call, a
prefix already carrying an allocation id, a prefix in the wrong VRF, two
prefixes holding the CIDR, and a re-read showing a different owner. In the
worker, a pending `ADOPT` operation that is recovered instead of wedging the
domain, and an adopted allocation that raises no `CRITICAL` while untagged and
reaches `ACTIVE` once tagged. End to end,
`test_netbox_holds_no_managed_prefix_the_api_does_not_know`
(`tests/e2e/test_e2e_allocation.py:179`) must still pass after an adoption,
together with its converse: every committed allocation's inventory id names a
prefix carrying that allocation's id.

One property must never regress, and it is the reason for every choice above:
no path other than the reviewed one can mint a committed allocation. Today
that is checkable by reading three lines — one construction
(`internal/service/service.go:213`) and two assignments of `Committed`
(`:253`, `internal/service/worker.go:66`) — and adoption must leave the count
at three rather than adding a fourth. The matching assertion is that every
committed allocation has a terminal `RESERVE` or `ADOPT` operation and an
audit event naming its actor, and that the API surface has not grown:
`api/openapi.yaml` and the routes at `internal/transport/http.go:60-69` are
unchanged by this work.

The position is therefore: adopt through one exported `Adopt` that shares
`Reserve`'s body with a pinned candidate and two narrowly justified overlap
exemptions; add one `Adopt` operation to the inventory port that converts a
reviewed imported prefix into a managed one; let the allocation be born
`RESERVED` so the existing reconciler finishes it when the owning team tags
the resource; and accept that adoption is irreversible.

## Consequences

ADR 0007's asymmetry stops being permanent. An existing VPC gains an owner, a
key, an audit trail, capacity accounting and a Terraform import path, without
a new endpoint and without a second way to build an allocation.

The cost is a branch in the most safety-critical function in the service, and
an entry point that acts as a tenant without that tenant's authentication.
`adopt` needs ledger credentials, NetBox credentials and cloud read access at
once — more authority than any other process mode holds — and the service
cannot verify the operator: the actor in the audit event is a string the
command was given. Restricting who may run it is a deployment control, not
something this design enforces.

Findings change meaning for a while. A newly adopted network is a committed
allocation whose resource is untagged, so `unmanaged_occupancy` and eventually
`reservation_aged` warnings are expected rather than actionable, and the line
between "waiting for the team to tag" and "nobody is ever going to tag this"
is a judgement call with no signal of its own. `reservation_aged` at least
fires relative to the adoption, which makes the wait measurable.

Reclaiming an adopted allocation would delete the NetBox prefix outright
(`internal/netbox/client.go:776-816`), taking the import tag, batch and source
with it — correct for an allocation, wrong for occupancy that was never ours
to release. The quarantine deadlock above stops it while the VPC exists, so
the failure is loud rather than silent, but the import would have to be re-run
to restore the occupancy if it ever completed.

Extending `domain.Inventory` touches every implementation and test double,
including the one in `safety_contract_test.go`. That is mechanical work and
also the point: a capability that turns occupancy into ownership should be
impossible to add without every inventory implementation admitting whether it
has it.

What is not known is stated plainly. Nobody has patched ownership fields onto
an imported prefix in the pinned NetBox release, so the
read-verify-patch-re-read sequence and its window are designed rather than
observed, and whether NetBox offers a conditional update that would close that
window has not been checked. (Both were checked by package F1 on 2026-09-18; the
result is the dated paragraph at the end of this record.) The interaction
with the plugin objects of
[ADR 0009](0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md) is unexamined: they
point at the prefix by foreign key, and whether an adopted prefix should keep,
lose or gain a plugin edge belongs to whoever builds package N3. The operator
identity model is a gap rather than a detail — there is no role on
`domain.Principal` and no proposal here to add one. And the scale at which the
serial, full-rewrite ledger stops being an acceptable way to adopt an estate
has not been measured: a few hundred networks is plainly fine, ten thousand is
plainly a different design.

Established on 2026-09-18 against NetBox 4.6.7, the pinned
`netboxcommunity/netbox:v4.6.7-5.0.2` development stack, using a throw-away
prefix `198.51.100.0/24` in VRF 1 created with the import tag and a batch and
deleted again afterwards. Two of the unknowns above are now measured, and the
first of them contradicts what is written higher up. This release does offer a
conditional update: a `GET` of `/api/ipam/prefixes/{id}/` answers with a weak
ETag derived from `last_updated`, `W/"2026-09-18T19:53:45.017476+00:00"`; a
`PATCH` carrying a deliberately wrong `If-Match` answers `412 Precondition
Failed` and leaves the object untouched; the same `PATCH` carrying the ETag the
`GET` returned answers `200`; and replaying that now-stale value answers `412`
again. `Adopt` therefore keeps the ETag of the read whose contents it checked
and sends it as `If-Match` on the write, which closes the window between the
checks and the patch rather than merely narrowing it. `If-Unmodified-Since` with
a stale date was ignored and the write applied, so it is not used, and the
unconditional read-verify-patch-re-read sequence stays as the fallback for a
release that returns no ETag, with the re-read guarding both paths. Patching
`platform_allocation_id`, `platform_allocation_key`, `platform_operation_id` and
`platform_state` together with `status` onto the imported prefix answered `200`,
and the re-read still carried the `platform-ipam-imported` tag,
`platform_import_batch` and `platform_import_source`: custom fields are merged
key by key, so one the request omits is left alone. Tags are not merged. A
`PATCH` carrying `"tags": []` cleared the import tag outright, while one that
omits the key leaves the tag list untouched, so `Adopt` sends no `tags` key at
all and its test fake reproduces both semantics.

Established on 2026-09-18 by package F2, which built the pinned path described
above and found two paragraphs of it written in the belief that the code could
already express them. The snapshot exemption is specified as the prefix
"carrying the `platform-ipam-imported` tag ... and no ownership fields, whose
NetBox id the operator named", but the snapshot's network type carried only an
id, a CIDR, an allocation id, an operation id and the pool marker
(`internal/domain/types.go`), so neither the tag, nor the other two ownership
fields, nor the import batch the audit reason wants was visible to the service
at all; the type gained `Imported`, `Owned` and `ImportBatch`, which the adapter
fills from the tag list and the custom fields it already reads. And the subnet
case is stricter than "parents before subnets" suggests. Adoption adds two
overlap exemptions and no more, so a subnet's own parent VPC is exempted from
the observation scan only by the rule a reservation uses, and that rule demands
a verified parent binding (`parentResourceMatches`,
`internal/service/service.go`). An adopted parent is born `RESERVED` with no
binding, so a subnet cannot be adopted straight after its parent: the owning
team must tag the parent VPC, the reconciler must promote it to `ACTIVE`, and
only then is the subnet adoptable. The alternative would be a third exemption
recognising the parent resource by CIDR, which this record forbids, so the
constraint belongs in the runbook instead. Lastly, the promise that a re-run
whose mutable fields changed is still a replay is kept by dropping description
and labels in `Adopt` rather than by hashing differently: an adoption request
then has no mutable fields, its body hash and its immutable hash coincide, and
the re-run replays through the adoption's own idempotency record.

Established on 2026-09-19 by package F3, which built the worker's recovery of a
pending `ADOPT` operation and found two things this record had assumed. The
first is where the reviewed evidence lives. The paragraphs above put it in the
audit reason string, which a recovery cannot parse back, so the pending
operation now carries a durable record of the operator's subject, the reviewed
inventory id, the reviewed resource id and the import batch, as a binding
candidate already rides on a verification. Operations persist as one JSON
payload per row, so this needed no schema change, and the public operation
projection names its fields explicitly, so none of it reaches the API. The
returned-id comparison, the observation exemption and the actor on
`ADOPT_COMMITTED` are all read from that record, which is what lets a replayed
`Adopt` call and the worker agree about what was reviewed. The second is the
claim that a newly adopted network merely contributes a `WARNING`-level
`unmanaged_occupancy` finding while its resource is untagged. It did, and it
should not have: that finding names no allocation, is fanned out to every
eligible tenant of every pool in the domain, and is raised exactly because no
tag points at an allocation, so an adopted network appeared to its own tenants
as unmanaged occupancy that they could neither act on nor tell apart from the
real thing, on every pass and for as long as the wait lasted. The reconciler now
passes over the one resource an adoption's durable record names, while it is
untagged and its type, account, region and primary CIDR still match the adopted
allocation. Review narrowed this from "any committed allocation of that domain":
an untagged resource sitting on an ordinary reservation was never reviewed by
anybody, and reporting it is how its owner learns to tag it. A second resource
of the same shape, the reviewed resource once its primary CIDR has changed, and
a resource carrying somebody else's claim are all still reported, and
`reservation_aged` is left as the measurable wait this record intended. Recovery
also adds one finding code, `adoption_stuck` at `CRITICAL`, because a pending
adoption fences its whole overlap domain: it is raised when the reviewed
resource is no longer alone at the adopted CIDR, when the reviewed prefix has
gone, become owned or lost its import tag, when the adapter refuses, and when
the inventory answers with an object that is not the reviewed one, and it is
resolved by the commit that ends it. Only a timeout or a cancellation counts as
uncertainty, which waits silently for the next pass; every other adapter answer
is a refusal somebody is told about, and both are tried again, since `Adopt`
writes nothing when it refuses.

Established on 2026-09-20 by package F5, against the development stack.
Everything this record promises for a single network holds end to end: the plan
writes nothing, the adoption keeps the import tag, batch and source on the
prefix, the owning team's first `POST` replays, a different prefix length
conflicts, tagging promotes the allocation to `ACTIVE`, release retires the key,
and the worker commits a pending `ADOPT` operation. What does not hold is the
premise that a VPC and its subnets can be adopted at all. An organization
inventory imports a VPC together with its subnets and the observer sees all of
them, so when the VPC is the pinned candidate its subnets' prefixes are "another
network in the inventory" and its subnet resources "another cloud resource", and
the adoption is refused; this record's own evidence list asks for exactly that
refusal (a second unmanaged prefix inside the candidate) without noticing that a
VPC's subnets are the ordinary case of it. Adopting the VPC before its subnets
are imported does not help, because the onboarding import then refuses the
subnets as overlapping platform-managed space. The exemption that is missing has
to be as narrow as the two that exist: an observed subnet whose parent is the
reviewed VPC, and an imported prefix that is exactly such a subnet's primary
CIDR. Until the maintainer decides on it, only a VPC without subnets can be
adopted.

Established on 2026-09-20 by package F7, which the owner approved, and which
amends the rule above that adoption adds two overlap exemptions and no more.
There are now three. The third applies only when the pinned scope is `vpc`, and
it rests on nothing the operator supplied beyond the two ids their record
already names. An observed resource overlapping the pin is exempt when it is of
type `subnet`, when its parent is the reviewed VPC's own resource id, when its
account and region are the request's, when it carries no
`platform-ipam:allocation-id`, and when its primary CIDR lies inside the pin; an
inventory network overlapping the pin is exempt when it is imported, unowned,
strictly inside the pin, and its CIDR equals the primary CIDR of one such exempt
subnet. Everything inside the pin must therefore be accounted for by an observed
child of the reviewed VPC, and the implication runs one way only: an imported
prefix no observed child explains is somebody else's occupancy and still
refuses, while an observed child with no imported prefix passes, because a
partial import is an import problem rather than an ownership ambiguity and the
reconciler keeps reporting that child until it is imported and adopted in its
own right. Still refused, each with a test: an imported prefix inside the pin
with no matching observed subnet, which is the case F2's own
`TestAdoptRefusesASecondUnmanagedPrefix` encodes and which passes unchanged; a
subnet whose parent is another VPC; a second VPC overlapping the pin, nested or
not; a subnet whose secondary CIDR reaches into the pin while its primary does
not; a subnet already claimed by an allocation; a subnet in another account or
region; and an owned or untagged prefix inside the pin. The exemption never
applies to a pinned subnet, whose space belongs to whoever put it there. The
rule lives in `reviewedVPCChildren` and `childOfReviewedVPC`, which
`pinnedCIDR`, `PlanAdoption` and the worker's `recoverAdoption` all reach
through the two functions they already shared, so planning, applying and
recovering agree by construction; `chooseCIDR` is untouched, and an ordinary
reservation still refuses space any child occupies. An adopted VPC's subnets
remain unmanaged occupancy and keep raising `unmanaged_occupancy` until they are
adopted themselves, which is what makes the second run of the procedure
necessary rather than optional. That second run needed one more change, in the
onboarding import: a row strictly inside a platform-managed vpc-scoped prefix is
now importable as unmanaged occupancy with an informational finding naming the
parent allocation, so a subnet created after its VPC was adopted can still be
brought in, while a row equal to a managed network, a row containing one, a row
partly overlapping one and any row overlapping a managed subnet are refused
exactly as before. `onboard` still never opens the ledger: the inventory records
no allocation scope, and no platform custom field carries one, so
`domain.Network` gained `ParentAllocationID`, read from the
`platform_parent_allocation_id` field the adapter already writes for every
managed prefix, and a managed network naming no parent is a vpc-scoped one
because a vpc request may carry no parent, a subnet request must carry one, and
an allocation is constructed in exactly one place. The evidence is a live run of
the whole procedure for a VPC with one subnet: plan defers the subnet on its
parent, the first apply adopts the VPC and reports the subnet
`waiting_for_parent` with exit 3, the owning team's tags promote the VPC to
`ACTIVE`, the second apply replays the VPC and adopts the subnet, and a third
writes nothing.

Established on 2026-09-20 by package H2c, which the owner approved
([ADR 0012](0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md), accepted
the same day). This amends this record's own "There is no undo" sentence,
which was written before an operator-only, audited abandon of an
*uncommitted* adoption existed to say otherwise. The sentence now reads: no
committed allocation may be deleted or un-adopted, and an uncommitted
adoption may be abandoned under ADR 0012. The distinction the amendment turns
on is commitment, not intention or the presence of a NetBox write: ADR 0012's
own abandon may clear a prefix an interrupted adoption already half-converted
back to imported occupancy, and may delete the uncommitted allocation row and
its adoption idempotency record, but it refuses a committed allocation
absolutely, in every state, exactly as this record's own protection —
`plan` and the review before it — remains the only defense once an adoption
has committed. Nothing else in this record changes: the "one constructor"
count, the two overlap exemptions plus the third this record's own later
paragraph adds, and the no-API-growth rule all hold unamended, because
`adopt abandon` is a third `adopt` subcommand, not a fourth place a committed
allocation is constructed, and it never touches `chooseCIDR`, `pinnedCIDR` or
either function this record names for the third exemption.
