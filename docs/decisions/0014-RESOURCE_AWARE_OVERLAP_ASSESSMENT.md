# ADR 0014: Overlaps are assessed per resource, and coverage is a result

Status: accepted, 2026-09-20, and built the same day by packages M1b1 to M1b4 of
the [work plan](../WORK_PLAN.md) as `platform-ipam onboard assess` ([overlap
assessment](../OVERLAP_ASSESSMENT.md)). It is demonstrated against synthetic
estates and against the real collector script driven by a stubbed `aws` CLI;
nothing has run against a live AWS Organization or a customer's file. Where
building it showed this record to be wrong or silent, the dated paragraphs at
its end say so and win over the text above them. It was proposed by package M1a
and is the first deliverable of gap M1 in the [overlap-migration
analysis](../IP_OVERLAP_MIGRATION.md), which deliberately declines to commit an
API, a schema or a CLI name through itself. It changes nothing in [ADR
0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md), whose import keeps
collapsing networks into prefixes exactly as it does today, and it decides
nothing about allocation, so it leaves [ADR
0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md) untouched.

## Context

The owner's words for what the report must be able to say, 2026-09-20: "Across
the AWS network data I actually observed, these 37 VPC/CIDR relationships
prevent the intended connectivity. Here is why each was classified as a
conflict, and here are the accounts/VPCs for which I cannot make a complete
statement because coverage is missing." Every clause of that sentence is a
requirement. It names relationships between resources, not occupied ranges; it
promises an explanation per relationship; it is scoped to data that was actually
read; and it ends by naming what is missing rather than by rounding it away.

Nothing in the checkout can say it. The path from AWS to a statement about
address space has two halves, and both halves lose exactly the information the
sentence needs.

The first half is the collector. `scripts/aws/org-inventory.sh` assumes a role
into every active account of an organization and writes three files into its
`OUT` directory. `accounts.json` is the raw `aws organizations list-accounts`
response, and the loop that follows scans only the accounts whose `Status` is
`ACTIVE`. `networks.csv` has the header `account_id,account_name,region,type,
resource_id,cidr,parent_id,az_id,name,state,primary`. `scan_region` emits one
row per VPC CIDR association, filtered to `CidrBlockState.State == "associated"`
so a disassociated range never appears, with `resource_id` the VPC id,
`parent_id` and `az_id` empty, `name` the value of the `Name` tag and nothing
else, `state` the VPC state, and `primary` the comparison
`.CidrBlock == $v.CidrBlock` — which `jq`'s `@csv` renders as a bare `true` or
`false` beside the quoted string columns. A subnet row from the same function
carries the subnet id, its own CIDR, its VPC id in `parent_id`, its
`AvailabilityZoneId` in `az_id`, and an empty `primary`. `failures.csv` has the
header `account_id,account_name,region,stage,error` and four stages:
`assume-role` and `identity` (where the assumed session lands in another
account, carrying `got <id>`) lose a whole account and leave `region` empty;
`describe-regions` loses a whole account the same way; and `describe` loses one
region of one account.

Four things the report needs are not in those files. The CIDR association id is
present in every `describe-vpcs` response and is simply not selected by the
`jq` filter, so two rows cannot be distinguished from one row written twice, and
nothing durable names the association a reviewer would be deciding about. There
is no observation time anywhere: no column, no run record, nothing but the file
modification time, which is not evidence, and the contrast with the runtime
observer is instructive — `domain.Observation` carries `StartedAt`,
`FinishedAt`, `Generation` and `Complete`, and `internal/cloud/aws.go` refuses
to set `Complete` unless it read what it meant to read. There is no record of
the source: `ROLE_NAME` and whether the management account was read with its own
credentials are known to the script and written nowhere. And there is no record
of what was attempted: when `REGIONS` is empty, `scan_account` asks
`describe-regions` for the account's enabled regions, iterates them and discards
the list, so an account-region pair that produced no row is indistinguishable
from one that was never visited, which is precisely the difference between "read
and empty" and "not read".

One behaviour of the script matters more than it looks. `scan_account` appends
`scan_region`'s standard output to `networks.csv` while it runs and appends a
`describe` row to `failures.csv` only if the function returns non-zero, and
`scan_region` runs `describe-vpcs` before `describe-subnets`. A failure in the
second call therefore leaves the VPC rows of that region in `networks.csv` and a
failure row beside them. A failure row does not mean "no data for this account
and region"; it means "this account and region is not complete", and anything
reading these files must treat rows and failures as independent facts.

The second half is the import, and it is not a defect but a mismatch of
purpose. `internal/onboard` reads those files well: `ResolveHeaders` matches
headers through `headerAliases` case- and punctuation-insensitively, keeps every
unrecognised column instead of dropping it, and `buildNetworkRows` turns one
source row into a `NetworkRow` per CIDR when a cell holds several, repairs an
account id a spreadsheet damaged, and appends everything it has no field for to
`Description`. `shiftRows` then renumbers so that a row number is the one the
operator sees in the file. But `NetworkRow` has no association id, no
observation time and no record of which file it came from, and `concatTables` in
`internal/onboardcmd/onboardcmd.go` merges several inputs whose row numbering
each restarts at the top of its own file, so a merged table cannot say whether
row 12 is row 12 of the first input or of the second.

