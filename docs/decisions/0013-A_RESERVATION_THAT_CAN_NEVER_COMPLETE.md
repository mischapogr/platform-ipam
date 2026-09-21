# ADR 0013: A reservation that can never complete is cancelled by its consumer

Status: accepted, 2026-09-20, and built by packages H8a to H8d of the [work
plan](../WORK_PLAN.md): the adapter, the service, `DELETE
/v1/operations/{operation_id}` with `platform-ipam client cancel`, and an
end-to-end demonstration against the real ledger and the development NetBox,
including a stuck reservation whose own prefix already existed. The window
between the fence and the delete is demonstrated through test hooks only, never
as a real race, and nothing has run outside the development stack. The dated
paragraphs at the end of this record say what building it measured and changed,
and win over the text above them. It amends exactly one paragraph of [ADR
0012](0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md) — the one that
puts a pending `RESERVE` out of scope — and it leaves [ADR
0011](0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md) untouched, because
the exit it describes is a write by an authenticated tenant and not by an
operator. Proposed by package H6, after package H4 made the condition visible.

## Context

A pending `RESERVE` costs exactly what a pending `ADOPT` costs, and it costs it
for the same reasons, because both run through one function. `reserve`
(`internal/service/service.go`) writes, in its first ledger transaction, the
uncommitted allocation, the pending operation, the `RESERVE_PLANNED` event with
the consumer's own subject as actor, and the POST idempotency record under
`idempotencyID(tenant, "POST", "POST /v1/allocations", key)`, where `key` is
the consumer's `Idempotency-Key`. Only then does it call `Inventory.Ensure`. A
second transaction — the commit closure — sets `Committed`, the inventory id
and the operation's terminal status.

Until that second transaction happens, the hold fences its whole overlap domain,
because `reserve` refuses any request for which `pendingDomain` finds a pending
operation in that domain, so every reservation there answers `503 domain_busy`.
It blocks its own CIDR against everything else: `chooseCIDR` walks the
allocations and marks a candidate blocked if it overlaps any allocation with
`Committed` false, and it does so in a branch that never looks at the row's
state, so no state a repair could set would release the space; the one overlap
exemption a subnet gets for its parent lives in the other branch, the one that
requires `a.Committed`. It occupies a quota slot, because `countTenant` counts
every allocation whose state is not `RELEASED` and never consults `Committed`.
And it holds the allocation key, which `reserve`'s tenant-and-key scan finds on
the row.

Package H4 made that state visible and changed nothing else about it.
`recoverReservations` (`internal/service/worker.go`) now classifies as
`recoverAdoption` does: uncertainty — no observer, an untrusted or stale
observation, or an error `uncertainInventoryError` recognises — leaves the hold
pending in silence and is retried, while a decision the adapter or the
observation reached on evidence it read raises `reservation_stuck`, `CRITICAL`,
through `flagStuckHold`. `flagAgedReservation` withholds even that until the
pending operation is older than one `Lifecycle.MaxObservationAge`, so a hold
that is merely racing its own creator's synchronous `Ensure` cannot flap a
`CRITICAL`. The finding is keyed on the allocation, is seen by the would-be
owning tenant and by the operator of ADR 0011, and is not in the list of codes
`reconcileAllocations` resets at the start of each pass, so it stays open until
a commit closes it. H4 said plainly, and `docs/FINDINGS.md` repeats, that the
only supported exit is still to make the ground truth match.

Sometimes it cannot be made to match. The dominant cause is an out-of-band
prefix at the held CIDR — `Ensure` answers "CIDR already occupied in managed
VRF" — and that one is usually fixable, by whoever owns the prefix, if they can
be found and are willing. The others are not. `Ensure` refuses with "duplicate
allocation marker in NetBox" when `findByMarker` returns more than one prefix
carrying this allocation's id, and the only fix is to delete one of them by
hand. `validateExisting` refuses a single marked prefix whose VRF does not match
`poolVRF(d, p)`, which a change to the pool's configured VRF between the create
and the next worker pass produces without anybody touching NetBox at all. And
`pendingReservationObservationSafe` refuses on a trusted observation that shows
a foreign or untagged resource overlapping the held CIDR, which is somebody
else's cloud estate and may simply be their VPC.

ADR 0012 left this out of scope for three reasons, and only one of them still
holds. The first was that nobody reviewed a reservation, so there is no reviewed
record to compare an exit against. That is true and it cuts the other way. An
adoption's abandon has to prove it is undoing the object an operator reviewed,
which is why the `AdoptionRecord` exists and why `AbandonAdoption` has to be
given what the prefix carried beforehand. A reservation's inventory object is
entirely the platform's own creation: `Ensure` posts `prefix`, `vrf`,
`status: reserved` and `ownedFields(a, opID)`, with no tags and nothing
inherited, and `validateExisting` checks it back by allocation id, operation id,
CIDR and VRF. There is no operator judgement an exit would have to reproduce,
and no provenance it would have to preserve. The absence of a reviewed record
makes a reservation's exit easier to make safe, not harder.

The second was the contract: a consumer holds the operation id from their own
`202` and may be polling `GET /v1/operations/{id}`, so what that endpoint
answers after an exit is a question an adoption does not have, since an
adoption's operation id exists only in an operator's report. That is the real
question, and the answer turns out to cost nothing, because the contract already
carries it. `api/openapi.yaml` defines `Operation` as a `oneOf` over
`PendingOperation`, `SucceededOperation` and `FailedOperation`, discriminated on
`status`; all three take `type` from an enum that contains `RESERVE` and
deliberately does not contain `ADOPT`; `FailedOperation` carries an
`OperationError` whose `code` is a free string with no enumeration. Section 5 of
`docs/API_V1.md` already requires operations to stay queryable for at least
thirty days after termination and states that a terminal operation's status and
result identity never change. A reservation operation that becomes `FAILED` with
a new code is therefore already a legal response of the published contract,
written before anyone asked this question.

