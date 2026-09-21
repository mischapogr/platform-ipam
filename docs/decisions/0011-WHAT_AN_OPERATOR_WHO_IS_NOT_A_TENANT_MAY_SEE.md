# ADR 0011: No eligible pool is a refusal, and an operator has no tenant

Status: accepted, 2026-09-18, as a design; built by packages G3a, G3c and G3b1
to G3b5 of the [work plan](../WORK_PLAN.md), complete on 2026-09-20. Every test
named in the evidence paragraph of the Decision exists and passes, with two
limits stated rather than hidden. The rejection of an identity file carrying
`role` by an older binary is shown as the mechanism (strict decoding refuses an
unknown field), not with a real older binary. The collapse of two tenants'
copies of one finding into one operator row is proven at the service boundary
only: the development stack has a single eligible tenant, so end to end the
operator's findings always equal the developer's, and what is shown there
instead is the point of the whole track — the same gate reaches the same verdict
for a tenant and for the operator, and refuses a verdict for an identity that
can see nothing. Proposed by package G1.

## Context

[ADR 0008](0008-PLATFORM_OWNED_OPERATOR_UI.md) found this and left it open, and
the code confirms it. Authentication maps a verified subject onto a server-side
identity and refuses an unknown one with `403 forbidden`, "Identity is not
onboarded to platform-ipam" (`Authenticate` in `internal/transport/auth.go`,
`83-87`). Onboarding demands a tenant: `Validate` rejects an identity without a
subject, a `tenant_id` and non-empty accounts, environments and regions
(`internal/config/config.go:226-236`). Nothing anywhere requires that tenant to
appear in any pool's `eligible_tenants`. An identity under a tenant such as
`ops` is therefore perfectly legal, and every read compares that tenant with
the object's: allocations in `Get` and `List`
(`internal/service/service.go:282`, `295`), findings in `Findings` (`:596`),
pools in `Pools` (`:582`, through `eligibleString`). Such a caller receives
`200` and an empty list, which is indistinguishable from a system with nothing
in it.

The sharp part is not that the reads are scoped. It is that the write path
already answers honestly and the read path does not. `validateRequest` walks the
pools and, finding none that matches the principal's tenant and target, returns
`422 policy_violation`, "no eligible pool matches the request"
(`internal/service/service.go:977-979`). Capacity is honest too: its pool
selection requires tenant eligibility plus the principal's environment, region
and one of its accounts, and otherwise returns `404 not_found`, "pool not found"
(`capacity` in `internal/service/capacity.go:54`, `63-65`). So of the endpoints
an `ops` identity can reach, two already refuse and three answer "nothing". That
inconsistency is not a design; it is an accident of where the tenant comparison
happens to sit in each function.

Domain-level findings do not exist as records. The reconciler's occupancy pass
iterates the domains, then the pools of that domain, then the `EligibleTenants`
of each pool, and writes one finding per tenant keyed on the domain, that
tenant, the account, the region, the resource type and the resource id
(`reconcileAllocations` in `internal/service/worker.go:212-226`, the key at
`:217`). No row carries an empty tenant. One unmanaged VPC in a domain with
three eligible tenants is three rows. The public projection then drops
`TenantID`, `DomainID` and the resource identity entirely, emitting id,
severity, code, a null allocation id, account, region, the two timestamps and
status (`findings` in `internal/transport/http.go:397`), so those three rows
differ only in an opaque hash. An identity that is eligible for no pool is in
none of those loops and sees none of them.

The consequence a pipeline can feel is recent. `platform-ipam client findings
--fail-if-open` (`findings` in `internal/cli/cli.go:561`) walks every page,
refuses a payload it cannot read, and turns "at least one returned finding is
OPEN" into exit code 7 (`ExitFindings`, `internal/cli/cli.go:33-42`). Run under
an `ops` identity there are no pages, nothing is OPEN, and it exits 0. A gate
whose verdict depends on who was given the credential is not a gate.