Then `Plan` collapses. `planPrefixCandidates` groups candidates by canonical
CIDR string and emits one `WriteEntry` per group with the contributing rows
listed, plus a `duplicate-cidr` warning saying how many rows collapsed. That is
the right behaviour for its purpose — a duplicate `(vrf, cidr)` fails the whole
inventory snapshot and turns every reservation in the domain into a 503, which
is why the rule exists — and it is exactly wrong as a conflict report. Two VPCs
in two accounts sharing `10.0.0.0/16` become one prefix and one warning whose
`Finding` carries a level, a rule, a CIDR and a list of row numbers, and no
account, no region and no resource id at all. The relationship the owner wants
to count has been discarded by the time the finding is written. Overlapping but
unequal rows are not even reported: `docs/ONBOARDING_IMPORT.md` section 5 lists
them as allowed, because a VPC and its subnets are exactly that shape and are
both simply occupied space.

What does exist is a precedent for the shape of the answer. `onboard drift`
(`internal/onboardcmd/drift.go`) is a read-only operator check that prints a
JSON report on stdout through `writeDriftReportJSON`, prints one summary line on
stderr, sorts its findings to a total order so two runs agree, keys each finding
to a stable rule identifier, exits `ExitValidation` when the report contains an
error and `ExitAdapter` when it could not be produced at all. The import already
refuses to reason over evidence it does not have, in `RuleIncompleteSnapshot`:
"an unreadable inventory is not an empty one", the same sentence this record
applies to AWS coverage instead of NetBox. And the `onboard` family already
promises never to open the ledger — `cmd/platform-ipam/main.go` dispatches it
before `storage.NewPostgresLedger` is constructed — which is half of what an
offline assessment needs. The other half is that `plan`, `apply` and `drift` all
require `IPAM_NETBOX_URL` and `IPAM_NETBOX_TOKEN` through `settings.validate`,
and an assessment needs neither.

Two facts about addresses and one about the customer frame the rest.
CIDR blocks cannot partially overlap: a block of prefix length `n` is an
interval of `2^(32-n)` addresses aligned to a multiple of its own size, so if
two blocks share any address, the one with the longer prefix lies wholly inside
the other, and otherwise they are disjoint. There are therefore exactly two
relationships to report and no third. A collision between two ranges is a
mathematical fact about the data; whether it blocks a required connection needs
intent, which no AWS API returns; and whether traffic actually flows needs a
test, which this project does not perform and must never imply. The estate is
about a hundred accounts, and neither its VPC count nor its region list is
known, so the design has to bound its own cost rather than assume a size. And
the inventory is customer data: `.gitignore` excludes `inventory/` with the
comment "account names and network layout", which is the rule the fixtures of
this work have to respect as well.

## Decision

An offline, read-only assessment reads the organization inventory's original
per-resource records, reports every equal or containing CIDR relationship
between different VPCs as a durable, identified conflict with both source
records attached, classifies each one's impact as `confirmed`, `potential` or
`unknown` against a reviewed connectivity matrix that the customer supplies, and
carries the coverage it could not obtain as a first-class part of the result
that mechanically forbids any claim of completeness. It never opens the ledger,
never calls NetBox, never calls AWS, never allocates and never says anything
about reachability.

### The input, and what must be added to produce it

The assessment reads the collector's output directory: `networks.csv`,
`failures.csv` and `accounts.json` as written by `scripts/aws/org-inventory.sh`.
It reads the networks table through `internal/onboard`'s existing reader and
normalizer, because that code already knows how to survive a spreadsheet, and it
reads nothing else — no pools configuration, no identity file, no NetBox
snapshot, no environment variable. The unit of the analysis is one observed VPC
CIDR association: the tuple account, region, VPC id, CIDR, association id.
Subnets are read but are not compared; they attribute a VPC and nothing more.

Three additions to the collector are required for the owner's sentence to be
sayable, and M1b makes them. `scan_region` selects the association id of each
VPC CIDR association into a new `association_id` column, empty for subnet rows.
Every row gains an `observed_at` column carrying the UTC RFC 3339 instant its
region was read. And the run writes a fourth file, `run.json`, recording the run
start and finish, the role name, whether the management account was read with
its own credentials, the configured region list, the script's own version, and
one entry per account and region that was attempted with its outcome. That last
file is the only way "read and empty" can ever be distinguished from "not read",
and no rule elsewhere in this design can compensate for its absence.

Correspondingly `NetworkRow` gains `AssociationID`, `ObservedAt` and
`SourceFile`, `headerAliases` gains the new names, and `concatTables` records
which input each row came from. `SourceFile` is not decoration: the traceability
property below requires a conflict to name the file and the row of each of its
two records, and today two merged files both number their rows from one. None of
this changes what an import does. `Plan` keeps every rule it has,
`duplicate-cidr` keeps collapsing rows into one write, and the columns arrive at
`Plan` as they arrive today at any unknown column — `knownColumns` decides what
a row builder reads, and everything else is preserved in the description.

When a column is absent the assessment either refuses or degrades, and it never
guesses. It refuses, producing no report, if `cidr`, `account_id`, `region`,
`type` or `resource_id` is missing, because without them a conflict cannot be
attributed to two resources at all and a report that named no resources would be
the prefix collapse again. It degrades, with an explicit statement, for
everything else. A missing `association_id` means every side carries a null
association id and the report's `input_limits` list carries
`association-id-missing`, which also states the consequence: two associations of
one VPC at one CIDR cannot be told from one association read twice. A missing
`observed_at` means null observation times and `observation-time-missing`, and
the rendered summary then says "the data in <file>, whose observation time is
not recorded" where it would otherwise name an instant. A missing `primary`
means each side reports `primary` as `unknown`, and no association is ever
described as secondary on the strength of a guess — not from its position in the
file, not from its prefix length. A missing `run.json` means the coverage result
carries `attempted-set-unknown` and can never report `complete`.