The third reason was that the tenant has a way out an operator does not: a new
key and a new `POST`. That one is simply wrong, and it is the fact that changes
the decision. A new key does not remove the old hold, and the old hold refuses
the new request. The new `POST` is for the same target and therefore the same
overlap domain, so it reaches `pendingDomain`, finds the old pending operation
and answers `503 domain_busy`. Meanwhile a retry under the *same* key never
reaches that check at all: the tenant-and-key scan finds the uncommitted row
first and replays it, so the consumer receives `202` with the same pending
operation for ever. Both doors are shut by the thing they were meant to route
around. "Reserve again under a new key" is not an escape from a stuck
reservation; it is one more request the stuck reservation refuses.

What a stuck `RESERVE` may already have written is the question the rest of this
record turns on, and the tempting answer is wrong. `Ensure` is idempotent by
marker: it calls `findByMarker(a.ID)` first, and where that returns exactly one
prefix and `validateExisting` agrees on allocation id, operation id, CIDR and
VRF, it returns that prefix's id and the caller commits. So a prefix carrying
this allocation's markers *in the shape `Ensure` wrote them* is a recoverable
hold and not a stuck one — the next worker pass finishes it. It is tempting to
conclude that a hold H4 classifies as stuck has therefore not created its
prefix. Five routes refute that.

`Ensure` refuses with "duplicate allocation marker in NetBox" when
`findByMarker` returns more than one, and `recoverAfterCreateError` refuses with
"duplicate allocation marker after uncertain NetBox create" in the same
situation after a create whose response was lost. Neither is routed through
`uncertainEnsure`, so both are definite refusals, and both are *defined* by
prefixes carrying this allocation's markers existing. `validateExisting` can
refuse the single marked prefix for an operation-marker conflict, a CIDR
conflict, a VRF conflict or absent custom fields; that is a definite refusal
with the prefix sitting there, and the VRF case needs no misuse to reach. The
create path checks its own response through `validateExisting` and can refuse
after a successful `POST`, and refuses with "NetBox create returned no ID" when
the created object comes back without one; in both the object exists.

The fifth route is the ordinary one, and it is the one that matters most.
`recoverReservations` evaluates `pendingReservationObservationSafe` *before* it
calls `Ensure`. So a hold whose prefix `Ensure` created successfully, and whose
commit was then lost — a process killed between the adapter call returning and
the commit transaction opening, which is exactly the window ADR 0012 describes
for adoption — will never reach `Ensure` again once a foreign cloud resource
appears on its CIDR. The worker flags `reservation_stuck` and stops, and the
prefix stays, indefinitely, carrying this allocation's markers for an allocation
nothing will ever commit. So the answer is: usually nothing has been written,
because the commonest refusal is the exact-CIDR guard that runs *before* the
`POST`; but a stuck `RESERVE` can certainly have created its prefix, and two of
the routes to it are defined by having done so. An exit that removed only ledger
rows would deliberately manufacture the state ADR 0012 exists to forbid.

Who removes such a prefix is the next question, and the existing port does not.
`Inventory.Delete` requires `Allocation.InventoryID`, which is written only in
the two commit paths — `reserve`'s commit closure and `commitRecovered` — so an
uncommitted hold has none, and `Delete` verifies identity by reading that very
id. `Inventory.AbandonAdoption` does not fit either, and the reason is worth
stating precisely because it is what keeps the two exits from collapsing into
one. It clears the owned custom fields and keeps the object, which is right for
an adoption, where the prefix belongs to an import and outlives the adoption
that failed exactly as it outlived the one that succeeded. A reservation's
prefix belongs to nothing else. Cleared rather than deleted, it would remain an
unowned, untagged object at that CIDR in the managed VRF, and `Ensure`'s own
exact-CIDR lookup would then answer "CIDR already occupied in managed VRF" for
every future reservation there — the platform would have poisoned its own pool
while tidying up.

## Decision

An exit exists, it is the consumer's, and it is a cancellation of their own
pending reservation through the API: `DELETE /v1/operations/{operation_id}`,
authenticated as the tenant that owns the hold, permitted only while an open
`reservation_stuck` finding names that hold's allocation. It fences the
operation, deletes the NetBox prefix if one carries this allocation's markers,
then deletes the uncommitted allocation row and the consumer's own idempotency
record, leaving the allocation key free and the audit trail intact. It refuses a
committed allocation absolutely, it is never available to an operator, and it
cannot touch another tenant's hold.

The resource is the operation and not the allocation, deliberately.
`DELETE /v1/allocations/{id}` already means release-with-quarantine, and its
semantics are the opposite of this one: it applies only to a committed
allocation, it preserves the CIDR hold, and it retires the key for ever. Giving
it a second meaning for an uncommitted row would put the project's one
irreversible deletion of a ledger row behind the same verb and path as its
safest reversible operation, distinguished by a field — `Committed` — that no
client can see, since `Get` and `List` both require it and `Release` answers
`404 not_found` without it. ADR 0012's consequences already name that shape as
the danger: a one-word distinction in a path where every other guarantee is
enforced several times over. The operation, by contrast, is the resource the
consumer actually holds. The `202` handed them `Location: /v1/operations/{id}`,
the CLI's `awaitOperation` and the provider's `waitForAllocation` both poll it,
and `Service.Operation` already scopes it to the tenant and already refuses an
operator, so the cancel inherits an authorization check that exists, is tested,
and was reasoned about in ADR 0011.

