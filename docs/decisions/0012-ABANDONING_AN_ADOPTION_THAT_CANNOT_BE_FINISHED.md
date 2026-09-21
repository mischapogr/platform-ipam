# ADR 0012: An adoption that never committed is abandoned, not undone

Status: accepted, 2026-09-20, as a design; built by packages H2a, H2b and H2c of
the [work plan](../WORK_PLAN.md) and demonstrated end to end on 2026-09-20
(`tests/e2e/test_e2e_adopt.py`, `AdoptAbandonE2ETest`): against the real ledger
and a real NetBox, a stuck adoption whose prefix was already half-converted was
abandoned with `platform-ipam adopt abandon` — the prefix lost every ownership
field and kept its import tag, batch, source and the account and region the
import wrote, the hold and its idempotency record were deleted, the audit events
survived, the `adoption_stuck` finding resolved, the domain answered
reservations again, the same network was adopted again under the same key with a
new allocation id, and a committed allocation was refused. Every test in the
evidence paragraph exists and passes; the opt-in PostgreSQL tests were run
against a throw-away PostgreSQL 17.5. Not shown: a real interruption between the
adapter write and the commit (the stuck hold is seeded), the re-marking race
(simulated by a test double), abandoning a VPC that has adopted subnets, and
`abandon` in a real cluster — the Helm `operatorJob` renders an `adopt abandon`
Job since package H7, validated by `helm lint` and `helm template` only. It
amends exactly one sentence of [ADR
0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md) and is bounded by [ADR
0011](0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md), which gives the
operator role no write of any kind. The dated paragraphs at the end correct the
body where building it proved the body wrong. Proposed by package H1.

## Context

Adoption persists before it mutates. The first ledger transaction of `reserve`
(`internal/service/service.go`) writes the uncommitted allocation, the pending
`ADOPT` operation carrying the reviewed record as `Operation.Adoption`, the
`ADOPT_PLANNED` event and the adoption's own idempotency record, and only then
calls `Inventory.Adopt`. A second transaction sets `Committed`, the inventory
id and the operation's terminal status. That ordering is what makes a crash
survivable, and it is also what makes a wrong reviewed record expensive: the
hold is durable from the first transaction onwards, and everything the hold
costs is paid whether or not the adoption can ever finish.

What it costs is four things at once, and only the first is widely known. The
pending operation fences the whole overlap domain, because `reserve` refuses
any request when `pendingDomain` finds a pending operation in that domain, so
every reservation there — consumer and operator alike — answers
`503 domain_busy`. The uncommitted allocation blocks its own CIDR against
everything else: both `chooseCIDR` and `pinnedCIDR` refuse a candidate
overlapping any allocation with `Committed` false, and they do so before they
look at its state, so no state a repair could set would release that space. It
occupies a quota slot, because `countTenant` counts every allocation whose
state is not `RELEASED` and never consults `Committed`. And it holds the
allocation key, which the owning team agreed in writing before the run
(`deploy/runbooks/ADOPTION.md` section 2), because the `allocations` table is
unique on `(tenant_id, allocation_key)` and `reserve`'s tenant-and-key scan
finds the row.

