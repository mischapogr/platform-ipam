# Migration progress: `platform-ipam onboard progress`

Status: implemented, 2026-09-22 (`docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md`,
work-plan packages M3b1-M3b4). Every flag, exit code, field name and example
below is copied from `internal/migrate`, `internal/onboardcmd/progress.go`,
`internal/cli/cli.go` and real runs of the command and the evidence export
(sections 3 and 9), never from memory. **Nothing here has run against a real
customer plan or a real AWS Organization estate** — every example is a
synthetic estate from `internal/assess/estategen` and a synthetic
`migration.yaml`; see section 10 for the complete list of what is not yet
verified.

## 1. What this is for, and what it deliberately is not

A **migration plan** is a file the customer writes and keeps in their own
repository, `migration.yaml`: a list of **moves**, one per old VPC, saying
what is to become of it (replaced, kept, retired, or undecided) and, where
something replaces it, the target allocation the owning team will eventually
request. `platform-ipam onboard progress` reads that plan alongside the same
organization inventory `onboard assess` reads and an authenticated export of
committed allocations, and derives — per move — three independent facts:
whether the old VPC is still observed, what has actually happened to its
target allocation, and which of the conflicts it claims to resolve are
actually gone. It never collapses those three facts into one word: there is
no `done`, no `complete` and no `migrated` field anywhere in its output.

The JSON move now carries the reviewed target identity in `target`, so the
NetBox workspace can show the tenant, allocation key and requested size
beside `target_fact`. This repeats the plan's CIDR-free request identity; it
does not assign or reserve a range.