### The relationships, exactly

Two association records conflict when they belong to different VPCs and their
CIDRs are equal or one contains the other. Different VPCs means a different
account, region and VPC id triple; the VPC id alone is not the identity, and the
same VPC id read under two accounts is reported as a data error rather than
silently treated as one resource or two.

`equal-cidr` is two associations of different VPCs with the same CIDR. The
reported intersection is that CIDR. `contains` is two associations of different
VPCs where one CIDR strictly contains the other: the sides carry the roles
`outer` and `inner`, the reported intersection is the inner CIDR, and the
relationship is emitted once rather than once per direction. There is no third
kind, because partial overlap between CIDR blocks is impossible for the reason
given above, and the report says so in its own header text so that a reader does
not look for a category that cannot exist.

A secondary association is a conflict on exactly the same terms as a primary
one, and the report says which it is: each side carries its own `primary` value,
so a relationship between a primary range and somebody else's secondary range
reads as what it is. Nothing is downgraded for being secondary. The collector
already emits every associated CIDR of a VPC rather than only the first, and the
analysis document says why — secondary ranges occupy space and are routed like
any other.

Two VPCs in the same account and the same region conflict. The account boundary
is not a routing boundary: two VPCs in one account are two routing domains that
have to be attached to something to communicate at all, and an ordinary route
table cannot distinguish two identical destinations whoever owns them. Both
sides carry their account, so a reviewer sees immediately that this conflict has
a single owner and is usually the cheapest to resolve; that is impact
information, not a reason to hide the fact.

Four things are not conflicts. A subnet inside its own VPC is excluded by
construction, because subnets are never compared: the containment of a subnet in
its parent is the definition of a subnet, and a conflict between two VPC ranges
already implies every conflict between their subnets, so reporting subnet pairs
would multiply one fact by thousands and bury the sentence the owner wants. A
subnet is still read for two checks: a subnet whose `parent_id` names no VPC in
the input produces a coverage note, because its VPC was not read, and a subnet
that lies outside every association of its own parent produces a data-error
note, because that parent's association set was read incompletely. The same
resource observed twice is not a conflict: records identical in account, region,
VPC id, CIDR and association id collapse to one observation and increment a
`duplicate_observations` count that the report carries, so a reader knows rows
were merged. Where the association id is absent, the collapse uses the tuple
without it; that rests on AWS refusing to associate one CIDR twice with the same
VPC, which is stated here as the assumption it is, and which the association id
makes moot as soon as the collector emits it. And a VPC range that equals a pool
container or an on-premises range is a different question: the assessment loads
no pools configuration at all, and non-AWS ranges from question Q5 enter only
through an explicit `--fixed` table of `cidr, description, owner`. A
relationship with a fixed range on one side is reported with the same two kinds,
marked `fixed` on that side, counted separately from the VPC-to-VPC total so the
headline number is not inflated, and can never be `confirmed`, because a range
with no VPC has no place in a connectivity group unless the matrix names it.

### Identity and determinism

A resource identity is the string `aws:<account>:<region>:<vpc>:<cidr>` with
`:<association>` appended when the association id is known. A conflict id is
`c-` followed by the first sixteen hex characters of the SHA-256 of the
relationship kind and the two resource identities, joined by a separator after
sorting the two identities as byte strings. Sorting before hashing makes the id
independent of which record was read first. The id deliberately excludes the
observation time, the file, the row, ownership, the matrix, the impact and every
other input that is not the two resources and their relationship, so a reviewer
who records a decision against `c-1a2b3c4d5e6f7a8b` keeps it across a new
collection run, across supplying a matrix, and across an inventory that grew:
only the conflicts whose resources actually changed change. A resource that
changes its CIDR yields a different identity and therefore a different conflict,
which is correct, because the old conflict no longer exists.

Determinism is a property of the whole report and is achieved the way `drift`
achieves it, only further. Conflicts are sorted by kind, then by the first
resource identity, then by the second, then by the id, which is a total order
with no ties. Coverage entries are sorted by account, then region, then stage.
Every collection in the JSON is a sorted slice; no Go map is ever encoded
directly. The encoder is `encoding/json` with two-space indentation and a
trailing newline, as `writeDriftReportJSON` already does. The report contains no
wall-clock field of its own — the instants it carries all come from its inputs —
because a generation timestamp would make byte-identical output impossible;
where an archival stamp is wanted, `--stamp` accepts one from the operator, and
it is the only field in the report that did not come out of an input file. Each
input is listed with its path as given and the SHA-256 of its contents, which is
what makes two reports comparable and what lets a reader prove which bytes
produced a conclusion.

### Impact is three-valued, and reachability is never claimed

A collision is a fact; whether it prevents an intended connection is a
judgement about intent, and intent comes from a reviewed, customer-supplied
connectivity matrix — gap M2 — and from nowhere else. Its minimal format is one
YAML document with a `version`, a list of `groups` each with an `id` and
`members`, a `must_communicate` list of group-id pairs, a `must_stay_isolated`
list of the same shape, and a `shared_services` list of group ids that must
communicate with every group. A member entry is an account id, a VPC id or a
product name, and a VPC is assigned to a group by the first of those that
matches, in that order of precedence: an explicit VPC id wins over a product,
and a product wins over an account, so the common case of "this whole account,
except that one VPC" needs no new syntax. A product name resolves through the
ownership table below rather than through tags, which keeps customer labels out
of the collector's CSV and out of this design entirely. A VPC that matches
entries of two groups is not guessed at: the matrix is reported as ambiguous for
that VPC, and every conflict with that VPC on either side is `unknown`.