Two contract rules frame every fix. `docs/API_V1.md` section 1 forbids accepting
`tenant_id`, `owner` or caller-supplied role claims as authorization, and
requires that an unauthorized object id return `404` without revealing another
tenant's allocation; `api/openapi.yaml`'s own description repeats the first. And
`domain.Principal` carries a subject, a tenant, accounts, environments and
regions and no role (`internal/domain/types.go:31-37`) — a gap ADR 0008 named
and [ADR 0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md) worked around
by making `adopt` a process mode that acts *as* a tenant, with the operator's
own subject appearing only as the actor string in an audit event.

One mechanical fact shapes what any answer may cost. Every response schema in
`api/openapi.yaml` is `additionalProperties: false` — `Allocation`, `Finding`,
`Pool` and all three page envelopes — even though `docs/API_V1.md` section 1
instructs clients to tolerate additive fields. A field that exists only for
operators is therefore a schema change, not an additive one, and cannot be
smuggled in as tolerated extra data.

## Decision

Both, staged, with the refusal narrowed to one endpoint rather than three. The
work plan's option (a) says "a distinct error on the list endpoints"; that is
the right instinct applied too widely, and the code says where it is wrong.

An identity may legitimately be onboarded before its pool exists. Configuration
and identities are separate files and `IPAM_IDENTITY_FILE` replaces the identity
list wholesale when set (`Load` in `internal/config/config.go:102-110`), so the
two roll independently and a new tenant's credential can land first. More
importantly, an empty `/v1/allocations` is the permanent normal state of a
tenant that has not reserved anything yet, and an empty `/v1/findings` is what a
healthy estate looks like — it is precisely the answer the gate is supposed to
be able to trust. Turning either into an error conflates "you are entitled to
nothing" with "there is nothing", and the second is a fact the API must keep
being able to state. `/v1/pools` is different in kind. It is the entitlement
endpoint, and its empty answer is never a legitimate steady state, because the
same configuration makes every reservation from that principal fail with `422
policy_violation`. Refusing there is not new policy; it is the read side finally
agreeing with the write side.

Stage one is therefore a warning at load and a refusal at one endpoint. At
configuration load, an identity whose tenant appears in no pool's
`eligible_tenants` is logged as a warning rather than rejected, for the ordering
reason above. At the API, `GET /v1/pools` answers `403` with a distinct code,
`no_eligible_pool`, when the principal is eligible for no pool, and every other
endpoint is untouched. The refusal belongs in the transport handler (`pools` in
`internal/transport/http.go`) rather than in `Service.Pools`: the service's
answer, "none", is correct, and the decision to call an empty entitlement an
error is a contract decision about a response. That placement also keeps stage
one out of `internal/service` entirely, which matters while Track F is
rewriting `Reserve` in that package.

Stage one costs the published contract almost nothing. `/v1/pools` already
declares a `403` response in `api/openapi.yaml`, and `OperationError.code` is an
unconstrained string, so no schema changes: only the `Forbidden` response's
description and the error table in `docs/API_V1.md` section 8 gain the new code.
No new field, no new query parameter. Its effect on clients is small but not
nil, and must not be reported as nil. The consumer CLI's `capacity` verb already
meets a `404` for such an identity and is unaffected. Package G2's gate asks
`GET /v1/pools` first and treats an empty list as grounds to refuse a verdict;
after stage one it meets a `403` instead, and left alone would take its error
branch and exit with its auth code rather than its own. Both are non-zero, so
the property G2 exists for survives either way, but a pipeline should not see
the code for one condition change under it: stage one therefore also teaches
the gate that `403 no_eligible_pool` from `/v1/pools`, and only that, means
"not eligible" — it still prints the findings and still exits `8`. The Terraform
provider does list pools:
`GetPool` fetches `/v1/pools` and scans the page for the requested id
(`providers/terraform/internal/client/client.go:297-310`), synthesizing its own
`not_found` when it is absent, and `poolDataSource.Read` surfaces any transport
error as "Unable to read pool". A tenant eligible for no pool thus gets a
clearer diagnostic where it previously got a synthesized `not_found`. Only a
deployment whose reservations already fail is affected, and the provider needs
no code change.