It never opens the ledger, never constructs a NetBox client, never calls AWS,
never loads pools configuration, and reads no `IPAM_` environment variable —
mirroring `onboard assess` exactly, for the same reason (ADR 0014's "no
ledger, no NetBox, no cloud credentials, no pools configuration").

What it deliberately never does:

- **It never writes a plan.** Nothing in this project creates, validates as
  authoritative, or stores `migration.yaml` — it is reviewed the way code is
  reviewed, in the customer's own version control, and this command only
  reads it.
- **It never lets a plan reserve space.** The plan schema has no `cidr`
  field anywhere; the strict decoder refuses any `cidr` key at any depth,
  naming it. A `replace` move's target names a `tenant_id` and an
  `allocation_key` — the identity the owning team's own `POST
  /v1/allocations` will use — never an address.
- **It never collapses three facts into a verdict.** Subject, target and
  conflicts are reported and counted separately; the one aggregate this
  report allows is a count of moves for which all three are affirmative at
  once, rendered as a sentence naming the four conditions, never as a label.
- **It never claims completeness while evidence is missing.** One boolean,
  `evidence.complete`, selects one of exactly two summary templates; a test
  asserts the incomplete rendering contains none of the words a reader would
  read as a readiness claim (section 7).
- **It never prints a percentage.** Not a bounded one, not one with its
  denominator beside it. A test asserts the rendered text contains no `%`
  character at all.
- **It never claims reachability.** The assessment's own disclaimer is
  carried into this report verbatim, plus a second disclaimer of this
  report's own: it describes address compatibility for the target design the
  plan names, and nothing about routes, Transit Gateway attachments,
  propagations, security controls, DNS or application connectivity.
- **It never schedules, executes, or authorizes a retirement.** A wave's
  `window` is free text nobody compares with a clock. Terraform or another
  provisioner creates the new AWS resources from committed allocations;
  application and network owners handle cutover. A `not-observed` subject is
  a fact about observation, never license to remove an imported prefix —
  that is gap M9's question.

## 2. Inputs and flags

```
platform-ipam onboard progress --plan FILE [--plan FILE ...] --inventory DIR \
    [--allocations FILE ...] [--format json|text] [--out PATH] [--stamp TEXT]
platform-ipam onboard progress --plan FILE --networks FILE [--failures FILE] [--accounts FILE] [--run FILE] \
    [--matrix FILE] [--ownership FILE] [--fixed FILE] [--decisions FILE] [--max-relationships N] ...
```

`--plan` is **required** and repeatable: at least one `migration.yaml` (YAML
or JSON). Every other assessment input flag (`--inventory`, `--networks`,
`--failures`, `--accounts`, `--run`, `--matrix`, `--ownership`, `--fixed`,
`--decisions`, `--max-relationships`) is read **exactly** as `onboard
assess` reads it — see [Overlap assessment](OVERLAP_ASSESSMENT.md) section 2
for the full absent/malformed degradation table, unchanged here. This
command calls `assess.Assess` **in process** rather than reading a report
`onboard assess` produced, so the two commands can never disagree about what
a conflict is or what a conflict id is.

`--allocations FILE` names one **allocation evidence file** (section 5),
repeatable; zero is allowed (`evidence.complete` is then unconditionally
`false`). `--format json|text` (default `text`), `--out PATH` (stdout and
`--out` always carry byte-identical bytes), `--stamp TEXT` (an archival
label carried into the report verbatim — never the wall clock, the only
field in the report that did not come out of an input file).

## 3. The plan, `migration.yaml`, field by field

A plan is one document with `version` (int), an optional `plan_id`, a list
of `waves` and a list of `moves`. Every field below is typed by a person,
carried into the report verbatim, and never treated as evidence.

**A wave**: `id`, `name`, an optional `window` (free text — a maintenance
window is a sentence, not a schema), an `owner`, and an optional `approval`
(`approved_by`, `approved_at`, `approves` — what was approved, in the
approver's own words).

**A move**: a `subject` (`account_id`, `region`, `vpc_id` — all three
required, ADR 0015's identity truncated before the CIDR: `aws:<account>:
<region>:<vpc>`), a `disposition` (exactly `replace`, `keep`, `retire` or
`undecided`), a `wave` naming a wave id defined somewhere in the supplied
documents, an optional `owner`, an optional `approval` (same shape as a
wave's), a `depends_on` list of other subjects, a `blockers` list of `{id,
description}`, a `rollback` (free text), a `verification` list of `{id,
description, ran_by, ran_at, outcome}`, a `resolves` list of conflict ids
(from the embedded assessment — section 4 of [Overlap
assessment](OVERLAP_ASSESSMENT.md)), and free-text `notes`.

**A target** (a `replace` move's `target`, required; a `keep` move's target,
optional): `tenant_id`, `allocation_key`, `scope`, `environment`, `region`,
`account_id`, `prefix_length`, and `parent_allocation_key` for a subnet.
These are exactly the fields `internal/service`'s `requestHash` covers once
description and labels are zeroed (ADR 0010) — **except** `address_family`
and `availability_zone_id`, which `requestHash` also compares and this
schema has no field for at all (section 10). A `keep` move's target may name
only `tenant_id` and `allocation_key` — adoption pins the CIDR the VPC
already has, so nothing else about it is a choice; naming anything more is
refused (section 4).

**There is no `cidr` field anywhere in this schema, on any type.** The
strict decoder (`encoding/json`'s `DisallowUnknownFields`, over a document
first read as YAML-or-JSON) refuses a `cidr` key at any nesting depth,
naming the field — the whole of "a draft never reserves space" expressed as
a schema. An unquoted, all-digit account id decodes as a YAML/JSON number
and is refused the same way `onboard assess`'s own inputs refuse one — quote
every account id.

A worked, synthetic example (two moves resolving one planted `equal-cidr`
conflict between two accounts):

```yaml
# reviewed migration plan -- synthetic worked example
version: 1
plan_id: doc-example
waves:
  - id: wave-1
    name: Wave 1
    owner: alice
moves:
  - subject:
      account_id: "100000000000"
      region: eu-central-1
      vpc_id: vpc-0000000013
    disposition: replace
    wave: wave-1
    resolves: ["c-82913251d179fcf5"]
    target:
      tenant_id: tenant-a
      allocation_key: alloc-a
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "100000000000"
      prefix_length: 20
  - subject:
      account_id: "100000000001"
      region: eu-central-1
      vpc_id: vpc-0000000014
    disposition: replace
    wave: wave-1
    resolves: ["c-82913251d179fcf5"]
    target:
      tenant_id: tenant-b
      allocation_key: alloc-b
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "100000000001"
      prefix_length: 20
```

`--plan` is repeatable specifically so a plan can be split per product or
per team: two files defining the **same** wave identically is accepted
(`wave-1` above could be repeated verbatim in a second file); two files
disagreeing about it, or naming the same subject twice, are refused (section
4).

## 4. Structural refusals: exit `4`, no report at all

Every refusal below names the file and the offending key
(`*migrate.DecodeError` or `*migrate.StructuralError`'s own `Error()`); the
command adds nothing beyond its own prefix. The first two are decode-time,
from the strict decoder itself; the rest are `Validate`'s, checked in this
fixed order (duplicate subjects, then duplicate target keys, then — per
move, in input order — incomplete subject, unknown disposition, undefined
wave, undefined dependency, missing/overspecified target, and finally,
once every dependency is known to name a real subject, a `depends_on`
cycle):

| Refusal | Trigger |
| --- | --- |
| Any `cidr` key at any depth | the strict decoder has no such field anywhere in the schema |
| An unquoted, all-digit account id (a YAML/JSON number where a string is expected) | the same strict-decode rule every reviewed input in this project uses |
| `duplicate_subject` | a second move names a subject identity an earlier move (in any supplied file) already named |
| `duplicate_target_key` | a second move's target names a `(tenant_id, allocation_key)` pair an earlier move's target already named |
| `incomplete_subject` | a subject is missing its account id, region or VPC id — **added in review**: an incomplete subject names no VPC |
| `unknown_disposition` | a disposition is not one of `replace`, `keep`, `retire`, `undecided` (a typo, or the field omitted) — **added in review**: it would otherwise have been derived as no disposition at all |
| `undefined_wave` | a move names a wave id no supplied document defines |
| `undefined_dependency` | a `depends_on` entry names a subject no move defines |
| `missing_target` | a `replace` move carries no `target` |
| `overspecified_target` | a `keep` move's target names anything beyond `tenant_id` and `allocation_key` |
| `dependency_cycle` | a cycle in the `depends_on` graph |
| `duplicate_wave` | a wave id is defined more than once **and the definitions differ** — **added in review**: identical repeats across files stay accepted (splitting a plan per product needs this); two different owners or approvals for one wave id do not say which a reader is looking at |

Two things this list deliberately does **not** refuse, decided and reported
by M3b1 rather than named by ADR 0015's own text: an empty document (no
waves, no moves) is refused as a decode failure (an empty file decodes to
YAML/JSON `null`, not a valid `Plan`); a `keep` move without a target, and a
`retire` move carrying one, are **not** refused at the structural level.

Beyond the plan itself, the same "no report at all" family covers the
assessment's own inputs unchanged from `onboard assess` — no network data
named at all (exit `2`, usage), an `--inventory` directory with no
`networks.csv` (exit `4`), a row the normalizer dropped, a row whose `type`
is neither `vpc` nor `subnet`, an input file that does not decode against
its own schema, or more relationships than `--max-relationships` would
produce. See [Overlap assessment](OVERLAP_ASSESSMENT.md) section 3 for the
complete table; this command adds nothing to it.

## 5. The allocation evidence export

The target facts need the ledger, which this command never opens directly —
that would give an offline reporting tool ledger credentials and put it
inside the global advisory lock every allocation contends for. Instead it
reads an **allocation evidence file**: an ordinary authenticated read's
output, produced ahead of time and handed to `--allocations`.

### `platform-ipam client evidence`

The consumer CLI (`internal/cli`, [Clients](CLIENTS.md) section 6) gained
one verb for exactly this: it walks **every page** of `GET
/v1/allocations` — refusing an unreadable payload, following `next_cursor`
until there is none, exactly as `client findings --fail-if-open` walks its
own pages — records the instant the walk started (the read instant belongs
here: this is an export of what a read observed, not a report written
later), and writes **one** JSON document: `read_at`, `scope`, and
`allocations[]`. Every page is buffered in memory before anything is
written, so a walk that fails partway leaves **no** output at all — never a
half document — and `--out PATH` writes through a temp file and an atomic
rename so a failure partway through the write itself cannot leave a
truncated file at that path either.

```bash
export PLATFORM_IPAM_URL=https://ipam.example.com
export PLATFORM_IPAM_TOKEN=...
platform-ipam client evidence --out evidence.json
# or, for a tenant credential (see the scope rule below):
platform-ipam client evidence --scope prod-team --out evidence.json
```

A real, synthetic export (one `ACTIVE`, verified allocation):

```json
{
  "read_at": "2026-09-22T01:51:54Z",
  "scope": "operator",
  "allocations": [
    {
      "tenant_id": "tenant-a",
      "allocation_key": "alloc-a",
      "scope": "vpc",
      "environment": "prod",
      "region": "eu-central-1",
      "account_id": "100000000000",
      "prefix_length": 20,
      "state": "ACTIVE",
      "binding_verified_at": "2026-09-21T00:00:00Z"
    }
  ]
}
```

This is exactly `migrate.EvidenceFile`'s own JSON shape, so `onboard
progress --allocations` reads it unchanged — a round-trip test
(`internal/cli/cli_test.go`) exports against an `httptest` server and
decodes the result through `migrate.DecodeEvidence`, asserting the same
rows come back out.

### The scope rule, and why it can refuse

ADR 0015 requires the file to carry "the scope of the principal that
produced it: the literal `operator`, or the tenant id" — the field that
lets the derivation distinguish "no allocation exists for this key" from
"this evidence could not have seen it". The record names two ways to obtain
it — infer it, or change the contract so the server states it — and this
command takes the first, as far as the read alone can support:

- **An operator's own read carries `tenant_id` on every returned row**
  (ADR 0011 — no other caller receives that field). When every row this
  walk saw carries a non-empty `tenant_id`, scope `operator` is inferred
  without a flag.
- **A tenant's own read carries `tenant_id` on no row at all.** Nothing in
  the transport tells a caller its own tenant id — the API "never
  accept[s] `tenant_id` ... as authorization" — so this command **cannot
  infer** which tenant it is, and refuses (a usage error) rather than
  guess. The caller must pass `--scope <tenant-id>` naming the tenant this
  credential belongs to; every row is then exported carrying that tenant
  id, which is safe because a tenant's own read of `GET /v1/allocations`
  already returns only that tenant's own allocations.
- `--scope operator` may be given explicitly too, and is cross-checked: a
  row missing `tenant_id` under a claimed operator scope refuses rather
  than silently exporting an incomplete claim.
- Rows that disagree with each other (some carrying `tenant_id`, some not,
  with no `--scope` given) refuse rather than guess which reading is
  right. Zero rows returned with no `--scope` given also refuses — there
  is nothing to infer from.

**The consequence for a tenant export**: a tenant credential run without
`--scope` refuses outright. This is deliberate. A wrong scope would
silently change a later `onboard progress` run's target facts — turning
ADR 0015's `unknown` (evidence that could not have seen a tenant) into an
incorrect `none`, or the reverse — which is a worse failure than a usage
error that names exactly what is missing.

`parent_allocation_key` (needed for a subnet target's mismatch comparison,
section 6) is resolved from the **same** read: the export walks every page
before writing anything, so a parent's own row — an ordinary allocation the
same credential is authorized to read, in any state — is present in the
export whenever it exists at all under that scope. A parent this read never
saw (not expected for an unfiltered operator or tenant-wide export) leaves
`parent_allocation_key` empty.

### Several exports, and their scopes union

`--allocations` is repeatable. ADR 0015: "several exports may be supplied
and their scopes union, which is how a customer with no operator
credential still assembles a complete picture from one export per team." A
tenant's own export never contains another tenant's allocations, so a
tenant running `onboard progress` over a plan naming other tenants' targets
learns `unknown` for those moves and nothing else — the platform never
serves the plan and there is nothing to leak through it.

## 6. What is derived: three facts, never collapsed

Per move, exactly three independent facts.

**Subject** — `observed`, `not-observed`, `unknown`:

| Value | Meaning |
| --- | --- |
| `observed` | the subject's VPC id appears in the inventory |
| `not-observed` | no row mentions it, and coverage is **complete** for its account and region — evidence the VPC is gone, never evidence its addresses are free |
| `unknown` | no row mentions it, and coverage is **incomplete** for its account and region — never reported as `not-observed`; an unreadable account must not be the cheapest way for a VPC to look retired |

A subject naming an account id the inventory never lists at all (not merely
unread, but absent from the account list too) is also `unknown`, with a
reason naming that the report cannot tell a typo from a resource outside
the scope that was read.

**Target** — `none`, `reserved`, `active`, `retired`, `mismatched`,
`unknown`:

| Value | Meaning |
| --- | --- |
| `none` | evidence that could have seen this tenant/key contains no allocation for it |
| `unknown` | no supplied evidence file's scope could have seen this tenant at all — **never** `none` |
| `reserved` | an allocation exists under that tenant/key, its immutable fields equal the plan's target, and it is `RESERVED` |
| `active` | the same, and the allocation is `ACTIVE` **and its binding carries a `verified_at`** — the platform's own verification, never a claim |
| `retired` | the allocation is `QUARANTINED` or `RELEASED` — the key is dead; the owning team's next request under it is refused `409 allocation_key_retired`, and the plan must name a different key |
| `mismatched` | an allocation exists under that tenant/key whose immutable fields **differ** from the plan's target — a concrete prediction that the team's first request will be refused `409 allocation_key_conflict`; the report names every differing field |

A `keep` move's target names only `tenant_id` and `allocation_key`
(adoption pins the existing CIDR), so it can never be `mismatched`. `retire`
and `undecided` moves carry no target at all, so their target fact is
`none` by construction — there is no key to look up.

**Conflicts** — for each id in the move's own `resolves` list, `present` or
`stale`:

| Value | Meaning |
| --- | --- |
| `present` | the current assessment still reports this conflict id — the move has **not** resolved what it claimed |
| `stale` | the current assessment no longer reports this conflict id — the record's own definition of "actually resolved": the conflict may have been fixed, or its resources may have changed underneath the id |

Plus, per move, the conflicts the current assessment reports **on that
subject** that the move does **not** claim in `resolves`, listed separately
as `unclaimed` — "a move that resolves nothing it claimed and has picked up
two conflicts it never mentioned is the case a plan most needs to surface."
And, at the report's top level (not per move), the conflicts the assessment
reports whose subject **no move mentions at all**, as `unplanned_conflicts`
— an estate that grew a conflict the plan never considered.

**`fully_evidenced`** (one boolean per move, the *only* aggregate this
report allows) is true exactly when: subject is `not-observed`, target is
`active`, every claimed conflict resolved `stale`, and there are zero
unclaimed conflicts. It is a **consequence**, not five independent checks —
see section 8's note on exit `0` for why a fully-evidenced move can never
also be `mismatched`, carry an unclaimed conflict, or be blocked.

## 7. `evidence.complete`, and the two templates

`evidence.complete` is `true` **if and only if**: the embedded assessment's
own `coverage.complete` is `true`, at least one `--allocations` file was
supplied, and the union of every supplied file's scope covers every tenant
the plan's targets name. Otherwise `false`. There is no third state and no
partial credit.

Every rendering of the summary sentence is selected by that one field, from
exactly two templates — there is no third template and no free-text summary
anywhere in this command. A test (`internal/migrate/report_test.go`)
asserts the text produced when `evidence.complete` is `false` contains none
of the forbidden phrases below, and asserts the template selection rather
than one example.

**Incomplete** (real, synthetic example — clauses are joined with `"; "`
when more than one applies):

> this progress report cannot make a full statement: the underlying
> assessment could not read 3 account(s) and 1 account-region pair(s); no
> allocation evidence was supplied at all

**Complete** (real, synthetic example):

> every account and account-region pair this plan's subjects touch was
> read, and evidence was supplied for every tenant this plan's targets
> name; 1 of 1 move(s) have subject, target and conflicts all affirmative
> at once

**The forbidden-word list**, extended from ADR 0014's own, checked
case-insensitively against this report's own prose (`summary.sentence` and
the two fixed notes) and against the full text-format rendering:
`conflict-free`, `no conflicts`, `ready`, `clean`, `complete`, `done`,
`migrated`, `cut over`, `reachable`, `unreachable`.

**The forbidden-word contradiction, and how it is handled.** ADR 0015 both
*mandates* a field named `evidence.complete` and *forbids* the word
"complete" anywhere in the output. The resolution, decided in review and
kept here: the guard scans this package's own **generated prose** —
`summary.sentence` and the two fixed notes, plus the full text rendering —
never the raw JSON field names or the embedded assessment's own verbatim
text (which legitimately says "complete", exactly as ADR 0014's own
template does). The mandated field keeps its name; nothing this package
writes itself ever uses the word.

The report prints no percentage, not a bounded one and not one with its
denominator beside it — a test asserts the rendered text contains no `%`
character in either format, which costs nothing because Go's own format
verbs leave none behind. Counts are printed instead: "1 of 1 move(s)" says
the same thing and cannot be quoted without its own denominator.

## 8. Exit codes

| Exit | Meaning |
| --- | --- |
| `0` | a report was produced and is **fully evidenced**: `evidence.complete` is `true`, the plan has at least one move, every move's `fully_evidenced` is `true`, and `unplanned_conflicts` is zero |
| `2` | usage error (a bad flag, an unrecognized `--format`, or no network data and no plan named at all) |
| `3` | a report was produced — printed to stdout exactly as for `0` — and is **not** fully evidenced: any incomplete-evidence case, any move not fully evidenced, or any unplanned conflict |
| `4` | no report exists at all: a plan decode or structural refusal (section 4), or any assessment-input refusal `onboard assess` itself would raise; nothing is printed to stdout |

**The exit-`0` reading.** ADR 0015's own prose says exit `0` is "a report
whose evidence is complete and in which nothing is blocked, stale,
mismatched, unclaimed or unplanned" — but that sentence is
self-contradictory read literally: a move can only ever resolve a claimed
conflict by that conflict becoming `stale` (an absent conflict id **is**
the record's own definition of "resolved"), so a report containing any move
that actually resolved something it claimed can never have zero `stale`
resolves. Requiring literally "zero stale" would make exit `0` unreachable
by the record's own worked example. The resolution — read from the
record's worked test list rather than its prose sentence — is: **every
move's `fully_evidenced` is true, plus no unplanned conflict.** The other
words in the sentence are consequences of `fully_evidenced` rather than
independent checks: a fully-evidenced move's target is always `active`,
never `mismatched`; it has zero unclaimed conflicts by definition; and its
subject is always `not-observed`, and the current assessment engine can
never report a conflict touching a subject it does not observe, so a
fully-evidenced move can never be blocked by one either. `unplanned` is the
one term that cannot be implied by any per-move fact (it names conflicts
touching no move's subject at all), so it is checked directly.

**A plan with zero moves is not clean.** A document with no `moves` at all
has evidenced nothing — it is an empty document, not a migration whose
every move is affirmative — and exits `3`, with a test pinning it.

**"Nothing has moved yet" also exits `3`, deliberately.** A move whose
subject is still `observed` (the VPC has not gone anywhere) and whose
target is `reserved` (the ledger holds the space, but the platform has not
yet verified a cloud binding) has complete evidence and yet is not fully
evidenced — ADR 0015 names this fixture explicitly, in advance, "so that
nobody builds a fixture that reaches `0` by leaving evidence out."

## 9. The JSON report, field by field

One `Report` value, rendered by two encoders (`--format json`/`--format
text`) that read the same struct and can never disagree. `report_version`,
an optional `stamp`, `inputs[]` (the plan files' own paths and SHA-256,
never the assessment's — those are nested under `assessment.inputs`),
`input_limits[]` (this package's own degradations — `no-evidence-supplied`,
`evidence-scope-missing`, `evidence-scope-not-covered` — independent of the
embedded assessment's own list), `assessment` (the embedded assessment's
own `inputs`, `input_limits`, `coverage` and `summary`, **verbatim** — see
[Overlap assessment](OVERLAP_ASSESSMENT.md) section 9 for that shape in
full), `evidence` (`complete`, `scopes[]`, `read_instants[]`, `digests[]`),
`summary` (`sentence`, and two **separate, never-summed** count blocks:
`derived` — `moves_total`, `fully_evidenced`, `by_subject_fact[]`,
`by_target_fact[]`, `resolved_present`, `resolved_stale`,
`unclaimed_conflicts`, `unplanned_conflicts`, `unmatched_subjects`; `typed`
— `moves_by_disposition[]`, `moves_approved`, `waves_total`,
`waves_approved`, `verifications_by_outcome[]`, `blockers_total`,
`keep_blocked_by_conflict`), `waves[]` (one roll-up per wave: id, name,
owner, approval, `moves_total`, `fully_evidenced`, `by_subject_fact[]`,
`by_target_fact[]`), `moves[]` (subject identity and its parts,
disposition, wave id, owner, approval, `depends_on[]`, `blockers[]`,
`rollback`, `verifications[]`, notes, `subject_fact` and its reason,
`unmatched`, `target_fact`, `target_mismatch_fields[]`, `resolves[]` — each
`{conflict_id, status}` — `unclaimed_conflicts[]`, `fully_evidenced`,
`keep_blocked_by_conflict`), top-level `unplanned_conflicts[]`, and `notes[]`
(always exactly the two disclaimers from section 1, in that order).

A real, synthetic report — the incomplete case from section 3's worked
plan, run against a gapped `estategen` estate with no `--allocations` at
all (excerpted; the actual output carries every field this section lists,
and `internal/migrate/derive_estate_test.go` /
`internal/onboardcmd/progress_estate_test.go` assert it byte for byte
against the planted facts):

```json
{
  "report_version": 1,
  "inputs": [
    {"path": "migration.yaml", "sha256": "7cd03ee1…"}
  ],
  "input_limits": ["evidence-scope-not-covered", "no-evidence-supplied"],
  "assessment": {
    "coverage": {"complete": false},
    "summary": {"sentence": "across the AWS network data actually observed, 1 VPC/CIDR relationship(s) prevent the intended connectivity (0 confirmed, 0 potential, 1 unknown); this report cannot make a complete statement for 3 account(s) and 1 account-region pair(s) because coverage is missing", "total_relationships": 1}
  },
  "evidence": {"complete": false, "scopes": [], "read_instants": [], "digests": []},
  "summary": {
    "sentence": "this progress report cannot make a full statement: the underlying assessment could not read 3 account(s) and 1 account-region pair(s); no allocation evidence was supplied at all",
    "derived": {"moves_total": 2, "fully_evidenced": 0, "resolved_present": 2, "resolved_stale": 0, "unclaimed_conflicts": 0, "unplanned_conflicts": 0, "unmatched_subjects": 0}
  },
  "moves": [
    {
      "subject": "aws:100000000000:eu-central-1:vpc-0000000013",
      "disposition": "replace", "wave_id": "wave-1",
      "subject_fact": "observed", "unmatched": false, "target_fact": "unknown",
      "resolves": [{"conflict_id": "c-82913251d179fcf5", "status": "present"}],
      "fully_evidenced": false
    },
    {
      "subject": "aws:100000000001:eu-central-1:vpc-0000000014",
      "disposition": "replace", "wave_id": "wave-1",
      "subject_fact": "observed", "unmatched": false, "target_fact": "unknown",
      "resolves": [{"conflict_id": "c-82913251d179fcf5", "status": "present"}],
      "fully_evidenced": false
    }
  ],
  "unplanned_conflicts": null,
  "notes": [
    "no route table, Transit Gateway attachment, propagation or traffic test was read; nothing in this report asserts that any two workloads can or cannot reach each other",
    "this report describes address compatibility for the target design the plan names, and nothing about routes, Transit Gateway attachments, propagations, security controls, DNS or application connectivity"
  ]
}
```

And the exit-`0` case (one move, subject not-observed, target active,
verified, no conflicts claimed or unclaimed):

```json
{
  "evidence": {"complete": true, "scopes": ["operator"], "read_instants": ["2026-09-22T01:51:54Z"]},
  "summary": {
    "sentence": "every account and account-region pair this plan's subjects touch was read, and evidence was supplied for every tenant this plan's targets name; 1 of 1 move(s) have subject, target and conflicts all affirmative at once",
    "derived": {"moves_total": 1, "fully_evidenced": 1, "unclaimed_conflicts": 0, "unplanned_conflicts": 0}
  },
  "moves": [
    {"subject": "aws:100000000000:eu-central-1:vpc-9999999999", "disposition": "replace", "wave_id": "wave-1",
     "subject_fact": "not-observed", "unmatched": false, "target_fact": "active", "fully_evidenced": true}
  ]
}
```

The text form renders the same value (a fixed-width table of moves —
subject, disposition, wave, owner, subject fact, target fact, conflicts,
fully evidenced — the wave roll-ups, the unplanned conflicts, and every
note in full).

## 10. Known limits

- **A target cannot express `address_family` or `availability_zone_id`.**
  `internal/service`'s `requestHash` compares six immutable request fields
  besides the ones this schema carries; two of them have no field anywhere
  in `migrate.Target` — a design decision (section 3), not an oversight —
  so a mismatch on either of those two fields is invisible to
  `target_fact: mismatched`.
- **The resolve status has only two values, `present` and `stale`** — not
  a third "absent" or "never claimed" value — because ADR 0015's own dated
  evidence section defines exactly two, and a move's `unclaimed_conflicts`
  list is a separate field entirely, not a third resolve status.
- **The forbidden-word contradiction** (section 7): `evidence.complete` is
  a mandated field name containing a forbidden word; the guard scans this
  package's own generated prose, never raw JSON field names or the
  embedded assessment's own verbatim text.
- **`parent_allocation_key` is resolved only from the same evidence
  read.** A parent whose row the same authenticated walk never saw (not
  expected for an unfiltered operator or tenant-wide export) leaves the
  field empty on export, which a plan naming that parent will then read as
  a `mismatched` target rather than a `retired` or `none` one.
- **Nothing has run against a real customer plan, a real estate, or a
  real deployment.** Every example and every end-to-end test
  (`internal/migrate/derive_estate_test.go`,
  `internal/onboardcmd/progress_estate_test.go`) uses
  `internal/assess/estategen`'s synthetic, generated estate and a
  hand-written synthetic plan. Whether four dispositions are enough,
  whether a move is the unit reviewers actually want, and whether a plan
  for a hundred accounts is readable as a flat list of moves are all
  untested (ADR 0015's own Consequences section says so).
- **The evidence export's confidentiality is the operator's own
  responsibility.** Nothing in this design encrypts, rotates or expires an
  evidence file; it lists allocations, tenants, keys and CIDRs, and an
  operator's export lists them across every tenant in the deployment. The
  report names the file's own SHA-256 digest so at least the bytes a
  conclusion rests on can be identified afterwards.
- Everything [Overlap assessment](OVERLAP_ASSESSMENT.md) section 11 lists
  about the embedded assessment applies here unchanged, since this
  command calls the identical engine.

## 11. Where this fits

[Organization inventory](AWS_ORGANIZATION_INVENTORY.md) collects the input;
[Overlap assessment](OVERLAP_ASSESSMENT.md) (`onboard assess`) turns it into
a conflict report with no remediation, no owner and no decision status.
This command is the next step over the **same** inventory and the **same**
engine: a reviewed plan of who is moving where, and the evidence-backed
progress of that plan against the ledger's own committed allocations. It
never replaces `onboard assess`'s own conflict listing, which this report
does not repeat — it embeds that report's `coverage` and `summary`
verbatim instead. It never creates a NetBox object, a ledger row or an
endpoint — see ADR 0015's own "Where it lives" for the full boundary. The
[adoption runbook](../deploy/runbooks/ADOPTION.md) and
[ADR 0010](decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md)
are where a `keep` move's target actually becomes an owned allocation; this
report only says whether that target, once made, is `reserved`,
`verified`, `retired` or `mismatched`.