A conflict is `confirmed` when both sides are assigned to groups and the matrix
requires those groups to communicate, which includes the case of both sides in
one group, since a group is by definition a set whose members must communicate,
and the case of a shared service reaching the other side's group. It is
`potential` when at least one side is assigned and the matrix does not decide
the pair, whether because the pair appears in neither list or because the other
side is unassigned. It is `unknown` when no matrix was supplied at all or when
neither side is assigned. Beside the impact, every conflict carries the raw
relation the matrix expressed — `must_communicate`, `must_stay_isolated`,
`undecided` or `not_in_matrix` — so that no judgement is hidden inside a single
word. A pair the matrix says must stay isolated stays a listed conflict at
impact `potential` and gains one sentence saying that isolation is a policy
intention enforced by route tables and security controls, not a property of the
addresses, and that overlapping ranges make a later change of that decision
expensive. The analysis document already refuses the opposite reading: an
account, product or environment label alone does not establish isolation, and
unique addressing does not authorize communication.

With no matrix the report is still the useful artifact it must be: every
conflict is listed as a fact, every impact is `unknown`, and the summary says
so in those words rather than reporting zero. Reachability is never claimed in
any mode. The report prints, once, that no route table, Transit Gateway
attachment, propagation or traffic test was read, and that nothing in it asserts
that two workloads can or cannot reach each other. The words "reachable",
"unreachable" and "ready" do not appear in any template it can produce.

### Coverage is a first-class result

The report carries a `coverage` object with a single boolean `complete` and four
lists. `failed` is one entry per row of `failures.csv`, carrying account,
region, stage and error, where an empty region means the whole account was lost.
`partial` is every account and region that appears both in `networks.csv` and in
a `describe` failure row, which is the case the collector's append-as-it-goes
behaviour produces. `not_attempted` is every `ACTIVE` account in `accounts.json`
for which no attempt is recorded, plus, when `run.json` is present, every
account and region pair it shows as never visited. `read_empty` is every account
and region `run.json` records as attempted and successful with no rows, which is
the statement nobody can make today and the reason `run.json` exists.

The expected set of accounts comes from `accounts.json`, which the collector
already writes and which already carries the `Status` the scan loop filters on,
so the account-level statement is available from today's files; only the
region-level statement needs the new run record. Where the region list is
unknown the coverage lists say so rather than inferring a region set from the
regions other accounts happened to return.

The mechanical rule is one field. `coverage.complete` is true if and only if
`failed`, `partial` and `not_attempted` are all empty and `run.json` was
present; otherwise it is false. Every rendering of the summary is selected by
that field from exactly two templates. The complete template may contain the
sentence "no conflicting relationship was observed in the scope read at
<instant>"; the incomplete template always contains "this report cannot make a
complete statement for <A> accounts and <R> account-region pairs" and never
contains the complete template's sentence. There is no third template and no
free-text summary anywhere in the tool, and a test asserts that the output
produced with `complete` false contains none of a forbidden list of substrings —
"conflict-free", "no conflicts", "ready", "clean". That is the whole of the
mechanism: one field, two templates, one forbidden list.

### Ownership

Product, environment and owner come from a customer-supplied ownership table
passed with `--ownership`, whose columns are `account_id`, an optional `vpc_id`
meaning the whole account when empty, `product`, `environment` and `owner`. They
come from nowhere else: not from the `name` column, which is an AWS `Name` tag
and frequently a machine name, and not from account naming conventions. A
resource with no matching entry carries the literal string `unknown` in all
three fields, never an empty string and never an omitted key, so that absence
cannot be mistaken for a value lost in a CSV. The AWS `Name` tag and the account
name are still carried on each side as evidence, labelled as what they are. The
summary counts how many conflict sides have unknown ownership, because after
"what is broken" the next question is "who do I call".

### The output

One `Report` value with two encoders, so the machine form and the human form
cannot disagree. The JSON carries a `report_version` integer, the list of inputs
with their digests, `input_limits`, `coverage`, `summary`, `conflicts` and
`notes`. The text form renders the same value as fixed-width columns — conflict
id, kind, impact, then each side's account, VPC, CIDR, product and owner — in
the same order, preceded by the summary statement and followed by the coverage
lists in full.

The summary holds the counts from which the owner's sentence is rendered, and
the rendered sentence itself: the total number of VPC-to-VPC relationships,
their breakdown by kind and by impact, the separate count of relationships
involving a fixed range, the number of accounts and of account-region pairs for
which no complete statement can be made, the number of VPCs implicated by those
gaps, the duplicate-observation count and the unknown-ownership count. A reader
who disbelieves the sentence can recompute it from the fields beside it.

Each conflict carries its id, kind, impact and matrix relation, the intersecting
CIDR, and two sides. A side carries its resource identity, account id and name,
region, VPC id, CIDR, association id, primary flag, AWS `Name` tag, state,
observation time, product, environment, owner, connectivity group, its role for
a `contains` relationship, and the source file and row it was read from. The
source pair is the traceability property: a conflict names the exact two input
records that produced it, by file and by row.

