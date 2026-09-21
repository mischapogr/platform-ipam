# ADR 0015: A migration plan is reviewed input, and its progress is derived

Status: accepted, 2026-09-22, as a design, and built the same day by packages
M3b1 to M3b4 of the work plan as `platform-ipam onboard progress` and
`platform-ipam client evidence` ([migration
progress](../MIGRATION_PROGRESS.md)); demonstrated against synthetic estates and
plans only, never against a real plan, estate or deployment. Where building it
showed this record to be wrong, silent or self-contradictory, the dated
paragraph at its end says so and wins over the text above it. It was proposed by
package M3a of the [work plan](../WORK_PLAN.md) and is the first design for gap
M3 of the [overlap-migration analysis](../IP_OVERLAP_MIGRATION.md), which
deliberately declines to commit an API, a schema or a CLI name through itself.
It leaves [ADR 0014](0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md) unamended and
answers the one question that record left open — whether its `decisions.yaml`
seam is the right shape — by reading that seam without widening it. It changes
nothing in [ADR 0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md), whose
import keeps collapsing networks into prefixes, and nothing in [ADR
0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md), whose adoption stays
the only way an existing network gains an owner: this record reads adoption's
results and adds no second path to them. It adds no endpoint, so [ADR
0011](0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md) is untouched, though
its boundary decides what a progress report is permitted to know. It decides
nothing the owner's open questions reserve for the customer; the last paragraphs
of the Consequences name each dependency on them.

## Context

Gap M3 is one sentence in the owner's analysis and every clause of it is a
requirement: a "versioned report linking old resources, proposed targets, later
committed allocations, owners, dependencies, blockers, rollback and
verification results", with "drafts never reserve space". Section 9.2 of the
same document adds the two halves that sentence assumes — a "reviewed target
plan" whose "draft targets are not reserved addresses", and a "migration
record" tracking "owners, dependencies, waves, approvals, actual allocations,
cutover/rollback evidence and remaining unknowns", with execution owned by the
customer's own infrastructure workflow. Section 4.2 says which networks stay
and which move is decided from dependencies, stateful services, maintenance
windows and migration cost; section 4.4 says old and new ranges both stay
occupied through the agreed rollback window, and that "one deleted duplicate VPC
does not make a range unused if another still uses it".

Nothing in the checkout records any of it. The assessment of ADR 0014 stops
exactly where a plan begins, and says so: "Remediation, responsible owner and
decision status do not live in the report", and "the reviewed migration plan
itself — waves, targets, approvals, cutover and rollback evidence — remains gap
M3 and is not this record's." The [overlap
assessment](../OVERLAP_ASSESSMENT.md) repeats it for an operator reading a
report: the tool "never says what to do next".

What the assessment does leave behind is the vocabulary a plan needs. A
conflict id is `c-` followed by sixteen hex characters of a SHA-256 over the
relationship kind and the two resource identities, sorted before hashing, and
it excludes everything that is not those three things — not the observation
time, not the file or row, not ownership, not the matrix, not the impact — so a
reviewer's decision survives a re-collection, a matrix supplied for the first
time, and an inventory that grew (`ConflictID` in
`internal/assess/identity.go`). A resource identity is
`aws:<account>:<region>:<vpc>:<cidr>` with `:<association>` appended when the
association id is known (`ResourceRecord.Identity`), and "different VPCs" is
the account, region and VPC id triple, which the engine already computes as an
unexported `vpcKey`. Coverage is a first-class result with a single `complete`
boolean that selects one of exactly two summary templates and mechanically
forbids any claim of completeness, and a test asserts the incomplete rendering
contains no member of `ForbiddenCompletePhrases`. `decisions.yaml` is keyed by
conflict id and carries a decision, a remediation note, a responsible party and
who reviewed it when (`assess.Decision`), and the report counts decided,
undecided and stale entries, a stale entry being one whose conflict id no
longer appears.

The ledger can already say more about a target than any plan could assert. An
allocation carries a tenant, an allocation key that is "the permanent logical
identity", the immutable request it was created from, a CIDR, a state and a
binding (`domain.Allocation`, `domain.Binding`). `RESERVED` means the ledger
holds the space and the inventory carries the prefix; `ACTIVE` means the
platform itself verified a cloud binding against a trusted observation —
`bindingObserved` demands a resource whose id, type, account and region match,
whose `platform-ipam:allocation-id` and `platform-ipam:allocation-key` tags
match the allocation, and whose primary CIDR equals the issued CIDR, as ADR
0010's Context quotes line by line — and the binding then carries a
`verified_at`. `QUARANTINED` and `RELEASED` mean the key is retired
permanently, and a later request under it is refused `409
allocation_key_retired` ([API v1](../API_V1.md) sections 5 and 7). A request
whose immutable fields disagree with an existing allocation under the same
tenant and key is refused `409 allocation_key_conflict`. Every one of those is
a fact about evidence the platform gathered, and none of them is a box a person
ticked.

The offline command family is the other precedent. `platform-ipam onboard` is
dispatched before the ledger is constructed, and ADR 0014 put `assess` in that
family because it needs strictly less than every other member: no ledger, no
NetBox, no cloud credentials, no pools configuration. `internal/onboardcmd`
gives it four exit codes — `0`, `2` usage, `3` a report that is not clean, `4`
no report at all — and `assess` reads no `IPAM_` variable and constructs no
adapter, pinned by `TestAssessRunsWithEmptyEnvironment` and
`TestAssessSourceImportsExcludeAdapterPackages`. The verb `plan` in that family
is already taken by the import planner, which is a different `plan` entirely.

Three constraints frame everything else. The first is the project's own
boundary, in `AGENTS.md`: "Terraform planning and data-source reads never
allocate." A migration draft is planning; if drafting one could hold space, the
draft would be a reservation with a different name, and the one constructor ADR
0010 protects would have acquired a second entrance. The second is ADR 0011's
tenancy. Every API read compares the object's tenant with the principal's; an
operator is a principal with *no* tenant that may read pools, capacity,
findings and allocations across tenants, and it is deployment-wide — the
owner's analysis says plainly in section 9.4 that "the existing
deployment-wide operator role is not a customer-scoped MSP role". A migration
plan spans tenants by nature: it names old resources in a hundred accounts and
targets that will be reserved by several different teams. The third is that the
consumer CLI cannot export what such a plan would need today. `client list`
issues one `GET /v1/allocations` and prints the payload it got, so it returns
the server's first page and its `next_cursor` and stops; only `client findings`
walks every page and refuses a payload it cannot read (`--fail-if-open`).
Neither records who asked, and therefore neither records what the answer could
have seen.