Stage two is the role, and its shape is the whole of the argument: an operator
is a principal with no tenant. The identity entry carries `role: operator` and
omits `tenant_id`, `accounts`, `environments` and `regions`; the role is decided
by the server-side file and never read from a token claim, so `docs/API_V1.md`
section 1 holds unchanged. Because the role is the *absence* of a tenant rather
than a tenant with extra rights, every existing comparison already refuses it
before any new code runs. `Reserve` stops at `validateRequest`'s first gate,
`403 forbidden`, "authenticated principal has no tenant"
(`internal/service/service.go:883-885`). `Patch`, `Bind`, `Release`, `Get` and
`Operation` each compare the allocation's tenant and return `404`. `List`,
`Findings` and `Pools` match nothing. `Capacity` returns `404`. An operator
therefore starts from deny-everything, and every read it is given has to be
granted explicitly, in a named place, one at a time. That is safer than any
allow-list bolted onto a tenant principal, and it depends on one invariant worth
testing rather than assuming: no allocation, operation or finding can carry an
empty tenant. For allocations and operations that holds today, because the
tenant is copied from a principal that passed the gate just quoted, and
`Validate` requires `tenant_id` on every identity. For findings it does not
hold yet, and review of this record found the gap: the occupancy fan-out takes
the tenant from each pool's `eligible_tenants`, and `Validate` checks only that
the list is non-empty, not that its entries are
(`internal/config/config.go`, the pool rule beside `len(p.EligibleTenants) ==
0`). A pool listing an empty string would store findings with an empty tenant
and would make `eligibleString` match an operator in `Pools` and `capacity`.
That rule — an empty entry in `eligible_tenants` fails the load — does not
depend on the role and closes a gap that exists today, so it ships with stage
one's configuration change rather than waiting for stage two. Track F's `adopt`
must
likewise refuse an acting principal without a tenant.

What an operator may read is `GET /v1/pools` (every pool, not an eligible
subset), `GET /v1/pools/{id}/capacity` for any pool, `GET /v1/findings`
domain-wide and de-duplicated, `GET /v1/allocations` across tenants, and `GET
/v1/allocations/{id}` for any committed allocation; the last two expose
`tenant_id`, which no response carries today (`publicAllocation` in
`internal/transport/http.go:445-459`). What it may never do is reserve, patch,
bind or release: those are refused by the checks just listed, and the test that
proves it is worth more than the code that would add a role check to each. `GET
/v1/operations/{id}` is deliberately not granted in v1. An operation id is not
discoverable except from a tenant's own `202` or error payload, so granting it
buys an operator almost nothing while widening the surface that has to keep
answering `404` correctly; if an operator ever needs it, that is a later,
separate grant.

An operator reading any allocation by id is acceptable, and the reason is
[ADR 0006](0006-OPERATOR_UI_AUTHENTICATION.md) rather than charity. The
population that authenticates through that proxy lands in the read-only
`platform-operators` group and can read every prefix in every VRF, including
the `platform_allocation_id` and `platform_allocation_key` custom fields the
adapter writes. Anyone who can log into the inventory can already enumerate
the estate's allocation ids. Refusing
the same facts over the API would protect nothing and would make the platform
less legible than the inventory it populates. The caveat is that the two
populations are not the same list — see below — so this argument is about the
information, not about the people.

`adopt` is not unlocked by this role and must not be. ADR 0010 makes adoption a
process mode precisely because it acts as a tenant: it must produce the request
hash the owning team's first `POST` will produce, it writes the ledger, and it
holds ledger, NetBox and cloud credentials at once. An operator principal has no
tenant and so cannot even form that request, which is a compile-and-config fact
rather than a policy check. Restricting who may run `adopt` stays a deployment
control, exactly as ADR 0010 records.