Remediation, responsible owner and decision status do not live in the report.
They live in a separate `decisions.yaml` keyed by conflict id, holding a
decision, a remediation note, a responsible party and who reviewed it when; the
assessment reads it with `--decisions` and echoes each decision into its
conflict, and counts decided, undecided and stale entries, a stale entry being
one whose conflict id no longer appears — which is how a reviewer learns that a
conflict was resolved or that its resources changed. The report stays a pure
function of its inputs and stays byte-identical; human review keeps its own
lifecycle; and the tool never writes into its own output. The reviewed migration
plan itself — waves, targets, approvals, cutover and rollback evidence — remains
gap M3 and is not this record's.

### Where it lives, and what it may never do

`platform-ipam onboard assess`. It belongs to the `onboard` family because it
needs strictly less than every member of that family already needs: no ledger,
which `onboard` already guarantees by being dispatched before the ledger is
constructed; no NetBox, which `plan`, `apply` and `drift` require through
`settings.validate` and this does not; no cloud credentials; and no pools
configuration. It reuses that package's readers and normalizer, which is the
code that already knows how to read the operator's tables. ADR 0010's argument
for making `adopt` a sibling rather than a subcommand runs the other way here:
`adopt` earned its own mode by opening the ledger and needing the observer.

The cost of that placement is one trap this project has already paid for.
Package H5 exists because `adopt` refuses to start without settings it never
uses; `assess` must not repeat it. `runAssess` does not call `newAdapter`, reads
no `IPAM_` variable, and a test runs it with the environment empty.

Its flags are `--inventory` naming the collector's output directory, or
`--networks` (repeatable, any format the import accepts), `--failures`,
`--accounts` and `--run` given individually; `--matrix`, `--ownership`,
`--fixed` and `--decisions` for the reviewed inputs; `--format json|text`
defaulting to text; `--out` to write that same content to exactly one path; and
`--stamp`. With no `--out` the report goes to stdout and the tool creates no
file at all.

Exit codes reuse the package's four with one addition to their meanings, stated
in the help text. `0` means a report was produced, coverage is complete and no
conflict is `confirmed`. `3` means a report was produced — it is on stdout in
this case too — and it is not clean: coverage is incomplete, or at least one
conflict is `confirmed`. Both are conditions a pipeline must not walk past, and
an estate that can never be read completely therefore never exits `0`, which is
the intended answer rather than an inconvenience. `4` means no report could be
produced: an input missing or unreadable, a required column absent, a matrix or
ownership file that does not decode, or more relationships than
`--max-relationships` allows. `2` is usage, unchanged. The package's existing
taxonomy calls `4` the adapter code; `assess` has no adapter, and the help text
says plainly that it uses `4` for "no report exists".

It may never write anywhere but stdout, stderr and a single `--out` path; never
open the ledger; never construct a NetBox client; never call AWS; never load
pools configuration, and therefore never be able to decide anything about
allocation; never claim reachability; and never claim completeness while
`coverage.complete` is false. The engine lives in a new package
`internal/assess` that imports neither `internal/netbox` nor the AWS SDK, so
several of those are properties of the import graph rather than of care.

### Confidentiality

Customer inventory never enters Git. `.gitignore` already excludes `inventory/`
with the comment "Output of the organization inventory procedure: account names
and network layout", and this design adds nothing that needs a new rule: the
report is written only to stdout or to the one path the operator names, there is
no default output path, no temporary file and no write back into the input
directory. Fixtures are synthetic and follow what the repository already does —
the all-zero account `000000000000`, the documentation ranges of RFC 5737 and
RFC 1918 space, and obviously fake resource ids — as
`tests/e2e/test_e2e_adopt.py` and the
[adoption runbook](../../deploy/runbooks/ADOPTION.md) already do. A test asserts
that no fixture in the package contains a twelve-digit account id other than the
all-zero one, a cheap mechanical guard against a real inventory arriving as a
convenient test case.

### Scale

The pairwise comparison is never written as a pairwise loop. Associations are
converted to integer intervals and sorted by start ascending and end descending,
so that any block precedes every block it contains, and a single sweep with a
stack of open containers emits the relationships: pop while the top of the stack
ends before the current block starts, then every remaining stack entry contains
the current block, and identical intervals from different VPCs form the
`equal-cidr` group. That is O(n log n) in the number of associations plus the
size of the output, and the output size is not something an algorithm can
improve, because every reported relationship must be enumerated. Subnets cost
one hash-map pass for attribution and are never compared.

The stated bound is that ten thousand VPC associations and fifty thousand
subnets across two hundred accounts must produce a report in under five seconds
on one core, measured in a test over a generated synthetic estate. The shape
that can still explode is the output: one CIDR shared by five hundred VPCs is
over a hundred thousand pairs. That is reported as it is, with a note naming the
CIDR and the count so a reader understands the size, and bounded by
`--max-relationships`, default one hundred thousand, which refuses with exit `4`
and names the count it would have produced rather than writing an unbounded
file. Pairs are not summarised into groups, because the unit a reviewer decides
about is a pair and a conflict id must be stable per pair.

### Relation to the records and gaps around it

ADR 0007 is untouched: the import reads the same records, collapses them into
prefixes for the allocator, and keeps every rule it has. The assessment reads
the same records for a different question and collapses nothing, which is why it
shares the readers and not `Plan`. ADR 0010 is untouched: the assessment holds
nothing, writes nothing and decides nothing about allocation, and its report is
not evidence for an adoption — that a listed conflict would also cause
`reviewedOccupancy` to refuse an adoption of either side is the same facts seen
twice, not a coupling. Gap M2 keeps the topology question: this record takes
intent from a reviewed matrix and leaves reading Transit Gateway attachments,
route tables and propagations to M2 if question Q11 ever requires it. Gap M3
keeps the plan: the decisions file keyed by conflict id is the seam between
them. Gap M9 keeps refresh and cleanup: the per-resource records here are the
evidence M9 will need to prove a range is still occupied after one duplicate VPC
is deleted, and this tool writes nothing and refreshes nothing.