Finally, section 9.3 of the owner's document is a warning written in advance
against the artifact this record describes. Report "no observed conflict within
this scope as of this time" with coverage and blockers. Qualify readiness "as
address compatibility for a specified target design". "Show separate counts for
complete/expected account-region observations, required connection pairs with
conflicts/unknowns, approved/completed migration waves, and traffic paths
tested. Any percentage needs a defined denominator and snapshot time; missing
accounts or untested paths must not disappear and improve the score."

## Decision

The reviewed migration plan is a versioned file the customer writes and keeps,
and its progress is a report derived from evidence, produced by a second
offline command in the `onboard` family that shares the assessment's engine,
reads the plan, reads the same organization inventory, reads an authenticated
export of committed allocations, and writes nothing anywhere. The plan carries
intent and never an address. The report carries facts and never a verdict: it
reports three independent derived facts per planned move and refuses to
collapse them into one word, it prints no percentage at all, and it can never
claim completeness while the evidence behind any of those facts is missing.

### The artifact: two reviewed files and one derived report

The plan is `migration.yaml`, a document in the customer's own repository
beside the `matrix.yaml`, `ownership.yaml` and `decisions.yaml` the assessment
already reads. It is reviewed the way code is reviewed, and nothing in this
project writes it.

The progress report is the output of `platform-ipam onboard progress`, a pure
function of the plan, the inventory, the reviewed inputs and one allocation
evidence file. It is a report in exactly the sense ADR 0014's is: one `Report`
value with two encoders so the machine form and the human form cannot disagree,
byte-identical across runs, every input listed with the SHA-256 of its bytes,
and no wall-clock field of its own.

The third thing this record deliberately does not create is a store. There is
no plan table in the ledger, no plan object in NetBox, and no endpoint. The
reasons are under "Tenancy" and "Versioning and review" below, and the
consequence is stated here so it cannot be missed: the platform is never the
custodian of the plan, and therefore never has to authorize, validate or serve
it.

### The unit is a move, and its identity is the VPC that moves

The candidates were a conflict, a wave, and an old-resource-to-target mapping.
A conflict is the wrong unit because resolving one conflict usually means
moving one of its two sides, and moving one VPC usually resolves several
conflicts at once: renumbering a VPC that collides with five others is one
decision, one owner, one window and one rollback, and recording it five times
would multiply a reviewer's work by the estate's accident. A wave is the wrong
unit because it is a grouping of decisions, not a decision. The unit is
therefore a **move**: one old VPC, what is to become of it, and — where
something is to replace it — the target that will.

A move's subject identity is `aws:<account>:<region>:<vpc>`, which is ADR
0014's resource identity truncated before the CIDR, and is the same triple the
engine's `vpcKey` already computes. The CIDR is excluded on purpose. A VPC's
primary IPv4 CIDR cannot be removed and a secondary association can come and
go, so including the CIDR would make a plan entry lose its identity the moment
somebody associated a range — which is one of the remediation paths under
discussion. A VPC id is not globally unique either, which is why the account
and region stay in the identity; the assessment already reports the same VPC id
read under two accounts as a data error rather than guessing.

What happens when an identity stops appearing has four answers, and each is
reported rather than inferred.

A subject that no inventory row mentions, where coverage is **complete** for
its account and region, is `not-observed`. That is evidence the VPC is gone; it
is not evidence that the move succeeded, and it is emphatically not evidence
that its addresses are free. The owner's section 4.4 rule holds: one deleted
duplicate VPC does not make a range unused while another still uses it, and
what a range's occupancy requires is gap M9's question, not this report's.

A subject that no inventory row mentions where coverage is **incomplete** for
its account and region is `unknown`. Never `not-observed`. This is ADR 0014's
coverage rule applied one level up, and it is the single place where a
migration report would most like to lie: an unreadable account is the cheapest
way for a VPC to look retired.

A conflict id a move claims to resolve, which the current assessment does not
report, is `stale` for that move, exactly as ADR 0014 already treats a stale
decision and for the same two reasons: the conflict may have been resolved, or
its resources may have changed underneath the id. Stale ids are counted in the
summary and listed per move; they are never dropped.

A subject that appears in no assessment at all — not as a conflict side, not as
an observed VPC — is `unmatched`, counted separately. A plan naming a resource
the inventory has never contained is either about something outside the scope
that was read or about a typo, and the report says that it cannot tell the two
apart.

### What a reviewer types

The plan is one document with a `version`, an optional `plan_id`, a list of
`waves` and a list of `moves`. Every field below is typed by a person, is
carried into the report verbatim, and is never treated as evidence.

A wave has an `id`, a `name`, an optional `window` (free text — a maintenance
window is a sentence, not a schema), an `owner`, and an `approval` with
`approved_by`, `approved_at` and `approves`, the last being what was approved
in the approver's own words.

A move has a `subject` (`account_id`, `region`, `vpc_id`), a `disposition`, a
`wave` naming a wave id, an `owner`, an optional `approval` of the same shape
as a wave's, a `depends_on` list of other subjects, a `blockers` list of
`{id, description}`, a `rollback` (free text: the agreed rollback and the
window it is valid in), a `verification` list of `{id, description, ran_by,
ran_at, outcome}`, a `resolves` list of conflict ids, and `notes`.

The `disposition` vocabulary is exactly four values and the fourth is the
honest one: `replace` (a new VPC takes over and this one is retired), `keep`
(this VPC stays and is brought under management), `retire` (this VPC goes and
nothing replaces it), and `undecided`. A fifth, for adding a unique secondary
range to an existing VPC, is deliberately **not** defined here: managed
secondary associations are gap M7 and need their own contract for association
identity, parents, observation and release, and the owner's question Q6 decides
whether that path is needed at all. A plan that must express it says so in
`notes` and the move stays `undecided`, which the report counts and never
hides.