The narrowing is what makes this safe, and it is not a policy knob. A cancel is
refused with `409 reservation_not_stuck` unless an open `reservation_stuck`
finding is keyed on the allocation the operation names. That finding is the
platform's own verdict, reached by `recoverReservations` on evidence it read,
age-gated past one observation interval, and already published to this tenant.
So a consumer cannot cancel a reservation that is merely slow, cannot cancel one
that is inside its own `Ensure`, and cannot cancel one whose refusal was
uncertain — those are precisely the cases H4 classifies as silence. The cancel
never appears on the hot path of a healthy reservation, which is the property
that keeps it from becoming a general-purpose undo of a `POST`.

Whose write this is, and what it is not. It is the consumer's, performed as the
tenant that authenticated, on a request that tenant made. ADR 0010's objection
to putting adoption behind an endpoint — that it acts as a tenant without that
tenant's authentication — does not apply here at all, and the audit trail is
stronger than adoption's for the same reason: the actor on the
`RESERVE_CANCELLED` event is `Principal.Subject`, a verified identity, where
`adopt abandon`'s `--operator` is an unverified string ADR 0010 recorded as
such. It is not the operator role of ADR 0011, which is a principal with no
tenant refused by every write path before any new code runs; `Service.Operation`
already refuses an operator's read of an operation, so the same comparison
refuses an operator's cancel, and ADR 0011 needs no amendment and gains no
write.

What a hostile or careless tenant gains from it is nothing, and the reasoning is
worth writing down because the fence is domain-wide. The only effect a cancel
has beyond the tenant's own hold is to *lift* a fence, never to raise one, and a
lifted fence is what every other tenant in the domain wants. It cannot reach
another tenant's hold, because the operation lookup compares `TenantID` exactly
as `GET /v1/operations/{id}` does today. It cannot be used to re-roll a CIDR,
because `chooseCIDR` is a deterministic first fit over the pool and a fresh
reservation under the same request lands on the same candidate. It cannot be
used to dodge quarantine, because quarantine exists to prove the absence of
cloud resources behind a committed allocation and an uncommitted hold has no
binding, no inventory id and nothing the ledger acknowledges as owned. And it
cannot be reached on purpose without first getting the platform to declare the
tenant's own reservation stuck, which requires an out-of-band prefix or a
foreign cloud resource on their own held CIDR. The worst a careless tenant does
is throw away a hold they could have rescued by fixing the ground truth, and the
answer to that is a fresh `POST`.

The adapter gains one operation, `CancelReservation(ctx, allocation,
operationID)` on the `domain.Inventory` port beside `AbandonAdoption`. It is on
the port for the argument ADR 0012 made: a capability that destroys an inventory
object should be impossible to add without every implementation and every test
double admitting whether it has it. Its evidence is the marker and nothing a
caller supplied. It searches by `findByMarker` for prefixes carrying this
allocation id, which is VRF-agnostic and so finds an object wherever a
misconfigured VRF left it; zero matches is success with nothing to do, more than
one is a refusal that names the duplicate-marker condition as the thing to fix
by hand, and the single match must also carry this operation's id in
`platform_operation_id`, so a prefix some other operation wrote is never
touched. It adds one refusal `AbandonAdoption` does not need: a matched prefix
that carries the import tag is refused and never deleted, because an imported
prefix was created by the onboarding import of
[ADR 0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md) and not by
`Ensure`, so a reservation's marker on it is evidence of an out-of-band edit
rather than of a create this platform performed. It then deletes the object and
confirms its absence by re-reading, as `Delete` already does, and treats a read
that still finds it as a refusal that has written nothing it can be blamed for.

The order of effects is forced, and it is ADR 0012's, for ADR 0012's reasons.
First a ledger transaction fences: it re-reads the allocation and its operation,
applies every refusal below, sets the operation to the terminal `FAILED`
carrying the code `reservation_cancelled`, appends one `RESERVE_CANCELLED`
event, and writes nothing else. From that moment no commit can happen, because
`reserve`'s commit closure and `commitRecovered` both re-check
`o.Status != operationPending` under the ledger lock and become no-ops,
`pendingRecoveryJobs` stops selecting the operation, `pendingDomain` stops
seeing it, and the domain is unfenced. Second, outside any transaction, the
adapter removes the prefix. Third, a ledger transaction deletes the allocation
row and the idempotency record, re-checking under the lock that the allocation
is still uncommitted and its operation is still the terminal one this cancel
wrote, and resolving the finding.

The invariant ADR 0012 states survives every interruption of that sequence, and
restated for a reservation it reads: no exit may leave a NetBox prefix carrying
an allocation's markers after that allocation's row is gone. After the fence the
ledger still holds the allocation and the prefix may still claim it, which is
consistent. After the removal the ledger still holds the allocation and nothing
claims it, which is also consistent — a hold with no inventory object is exactly
what every reservation starts as. Only then does the row disappear. The reverse
order would, on a crash between the two writes, leave a prefix claiming an
allocation that does not exist, and for a reservation there is no second place
that names the object: `InventoryID` is unset and there is no `AdoptionRecord`
carrying a reviewed id, so the only handle on that prefix would be the marker of
an allocation nothing remembers.

The adapter is called twice, immediately before the delete as well as after the
fence, for the reason the H2b review recorded on ADR 0012 and for one more of
this record's own. The port offers no read by marker, so a second idempotent
removal is stronger than a read: where nothing is marked it writes nothing and
answers at once, and where something re-marked the prefix it removes it again
and the cancel converges instead of asking for a re-run that would do exactly
this. Either way an answer of nil means no prefix carried the markers at the
moment it read them, which is the question the delete turns on, and anything
else stops the run with the allocation row still in place.