### The evidence that moves this record to accepted

A test list, and two items head it because two properties must never regress.
Every reported conflict is traceable to the exact two input records that
produced it: a test reads the produced report, opens each side's named file at
its named row, and asserts that the row yields that resource id and that CIDR;
and a mutation that renumbers the rows of the input must change those numbers,
so the test cannot pass against fabricated provenance. And a report over
incomplete coverage never claims completeness: a table-driven test over every
route to incompleteness — a failed `assume-role` row, a failed `describe` row, a
region that is both partial and populated, an `ACTIVE` account in
`accounts.json` that was never attempted, and a missing `run.json` — asserts
`coverage.complete` is false, that the incomplete template was chosen, and that
the output contains no member of the forbidden list.

Then the definitions, each with a positive and a negative fixture: equal CIDRs
in two VPCs against the same CIDR twice in one VPC; containment against two
adjacent blocks, `10.0.0.0/17` and `10.0.128.0/17`, which share no address and
must produce nothing; a secondary association reported as secondary on its own
side; two VPCs in one account and region reported; a subnet inside its own VPC
excluded, a subnet whose parent is absent producing a coverage note, and a
subnet outside its own parent's associations producing a data-error note; a
duplicate observation excluded and counted; a fixed range counted separately and
never `confirmed`. Then determinism: two runs over one input are byte-identical,
and a run over the same input with its data rows shuffled produces the same
report except for the row numbers inside each side, which must name the rows the
records now occupy. Then stable ids: a conflict's id is unchanged when an
unrelated VPC is added, when a matrix is supplied, when ownership is supplied,
and when the observation time changes. Then the three impact values against one
matrix fixture, including a group pair that must stay isolated and a VPC the
matrix assigns twice; and the same input with no matrix at all, where every
conflict is still listed at `unknown`. Then a missing `association_id` column,
degrading with the limit named and null association ids throughout, and a
missing `resource_id` column, refusing with exit `4` and an empty stdout. Then
the hundred-account synthetic estate within the time bound, and the
`--max-relationships` refusal. And finally end to end from the collector's own
test fixture: `tests/aws/test_org_inventory.sh` stubs an organization in which
account `111111111111` has one VPC with an associated `10.0.0.0/16` and
`10.1.0.0/16`, one subnet, and a disassociated range that is filtered out, while
account `222222222222` fails `assume-role`. Over exactly that output the
assessment must report zero conflicts — the two ranges belong to one VPC — must
report coverage incomplete because of the failed account, and must render the
incomplete statement; adding one synthetic second account that also holds
`10.0.0.0/16` must produce exactly one `equal-cidr` conflict at impact
`unknown`.

### The position, and the cut of M1b

The position is therefore: add `platform-ipam onboard assess`, an offline
read-only report over the organization inventory's original per-resource
records; extend the collector with the association id, an observation time and a
run record, and the table model with the association id, the observation time
and the source file; report exactly two relationships, `equal-cidr` and
`contains`, between associations of different VPCs, excluding subnets in their
own VPC and duplicate observations; identify each conflict by a hash of its two
resource identities and its kind so a review decision survives a re-run;
classify impact as `confirmed`, `potential` or `unknown` against a supplied
matrix and never claim reachability; carry coverage as a first-class result
whose single `complete` field selects one of two summary templates; take
ownership only from a supplied table with `unknown` as an explicit value; print
JSON or text from one report value, with remediation and decisions in a separate
file keyed by conflict id; exit `0` clean, `3` not clean, `4` no report, `2`
usage; and keep customer inventory out of Git.

Package M1b is four cuts. **M1b1, tier M:** the collector and the table model —
`scripts/aws/org-inventory.sh` gains the `association_id` and `observed_at`
columns and writes `run.json`, `tests/aws/test_org_inventory.sh` gains cases for
the new columns, the run record, a region that is both partial and populated and
a region read empty, `internal/onboard` gains the three row fields and their
header aliases, `internal/onboardcmd` records the source file in `concatTables`,
and `docs/AWS_ORGANIZATION_INVENTORY.md` documents the new output. Its checker
is external, which is what makes it an M rather than an L. **M1b2, tier L with
an X review:** the pure engine in a new package `internal/assess` — records and
identities, the sweep, the two relationships and the exclusions, conflict ids,
the coverage model, the matrix and ownership decoders, the three-valued impact,
the report types and the two encoders — with no I/O beyond decoding what it is
handed, in the shape `internal/onboard.Plan` already has, and with the two head
properties and the scale test among its table tests. **M1b3, tier M:** the
command, `internal/onboardcmd/assess.go`, in the shape of `drift.go`: flags,
reading the inventory directory, `--out`, the exit codes, and the test that it
runs with no `IPAM_` variable set and constructs no NetBox client. **M1b4, tier
S:** the synthetic estate generator, the end-to-end run from the `tests/aws`
fixture through the report, and the documentation — `docs/ONBOARDING_IMPORT.md`,
`docs/AWS_ORGANIZATION_INVENTORY.md`, `CHANGELOG.md`, the onboarding-import
skill and a dated implementation note in this record. M1b1 and M1b2 are
independent and run in parallel; M1b3 waits for M1b2; M1b4 waits for both M1b1
and M1b3.

## Consequences