A `replace` move carries a `target`, and a target is the immutable half of the
request the owning team will eventually make: `tenant_id`, `allocation_key`,
`scope`, `environment`, `region`, `account_id`, `prefix_length`, and
`parent_allocation_key` for a subnet. Those are exactly the fields
`requestHash` covers once description and labels are zeroed (ADR 0010's
Context), which is what makes the derived comparison below an equality rather
than a judgement. A `keep` move carries a target with a `tenant_id` and an
`allocation_key` only: adoption pins the CIDR the VPC already has, so nothing
else about it is a choice.

Verifications are typed, including their outcome, because no offline tool can
run a traffic test and this one must never look as though it had. They are
printed verbatim, counted in their own block, and can never move a derived
fact. Approvals are typed for a harder reason: the platform cannot verify that
the named approver had authority, exactly as ADR 0010 records that `adopt`'s
actor "is a string the command was given". The mitigation is not a check; it is
that the plan is reviewed in the customer's version control, where the approval
and the diff it approves sit in one history.

### What is derived, and the three facts it is never collapsed into

Everything else in the report is computed, and every computed field has an
`unknown` that is reachable and tested.

Per move the report derives exactly three independent facts.

**Subject**: `observed`, `not-observed` or `unknown`, by the rules above, with
the coverage entry that made it `unknown` named beside it.

**Target**: `none`, `reserved`, `active`, `retired`, `mismatched` or `unknown`.
`none` means the allocation evidence could have seen an allocation for this
tenant and key and contains none. `reserved` means it contains a committed
allocation under that tenant and key whose immutable request fields equal the
plan's. `active` means the same allocation is `ACTIVE` and its binding carries
a `verified_at` — the platform's own verification, not a claim. `retired` means
the allocation is `QUARANTINED` or `RELEASED`, so the key is dead and the
owning team's request under it will be refused `409 allocation_key_retired`;
the plan must name a different key, which is a plan edit and therefore a
review. `mismatched` means an allocation exists under that tenant and key whose
immutable fields differ from the plan's, which is a concrete prediction that
the team's first request will be refused `409 allocation_key_conflict`, with
the differing field names listed. `unknown` means the evidence could not have
seen it — there was none, or its scope excludes that tenant.

**Conflicts**: for each id in `resolves`, `present`, `absent` or `stale`, plus
the conflicts the current assessment reports on this subject that the move does
*not* claim to resolve, listed as `unclaimed`. A move that resolves nothing it
claimed and has picked up two conflicts it never mentioned is the case a plan
most needs to surface.

These three are never combined into a state. There is no `done`, no `complete`
and no `migrated` field anywhere in the report, and that absence is the
mechanism rather than a matter of taste: a single state would have to pick a
winner among three facts with three different `unknown`s, and every reader
would quote the word instead of the facts. The one aggregate the report allows
is a count of moves for which all three facts are affirmative at once — target
`active`, subject `not-observed`, every claimed conflict `absent`, and no
unclaimed conflict — and it is rendered as a sentence naming those four
conditions, never as a label. A wave rolls up the same way: counts per fact,
side by side, each with its denominator.

Derived counts and typed counts are printed in two separate blocks and are
never added together. That is the direct answer to the owner's section 9.3: an
approved wave and a completed one are different facts, and "traffic paths
tested" is an assertion by people that belongs beside, not inside, the numbers
the tool computed.

### The plan has nowhere to write a CIDR

A target has no CIDR field, and neither does anything else a reviewer types.
The strict decoder refuses an unknown field, as every reviewed input of ADR
0014 already does, so an operator who writes `cidr:` under a target gets an
error naming the field rather than a plan the tool silently half-reads.

This is the whole of "a draft never reserves space", expressed as a schema. A
plan that named addresses would be read as an intention to hold them, would be
compared against capacity, and would eventually be asked to check that its
intended ranges are still free — and the first person to ask the tool to "check
my plan is still allocatable" would be asking for a reservation with the
receipt withheld. The correct place for an intended range is a reservation,
which is what the platform is for; until the owning team makes one, the
report's answer to "what will this become" is `none`, which is the truth. A
customer who wants to record an intended range for discussion writes it in
`notes`, where nothing reads it, and this record says so plainly rather than
pretending the wish does not exist.

The plan does carry `prefix_length`, because size is what capacity planning
needs and what the owner's section 4.2 asks the network team to confirm: that
the replacement pool is unused throughout the connected estate and has room for
old and new to coexist. A size is not an address.

Three mechanisms keep the draft honest and only one of them is care. The schema
has no field. The command opens no ledger, constructs no NetBox client and
reads no `IPAM_` variable, pinned by the same two tests `assess` already has.
And the engine lives in a package that imports neither `internal/netbox`,
`internal/storage`, `internal/service`, `internal/cloud` nor the AWS SDK, which
makes most of it a property of the import graph rather than of vigilance.

### Where it lives, and what it may never do

`platform-ipam onboard progress`. The `onboard` family, for ADR 0014's reason
exactly: it needs strictly less than every other member — no ledger, no NetBox,
no cloud credentials, no pools configuration — and `onboard` is dispatched
before the ledger is constructed. The verb is `progress` rather than `plan`
because `onboard plan` is the import planner and means something else entirely;
the collision is worth naming here so that nobody renames it later.

Its flags are `--plan` (repeatable, YAML or JSON), the assessment's own input
flags unchanged (`--inventory`, or `--networks` repeatable with `--failures`,
`--accounts` and `--run`; `--matrix`, `--ownership`, `--fixed`, `--decisions`),
`--allocations` naming an evidence file (repeatable), `--format json|text`
defaulting to text, `--out` writing the same bytes to exactly one path,
`--stamp`, and `--max-relationships` passed straight through to the engine.