The race is the one ADR 0012 solved and one it did not have. A worker pass that
began before the fence already holds its job list, calls `Ensure`, and can
create the prefix after the first removal; its `commitRecovered` then writes
nothing, because the operation is no longer pending, but the markers would be
back, and the second removal is what answers that. The new one is the request's
own in-flight `Ensure`. `reserve` calls the adapter outside any transaction and
opens its commit transaction afterwards, so a cancel whose fence lands in
between meets a closure that finds `o.Status != operationPending`, copies the
current allocation and operation into the caller's variables and returns nil.
`reserve` then falls through to `return allocation, operation, 202, nil`, and
the transport's `outcome` writes `202` with `publicOperation` of an operation
whose status is already `FAILED`. That is a lie of the kind `abandoningReplay`
was added to prevent for adoptions, and it is also a schema violation, since the
`202` response is `PendingOperation` and that schema pins `status` to the
constant `PENDING`. The implementing package must fix it: after a commit closure
that found the operation no longer pending, `reserve` must answer from the
operation's real status rather than from the constant `202`. That is the one
line of `reserve` this work changes, and it is a defect that exists today for
`adopt abandon` too.

The window between the fence and the delete needs the same refusal ADR 0012
added. In it the hold is uncommitted with no pending operation, which every
replay path in `reserve` reads as "still being worked on" and answers `202` with
no operation — and with no operation the transport's `outcome` falls through to
`503 dependency_unavailable`, "Missing durable operation result". So
`abandonInProgress` is generalised to recognise a fenced operation of either
type and `abandoningReplay` answers `409 reservation_cancelling`, retryable, for
a `RESERVE` and keeps `409 adoption_abandoning` for an `ADOPT`; the four replay
sites in `reserve` and the two in `PlanAdoption` are unchanged, so the count of
places stays what H2b left.