Nobody can clear it. The allocation is invisible to `GET` and `LIST`, which
both require `Committed`, and `Service.Release` refuses an uncommitted
allocation with `404 not_found` for the same reason, so the owning tenant has
no lever at all. `recoverAdoption` retries on every worker pass — the loop
in `cmd/platform-ipam/main.go` ticks every `full_scan_interval_seconds` — and
where it cannot converge it calls `flagStuckAdoption`, which raises one
`CRITICAL` `adoption_stuck` finding keyed on the allocation and changes
nothing else. That finding is not in the list of codes `reconcileAllocations`
resolves at the start of each pass, so it stays open until a commit resolves
it, which is precisely the event that is not going to happen. The runbook says
so plainly in section 10 ("Gap: the code offers no supported way to abandon a
stuck adoption") and section 12 ("There is no undo"), and names editing the
ledger by hand as the only remaining path.

The recovery path ends in `flagStuckAdoption` on six routes, and the important
question for this record is which of them can follow an adapter write.
`recoverAdoption` (`internal/service/worker.go`) flags when the pending
operation carries no `Adoption` record, when the allocation's CIDR does not
parse, when `reviewedOccupancy` no longer sees the reviewed resource alone at
the CIDR, when `reviewedNetwork` no longer sees the reviewed prefix imported
and unowned, when `Inventory.Adopt` returns an error that
`uncertainInventoryError` does not classify as a timeout or a cancellation,
and when `adoptedTheReviewedObject` finds the returned id is not the reviewed
one. Two of those cannot follow a write: the unparseable CIDR is a defensive
guard reached before any adapter call, and a `412` from the conditional
`PATCH` — which `internal/netbox/adopt.go` turns into `ErrAdoptConflict` and
the worker therefore treats as a definite refusal — means the write was
rejected, which is what `If-Match` is for. The rest can. `ErrAdoptReadBack` is
defined in that file as exactly this case: "the prefix was patched and then did
not read back as this allocation's. The write has already happened."
`adoptedTheReviewedObject` failing is the same situation with a worse shape,
because `Adopt` locates its prefix by CIDR and VRF rather than by id, so a
mismatch means the ownership fields landed on an object nobody reviewed. The
missing-record case cannot be classified at all, which is the same as assuming
a write. And the two ground-truth routes — `reviewedOccupancy` and
`reviewedNetwork` — become reachable after a write whenever a process died
between the adapter call and the commit and the estate then moved: the same
two functions guard the original `Adopt` call in `pinnedCIDR`, so what they
refuse on a later pass is a world that has changed since. `reviewedNetwork`
tolerates a prefix already carrying this allocation's own id, which is what
makes an ordinary crash converge, so its refusal specifically means the prefix
has gone, lost its import tag, or become somebody else's.

The synchronous path has the same window without the worker. In `reserve`,
where the adapter answers with an id that is not the reviewed one, the caller
refuses to commit and returns `409 adoption_conflict` with the operation left
pending — after the write. And a process killed between `Inventory.Adopt`
returning and the commit transaction opening leaves exactly the same state
with no error recorded anywhere.

What such a prefix carries is larger than the phrase "the four ownership
fields" suggests. `Adopt` checks four custom fields to decide whether a prefix
is free (`prefix.owned` in `internal/netbox/client.go` reads
`platform_allocation_id`, `platform_allocation_key`, `platform_operation_id`
and `platform_state`), but what it writes is `ownedFields`, twelve custom
fields including tenant, environment, pool, policy version, parent allocation
and the three AWS values, together with `status: reserved`. It preserves every
unowned field and sends no `tags` key, so the import tag, batch and source
survive — the provenance ADR 0007 put there is intact on a half-converted
prefix, which is what makes restoring it conceivable at all.

No operation restores it. Every prefix write in `internal/netbox` is one of
five: `Ensure`'s create, `EnsureOccupancy`'s create, `Sync`'s patch, `Adopt`'s
patch and `Delete`'s delete. `Sync` refuses anything that does not already
carry this allocation's marker and only rewrites the owned fields with current
values; `Delete` removes the whole object after verifying the marker, and
takes the import tag, batch and source with it; and it needs
`Allocation.InventoryID`, which an uncommitted hold does not have, since that
field is set at the commit. There is no function anywhere in the package that
clears the ownership fields from a prefix without deleting it, and the
occupancy writer refuses to produce one: `refuseManagedFields` stops
`EnsureOccupancy` from ever writing those keys, so a half-converted prefix
cannot be repaired by re-running the import either.

The ledger, by contrast, makes removal mechanically trivial and that is the
danger. `persistState` (`internal/storage/postgres.go`) deletes and rewrites
nine tables from the in-memory `State` on every write, so an allocation
removed from `State.Allocations` inside a `Ledger.Update` closure simply is not
written back, and `allocation_keys`, `holds` and `operation_barriers`, all
regenerated from the same maps, lose their derived rows with it. There are no
foreign keys in the schema. One table is not in that delete list:
`audit_events` is insert-only with `ON CONFLICT DO NOTHING`, and its
`allocation_id` is a plain column, so events outlive the allocation they
describe. Retirement, worth stating precisely because ADR 0010 rests on it, is
not read from `allocation_keys` at all — nothing in the service queries that
table; it is derived in `reserve`'s two scans from `State == QUARANTINED ||
RELEASED` on the allocation row itself.

ADR 0010's prohibition is a single sentence and it is about ownership: "The
only mechanism that could return the key would delete the allocation row, and
since `allocation_keys` is regenerated from the allocations map on every write,
that would erase the tombstone itself: the one operation this project must not
own." The work plan's Track F rule says the same in fewer words: "No package
may add a delete or 'un-adopt'." ADR 0011 forbids the other obvious shape: an
operator is a principal with no tenant, refused by every write path before any
new code runs, and that record explicitly keeps `adopt` out of the role's
reach.

## Decision

An operator-only, audited abandon of an **uncommitted** adoption exists, as a
third `adopt` subcommand. It deletes the hold, it clears the half-converted
prefix first if there is one, and it leaves the allocation key free for the
adoption that should have happened. It is never an endpoint, it is never
unlocked by ADR 0011's operator role, and it refuses a committed allocation
absolutely.

The reason this is not the undo ADR 0010 forbids is the tombstone. That
sentence is about a key whose allocation once existed: release moves a
committed allocation to `QUARANTINED`, the key is dead in `reserve`'s scans,
and deleting the row would erase the record that it ever was owned. An
uncommitted adoption has no tombstone to erase, because it was never
ownership. It is a reservation of intent that the code itself calls incomplete
in five places — `Get`, `List`, `Release`, `pendingRecoveryJobs` and the commit
guard all test `Committed` — and the key it holds is not retired, only
occupied. Deleting the row therefore removes an intent, not a fact, and the
audit trail of the attempt survives it in `audit_events` whatever the
allocation table says.

Where that argument breaks is the adapter write, and the break is not
hypothetical: of the six routes to `adoption_stuck`, four can follow one, and
the two that matter most — a read-back that did not verify, and a conversion
that landed on an object nobody reviewed — are *defined* by having written.
So an abandon that only removed ledger rows would deliberately manufacture the
state this record exists to forbid: a NetBox prefix claiming an allocation the
ledger does not hold, with nothing left anywhere naming that prefix, since the
reviewed id lives only on the operation. Abandon therefore owns the prefix too.
Refusing the half-converted case and leaving it to a person was considered and
rejected for the same reason: it refuses in precisely the cases where fixing
forward is impossible, which is the whole of the problem.

The adapter gains one operation, `AbandonAdoption(ctx, allocation,
operationID)` on the `domain.Inventory` port beside `Adopt`. Putting it on the
port rather than on the concrete client is the argument ADR 0010 made for
`Adopt`, run in reverse: a capability that takes ownership away should be
impossible to add without every inventory implementation and test double
admitting whether it has it. Its evidence is the marker and nothing the
operator typed. It searches by `findByMarker` for prefixes carrying this
allocation id, which is VRF-agnostic and therefore catches an object wherever
it is; zero matches is success with nothing to do, more than one is a refusal
that needs engineering, and the single match must also carry this operation's
id in `platform_operation_id`, so a prefix half-written by some other operation
is never touched. It then patches the twelve keys `ownedFields` writes back to
empty, sends no `tags` key so the import tag survives exactly as it survives
adoption, leaves every unowned custom field alone so the import batch, source
and AWS values stay, and sets `status` back to `active` — the value
`ensureOccupancyPrefix` writes when an import creates occupancy. The write
carries the read's ETag as `If-Match`, as `Adopt` does, on the measurement
recorded in ADR 0010; a `412` is a refusal, not a retry. It then re-reads and
requires the prefix to carry none of the four ownership fields and still carry
the import tag, and anything else is a refusal that has written nothing it can
be blamed for.

The pre-adoption status is the one thing the clear cannot recover, and the
honest fix is to record it. Nothing in the ledger or on the prefix remembers
what `status` a prefix had before `Adopt` set it to `reserved`, so restoring
`active` restores the import's own convention rather than the operator's. H2
should add a `PriorStatus` field to `domain.AdoptionRecord`, filled by `Adopt`
from the read it already performs and used by the clear when it is present;
holds created before H2 have none and fall back to `active`. `Snapshot` reads
no status, so nothing the allocator decides depends on this either way — it is
what an operator sees in NetBox, which is reason enough.

The order of effects is forced, and it is three steps rather than two. First a
ledger transaction fences: it re-reads the allocation and its operation, checks
every refusal below, sets the operation's status to the terminal `FAILED` with
an error naming the abandon, appends the `ADOPT_ABANDONED` event, and writes
nothing else. From that moment no commit can happen, because both commit paths
— `reserve`'s commit closure and `commitRecovered` — re-check
`o.Status != operationPending` under the ledger lock and become no-ops, and
`pendingRecoveryJobs` stops selecting the operation, so the worker stops
retrying and the domain stops being fenced. Second, outside any transaction,
the adapter clears the prefix. Third, a ledger transaction deletes the
allocation row and the adoption's idempotency record, re-checking under the
lock that the allocation is still uncommitted and its operation is still the
terminal one this abandon wrote, and resolving the `adoption_stuck` finding.

The invariant survives every interruption of that sequence, which is why the
order is this way round and not the other. After the fence, the ledger still
holds the allocation and the prefix may still claim it, which is consistent.
After the clear, the ledger still holds the allocation and the prefix claims
nothing, which is also consistent — a hold with no inventory object is exactly
what every adoption starts as. Only after the prefix is confirmed clean does
the allocation disappear. The reverse order would, on a crash between the two
writes, leave a prefix claiming an allocation that does not exist and no record
anywhere naming that prefix, which is unrecoverable except by hand. A crash
anywhere in the sequence converges on a re-run: the second invocation
recognises an uncommitted allocation whose `ADOPT` operation is already
terminal and abandoned, resumes at the clear, finds the prefix either still
marked or already clean, and finishes. A clear whose outcome is unknown — a
timeout — never advances to the delete; the run stops and says so.

One window cannot be closed by the ledger lock, and it should be stated rather
than designed around. The adapter call in a worker pass that started before the
fence happens outside any transaction, so its `PATCH` can land after the
clear, re-marking the prefix; its `commitRecovered` will then write nothing,
because the operation is no longer pending, but the markers would be back. The
delete step therefore re-reads by marker immediately before it deletes and
refuses to delete if anything claims the allocation again, telling the operator
to re-run once the current worker pass is over — an interval, not a mystery.
That is also the answer to the race with a concurrent `apply`: a second
`adopt apply` over the same record enters `reserve`, finds the allocation under
the tenant and key, and replays it rather than adopting again, so it reaches
`Inventory.Adopt` only through the same commit guard; and after the fence there
is no pending operation for it to finish. `PostgresLedger` takes one global
advisory transaction lock on every `View` and `Update`, so abandon's
transactions and every commit transaction are strictly serialized: whichever
acquires the lock first wins and the other sees the state it left.

The allocation key is free again, and that is a decision rather than a
consequence. The owning team agreed that key for that network in writing before
the run, and the network is still unowned after the abandon; retiring the key
would punish the tenant for an operator's wrong record and force a
renegotiation of a name that was never used for anything. It is also what the
code does on its own once the row is gone: retirement is derived from
`State == QUARANTINED || RELEASED` on an allocation row, so no row means no
retirement, and the `allocations` table's uniqueness on `(tenant_id,
allocation_key)` is released with it. The alternative of keeping the row in
some terminal state was examined and is not available: `chooseCIDR` and
`pinnedCIDR` both block on any allocation with `Committed` false before they
test its state, so a terminal uncommitted row would fence its CIDR against
every future reservation and every future adoption for ever, which is most of
the problem this record is solving. `countTenant` would keep charging the
tenant a quota slot for it unless the state were exactly `RELEASED`, which
would retire the key as well. So the row goes, and what the ledger holds
afterwards is the operation row in `FAILED` status — kept deliberately, because
it carries the `AdoptionRecord` and so preserves what was reviewed, and because
nothing iterating operations requires its allocation to exist — plus the two
audit events and a resolved finding.

The rest of the hold follows from that. The adoption's idempotency record must
be deleted in the same transaction, and this is forced rather than chosen: the
record is stored under `ADOPT`'s own method and path keyed by the allocation
key, and `reserve` consults it before anything else, so a record pointing at a
deleted allocation makes `stateResults` return nothing and drives every later
`Adopt` under that key into `reserve`'s final `apiErr(503, "ledger_error")` for
ever. Removing it also answers the question it raises: a later, correct `apply`
under the same key is a fresh adoption with a new allocation id, a new
operation and a new pair of audit events, never a replay of the abandoned one.
The `ADOPT_ABANDONED` event carries the operator's subject as actor, exactly as
`ADOPT_PLANNED` and `ADOPT_COMMITTED` do, and a mandatory free-text reason the
command refuses to run without; `domain.Event` has no structured details, so
the reason string also carries the allocation id, the operation id, the
reviewed inventory and resource ids from the durable record, and whether a
prefix was cleared. The `adoption_stuck` finding is resolved in the delete
transaction, because nothing else ever would: it is keyed on the allocation id,
`reconcileAllocations` does not reset its code, and an orphaned open `CRITICAL`
would fail `client findings --fail-if-open` for that tenant and for the
operator for ever. The quota slot returns with the row.

Who may do it is unchanged from `adopt`, and that is the honest answer rather
than a new control. It is a third subcommand of the same process mode,
`platform-ipam adopt abandon --allocation-id … --operator … --reason …`, with
an optional `--operation-id` that must match when given, taking its ids from
the `202` outcome of the apply report, which carries `allocation_id` and
`operation_id` for exactly this purpose, or from the `adoption_stuck` finding.
It is not an API endpoint: ADR 0010 keeps adoption out of the API because it
acts as a tenant without that tenant's authentication, and this writes the same
ledger while holding the same NetBox credentials. It is not unlocked by ADR
0011's operator role, which is a principal with no tenant that every write path
already refuses, and which that record deliberately grants no write at all; a
role that could delete a hold would be a different role, proposed in a
different record. The service method takes no `domain.Principal` at all — there
is no tenant to act as, and a signature that cannot be filled from a request
context is one more reason a handler will not grow around it — and a
source-parsing test in the style of ADR 0010's "three places" pins its only
caller to `internal/adoptcmd`.

What it must refuse, each for a stated reason. A committed allocation, because
that is exactly the undo ADR 0010 forbids and the answer is
`DELETE /v1/allocations/{id}` with everything section 12 of the runbook says
about it. An allocation that does not exist, or one that is uncommitted but has
no `ADOPT` operation, because this code cannot produce that state and
abandoning it would mean guessing whether an adapter write ever happened. An
operation of type `RESERVE`, for the scope reason below. An operation that is
`SUCCEEDED`, which means the commit won the race. A ledger or NetBox failure at
any point, which leaves the fence in place and asks for a re-run. And the
delete refuses, as described, when anything claims the allocation again.

A dry run is worth its cost and should ship with it. `adopt abandon --dry-run`
runs every check of the fence step and the marker search, writes nothing, and
prints what the ledger holds, what the operation records as reviewed, what was
found on the prefix — clean, carrying this allocation's markers, or carrying
somebody else's — and what a real run would do. The precedent is `adopt plan`,
whose value in package F5 was that the operator could see the service's own
verdict before the irreversible step; abandon is not irreversible in the same
way, but it is the one command in this project that deletes a ledger row, and
it should be possible to look first.

The cheaper alternative was weighed and is rejected. It is to narrow the fence
rather than to abandon: let a stuck `ADOPT` whose adapter write provably never
happened stop answering `503 domain_busy` for the rest of its domain while
still holding its own CIDR. Three things are wrong with it. "Provably never
happened" is not available: the adapter's error taxonomy already separates
refusals from uncertainty, and it is the uncertain answers — and the crash that
produces no answer at all — that leave a write in doubt, so the proof would
have to be a fresh snapshot showing the prefix unowned, which is evidence about
this instant and not about a write another process may be making right now. The
domain-wide fence is also not incidental: the hold's own CIDR is already
blocked by `chooseCIDR` without any operation, so what the barrier protects is
the trustworthiness of the *snapshot* every other reservation in that domain is
decided against while an adapter write is unfinished, and `operation_barriers`
is keyed by `domain_id` with one row per domain, so narrowing it is a change to
the storage model as much as to the policy. Above all it does not solve the
problem: the wrong hold still holds its CIDR, its key and its quota slot for
ever, and the operator still has no exit. It makes the dead end more
comfortable instead of removing it, and abandon removes the fence anyway as a
consequence of resolving the hold.

The same dead end does exist for a pending `RESERVE`, more quietly, and it is
deliberately out of scope. `recoverReservations` calls `Ensure` and `continue`s
on every error, raising no finding whatsoever, so a reservation whose chosen
CIDR was occupied out of band between the hold and the create — `Ensure`
answers "CIDR already occupied in managed VRF" permanently — or whose
`pendingReservationObservationSafe` guard can never be satisfied, retries for
ever in silence and fences its domain exactly as an adoption does. It is out of
scope for three reasons. Nobody reviewed it, so there is no reviewed record to
compare an abandon against. A consumer is holding the operation id from their
own `202` and may be polling `GET /v1/operations/{id}`, so what that endpoint
answers after an abandon is a contract question adoption does not have, since
an adoption's operation id exists only in an operator's report. And the tenant
has a way out that an operator adopting under an agreed key does not: a new key
and a new `POST`. What should be fixed first, and separately, is the silence —
a `reservation_stuck` finding beside `adoption_stuck` would cost little and
would stop this class of wedge from being invisible.

Dated note, 2026-09-21, package H8d. The third reason above — that a
tenant has a way out an operator does not, a new key and a new `POST` — is
superseded by [ADR 0013](0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md),
which found it simply wrong: a new key does not remove the old hold, and
the old hold refuses the new request either way, on the same overlap
domain the first one already fenced. That record adds the actual exit,
built by packages H8a-H8d: the consumer's own `DELETE
/v1/operations/{operation_id}`, permitted only while the platform has
itself raised `reservation_stuck` for the hold. It fences, then deletes
any NetBox prefix carrying the allocation's own marker, then deletes the
uncommitted row, in that order, mirroring the fence-clear-delete sequence
this record established for an adoption — deleting rather than clearing,
because a reservation's prefix belongs to nothing else. The first and
second reasons above still hold: there is still no reviewed record to
compare an exit against, and ADR 0013 found that fact makes a
reservation's exit easier to make safe rather than harder, since there is
no operator judgement or provenance an exit must preserve.

The runbook changes in two places. Section 10's "Gap" paragraph becomes the
procedure: when the ground truth cannot be made to match the reviewed record,
run `adopt abandon --dry-run`, read it, run it for real with a reason, confirm
the finding resolved and the domain unfenced, then re-review and adopt again
under the same key. Section 12 keeps its title and gains one sentence saying
what abandon is not — it applies only to an adoption that never committed, and
nothing returns a committed, wrongly adopted network to unmanaged occupancy.
Ordering: H2 touches `internal/service`, `internal/netbox` and
`internal/domain`, so it runs alone as Track F's packages did; it does not
depend on H3 and H3 does not depend on it, except that H3's Job template
already takes its subcommand and arguments from values, so `adopt abandon`
needs nothing of its own there.

The evidence that moves this record to accepted is a test list, and two items
head it because two properties must never regress. No committed allocation can
ever be abandoned: a test attempting it for every state a committed allocation
can be in, each naming the check that refuses it, and a mutation of that check
must break a test rather than open a delete. And no abandon can leave a NetBox
prefix claiming an allocation the ledger does not hold: an adapter-level test
for a clear, an idempotent second clear, a refusal when a second prefix claims
the allocation, a refusal when the single match carries another operation's id,
a `412` refusal, and a service-level test that interrupts the sequence after
the fence and after the clear and shows a re-run converging, plus one that
makes the marker re-appear between the clear and the delete and shows the
delete refusing. Beside them: the key is reusable afterwards, shown by adopting
the same network correctly under the same key after an abandon; the idempotency
record is gone, shown by that same adoption being a fresh allocation id rather
than a replay or a `503`; the `adoption_stuck` finding is `RESOLVED` and
`client findings --fail-if-open` recovers; the quota slot returns; the domain
answers reservations again; the audit events survive the deleted row, including
`ADOPT_PLANNED` from the attempt that failed; a pending `RESERVE`, a committed
allocation, an unknown id and an operation another process has already
committed are each refused with their own message; and end to end in
`tests/e2e/test_e2e_adopt.py`, against the real ledger and NetBox, an adoption
seeded into the pending state is abandoned, the prefix is shown to keep its
import tag, batch and source and to have lost every ownership field, and
`test_netbox_holds_no_managed_prefix_the_api_does_not_know` and its converse
both still pass.

The position is therefore: add `platform-ipam adopt abandon`, operator-only and
audited, acting only on an allocation that is not committed; fence the
operation, clear any prefix that carries this allocation's marker back to
imported occupancy, then delete the allocation row and the adoption's
idempotency record and resolve the finding; leave the allocation key free and
the audit trail intact; refuse a committed allocation absolutely; and leave a
pending `RESERVE` to a later record. Package H2 is then roughly three cuts:
the port and the adapter (`internal/domain/types.go`,
`internal/netbox/abandon.go` and its `httptest` cases, every inventory double);
the service (`internal/service/abandon.go`, its tests and the two safety
properties in `safety_contract_test.go`); and the command, the runbook and the
end-to-end module (`internal/adoptcmd`, `deploy/runbooks/ADOPTION.md`,
`tests/e2e/test_e2e_adopt.py`), with a dated paragraph in ADR 0010 amending its
no-undo sentence to read: no committed allocation may be deleted or un-adopted,
and an uncommitted adoption may be abandoned under this record.

## Consequences

The project acquires its first operation that removes a ledger row, and the
argument that it is safe rests entirely on the row being uncommitted. That is a
one-word distinction in a code path where every other guarantee is enforced by
several checks at once, so the test that no committed allocation can be
abandoned is not documentation of an intention but the thing that holds the
line. It has to be written the way ADR 0010's "three places" test is written —
against the source, not only against behaviour — because the failure it guards
against is a later change that helpfully relaxes the check.

The estate gains a third way for a prefix to be written by this platform, after
`Ensure` and `Adopt`, and it is the first that removes markers. Every tool that
reasons about NetBox state — the onboarding import's refusals, the drift check,
the AWS plugin objects of
[ADR 0009](0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md) — has so far been able to
assume that a prefix carrying ownership fields acquired them from an allocation
that exists. After this it can carry them, briefly, from one that is being
withdrawn, and a snapshot taken in that window shows an owned prefix whose
allocation id matches nothing. Nothing decides anything on that combination
today, and nothing should be allowed to start without saying so here.

An abandoned adoption leaves an asymmetric trace: two audit events and a
terminal operation that name an allocation id no allocation table row has, and
a resolved finding pointing at the same absent id. That is deliberate — it is
the record that something was attempted and withdrawn — but it means a reader
following an allocation id from an audit event can now legitimately find
nothing, which was previously impossible, and any future export or report over
`audit_events` has to tolerate it.

The operator's authority grows without any new control over who holds it. ADR
0010 already recorded that restricting who may run `adopt` is a deployment
control and that the `--operator` subject is an unverified audit string; this
adds the power to delete a hold to that same unguarded position, and the honest
description is that anyone who can run `adopt` in an environment can now also
undo an in-flight adoption in it. The mitigation is the narrowness of the
target — one uncommitted allocation, named by an id that only an apply report
or a finding produces — and not any check the code makes about the person.

Fixing forward remains the first answer and the runbook must keep saying so.
Abandon makes a wrong reviewed record recoverable, which lowers the cost of
getting one wrong, and the review before `apply` is still the only thing that
prevents adopting the wrong network correctly — a mistake abandon cannot touch,
because that adoption commits.

What is not known is stated plainly. Whether NetBox 4.6.7 clears a custom field
when a `PATCH` sends it as `null` or as an empty string has not been measured;
ADR 0010's F1 probe established that custom fields are merged key by key and an
omitted key is left alone, which is the opposite question, so H2's first step is
the same kind of throw-away probe against the development stack, and if neither
value clears a field the clear has to be redesigned around whatever does. The
window in which a worker pass started before the fence can re-mark a prefix is
bounded by the reconciliation interval but has never been observed, and the F5
experience — an adoption finishes in under a second, so a real interruption
could not be produced deterministically — says the end-to-end test will have to
seed the state rather than race it. Whether an operator would rather abandon or
be told to call an engineer is unknown, because no operator has yet met a stuck
adoption outside a test. And the same dead end for a pending `RESERVE` is
recorded here rather than solved, together with the observation that it raises
no finding at all, which is the more urgent half of it.

Measured against the development NetBox 4.6.7 on 2026-09-20, package H2a, on a
throw-away prefix in the documentation range created as an import creates
occupancy and then patched as `Adopt` patches it. A `PATCH` whose
`custom_fields` sends every owned key as JSON `null` answered `200` and the
independent read-back showed all fifteen of them — the twelve `ownedFields`
always writes and the three it writes only for an allocation that carries the
value, including the two `datetime` fields and the `select` typed
`platform_state` — as JSON `null`, which is exactly the shape a freshly created
prefix returns and which `stringCF` already reads as unset, so `prefix.owned`
reports the cleared object unowned; none of the owned keys is required or
refuses `null`. The body carried no `tags` key and no unowned key, and the tag
list, the import batch and source, an unrelated custom field and the description
all survived untouched, so the merge semantics ADR 0010 recorded hold for a
clearing patch as they do for a converting one. An empty string is accepted too
but reads back as `""` rather than `null`, so `null` is what the clear sends.
The detail `GET` carries the weak ETag `W/"<last_updated>"`, the conditional
clear under it answered `200`, and repeating the same clear under the now-stale
ETag answered `412`, so a `412` here is a refusal exactly as it is for `Adopt`;
the legal prefix statuses are `container`, `active`, `reserved` and
`deprecated`, and the throw-away prefix was deleted (`204`) and confirmed gone
(`404`, nothing left at the CIDR). Three things the measurement settled about
the design. The port signature becomes `AbandonAdoption(ctx, a, operationID
string, prior PriorInventory)`: what the network carried before the adoption is
ledger evidence and the adapter must not learn to read the ledger to get it, so
`domain.Network` gains a `Status`, an `AWSAccountID` and an `AWSRegion` that
`Snapshot` fills, and `reserve` records them on the `AdoptionRecord` in the
transaction that creates the hold, which is the only place that still knows
them. The clear derives its key set by calling `ownedFields` with the allocation
being abandoned rather than listing the keys, so the two cannot drift, and an
uncommitted hold has no binding, which is why `platform_aws_resource_id` is
neither written by the adoption nor emptied by the clear and an import's value
there survives both. And one sentence of this record is too generous:
`platform_aws_account_id`, `platform_aws_region` and `platform_aws_az_id` are
keys `ownedFields` owns, so an adoption overwrites the import's account and
region with the allocation's and the clear empties them — left at that, a
cleared prefix would keep its import tag, batch, source, description and
resource id but lose the AWS account and region the import wrote. Review closed
that the way the status was closed: the snapshot carries both, the durable
record keeps them as `PriorAccountID` and `PriorRegion`, and the clear writes
them back in place of the null, so a round trip returns every field an import
wrote; only a hold created before the record carried them is cleared to null,
because the allocation's values must not stay on a prefix the allocation no
longer owns. The import writes no availability zone, so that key is simply
emptied.

Built as package H2b on 2026-09-20, and four things this record says about the
service needed correcting or filling in. The `ADOPT_ABANDONED` event cannot say
"whether a prefix was cleared", because the fence writes it and the fence runs
before the clear — deliberately, since that order is what stops an interruption
from leaving an object claiming a deleted allocation — so the reason string
carries the operator's words, the allocation, the operation and the reviewed
inventory and resource ids, and says instead when no reviewed record was
persisted at all. The delete's promised re-read by marker is not available: the
port has no read by marker, only `Snapshot`, which is scoped to the domain's VRF
while the marker search deliberately is not; so the delete repeats the clear
itself immediately before its transaction, which is stronger than a read — where
nothing is marked the adapter writes nothing and answers at once, and where a
worker pass has re-marked the prefix it removes the markers again and the
abandon converges instead of asking for a re-run that would do exactly this,
while a refusal or an uncertain answer still stops the run with the allocation
row in place. The residual window is unchanged by that choice and is the one
this record already names, narrowed to the gap between that call's own read-back
and the ledger transaction. A re-run recognises its own fence by the code
`adoption_abandoned` stored in the operation's durable `Error` beside the
terminal status, never by reading the reason, and resumes in silence so that one
withdrawal leaves exactly one event. The window the H1 review added answers `409
adoption_abandoning`, retryable, from the two replay scans of `reserve` and the
two `PlanAdoption` repeats, so a concurrent apply, the owning team's own `POST`
and a plan all say the same thing. Uncertainty crosses the port as one
domain-level sentinel, `domain.ErrInventoryUncertain`, which keeps the service
free of any dependency on `internal/netbox`. The package first used only the
worker's timeout classification, which reported a NetBox `5xx` during the search
or the write as a refusal; review closed that in the adapter, where one function
decides what is no answer at all — a transport failure, a timeout, a `5xx` or a
`429` — and marks it, while a `4xx` is NetBox reading the request and refusing
it, stays a refusal and is never retried in silence. `Adopt` is classified by
the same function, so the worker's recovery no longer raises a `CRITICAL`
`adoption_stuck` because the inventory had a bad minute. Review also added the
two tests the delete's own guards lacked — the operation no longer being the
fence this abandon wrote, and a consumer's raced idempotency record naming the
hold — and ran the opt-in PostgreSQL tests, which had never executed, against a
throw-away PostgreSQL 17.5: all eight pass, this record's storage claims among
them. What this record says about storage holds exactly: `persistState` really
deletes a row removed from `State.Allocations`, `allocation_keys` and `holds`
lose their derived rows with it, `operation_barriers` is regenerated from the
pending operations and so empties at the fence, `audit_events` is insert-only
and outlives the allocation, and the `(tenant_id, allocation_key)` uniqueness is
released, which is what makes the key free again.