It calls `assess.Assess` in process rather than reading a report `onboard
assess` produced. One comparison engine means the two commands can never
disagree about what a conflict is or what a conflict id is, and a reviewer gets
that consistency without holding two files in step. The rejected alternative —
`--assessment report.json` — would need a digest cross-check against the
inventory to stop a progress report mixing conflict statements from one
snapshot with observation statements from another, and would add a second
decoder for a rendering rather than an input. The progress report carries the
assessment's `inputs`, `input_limits`, `coverage` and `summary` verbatim, so a
reader sees both halves of one snapshot, and does not duplicate the conflict
list, which `onboard assess` already prints.

Exit codes are the family's four with the meanings ADR 0014 gave them. `0` is a
report whose evidence is complete and in which nothing is blocked, stale,
mismatched, unclaimed or unplanned. `3` is a report that was produced —
printed on stdout exactly as for `0` — and is not clean by that test, which
includes every incomplete-evidence case, so a plan over an estate that cannot
be read completely never exits `0`. `4` is no report: an input missing or
unreadable, a plan that does not decode, a structural refusal from the failure
list below, or the engine's own `--max-relationships` refusal. `2` is usage.

It may never write anywhere but stdout, stderr and a single `--out` path; never
open the ledger; never construct a NetBox client; never call AWS; never load
pools configuration; never claim reachability; never claim that evidence is
complete when it is not; and never print a percentage.

### Tenancy, authentication and the allocation evidence

The target facts need the ledger, and the ledger is behind an authenticated
API. There were three ways to reach it and this record takes the third.

Opening the ledger directly is refused: it would give an offline reporting tool
ledger credentials, break the `onboard` family's one structural promise, and
put a reader inside the global advisory lock every allocation contends for.
Calling the API from the command is refused: it would make an offline report
need a token, a network path and a live service to say anything, and the
assessment's whole value is that a customer can run it before deploying
anything.

The command therefore reads an **allocation evidence file** produced by an
ordinary authenticated read and handed to it, exactly as the matrix and the
ownership table are. The file is a JSON document carrying the allocations that
read returned, the instant of the read, and — the field that makes the rest
safe — the **scope** of the principal that produced it: the literal `operator`,
or the tenant id. With that, the report can distinguish "no allocation exists
for this key" from "this evidence could not have seen it", which is the same
distinction ADR 0014 draws between "read and empty" and "not read", and which
nothing in a bare list of allocations can express by itself.

The scope rule is mechanical. An `operator` export covers every tenant, because
ADR 0011 grants an operator cross-tenant reads on `GET /v1/allocations` with
`tenant_id` on each row. A tenant export covers exactly that tenant, and every
move whose target names another tenant is `unknown` — never `none`. An export
whose scope field is absent is treated as covering nothing, and the report is
incomplete. Several exports may be supplied and their scopes union, which is
how a customer with no operator credential still assembles a complete picture
from one export per team.

This is also the answer to "a tenant must never learn another tenant's plan".
The platform never serves the plan, so there is nothing to leak through it; the
plan is a file, and who may read it is a permission in the customer's
repository. What the platform does control is the evidence, and there the
existing rule already holds without a new check: a tenant's export contains
only that tenant's allocations, so a tenant running `onboard progress` over a
plan that names other tenants' targets learns `unknown` and nothing else. The
report says so in `input_limits` rather than leaving a reader to infer it.

Producing that file needs one small addition to the consumer CLI, and the gap
is worth naming precisely because it is easy to assume away. `client list`
issues one `GET /v1/allocations` and prints the payload, so it returns the
first page and a `next_cursor`; only `client findings` walks every page and
refuses a payload it cannot read. The evidence export must do what `findings`
does — walk every page, refuse an unreadable payload — and additionally record
the read instant and the principal's scope. Where the scope comes from is left
to the package that builds it: the CLI knows which credential it used but not
what role the server assigned it, so either the export infers the scope from
what it observed (`tenant_id` present on every row is an operator read, since
no other caller receives that field) or the server must say. The second is a
contract change and is not proposed here.

No endpoint is added. A `GET /v1/migration-plans` was considered and refused on
three grounds. It would make the platform the custodian of a document it cannot
validate, which is authority without evidence. It has no answer in the identity
model: a plan spans tenants, ADR 0011's operator is the only principal that can
read across them and is deployment-wide rather than customer-scoped, so the
endpoint would either be operator-only — locking out the application teams who
own the moves — or would have to filter a document whose rows name other
tenants' resources, which is precisely the leak. And its write path would be
`Ledger.Update`, a stop-the-world rewrite of nine tables under one global lock
(ADR 0010), for a document a reviewer edits many times a day. Storing the plan
in NetBox was refused for a different reason: NetBox is the intended inventory,
[ADR 0009](0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md) already settled that the
AWS plugin is a view and never an allocation input, and a plan row about a VPC
nobody has imported has no object to hang on.

### Coverage, the two templates, the forbidden list and no percentage

The report carries one boolean, `evidence.complete`, and it is true if and only
if the embedded assessment's `coverage.complete` is true, at least one
allocation evidence file was supplied, and the union of those files' scopes
covers every tenant the plan's targets name. Otherwise it is false. Every
rendering of the summary is selected by that field from exactly two templates.
There is no third template and no free-text summary anywhere in the tool.

The incomplete template always names what is missing — the accounts and
account-region pairs the assessment could not read, and the tenants no evidence
could have seen — and never contains the complete template's sentence. The
forbidden list extends ADR 0014's with the words a migration report would reach
for: `conflict-free`, `no conflicts`, `ready`, `clean`, `complete`, `done`,
`migrated`, `cut over`, `reachable`, `unreachable`. A test asserts that the
text produced with `evidence.complete` false contains none of them in either
format, and asserts the template selection rather than one example.

The report prints no percentage. Not a bounded one, not one with its
denominator beside it. The owner's section 9.3 asks for a defined denominator
and a snapshot time for any percentage, and the honest form of that requirement
is counts: "14 of 57 moves have an active target" says the same thing and
cannot be quoted without its denominator. The rule is mechanical — a test
asserts the rendered text contains no `%` character at all, which is cheap
because Go's own format verbs leave none behind.

The assessment's reachability disclaimer is carried into this report verbatim
and printed once, because a progress report is the artifact most likely to be
read as a readiness statement. Beside it the report prints, once, that it
describes address compatibility for the target design the plan names, and
nothing about routes, Transit Gateway attachments, propagations, security
controls, DNS or application connectivity.