The project acquires a second reader of the operator's tables, and the two
readers answer different questions from one file. That is the point, and it is
also the risk: a change to `internal/onboard`'s normalizer now moves two
outputs, one of which is an import that changes NetBox and one of which is a
report a customer is shown. The shared code is exactly the part that should be
shared — header resolution, cell repair, row numbering — and the part that must
not be shared is `Plan`, whose collapse is right for occupancy and wrong for an
assessment. That boundary is worth restating in the package comment when M1b2
lands, because the obvious next contribution is to make `Plan` "also" report
conflicts.

The collector's output gains two columns and a file, and the customer may
already have run it without them. A first assessment therefore has to degrade —
null association ids, null observation times, coverage that can never be
complete — which is honest but is a weaker artifact than the one this record
describes, and re-running a hundred-account collection is not free. The
degradations are named in `input_limits` for exactly this reason.

The report can never say "conflict-free" for an estate with one unreadable
account, and that is deliberate. The practical consequence is that a customer
who cannot grant read access everywhere gets a permanently non-zero exit code
and a permanent paragraph naming the accounts. The pressure to add an override
flag will be real, and the answer this record gives in advance is that the
override already exists in the honest form: fix the access, or accept a report
that says what it does not know. A percentage with a moving denominator is the
thing the analysis document specifically warns against.

Conflict ids are stable against everything except the two resources, which means
a re-collection that changes a VPC's CIDR silently retires its conflicts and
mints new ones. That is correct and it is also a trap for a reviewer who has
been recording decisions for a month: the stale-decision count in the decisions
file is the only thing that will tell them, so it has to be shown prominently in
the text output and not only in the JSON.

The customer sees a number — "37 relationships" — and numbers acquire authority
they have not earned. Every one of them is a statement about the data that was
read, at an instant, under a matrix somebody wrote; none of them is a statement
about traffic. The report says so in its own text, but the mitigation that
actually works is that the three impact values are always shown separately, so a
headline of 37 that is 4 confirmed, 11 potential and 22 unknown cannot be quoted
as 37 confirmed without the quote being visibly wrong.

What is unknown is stated plainly. `scripts/aws/org-inventory.sh` has never run
against a live organization — its own procedure document says so — so every
claim here about what a real inventory looks like is a claim about a stubbed
fixture, and the first real run may find pagination, permission or output
surprises that change the input model. Whether AWS can ever return two
associations with the same CIDR for one VPC has not been confirmed, and the
duplicate-observation collapse rests on it until the association id column
exists. The relationship count at the customer's scale is unknown, so whether a
pairwise list is readable at all, or whether owners will demand grouping, is
untested. No measurement of the sweep exists yet; the five-second bound is a
target this design believes is comfortable, not a result. And whether the
`decisions.yaml` seam is the right shape will not be known until gap M3 has a
real reviewed plan to hang on it.

The design depends on the owner's open questions in specific ways. It depends
most on Q2, which products and environments must communicate and which must
stay isolated: without an answer every conflict is `unknown` and the report is a
list of facts rather than a list of blockers. It depends on Q11, whether a
reviewed matrix is sufficient initially: if the answer is that topology must be
discovered, the matrix format survives but M2 adds a second, differently
trustworthy source of intent, and the record that merges them will have to say
which wins. It depends on Q4, whether every relevant account and region can be
read and who owns denied coverage, because that decides whether
`coverage.complete` can ever be true for this customer. It depends on Q3 for the
scale bound and on Q5 for the `--fixed` input. And it deliberately does not
depend on Q1, Q6, Q7 or Q13: what protocols must work, which workloads can move,
who owns the cutover and what the pilot costs are all questions about the
migration this report exists to inform, and none of them changes what the report
is allowed to say.

Amended on 2026-09-20 by the review of packages M1b1 and M1b2, in three places
where building the design showed the record to be wrong or silent. First,
coverage. The record derived `failed` and `partial` from `failures.csv` alone,
and `failures.csv` is an optional input, so a run record that itself said a
region had failed or been only partly read produced `complete: true` whenever
that file was left out — the one statement this design exists to make
impossible. The run record is evidence in its own right: an attempt whose
outcome is `partial` enters `partial`, an attempt whose outcome is `failed`, or
is any word this version does not know, enters `failed` unless a failure row
already names that account and region, and the collector's fourth outcome,
`partial`, is part of the run record's vocabulary. The expected set of accounts
comes from `accounts.json` and from nowhere else, so a report produced without
it carries `accounts_missing: true` in `coverage` and
`expected-accounts-unknown` in `input_limits` and can never report `complete`;
the mechanical rule is therefore `failed`, `partial` and `not_attempted` empty,
`run.json` present and the account list present. Second, the fixed table. The
record gave a fixed range no identity, and the first implementation took it from
the row's position, which gave one conflict two ids depending on the order
somebody wrote the table in; a fixed range's identity is its CIDR, and a CIDR
written twice is one range. Third, the import. The record said the two new
columns would reach `Plan` like any unknown column and be preserved in the
description. They are known columns instead and are not written into the
description, because an observation time in every description would turn every
re-import of a fresh collector run into a drift on every prefix; no file that
existed before this change carried either column, and a test shows `Plan` is
byte-identical for every existing fixture. Two smaller corrections: the
collector scans accounts sequentially, not in parallel, and an interrupted run
leaves no `run.json` at all, which the rule above already treats as unknown
coverage; and the reviewed inputs are decoded by the engine as JSON, because the
engine imports only the standard library, with the command converting the YAML
documents this record describes before handing them over.