Findings need a real domain view, and the fan-out is why a naive union will not
do. Two eligible tenants over one unmanaged VPC produce two stored rows whose
public projections are identical except for an opaque id, so a cross-tenant list
would show one problem twice with no way for the reader to tell, and any count
an operator derived would be a function of tenancy configuration rather than of
the estate. The grouping key cannot be recovered from what is stored: the
resource type and id exist only inside the hashed finding id
(`findingID` in `internal/service/service.go:1404-1407`, fed the composite
string built at `internal/service/worker.go:217`), and collapsing on the fields
the DTO does expose — code, account, region, timestamp — could merge two
genuinely different resources observed in the same pass, which is unacceptable
in the one view that is supposed to be the truth. So `domain.Finding` gains a
resource type and a resource id, set by the fan-out; the operator view groups on
domain, code, account, region, resource type and resource id, keeps the earliest
`first_observed_at` and the latest `last_observed_at`, and exposes `domain_id`
and the resource id to operators only. `tenant_id` appears on an operator's
finding only where `allocation_id` does, because a finding without an allocation
is domain-level by nature and naming one of its recipients would be misleading.
The honest model — one domain-level record with the per-tenant rows as a
projection — is deliberately deferred: it would make the tenant read path
compute pool eligibility at read time, in the same functions Track F is
rewriting, and it would change what tenants see. The price of deferring is that
storage stays linear in the number of tenants per domain. It is small: findings
are resolved and re-opened on every reconciliation pass, so the new fields fill
themselves in within one worker interval and need no migration.

Capacity needs no domain-wide redesign, because it is already domain-wide where
it counts. The occupied set it computes includes every non-parent snapshot
network, every observed resource CIDR and every non-released allocation in the
domain irrespective of tenant (`internal/service/capacity.go:95-142`); only the
lifecycle breakdown is tenant-scoped, by a single conjunct, `a.TenantID ==
p.TenantID && a.Scope == "vpc"` (`:127`). The operator change is to select the
pool by id alone and to drop that one conjunct. `allocatable` is already the
same number for everybody.

Pagination and ordering survive unchanged for allocations. `page` sorts on the
item id and advances the cursor by `id > cursor`
(`internal/transport/http.go:401-438`), and allocation and finding ids are
random or hashed but total and stable, so widening the filter widens the page
and nothing else. The collapse is the one place this could break: if the
surviving row of a group were chosen by map iteration, the id would differ
between calls and a cursor could skip or repeat a row. The representative is
therefore the lexicographically smallest id in the group, which makes the
operator's finding list as stable as a tenant's. The cost to name is that
`List` already scans every allocation under one global advisory lock and `page`
already sorts the whole slice, so an operator list is that same full scan with a
wider predicate — a constant factor on work that is already O(ledger), not a new
access pattern.

The identity file expresses the role as one scalar field with exactly two legal
values, and configuration validation must catch four things: a `role` that is
neither empty nor `operator`, which must fail rather than quietly mean "no
role", because that is how a typo becomes a privilege; an operator carrying a
`tenant_id`, which would make it a tenant with cross-tenant reads and conflate
the audit trail; an operator carrying accounts, environments or regions, which
gate only `validateRequest` and `capacity` and would sit on the principal
looking like constraints while constraining nothing; and, unchanged, the
existing duplicate-subject rule, which already prevents one subject being both.
A separate `operators:` section was rejected on evidence: the identity file is
decoded into an anonymous wrapper with `KnownFields(true)`
(`decode` in `internal/config/config.go:113-129`, `:120`), so a new top-level
key is refused by exactly the same mechanism as a new field, buying no
compatibility while adding a second subject namespace to keep disjoint. The
compatibility direction is worth stating precisely: an existing file without
`role` decodes unchanged, and a file *with* `role` fails on an older binary at
start, loudly. It is a forward-incompatible config change, so the binary is
deployed before the file and a rollback needs the old file back. The Compose
fixture gains an operator entry; the existing `developer` identity
(`deploy/compose/fixtures/identities.yaml`) is never converted, or every
end-to-end test that reserves would break.