### Versioning and review: a file, not rows in the ledger

The plan's unit of change is a review, and version control already gives a
review a history, a diff, an author and an approval. The ledger gives none of
those: its records are allocation-scoped, its writes are stop-the-world, and a
row has no notion of a proposed change. Rows would also have to be
tenant-scoped, which a cross-tenant plan cannot be.

The costs are real and are accepted rather than solved. Nothing stops two
people keeping two copies of the plan, and nothing stops an out-of-date plan
being handed to the command. The mitigations are the ones the assessment
already uses: every plan file is listed in the report's `inputs` with the
SHA-256 of its bytes, so two reports can be proved to be about the same plan or
not; the plan carries its own `version` and `plan_id`; and `--plan` is
repeatable with a duplicate-subject refusal across files, so splitting a plan
per product is supported and silently forking one is not.

The plan is versioned; the report is not. The report is a function, and
re-running it over the same inputs is how a reader gets the current answer. The
`--stamp` flag is the only place an archival label enters, exactly as in ADR
0014, and it is the only field in the report that did not come out of an input
file.

### Relation to reservations, to adoption, and to ADR 0014's decisions seam

A plan entry **names an allocation key; it never creates one**. The key in the
plan is the key the owning team will use in its own `POST /v1/allocations` or
its Terraform configuration, and the target's other fields are the immutable
request that key will carry, so the derived comparison is an equality. Nothing
in this design reserves, adopts, patches, binds or releases anything, and the
command that produces the report could not if it wanted to: it holds no
credential that would let it.

Adoption is where the plan and ADR 0010 touch. A `keep` move is the adoption
case, and the report derives a blocker for it that no reviewer would think to
type: while the current assessment still reports any conflict one of whose
sides is this subject, the VPC is **not adoptable**, because `reviewedOccupancy`
refuses an adoption whose candidate overlaps a second unmanaged prefix or a
second observed resource, and a conflicting VPC in the same domain is exactly
that. The owner's section 4.3 says it in words too: keeping a VPC "does not
mean it can immediately be adopted: conflicting old VPCs still visible in its
domain must first be resolved." The report therefore says, per `keep` move,
whether the conflicts blocking its adoption are still reported. The exemption
ADR 0010's third dated paragraph adds for a reviewed VPC's own observed subnets
needs no special case here, because a subnet is never compared by the
assessment and so can never be one of those conflicts.

A `replace` move's target, once reserved, is an ordinary allocation with an
ordinary lifecycle, and this record adds nothing to it. The one thing worth
stating is what a released target means: the key is retired for ever, the
target fact becomes `retired`, and the plan must name a different key. The
report never proposes one.

ADR 0014's `decisions.yaml` seam is judged and kept. It is right in kind: a
reviewer's verdict on a *fact*, keyed by the id of that fact, echoed into the
assessment's own report, with stale entries counted so that a month of
decisions cannot quietly detach from the conflicts they were about. It is
insufficient in extent, and that is not a defect: a decision is per conflict, a
migration is per subject, and one move resolves many conflicts. So the plan is
a **second** reviewed file with its own unit, and the link between them is the
`resolves` list, read in one direction only. Nothing in this design writes
`decisions.yaml`, and nothing adds plan fields to it. Growing `decisions.yaml`
into the plan was the obvious alternative and is refused: it would make the
assessment's report — which must stay a pure function of observation and
reviewed intent, and byte-identical across runs — depend on migration state
that changes daily, and the assessment would start reporting on something it
cannot observe.

The two files are cross-checked and the check runs both ways. A move claiming a
conflict id the assessment does not report is `stale`. A conflict the
assessment reports on a subject no move mentions is `unplanned`, counted, and
listed: that is how a reviewer learns the estate grew a conflict the plan never
considered, which the owner's section 9.2 names as its sixth outcome,
"detecting newly introduced conflicts and coverage regressions". A conflict
carrying a decision in `decisions.yaml` but no move is a note rather than an
error, because "accepted risk" is a legitimate decision that needs no
migration.

### Determinism, the output and confidentiality

Determinism is ADR 0014's, unchanged and extended to the new collections. Moves
are sorted by wave id, then by subject identity; waves by id; every list is a
sorted slice; no Go map is encoded directly; the encoder is `encoding/json`
with two-space indentation and a trailing newline. Two runs over one input are
byte-identical, and so is a run over the same plan with its moves written in a
different order. The report carries no wall clock; every instant in it — an
observation time, an evidence read instant, an approval's date — came out of an
input.

The JSON carries `report_version`, `stamp`, `inputs`, `input_limits`,
`assessment` (the embedded `inputs`, `input_limits`, `coverage` and `summary`),
`evidence` (its `complete` flag, the scopes, the read instants and the
digests), `summary` with its two separated blocks, `waves`, `moves` and
`notes`. The text form renders the same value: the selected summary sentence,
the derived counts, the typed counts, a fixed-width table of moves — subject,
disposition, wave, owner, then the three derived facts — the wave roll-ups, the
unplanned conflicts, and the notes in full.

Customer inventory never enters Git, and a migration plan is customer data of
the same kind with names attached: it carries account ids, VPC ids, product
names, owners' names and maintenance windows. `.gitignore` already excludes
`inventory/`; a plan belongs with it or in the customer's own repository, and a
plan file committed to this checkout is a mistake rather than a fixture.
Fixtures are synthetic and follow what the repository already does — the
all-zero account `000000000000`, RFC 1918 and RFC 5737 ranges, obviously fake
resource ids — and the package repeats `internal/assess`'s mechanical guard
that no fixture contains a twelve-digit account id other than the all-zero one.

### Failure modes

The plan drifts from the estate and nobody notices. This is the default failure
of every planning document, and the answer is that drift is what the report
computes: stale conflict ids, unmatched subjects, unplanned conflicts and
mismatched targets are all counted in the summary, not buried per row.

A draft becomes a reservation. Prevented three ways, none of which is a rule
somebody must remember: there is no field, there is no credential, and the
import graph refuses the packages that could allocate.

An approval is typed by somebody without the authority to give it. Not solvable
here, and said so: the platform cannot verify the approver. What it can do is
print the approval verbatim, never convert it into a state, and never let it
move a derived fact.