The allocation key is free again, and for a consumer's own key the argument is
stronger than it was for an operator's. The row must go in any case:
`chooseCIDR` blocks on any allocation with `Committed` false before it looks at
the state, so a terminal uncommitted row would fence its CIDR against every
future reservation for ever; `countTenant` would keep charging the quota slot
unless the state were exactly `RELEASED`, which is one of the two values
`reserve`'s scans read as retired. Retirement is derived from
`State == QUARANTINED || RELEASED` on an allocation row, so no row means no
retirement, and the `allocations` table's uniqueness on `(tenant_id,
allocation_key)` is released with it. Beyond the mechanics, `allocation_key` is
what `docs/API_V1.md` section 5 calls the permanent logical identity, and for a
Terraform consumer it is a name in a configuration file — the provider derives
its idempotency key from it in `reserveKey`. Retiring it would mean every later
`terraform apply` failing with `409 allocation_key_retired` until somebody
renames the network, as the price of a hold that never owned anything. A
tombstone records that a CIDR *was* owned; an uncommitted hold was never
ownership. So the key is free, and what the ledger keeps afterwards is the
operation row in `FAILED`, the audit events, and a resolved finding.

The consumer's idempotency record is deleted in the same transaction, and this
is forced rather than chosen. `reserve` consults `st.Requests[requestID]` before
anything else; with the allocation gone, `stateResults` returns neither an
allocation nor an operation, the read path leaves `priorAllocation` nil, the
transaction's first branch returns with `allocation` nil, and `reserve` ends at
its final `apiErr(503, "ledger_error", "ledger returned no result")` — for that
tenant, under that `Idempotency-Key`, for ever. The delete therefore removes the
`POST` record keyed on the consumer's key and any other request record naming
this allocation, exactly as the abandon does.

So a retry under the same `Idempotency-Key` after a cancel is a fresh
reservation — a new allocation id, a new operation, a new `RESERVE_PLANNED` —
and not a replay of the failure. The honest case for that, rather than the
merely convenient one, is that this contract has two identities and says which
is the weaker. Section 5 makes `allocation_key` permanent and explicitly allows
the HTTP request record to expire after thirty days once terminal while
allocation-key tombstones remain; an expiry is a deletion, so removing the
record is something the published contract already permits a server to do. The
allocation-key identity, meanwhile, is genuinely gone, because the tenant asked
for it to be gone. There is also no precedent to follow: no `POST` idempotency
record can name a `FAILED` operation today, because the only terminal status
`reserve` ever writes on its own operation is `SUCCEEDED`, so a replay of a
failed create is a behaviour that would have to be invented rather than
preserved. Section 5's existing advice — retry a definitively failed allocation
under a new logical key or an audited operator recovery — needs one sentence
added by the implementing package: a cancelled reservation is retried under the
same key, because the cancel is the audited recovery and the tenant performed
it.

The rest follows. `reservation_stuck` is resolved in the delete transaction,
because nothing else ever would: it is keyed on the allocation id,
`reconcileAllocations` does not reset its code, and an orphaned open `CRITICAL`
would fail `client findings --fail-if-open` with exit 7 for that tenant and for
the operator for ever. The quota slot returns with the row. One audit event is
written, by the fence and not by the delete, so that an interrupted cancel that
converges on a re-run leaves exactly one; `domain.Event` carries no structured
details, so its reason string carries the allocation id, the CIDR, the key, the
operation id and the condition the finding recorded. A reason from the consumer
is not required, which is a deliberate difference from `adopt abandon`: section
5 says `DELETE` needs no body, the platform supplies the objective justification
in the finding, and a tenant ending their own dead request is not exercising
authority over anybody else.

What the cancel refuses, each for a stated reason. An unknown operation id, or
one belonging to another tenant, with the `404 not_found` `Service.Operation`
already answers, so the cancel leaks nothing an existing read did not. An
operation that is not a `RESERVE`, because a `VERIFY_BINDING`, a `RELEASE` or a
`RECLAIM` is a different lifecycle and an `ADOPT` is ADR 0012's, each naming the
other. An operation that has already succeeded, because the commit won the race
and the allocation is now owned; the answer is `DELETE /v1/allocations/{id}` and
everything section 7 says about quarantine. A committed allocation, which cannot
be reached through a pending `RESERVE` at all but is checked first anyway, so
that no later condition can be the reason a committed allocation is examined. A
pending operation with no open `reservation_stuck`. An inventory refusal or an
uncertain inventory answer, which leave the fence in place, delete nothing and
ask for a repeat, classified by `uncertainInventoryError` exactly as the
abandon's `abandonClearError` classifies its own. And a re-run whose operation
is no longer the terminal one this cancel wrote.

One mechanism cannot serve both `RESERVE` and `ADOPT`, and the boundary is
exactly where the two differ in fact. What is shared is the service-level shape:
the forced three-step order, the recognition of a fence by a stored code beside
a terminal status rather than by a reason string, the window refusal, the delete
transaction's re-checks, the removal of the idempotency record, the resolution
of the stuck finding, and the classification of an adapter answer into refusal
or uncertainty. What must not be shared is the adapter operation, because one
clears and one deletes, and the wrong choice in either direction is a real
defect: a cleared reservation prefix poisons its CIDR, and a deleted adoption
prefix destroys an import's occupancy. What must also not be shared is the
operation-type check. ADR 0012's `abandonCheck` refuses a `RESERVE` with
`abandon_not_an_adoption`, and that refusal is today one of the things keeping
`adopt abandon` away from a consumer's hold; the shared code must therefore take
the expected type from its caller rather than accept a disjunction, so that each
exit still refuses the other's operations by name.

Neither of ADR 0012's two properties is weakened, and a third is added. No
committed allocation can be abandoned: the shared code tests `Committed` once,
first, and the source-level test that pins it now covers two entry points
instead of one, which makes it stronger rather than weaker. No exit may leave an
inventory object claiming an allocation the ledger does not hold: restated above
for a reservation, with deletion in place of clearing and the same
fence-remove-delete order proving it under interruption. And no tenant may
affect another tenant's hold, which is new because the actor is new, and which
rests on the tenant comparison `Service.Operation` already makes.

The alternatives were weighed and each is rejected for a reason. An operator's
process-mode command, a sibling of `adopt abandon`, would need no contract
change and no provider change and would match ADR 0012 exactly; it is rejected
because it acts on somebody else's live request with no way to know whether that
consumer still wants it, because nothing in the system would tell the consumer
it had happened — their poll would simply start answering `FAILED` — and because
the consumer is the one principal who can both authenticate to the hold and know
the answer. A timeout, in which the platform itself fails an operation that has
been definitively refused for longer than a bound, is the most tempting of the
three: H4's classification already separates decision from uncertainty, the age
gate already exists, and the harm of a wrong timeout is bounded because an
uncommitted hold never entered anybody's Terraform state. It is rejected because
the bound cannot be chosen honestly. A bound short enough to be useful will fire
while a consumer or an operator is still removing the out-of-band prefix that
caused it, and a bound long enough not to interrupt them leaves the domain
fenced for hours anyway; and because the timeout would have to perform the same
marker search and deletion unattended, on the strength of a refusal read at one
instant, which is the reasoning ADR 0012 refused for adoptions when it rejected
"provably never happened". Doing nothing is rejected because the dead ends
enumerated in the Context are real: the duplicate-marker refusal cannot be fixed
by making the ground truth match, since the ground truth it objects to is the
platform's own markers, and hand-editing NetBox is the path ADR 0012 already
declined to leave as the only one.

The documentation changes in four places. `docs/API_V1.md` gains the endpoint in
section 2's table, the new error codes in section 8's table, the terminal
representation of a cancelled reservation in section 5 beside the pending one it
already shows, and the sentence about retrying under the same key.
`docs/FINDINGS.md` loses the "Open decision, not yet built" paragraph under
`reservation_stuck` and gains the cancel as a second resolution beside the
commit. `docs/CLIENTS.md` and the CLI's help text gain the new verb. And the
adoption runbook's section 12 sentence about what abandon is not gains its
mirror: a cancel applies only to a reservation that never committed, and nothing
returns a committed allocation to unreserved space except release and its
quarantine.

Ordering. This edits `internal/service`, `internal/domain`, `internal/netbox`,
`internal/transport`, `internal/cli`, `api/openapi.yaml`, `providers/terraform`
and `tests/e2e`, so it runs alone, after H2c has copied back — they share
`tests/e2e` and the service's test files — and after H4, whose
`reservation_stuck` it depends on and whose end-to-end test seeds precisely the
state a cancel test needs. It has nothing to do with H7, which is the chart.

The evidence that moves this record to accepted is a test list, and three items
head it because three properties must never regress. No committed allocation can
be cancelled by this path: a test attempting it for every state a committed
allocation can be in, each naming the check that refuses it, a test that a
`SUCCEEDED` operation is refused, and a source-level test in the style of ADR
0010's "three places" that a mutation of the `Committed` check breaks a test
rather than opening a delete. No cancel can leave a prefix carrying an
allocation's markers after the row is gone: adapter-level tests for a delete, an
idempotent second delete, a refusal when two prefixes claim the allocation, a
refusal when the single match carries another operation's id, a refusal when the
match carries the import tag, and service-level tests that interrupt the
sequence after the fence and after the removal and show a re-run converging,
plus one that re-marks the prefix between the removal and the delete and shows
the second call removing it again. And no tenant can affect another tenant's
hold: a cancel of another tenant's stuck reservation answers `404`, and an
operator principal answers `404` as well.

Beside them: a healthy pending reservation with no finding is refused
`409 reservation_not_stuck`; the key is reusable, shown by a fresh `POST` under
the same key and the same body producing a new allocation id rather than a
replay or a `503 ledger_error`; `GET /v1/operations/{id}` answers `200` with a
`FailedOperation` carrying `reservation_cancelled` and keeps answering it;
`GET /v1/allocations/{id}` answers `404` before and after, unchanged;
`reservation_stuck` is `RESOLVED` and `client findings --fail-if-open` recovers;
the quota slot returns; the domain answers reservations again, shown by another
tenant's reservation in the same domain succeeding immediately afterwards; the
`RESERVE_PLANNED` and `RESERVE_CANCELLED` events survive the deleted row in
`audit_events`; a `POST` arriving between the fence and the delete answers
`409 reservation_cancelling` rather than `202` or `503`; a commit closure that
finds its operation fenced answers the operation's real status rather than
`202`; and the OpenAPI document still validates and the response of every
altered path still matches its schema. End to end, extending H4's
`tests/e2e/test_e2e_reservation_stuck.py` against the real ledger and NetBox: a
seeded stuck reservation is cancelled by its own tenant's token, the out-of-band
prefix that caused it is untouched, no prefix carrying the allocation's markers
remains, the finding resolves, the domain answers reservations again, and the
same key reserves successfully afterwards — within the suite's budget, which the
cancel helps rather than strains, since it returns a quota slot.

The position is therefore: add `DELETE /v1/operations/{operation_id}`, available
to the tenant that owns the operation and to nobody else, acting only on a
pending `RESERVE` whose allocation carries an open `reservation_stuck` finding;
fence the operation to `FAILED` with `reservation_cancelled`, delete any prefix
carrying this allocation's marker and this operation's id, then delete the
uncommitted allocation row and the consumer's idempotency record and resolve the
finding; leave the allocation key free and the audit trail intact; refuse a
committed allocation absolutely; and leave `adopt abandon` and ADR 0011 exactly
as they are. The work is four cuts. The port and the adapter —
`internal/domain/types.go`, `internal/netbox/cancel.go` with its `httptest`
cases and every inventory double — at tier X, because it is `internal/netbox`.
The service — `internal/service/cancel.go`, the generalised window predicate in
`abandon.go`, the one line of `reserve`, and the third safety property in
`safety_contract_test.go` — at tier X, alone, because it is `internal/service`.
The contract and the clients — `api/openapi.yaml`, `internal/transport/http.go`,
`internal/cli/cli.go`'s new verb and its fix to `awaitOperation`, and the
provider's two diagnostics — at tier M with an X review. And the documentation
and the end-to-end evidence — `docs/API_V1.md`, `docs/FINDINGS.md`,
`docs/CLIENTS.md`, the runbook and `tests/e2e/test_e2e_reservation_stuck.py` —
at tier L with an X review, running after H2c has released the stack.

The clients need less than expected, and what they need is worth naming here
because H6 was asked to establish it. The Terraform provider already behaves
correctly: `waitForAllocation` turns a `FAILED` operation into an error carrying
the operation's own code, `Create`'s error path then calls
`FindAllocationByKey`, which after a cancel finds nothing because the row is
gone and `List` omits uncommitted rows in any case, so the recovery branch is
skipped, the diagnostic is raised and **no state is written** — which is the
right outcome, and a later `terraform apply` succeeds as a fresh reservation
under the same derived key. Two cosmetic defects should be fixed with it: the
diagnostic reads "HTTP 0" because that path constructs an `HTTPError` without a
status, and its advice does not mention the cancel. The provider must **not**
cancel on its own create timeout, and that is deliberate: a Terraform timeout is
routinely shorter than a reconciliation interval, and a provider that cancelled
on it would destroy holds that were one worker pass from committing. The CLI
needs a real fix rather than a cosmetic one: `awaitOperation` treats every
terminal status as success-shaped, so on a `FAILED` reserve it reads
`operation.AllocationID`, finds it non-empty, and issues
`GET /v1/allocations/{id}`, which answers `404` — the consumer is shown a
not-found error instead of the reason their reservation failed. It must print
the terminal operation, which carries the error, and exit on it; and it gains a
`cancel` verb taking the operation id, exiting `5` on a refusal and `0` on a
cancel that converged.

## Consequences

The project acquires its first tenant-facing operation that removes a ledger
row. ADR 0012's abandon is reachable only by somebody with a shell, a database
and NetBox credentials; this is reachable by anyone holding a tenant token, over
HTTP, and the only thing standing between a token and a deleted allocation row
is the pair of conditions above — uncommitted, and carrying an open
`reservation_stuck`. That is a narrower gate than it sounds, because the second
condition is written by the worker and not by the caller, but it means the test
that a committed allocation can never be cancelled has to be written against the
source and not only against behaviour, for the reason ADR 0012 gives: the
failure it guards against is a later change that helpfully relaxes the check.

The estate gains a second way for this platform to delete a NetBox prefix, after
`Delete`, and it is the first that finds its target by marker rather than by a
stored inventory id. Everything that reasons about NetBox state has so far been
able to assume that a prefix carrying ownership fields was either current or
being converted; after this it can also be one that is being destroyed, and a
snapshot taken in that window shows an owned prefix whose allocation id will
match nothing a moment later. Nothing decides anything on that combination
today, and ADR 0012 already said the same about its own window; the two windows
are now two, which is worth remembering the next time something is tempted to
reason from an owned prefix to a live allocation.

A cancelled reservation leaves the same asymmetric trace an abandoned adoption
leaves — audit events and a terminal operation naming an allocation id no row
has, and a resolved finding pointing at the same absent id — but it leaves it in
a tenant's own history rather than an operator's. A consumer reading their audit
trail can now find a `RESERVE_PLANNED` and a `RESERVE_CANCELLED` for an
allocation that `GET` answers `404` for, which is correct and is also the first
time a tenant-visible trail has referred to something that does not exist.

The exit depends on the worker. If the reconciliation loop is not running, no
`reservation_stuck` is ever raised, and a stuck reservation therefore cannot be
cancelled at all — the consumer's `DELETE` answers `409 reservation_not_stuck`
about a hold that is genuinely dead. That is the honest behaviour, because
without the worker nothing has established that the hold is dead, but it means
an outage of the worker turns a recoverable situation into an unreachable one
until it returns, and an operator meeting this should be told to start the
worker rather than to reach for the database.

One case has no exit, and it is stated rather than solved. If the tenant that
owns a stuck reservation never cancels it — a decommissioned pipeline, a rotated
token, a team that no longer exists — the domain stays fenced for every other
tenant and nobody else can lift it, because the cancel is scoped to the owning
tenant and the operator role may write nothing. Making the ground truth match
still works in most such cases and commits the hold into an ordinary allocation,
which the tenant could then release; but where the ground truth cannot be made
to match, the remaining path is the hand-editing ADR 0012 refused to accept. If
an operator ever meets that, the fix is a second record granting an operator
command that reuses this record's service method and its forced order, not a new
mechanism and not a widening of this endpoint.

What is not known is stated plainly. Whether `DELETE` on the operation is the
right shape, rather than `POST /v1/operations/{id}/cancel`, is a judgement about
what a reviewer of this API expects; `DELETE` was chosen because it is
idempotent by definition and needs no body, and the alternative would read
better only if a cancel ever needed parameters. Whether a prefix can carry both
the import tag and a reservation's marker in practice has not been observed —
the refusal costs one comparison and is included on principle. Whether the
narrowing to an open finding is too strict is unmeasured, and so is everything
else about how often this happens: H4's `reservation_stuck` end-to-end test had
not been run when this record was written, so no stuck reservation has yet
existed outside a unit test, and the value of an exit for a condition nobody has
met in production is an estimate. The NetBox behaviour this depends on is
measured, at least: ADR 0012's H2a probe against the development NetBox 4.6.7
deleted a throw-away prefix and confirmed its absence with a `404`, which is
exactly the shape `CancelReservation` needs. And whether a consumer would rather
cancel than wait for somebody to remove the offending prefix is unknown for the
same reason it is unknown for `adopt abandon`: nobody has been in the position
yet.

Measured against the development NetBox 4.6.7 on 2026-09-20, package H8a, on a
throw-away prefix in the documentation range 198.51.100.0/24 created in the
development VRF exactly as `Ensure` creates a reservation's: status `reserved`,
the ownership custom fields set, and no import tag, which the created object
confirmed with an empty tag list. `DELETE /api/ipam/prefixes/{id}/` does honour
`If-Match` against the weak ETag the detail `GET` returns, so the question this
record left open has the better of its two answers: a wrong value answered `412`
with `{"detail":"Precondition failed."}` and left the object in place, shown by
the `GET` that followed answering `200`, and the value read a moment earlier
answered `204`. The delete is therefore conditional exactly as ADR 0010's and
ADR 0012's `PATCH`es are, a `412` is a refusal and never a retry, and nothing
has to be invented to close the window between the checks and the write. The
re-read of the deleted prefix answered `404`, the list filtered by
`cf_platform_allocation_id` answered `200` with `count` zero, and a second
`DELETE` of the same id answered `404`, so an interrupted cancel that re-runs
converges on nothing to do rather than on an error. Nesting was measured
separately, because a stuck VPC reservation could in principle contain somebody
else's prefix: a second prefix, `198.51.100.128/25` carrying a foreign import
batch, was created inside the first, NetBox reported `"children": 1` on the
parent, and the parent's conditional `DELETE` answered `204` while the child
answered `200` and read back unchanged. NetBox neither refuses such a delete nor
cascades it -- prefix nesting is computed from the CIDR and is not a relation
the delete follows -- so the adapter needs no child check and gets none, and a
cancelled VPC reservation whose prefix contained another object leaves that
object exactly as it was, which for anything this platform did not create is the
unmanaged occupancy it already was. Everything the probe created was deleted and
shown gone, with `count` zero both within 198.51.100.0/24 and under the marker.
One thing went wrong and is recorded because it is the same class of mistake
this operation exists to make impossible: the probe's first attempt read the
wrong `id` out of the created object's JSON and deleted the development pool's
own container prefix, 10.64.0.0/16, identifying an object by position rather
than by its own marker. Only the development stack was affected, the seed
recreated the container idempotently under a new NetBox id -- which nothing
addresses, since `ParentPrefixID` is only required to be positive by
`internal/config/config.go` -- and the probe was rewritten to refuse to touch
any id whose prefix lies outside the documentation range before it measured
anything.

Dated note, 2026-09-21, package H8d, against the evidence paragraph above,
item by item. The three head properties: no committed allocation can be
cancelled -- demonstrated by a service test per committed state, a
`SUCCEEDED`-operation test, and the source-level test matching ADR 0010's
"three places" style (H8b's review: `committedHoldRefusal` tested once for
two entry points); not independently re-run by this package, but `go test
./...` is green across `internal/service` with this code in the tree. No
cancel leaves a prefix claiming a deleted allocation -- demonstrated by
adapter tests (H8a's review: seventeen mutation-tested guards, all killed,
covering a delete, an idempotent second delete, two claimants, another
operation's id, the import tag, and the confirmation read) and by service
tests interrupting the sequence after the fence and after the removal
(H8b's review: thirty-four guards, all killed); also demonstrated end to
end, this package's own evidence below. No tenant affects another
tenant's hold -- demonstrated by a service test (`404` for another tenant
and for the operator, H8b's review) and end to end, this package's
`test_02_another_tenant_and_the_operator_get_404`, which additionally
shows the two answers byte-identical (modulo `request_id`) to an unknown
operation id.

The items beside them. A healthy pending reservation refused
`409 reservation_not_stuck` -- service test and end to end (this
package's `test_01`, which further shows the refusal *before* the finding
has even opened, not only once it never opens). The key reusable under a
fresh `POST` -- service test and end to end (`test_05`: a new allocation
id, distinct from the cancelled one). `GET /v1/operations/{id}` answering
a `FailedOperation` carrying `reservation_cancelled` for ever -- transport
test (H8c's review) and end to end (`test_03`, `test_04`, which also
re-reads it after a converged repeat cancel). `GET /v1/allocations/{id}`
answering `404` before and after -- end to end (`test_03`); before the
cancel this package found the response is `404 not_found`, not the
`409 allocation_pending` section 5 documents for any uncommitted allocation
with a known operation -- `internal/service/service.go`'s `Get` returns
`404` for every uncommitted allocation regardless of a pending operation,
and no `allocation_pending` code exists anywhere under `internal/`; that is
a real, pre-existing documentation/implementation gap this package found
and is reporting rather than fixing, because it is unrelated to cancel,
predates this record, and would touch a `Get` every other module's tests
also exercise. `reservation_stuck` `RESOLVED` and `client findings
--fail-if-open` recovering -- end to end (`test_03`, read directly through
`GET /v1/findings`; not re-verified through the CLI gate itself, because a
full-suite run's other modules can hold unrelated findings open at the same
moment, which would make that particular check a fact about the whole
run rather than about this allocation). The quota slot returning --
end to end (`test_03`: `capacity`'s `pending` count for `/22` strictly
decreases across the cancel; `capacity_when_complete` could not supply the
"before" reading, because `internal/service/capacity.go` marks the whole
pool incomplete while any operation fences its domain, which a pending
hold structurally is). The domain answering reservations again -- end to
end, demonstrated the strong way: `test_05`'s fresh reservation under the
same key actually succeeds, not merely a read-only probe. The
`RESERVE_PLANNED`/`RESERVE_CANCELLED` events surviving the deleted row --
end to end (`test_03`, read from `audit_events` directly). A `POST`
landing inside the fence-to-delete window answering
`409 reservation_cancelling`, and a commit closure that finds its
operation fenced answering the real status rather than `202` -- **NOT
demonstrated** by this package: both need a race inside `reserve`'s own
in-flight `Ensure` call, which H8b's review states it drove through test
hooks rather than a real race, and this package did not attempt one either;
service-test coverage only. The OpenAPI document validating and every
altered response matching its schema -- `scripts/ai/check-contract`
PASSED with this code in the tree (this package's own run).

End to end, against the real ledger and NetBox, extending
`tests/e2e/test_e2e_reservation_stuck.py` with `ReservationCancelE2ETest`:
a seeded stuck reservation cancelled by its own tenant's token; the
foreign, out-of-band prefix that caused the refusal left untouched; no
prefix anywhere carrying the allocation's marker; the finding resolved;
the domain answering reservations again, shown by a fresh reservation
under the freed key succeeding; and a repeat cancel converging
(`already_fenced`, `deleted` true, `removed` false) rather than refusing.
Also demonstrated, beyond what the evidence paragraph above asked for: the
CLI (`platform-ipam client cancel --operation-id`), one JSON document,
exit `0` on success and exit `5` with the error envelope for an unknown
operation id; and the fifth route this record's Context names -- a
reservation whose prefix `Ensure` had already created, stuck instead by a
foreign, untagged cloud resource appearing on its CIDR
(`pendingReservationObservationSafe` refusing before `Ensure` is ever
called again) -- cancelled, with the pre-created prefix confirmed deleted
by its own id and absent by marker. Not demonstrated: nesting (a stuck
reservation's prefix containing another object), which ADR 0012's own H2a
probe already measured for the shared adapter behavior and this record's
own probe paragraph above repeats for a cancel specifically; and any real
concurrency, on either side of the fence. The full suite ran green in two
halves under one run id, 59 and 53 tests (the second including this
module's own new coverage), with `go test ./...`, `go vet ./...` and
`gofmt` clean across `cmd` and `internal`, and `scripts/ai/check-contract`
PASSED.

Dated note, 2026-09-21, package E3. The documentation/implementation gap
the H8d note above left open -- `GET /v1/allocations/{id}` answering
`404` for an uncommitted allocation with a known `PENDING` operation,
where `docs/API_V1.md` section 5 promised `409 allocation_pending` -- is
closed: `Service.Get` now returns that code, with the operation id under
`error.details.operation_id`, for the owning tenant only; every other
caller keeps the `404` it always had, and
`tests/e2e/test_e2e_reservation_stuck.py`'s `ReservationCancelE2ETest`
now asserts the `409` before its seeded cancel instead of recording the
mismatch.