Amended further on 2026-09-20 by the execution of package M1b4, which adds
`internal/assess/estategen` (a synthetic estate generator, not part of the
command surface, importing only this package's own `Identity` and `ConflictID`
so what it predicts is computed the same way the engine computes it),
`tests/aws/test_assess_from_inventory.sh` (the first run of the real collector
script, through a stubbed `aws` CLI, into the real `platform-ipam onboard
assess`, which nothing before this package had exercised end to end), the wiring
of both `tests/aws` scripts into a new `scripts/ai/check-aws` and a new CI job,
and `docs/OVERLAP_ASSESSMENT.md` with pointers from the onboarding-import
documents and skill. Read against the evidence paragraph above: traceability to
the two source rows by file and row, and a report over incomplete coverage never
claiming completeness, are now demonstrated a second way, against real CSV bytes
rather than only a hand-built `Input`, by `test_assess_from_inventory.sh`; every
definition with its positive and negative fixture, determinism across runs and
across input order, stable ids across an added VPC or a supplied matrix or
ownership or a changed observation time, the three impact values against one
matrix fixture including an isolated pair and an ambiguous VPC, the no-matrix
case, the missing-association-id and missing-resource-id fixtures, and the
`--max-relationships` refusal were all already demonstrated by packages M1b2's
and M1b3's own tests and remain unchanged by this package; the
secondary-association fixture and the duplicate-observation fixture are now also
demonstrated reaching a real report through `estategen`'s generated estate and
the real command, not only through M1b2's hand-built one; the hundred-account,
ten-thousand-association scale bound is demonstrated only by `internal/assess`'s
own scale test over its unexported in-memory generator, which this package's
`estategen` does not repeat or supersede, so the five-second bound remains a
target the design believes is comfortable rather than a measurement this package
adds evidence for; and the collector's own worked example from the record's
evidence paragraph — account `111111111111` with `10.0.0.0/16` and
`10.1.0.0/16`, zero conflicts, coverage incomplete because account
`222222222222` failed assume-role, and one added second account producing
exactly one equal-cidr conflict at impact unknown — was demonstrated at the
`Input` level by package M1b2 and is now additionally demonstrated end to end,
from the real script through the real command, by this package. What remains
exactly as unknown as before: `scripts/aws/org-inventory.sh` has never run
against a live AWS Organization, so every claim about what a real inventory
looks like is still a claim about a stubbed fixture and a synthetic estate, not
a measurement against real AWS output.

Amended on 2026-09-21 by package M1c, which closes the one hole the M1b1 and
M1b3 review notes both named: `run.json`'s own `row_count`, on a `succeeded`
or `partial` attempt, was decoded and then dropped, so a truncated or
hand-filtered `networks.csv` sitting beside an intact `run.json` still
reported `complete: true`. The rule lives in the engine, as a coverage
input, not in the command: `internal/assess/coverage.go`'s
`rowCountMismatches` is called from `computeCoverage`, so the comparison is
part of the same pure, I/O-free function that already turns `failures.csv`
and `run.json` into `Coverage`, and the command's only new duty is to stop
dropping the field it was already decoding. For every account and region
`run.json` names with a row count, the rows actually present in the merged
networks input are counted and compared. Present counts SOURCE rows, not
the engine's own deduplicated associations, because a duplicate row in one
file was still a row the collector wrote and both sides of a duplicate
observation remain present rows even after the sweep collapses them into
one association; the one thing that must not count twice is the identical
physical row read twice because the very same file was named more than
once on the command line, which is detected and collapsed by the pair
(source file, source row) before counting, so repeating one input cannot
manufacture a surplus that was never collected. Two different files that
both happen to cover one account and region are not collapsed this way and
their rows genuinely sum, which is exactly what should make a lone
`run.json`'s row count look exceeded when it happens. Fewer rows present
than recorded is `coverage.row_count_short`, a new entry list of exactly
the same shape and standing as `partial` or `not_attempted`: it is counted
in the summary's incomplete accounts and region pairs, and its presence
alone forces `coverage.complete` to `false` and the incomplete template.
More rows present than recorded is `coverage.row_count_exceeded`: a
self-contradictory data error, since there is nowhere for the extra rows
to have come from if the collector's own count is correct, so it is
reported through a `row-count-exceeds-recorded` note naming the account,
region and both counts rather than folded into the incomplete-pairs count,
but it still forces `coverage.complete` to `false`, because a report whose
own inputs disagree about how much was read cannot honestly claim
completeness either. An account or region present in the networks input
but named by no attempt at all in `run.json` is left uncompared: there is
no recorded row count to check against, and whether that pair was ever
meant to be attempted is a question the existing coverage lists, built
from `accounts.json` and `run.json`'s own attempted set, already answer.
A `run.json` older than `script_version 2` never carried `row_count` at
all; an attempt missing it is skipped rather than guessed at, and
`input_limits` gains `row-count-missing` to say so, following the same
"never guess" rule the association id and observation time columns
already follow. `tests/aws/test_assess_from_inventory.sh` gained a third
scenario: the real collector's own `run.json` for one clean account and
region, replayed against a `networks.csv` truncated to its header alone,
asserts exit `3`, `coverage.complete: false`, and a `row_count_short`
entry naming that exact account and region with both counts. Package
M1b4's `internal/assess/estategen` needed no change: every row it writes
already increments the same account/region's row-count tally the run
record it writes carries, so its clean estate remains clean and its
gapped estate's planted facts are unchanged by this package — a fact this
package's own test suite checks rather than assumes.