The evidence is stale. The report carries the evidence file's read instant and
digest and hides neither, but it cannot tell a file read an hour ago from one
read last month except by that instant, and an operator who exports once and
reports for a week gets a week-old answer with an honest timestamp. No
freshness rule is proposed, because the tool has no clock of its own and
inventing one would put a wall-clock field in a report that must stay
byte-identical.

A target key is reused. Two moves naming one `tenant_id` and `allocation_key`
is a structural refusal, exit `4`: mapping two old VPCs onto one key is either
a merge — which needs a design this record does not attempt — or a mistake.

The dependency graph is wrong. A `depends_on` naming a subject no move defines,
a `wave` naming a wave no document defines, and a cycle in `depends_on` are
each a structural refusal, exit `4`, decided before any evidence is read.

Coverage regresses between runs. An account that was readable last month and is
not readable today turns affirmative facts into `unknown`, and the report's
counts move backwards. That is correct, and it will look like a regression in a
status meeting; the alternative — a report with a memory — is a report that
asserts what it can no longer see.

The report is quoted as readiness. The most likely misuse, and the one section
9.3 warns about. Mitigated by the absence of a `done` field, the two separated
count blocks, the forbidden list, the absence of percentages, and the two
disclaimers printed once each. Not eliminated: a number on a slide acquires
authority nobody granted it.

### What this record deliberately does not do

It does not model connectivity. Intent still comes from the reviewed matrix of
gap M2 and from nowhere else, and this record reads the assessment's impact
classification without adding to it.

It does not schedule. A wave has a window written as a sentence, and the tool
never compares it with a clock, never orders waves by date and never says a
wave is late.

It does not execute. Terraform or another separately authorized provisioner
creates the new AWS resources from committed allocations, and application and
network owners handle data migration, DNS, access controls, TGW configuration,
cutover and rollback, exactly as the owner's section 4.3 says.

It does not authorize a retirement. A `not-observed` subject is a fact about
observation. Whether an imported prefix may then be removed, and what evidence
that needs, is gap M9 and question Q12, and this report never says a range is
free.

It does not define a secondary-CIDR disposition. That is gap M7, conditional on
question Q6.

It does not add an endpoint, a schema to `api/openapi.yaml`, a ledger table, a
NetBox object, a finding code, or any CLI verb other than the evidence export
named above.

### The evidence that moves this record to accepted

A test list, and two items head it because two properties must never regress.

**No move is reported as fully evidenced on typed input alone.** A
table-driven test over every route to "not fully evidenced" — no allocation
evidence at all; evidence whose scope excludes the target's tenant; an
allocation that exists but is `RESERVED`; an `ACTIVE` allocation whose binding
carries no `verified_at`; a subject whose account and region lie in a coverage
gap; a claimed conflict that is still present; an unclaimed conflict on the
subject — asserts that the move is not counted as fully evidenced, that
`evidence.complete` is false wherever the cause is missing evidence, that the
incomplete template was chosen, and that the rendered text in both formats
contains no member of the forbidden list and no `%` character. Beside it, a
mutation that flips any typed field — an approval, a verification outcome, a
blocker's text — must change no derived fact, which is the assertion that
typed input cannot lie.

**A draft never reserves space.** A source-parsing test that the new package
and the command import none of `internal/netbox`, `internal/storage`,
`internal/service`, `internal/cloud`, `internal/transport` or the AWS SDK, in
the style of `TestAssessSourceImportsExcludeAdapterPackages`; a run with the
environment cleared; a test that a full run creates no file when `--out` is
absent and exactly one when it is present; and a test that a plan carrying a
`cidr` key anywhere is refused by the strict decoder, naming the field.

Then the definitions, each with a positive and a negative fixture. A subject
identity unchanged when a secondary association appears or disappears, and
changed when the account or the region differs. `not-observed` under complete
coverage against `unknown` under incomplete coverage for the same missing
subject — the pair that matters most. A claimed conflict id absent from the
assessment reported `stale`, against one still present reported `present`. A
conflict on a subject no move names reported `unplanned`. A `keep` move whose
subject still carries a reported conflict marked not adoptable, against one
whose conflicts are gone. A target `retired` for a `QUARANTINED` and for a
`RELEASED` allocation. A target `mismatched` naming the differing immutable
fields, once for a prefix length and once for an environment. Two moves on one
target key refused; a `depends_on` cycle refused; an undefined wave id refused;
a duplicate subject across two `--plan` files refused.

Then determinism: two runs byte-identical; the same plan with its moves and
waves reordered producing the same report; and both encoders carrying the same
facts for one input. Then the embedded assessment: the report's
`assessment.coverage` and `assessment.summary` equal to what `onboard assess`
prints for the same inputs, asserted by running both.

And finally end to end, from `internal/assess/estategen`'s generated estate.
The gapped estate — which already plants an unassumable account, a partial
region, a region read and empty, and an `ACTIVE` account never attempted —
gains a synthetic plan over its planted conflicts, and the run must reproduce
every planted fact: a move whose subject lies in the unread account is
`unknown` and not `not-observed`; the moves over the planted `equal-cidr` pairs
list those exact conflict ids as `present`; `evidence.complete` is false with
no allocation evidence supplied; the exit is `3`. Its clean sibling, with a
plan whose targets are all supplied by a synthetic operator-scoped evidence
file and whose subjects are all still observed, exits `3` as well — because
nothing has moved yet — and this record says so in advance so that nobody
builds a fixture that reaches `0` by leaving evidence out. A third fixture, in
which every move's three facts are affirmative, is the only one that exits `0`.

### The position, and the cut of M3b

The position is therefore: keep the reviewed migration plan out of the
platform, as a versioned file in the customer's repository whose unit is a
**move** identified by the VPC that moves; give a move exactly four
dispositions, with the secondary-range case deferred to gap M7; let a reviewer
type intent, owners, waves, approvals, dependencies, blockers, rollback and
verification outcomes, and give them nowhere to write an address; add
`platform-ipam onboard progress`, an offline command in the `onboard` family
that calls the assessment's own engine, reads the plan and an authenticated
allocation evidence file whose scope it records, and derives three independent
facts per move — subject, target and conflicts — which it never collapses into
a single state; add no endpoint and no ledger record, for the tenancy and
custody reasons above; carry ADR 0014's coverage discipline forward as one
`evidence.complete` field, two templates, an extended forbidden list and no
percentage at all; read ADR 0014's `decisions.yaml` seam without widening it;
and exit `0` fully evidenced, `3` a report that is not, `4` no report, `2`
usage.