Adding a field to `domain.Principal` is mechanically cheap and semantically
not. Of the eleven occurrences of `domain.Principal{` in the repository, ten
are keyed literals in tests and the eleventh declares the empty map in
`NewAuth`; the only production source of a principal is the YAML decode in
`config.Load`, since `NewAuth` merely indexes what it was handed, so the field
is additive and compiles everywhere untouched. The real radius is the ten
functions that compare a principal's tenant — `validateRequest` on behalf of
`Reserve`, `Get`, `List`, `Patch`, `Bind`, `Release`, `Operation`, `Pools` and
`Findings` in `internal/service/service.go` and `capacity` in
`internal/service/capacity.go` — of which four are writes (`Reserve`, `Patch`,
`Bind`, `Release`) that must be left exactly as they are. Track F's acting
principal for `adopt` must leave the role
empty, and the sibling of package F2's "three places" source-parsing test
belongs here: nothing may construct a principal with the operator role outside
`config.Load`.

Operator reads are logged, but not in the ledger. `domain.Event` is
allocation-scoped and every event is written inside `Ledger.Update`, which
ADR 0010 documents as a stop-the-world rewrite of nine tables under one global
advisory lock; turning each operator `GET` into one of those would be a denial
of service against allocation. Reads are logged with `slog` at the transport
layer, where failures are logged already, recording subject, method, path and
the number of rows returned, for operator principals only. The limitation is
stated rather than papered over: an application log is not tamper-evident and is
not the audit trail the ledger is.

This does not contradict ADR 0006 or ADR 0008, and it is worth being explicit
about why. ADR 0006 governs a different population with a different credential:
browser users authenticated by the proxy against Entra, LDAP or a bcrypt hash,
landing in a NetBox group. This record governs subjects in the platform identity
file presenting bearer tokens. Nothing here maps one to the other, and the
absence of that mapping is deliberate — building it is exactly the work ADR 0008
says a platform-owned UI or a first-party NetBox plugin would need. ADR 0008
names the trigger to revisit as "the first time someone must answer the capacity
or the quarantine question for a tenant they do not belong to", and says that
when it arrives the expensive part must be built regardless and only the
rendering stays open. This record is that expensive part, arriving because a CLI
gate needs it rather than because a browser does. It removes one of the three
costs ADR 0008 priced — the role and the cross-tenant read — and touches none of
the other two, the session with its CSRF protection and the OIDC
authorization-code client. So the argument "the role exists now, therefore the
UI is cheap" is answered in advance: the role was built for a machine caller,
and ADR 0008's recommendation rests on browser machinery this record does not
provide.

Ordering against Track F follows from where the edits land. Stage one changes
`internal/config/config.go`, `internal/transport/http.go`, `api/openapi.yaml`
and `docs/API_V1.md`, and no file in `internal/service`, so it is safe beside
F1, F2 and F3. Stage two edits the read functions of
`internal/service/service.go`, the pool selection in
`internal/service/capacity.go` and the fan-out in `internal/service/worker.go` —
the last two of which F2 and F3 are rewriting — so it starts after F3 lands, as
the work plan already requires of package G3.