Package M3b is four cuts, each independently reviewable.

**M3b1, tier M:** the plan schema and its decoder in a new package
`internal/migrate` — the plan, wave and move types, the strict YAML-or-JSON
decode through the repository's existing library with unknown fields refused,
the evidence file's own type, and the structural validation that produces exit
`4`: a duplicate subject within or across files, two moves on one target key, a
`depends_on` naming an undefined subject, a cycle, an undefined wave id, a
`replace` move with no target, a `keep` move whose target names more than a
tenant and a key, and any `cidr` key anywhere. Its checker is external — the
decoder plus a table of refusal fixtures — which is what makes it an M.

**M3b2, tier L with an X review:** the derivation. Given a decoded plan, an
`assess.Report` with the `Input` it was computed from, and zero or more
evidence files, compute the three facts per move, the wave roll-ups, the
unplanned and stale lists, the two count blocks, `evidence.complete`, the two
templates and the forbidden-list guard — with no I/O beyond decoding what it is
handed, and with the two head properties among its table tests. This is where a
false "fully evidenced" could be produced, which is what makes it an L.

**M3b3, tier M:** the command, `internal/onboardcmd/progress.go`, in the shape
of `assess.go`: the flags, the in-process `assess.Assess` call with the same
options, the evidence decoder, `--out`, the four exit codes, and the tests that
it reads no `IPAM_` variable and constructs no adapter.

**M3b4, tier M, alone:** the evidence export — whatever `internal/cli` must
gain so that an authenticated read walks every page, refuses an unreadable
payload, and records its read instant and its scope — plus the end-to-end run
from `estategen`'s estate and a synthetic plan through `onboard progress`, a
new `docs/MIGRATION_PROGRESS.md`, pointers from the assessment and import
documents, the index rows, and a dated implementation note in this record. It
is the only package that touches the consumer CLI, so it runs by itself.

M3b1 and M3b2 may run in parallel if M3b2 defines the input types it needs and
M3b1 maps onto them, exactly as M1b1 and M1b2 did; M3b3 waits for M3b2; M3b4
waits for M3b3.

## Consequences

The project acquires a third reader of the operator's tables and a second
consumer of the assessment engine, and that engine becomes load-bearing for two
artifacts a customer is shown. That is the intended shape — one definition of a
conflict, one definition of a conflict id — and it is also the risk ADR 0014
named about the shared normalizer, one level up: a change to the sweep or to
`ConflictID` now moves a report *and* every decision and plan entry keyed by
the ids in it. The stale counts in both reports are what make that visible, and
they have to stay prominent in the text output rather than only in the JSON.

The customer gains a document the platform cannot lose, cannot corrupt and
cannot validate. All three are deliberate and only the third is uncomfortable:
a typo in an account id makes a move `unmatched` rather than an error, and the
report's honesty about that depends on a reader looking at the unmatched count.
It is in the summary for exactly that reason.

Progress can move backwards, and a status meeting will not like it. An account
that stops being readable turns affirmative facts into `unknown`; a VPC that
reappears turns `not-observed` back into `observed`. The alternative is a
report with a memory, which is a report that can assert what it can no longer
see.

There is no single number. A sponsor who asks "how far along are we" gets
counts in two blocks with their denominators and a sentence naming four
conditions, and will ask for a percentage. The answer this record gives in
advance is the one ADR 0014 gives about its own override pressure: the honest
form of the request already exists, and a percentage with a moving denominator
is the specific thing the owner's analysis warns against, because missing
accounts and untested paths disappear into it.

The evidence export is a new artifact with the same confidentiality weight as
the inventory. It lists allocations, tenants, keys and CIDRs, and an operator's
export lists them across every tenant in the deployment. Nothing in this design
encrypts it, rotates it or expires it; it is a file an operator produced and is
responsible for, and the report names its digest so that at least the bytes a
conclusion rests on can be identified afterwards.

The plan's approvals sit outside the ledger's audit trail. An adoption writes
`ADOPT_PLANNED` and `ADOPT_COMMITTED` with an actor into an append-only event
log; a migration approval is a line in a YAML file with whatever authority the
customer's review process gives it. That is the correct home for a decision the
platform did not make, but it means the two trails never join, and a reader
reconstructing why a CIDR exists will find the reservation in the ledger and
the reason in a repository the platform has never seen.

What is unknown is stated plainly. Nobody has written such a plan, so whether
four dispositions are enough, whether a move is the unit reviewers actually
want, and whether a plan for a hundred accounts is readable as a flat list of
moves are all untested. No customer has supplied a plan, an estate or a wave
structure. The number of moves a real plan contains is unknown — it is bounded
by review capacity rather than by the estate, which is a guess and not a
measurement. The evidence export's scope field is designed and not built, and
whether the CLI can know its own role without a contract change is open.
`scripts/aws/org-inventory.sh` has still never run against a live AWS
Organization, so every statement this design makes about what a subject looks
like in real inventory inherits that limit from ADR 0014 unchanged. And nothing
here has been shown to a reviewer: the whole design rests on the belief that a
person would rather read three facts than one word, which is a claim about
people and not about code.

This design depends on the owner's open questions in specific ways, and decides
none of them. It depends most on **Q6** — which workloads can move, and which
require existing VPC ids, secondary ranges, stable IPs, original source IPs or
special DNS behaviour — because that answer decides the disposition vocabulary
itself: the four values here cover replace, keep and retire, and the
secondary-range path is deferred to gap M7 precisely because Q6 has not said
whether it is needed. It depends on **Q7** — who owns stateful migration,
cutover, rollback and deletion approval, and what downtime is acceptable —
because approval and rollback are free text carried verbatim exactly while
nobody has said what roles exist; if Q7 names them, a later record can
constrain those fields, and this one should not guess. It depends on **Q2**
through the assessment, since a move's priority rests on whether the conflicts
it resolves are `confirmed`, `potential` or `unknown`, and with no matrix every
conflict is `unknown` and a plan is a list of facts to be ordered by hand. It
depends on **Q4** — whether every relevant account and region can be read, and
who owns denied coverage — because that decides whether `evidence.complete` can
ever be true for this customer, and therefore whether the report can ever exit
`0`. It depends on **Q12** — what evidence and retention are required before
removing a contributing resource or an imported prefix — for the boundary this
record draws around `not-observed`: gap M9 owns what licenses a removal, and if
Q12's answer is stricter than this record assumes, only M9 changes. And it
depends on **Q3** for whether a flat list of moves is a usable artifact at the
customer's scale.

It deliberately does not depend on **Q1** or **Q11**. What protocols and
directions must work, and whether topology must be discovered rather than
reviewed, change what a *verification* has to demonstrate — and verifications
are typed, named and never asserted by this tool, so neither answer changes
what the plan records or what the report may claim. It does not depend on
**Q13** or **Q14**, which are commercial. And it does not depend on **Q5**,
**Q8**, **Q9** or **Q10**: connected non-AWS ranges enter through the
assessment's `--fixed` table unchanged, the customer's existing tooling does
not change what a plan is, performance targets belong to gap M4, and who can
create VPCs outside approved pipelines is gap M8's prevention question rather
than this report's.

Established on 2026-09-22 by package M3b4, which builds the evidence export
(`platform-ipam client evidence`, internal/cli) and the end-to-end run from
internal/assess/estategen's generated estate through the real onboard progress
command, and reads the record's own evidence paragraph item by item against
everything the four packages built. No move reported fully evidenced on typed
input alone, including the mutation that a typed field must change no derived
fact, was demonstrated by package M3b2's table-driven tests in internal/migrate
and is unchanged by this package. A draft never reserves space was demonstrated
by package M3b1's decode-time refusal of any cidr key at any depth and by
package M3b3's source-parsing and empty-environment tests for the command; this
package closes the one gap those left, "a test that a full run creates no file
when --out is absent and exactly one when it is present," which progress_test.go
never covered, now in internal/onboardcmd/progress_estate_test.go. Every
definition with its positive and negative fixture (subject identity across a
secondary association, not-observed against unknown for the same missing
subject, present against stale, unplanned, a keep move blocked or unblocked by a
reported conflict, retired for QUARANTINED and for RELEASED, mismatched naming
the differing fields), every structural refusal (duplicate subject, duplicate
target key, undefined wave, undefined dependency, a depends_on cycle, plus the
three added in review: unknown_disposition, incomplete_subject, and
duplicate_wave for two differing definitions of one wave id), determinism across
runs and across file order, and the embedded assessment's coverage and summary
equalling what onboard assess itself produces for the same inputs, were all
demonstrated by packages M3b1 through M3b3's own tests in internal/migrate and
internal/onboardcmd and remain unchanged by this package. The end-to-end fixture
against estategen's gapped estate (a move in the unread account reading unknown
rather than not-observed, the planted equal-cidr pair's moves listing that exact
conflict id as present, evidence.complete false with no allocation evidence
supplied, exit 3) and its "nothing moved yet" sibling (a RESERVED, unverified
target under complete evidence, still not fully evidenced, still exit 3) were
already demonstrated at the Derive level, called directly, by package M3b2's
derive_estate_test.go; this package demonstrates the identical two fixtures a
second way, through the real onboardcmd.Main binary surface rather than a direct
call, and adds the one fixture no earlier package attempted: the third,
every-fact-affirmative case that is the only one to exit 0, built from a VPC id
estategen never wrote into an otherwise fully covered account and region, and an
allocation evidence file produced by a real run of the new client evidence
export against a real httptest server standing in for the platform API, not
written by hand. Building the export settled one question the record left open
on purpose, between inferring the evidence file's scope and changing the
contract to have the server state it: this package infers operator scope only
when every row the read returned carries tenant_id, which only an operator's own
read receives, and otherwise requires an explicit --scope naming the tenant,
refusing rather than guessing when the two disagree or when zero rows give it
nothing to infer from; a tenant export run without that flag therefore refuses
outright, by design, rather than risk a wrong scope silently turning an unknown
target fact into an incorrect none or the reverse. A target's
parent_allocation_key is resolved only from rows the same evidence read itself
saw, since the export buffers every page before writing anything; a parent
outside that read's own scope, which does not arise for an unfiltered operator
or tenant-wide read, leaves the field empty rather than guessed at, a limit this
package records in docs/MIGRATION_PROGRESS.md rather than assumes away. The
exit-0 reading package M3b3 settled against the record's self-contradictory
prose (every move's fully_evidenced true, plus no unplanned conflict, rather
than the five words taken literally, which no plan resolving a real conflict
could ever satisfy) is unchanged and is exactly what this package's own third
fixture confirms end to end: exit 0 exactly once, for the all-affirmative case,
and exit 3 for both of the record's other two. The resolve status carries
exactly two values, present and stale, because the record's own evidence
paragraph defines exactly two; a target has no field for address_family or
availability_zone_id, two of the six fields internal/service's requestHash
compares, so a mismatch on either is invisible to target_fact mismatched, a
limit inherited from package M3b1's schema and recorded rather than fixed here;
the contradiction between the mandated evidence.complete field name and the
forbidden word "complete" is handled, as package M3b2 decided, by scanning only
this package's own generated prose, never a raw JSON field name or the embedded
assessment's own verbatim text; and Derive takes the plan's own exported types,
Plan, Move, Wave and Target, rather than the private copy package M3b2 was
originally built against before the lead unified the two packages' shapes in
review, ahead of package M3b3. What remains exactly as unknown as before: nobody
has written such a plan, whether four dispositions are enough or a move is the
unit reviewers actually want is untested, no customer has supplied a plan or an
estate, and scripts/aws/org-inventory.sh has still never run against a live AWS
Organization, so every statement here about what a subject looks like in real
inventory inherits that limit from ADR 0014 unchanged.