The evidence that moves this record to accepted is a test list, and one item
heads it because one property must never regress: a non-operator can never learn
that another tenant's object exists. The existing guard is
`TestTenantAndInternalFields` in `internal/transport/http_test.go`, which
asserts `404` for a foreign allocation id and no foreign row in the list; it
must be extended so that both hold *with an operator identity configured*,
proving the operator's existence changes nothing for tenants. Beside it: a
tenant response contains no `tenant_id`, `domain_id` or resource id field, in
the style of the neighbouring projection test; each of the four writes attempted
with an operator credential is refused, each test naming the existing check that
refuses it, so a later change that helpfully gives the operator a tenant breaks
a test instead of opening a write; a role value nobody defined fails config
load, as do an operator with a tenant, an operator with accounts,
environments or regions, and a pool with an empty `eligible_tenants` entry; an
identity file carrying `role` is rejected by the
previous binary, recorded once as a rollback fact; a domain with two eligible
tenants and one unmanaged resource stores two findings and shows an operator
exactly one row, with the earliest `first_observed_at` and the same surviving id
across repeated calls; an operator list paged with `limit=1` visits every row
exactly once; nothing constructs the operator role outside `config.Load`; and
end to end, `tests/e2e/test_e2e_cli_findings.py` gains an operator run that sees
findings the tenant cannot, while the allocation suite's existing invariants
still pass.

The position is therefore: refuse an empty entitlement at `GET /v1/pools` alone
with a new `no_eligible_pool` code and warn at load, now, in transport and
configuration only; then, once Track F has left `internal/service`, add a
read-only `operator` role that is a principal with no tenant, granting
cross-tenant reads on pools, capacity, findings and allocations with `tenant_id`
exposed only to it, refusing every write through the checks that already exist,
de-duplicating the finding fan-out on a resource identity added to
`domain.Finding`, and leaving `adopt`, the OpenAPI schemas' tenant-facing shape
and the Terraform provider alone.

## Consequences

The API gains its first principal that is not a tenant, and with it the first
authorization question that is not answerable by one string comparison. That is
the cost being accepted: today "can this caller see this object" is a single
equality that a reader can verify in each function, and afterwards it is an
equality plus a role, in ten functions, four of which must never consult the
role
at all. The mitigation is the shape rather than a promise — an operator with no
tenant is refused by every one of those comparisons by default, so the failure
mode of a forgotten grant is that an operator sees too little, never that a
tenant sees too much.

Two lists of operators now exist and nothing reconciles them: the NetBox side
governed by ADR 0006, and the platform identity file. The same person needs an
entry in both, granted by different administrators through different mechanisms,
and removing someone from one does not remove them from the other. No check is
proposed here, and that is the largest operational weakness of this record. It
is also the strongest argument for the NetBox plugin that ADR 0008 names as the
fallback, since a plugin would have to build the mapping that is missing.

Stage one makes one currently silent misconfiguration loud, and in doing so
changes the exit code that package G2's gate produces for exactly that identity.
Both codes are non-zero, so no pipeline that was failing starts passing; a
pipeline that distinguishes exit codes will need updating, which is the correct
kind of breakage and is documented rather than avoided.

Keeping the per-tenant fan-out means the operator view is a de-duplication of a
denormalized store rather than a read of the fact itself. Storage grows with the
number of tenants eligible in a domain, and any future change to the fan-out key
has to change the grouping key in the same commit or the operator view silently
starts double-counting again. The test that a two-tenant domain shows exactly
one row is what makes that coupling visible.

What is not known is stated plainly. Nobody has run any of this: there is one
identity in `deploy/compose/fixtures/identities.yaml` and no second tenant
anywhere in the fixtures, so "two eligible tenants produce two findings" is read
from `internal/service/worker.go` and has never been observed. The cost of a
cross-tenant list is unmeasured at every scale, as ADR 0010 says of the ledger
generally. Whether operators want `GET /v1/operations/{id}` is unknown because
no operator has used this API. Whether a static identity file is the right home
for operators at all is doubtful for the same reason ADR 0006 gives for `basic`
mode — a static list with no rotation workflow — and the answer would be the
same one ADR 0006 reached, which is that it is acceptable as a starting point
and not as a target state. And the population question underneath all of it is
open: this record says what an operator may see, and not who decides that
someone is one.
