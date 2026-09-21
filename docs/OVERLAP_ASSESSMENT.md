# Overlap assessment: `platform-ipam onboard assess`

Status: implemented, 2026-09-20 (docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md,
work-plan packages M1b1–M1b4). Every flag, exit code, field name and example
sentence below is copied from `internal/onboardcmd/assess.go`, `internal/assess`
and a real run of the command (the worked example in section 9), never from
memory. **`scripts/aws/org-inventory.sh` has never run against a live AWS
Organization** ([organization inventory](AWS_ORGANIZATION_INVENTORY.md)), so
every claim below about what a real inventory looks like is a claim about a
stubbed fixture and this document's own generated estate; see section 11 for
the complete list of what is not yet verified.

## 1. What this is for, and what it deliberately is not

`platform-ipam onboard assess` reads the organization inventory's original,
per-resource records — one row per observed VPC CIDR association, not the
collapsed prefixes an import produces — and reports every pair of different
VPCs whose CIDRs are equal or one contains the other, as a durable, identified
conflict with both source records attached. It classifies each conflict's
impact as `confirmed`, `potential` or `unknown` against a connectivity matrix
the customer supplies, and it carries the coverage it could not obtain as a
first-class part of its own result, so it can never claim to be complete when
it is not. It is the tool that lets an operator say, in the owner's own words:
"Across the AWS network data I actually observed, these N VPC/CIDR
relationships prevent the intended connectivity. Here is why each was
classified as a conflict, and here are the accounts/VPCs for which I cannot
make a complete statement because coverage is missing."

It never opens the ledger, never calls NetBox, never calls AWS, never
allocates, and reads no `IPAM_` environment variable — a test
(`TestAssessRunsWithEmptyEnvironment`) runs it with the environment cleared,
and a source-parsing test (`TestAssessSourceImportsExcludeAdapterPackages`)
asserts `assess.go` imports none of `internal/config`, `internal/netbox`,
`internal/storage`, `internal/service`, `internal/cloud` or
`internal/transport`. It writes to nowhere but stdout, stderr and the single
path a `--out` flag may name.

What it deliberately never does, in its own words and in this document's:

- It never claims reachability. It prints, once, in every report: "no route
  table, Transit Gateway attachment, propagation or traffic test was read;
  nothing in this report asserts that any two workloads can or cannot reach
  each other." The words "reachable", "unreachable" and "ready" do not appear
  in any template it can produce (see section 4's forbidden-phrase list).
- It never reads route tables, Transit Gateway attachments or propagations.
  Two overlapping CIDRs are a mathematical fact about the data; whether that
  fact blocks an *intended* connection needs a reviewed connectivity matrix
  (section 6), and whether traffic actually flows needs a test this project
  does not perform.
- It never says what to do next. There is no remediation field in the report
  itself — decisions and remediation notes live in a separate file the
  operator maintains (section 8) — and no migration plan, wave, or cutover
  step. That is gap M3 of
  [Overlapping AWS networks](IP_OVERLAP_MIGRATION.md), not this tool's.
- It never claims completeness while `coverage.complete` is `false`. A single
  boolean field selects one of exactly two summary templates (section 4); a
  test asserts that the text produced when `coverage.complete` is `false`
  contains none of "conflict-free", "no conflicts", "ready" or "clean".

## 2. Inputs, and how each absent one degrades

```
platform-ipam onboard assess --inventory DIR [--format json|text] [--out PATH] [--stamp TEXT]
platform-ipam onboard assess --networks FILE [--networks FILE ...] [--failures FILE] [--accounts FILE] [--run FILE] ...
```

`--inventory DIR` is shorthand for `--networks DIR/networks.csv --failures
DIR/failures.csv --accounts DIR/accounts.json --run DIR/run.json`; an explicit
flag always wins, and a file `--inventory` would imply that does not exist on
disk is silently treated as not given (an interrupted collector run legitimately
leaves no `run.json`, per the [organization inventory](AWS_ORGANIZATION_INVENTORY.md)
procedure). An explicitly named `--failures`/`--accounts`/`--run` that does not
exist is a hard error: naming a file is a promise it will be read.

`--networks` is repeatable and accepts anything `onboard plan` accepts (CSV,
TSV, a pasted table, `.xlsx`, or `-` for stdin), through the same reader and
normalizer ([onboarding import](ONBOARDING_IMPORT.md) section 3–4). Several
inputs are concatenated; each keeps its own source file name for traceability
(section 5), which is why `internal/onboardcmd`'s `concatTables` stamps the
source file onto every row rather than letting row numbers restart silently.

At least one `--networks` input (directly or via `--inventory`) is required;
with none, the command refuses at usage level (exit `2`) if no `--inventory`
was named either, or at adapter level (exit `4`, naming the directory) if
`--inventory` was named but its `networks.csv` does not exist. "No network
data" is not the same fact as "an empty estate", and the command never
conflates them — see the refusal list in section 3.

| Input | Absent | Present but a column is missing |
| --- | --- | --- |
| `cidr`, `account_id`, `region`, `type`, `resource_id` columns | — (these are the networks table itself) | **refuses**, exit `4` (section 3) |
| `association_id` column | every side's `association_id` is `null`, `input_limits` carries `association-id-missing` | same |
| `observed_at` column | every side's `observed_at` is `null`, `input_limits` carries `observation-time-missing` | same |
| `primary` column, or a cell this command does not recognize (anything but the literal `true`/`false`, case-insensitively) | that side's `primary` is `"unknown"`, `input_limits` carries `primary-missing` | same |
| `--accounts` (`accounts.json`) | `coverage.accounts_missing` is `true`, `input_limits` carries `expected-accounts-unknown`, `coverage.complete` can never be `true` | n/a (foreign AWS shape, not validated against a schema) |
| `--run` (`run.json`) | `coverage.run_missing` is `true`, `input_limits` carries `attempted-set-unknown`, `coverage.complete` can never be `true` | n/a |
| `run.json`'s own `row_count` on a `succeeded`/`partial` attempt (package M1c) | that account/region pair's row count is never cross-checked against the rows present, `input_limits` carries `row-count-missing` (an older collector run, `script_version 1`, which predates `row_count` entirely) | n/a — `row_count` is a field inside `run.json`, not a column of a table with a header to be missing from |
| `--failures` (`failures.csv`) | no `failed` entries are added from it (an incomplete region can still be named via `run.json`'s own `partial`/`failed` outcomes — see the dated amendment in [ADR 0014](decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md)) | required columns missing: **refuses**, exit `4` |
| `--matrix` | every conflict's impact is `unknown`, `matrix_relation` is `not_in_matrix` | decode error: **refuses**, exit `4` |
| `--ownership` | every side's `product`/`environment`/`owner` is the literal string `"unknown"` | decode error: **refuses**, exit `4` |
| `--fixed` | no fixed-range relationships | decode error: **refuses**, exit `4` |
| `--decisions` | every conflict's `decision` field is absent, `decisions_undecided` counts them all | decode error: **refuses**, exit `4` |

Column presence is tracked **per input file**, from that file's own header, not
globally: an old-format `networks.csv` (no `association_id`/`observed_at`
columns) merged with a new-format one produces `null` association ids and
observed times only on the old file's side of a conflict, and both limits still
appear once in `input_limits` because at least one surviving row is missing
each column.

## 3. Refusals: exit `4`, no report at all

These are the only conditions under which the command produces **no output on
stdout** (an important distinction from exit `3`, section 4, where a report
*is* produced and printed):

| Refusal | Where it is checked |
| --- | --- |
| A required column (`cidr`, `account_id`, `region`, `type`, `resource_id`) is missing from a networks input | `internal/assess.Validate`, called first inside `Assess` |
| A row the normalizer dropped (an error-level diagnostic: an unparsable CIDR was rejected before the row even reached comparison) | `internal/onboardcmd/assess.go`'s own check of `table.Diagnostics`, naming the file, row and message for every dropped row |
| A row's `type` is neither `vpc` nor `subnet` | `internal/onboardcmd/assess.go`'s `buildResourceRecords`, naming the file, row and the type it read |
| No network data: `--inventory DIR` has no `networks.csv`, or `--inventory` names a directory but no `--networks`/`--inventory` was given at all | the first case is exit `4` (a directory was named and had nothing to read); the second is exit `2` (usage — nothing at all was named) |
| An input file does not exist, or is not readable | `os.ReadFile` error, any input |
| `accounts.json`, `run.json` or `failures.csv` does not decode as its own schema, or `failures.csv` is missing one of its five required columns | the matching decoder in `internal/onboardcmd/assess.go` |
| `--matrix`, `--ownership`, `--fixed` or `--decisions` is not valid YAML (or JSON) at all, or does not decode against its own schema (`DisallowUnknownFields` is set on every one of them) | `readYAMLOrJSONFile` and the matching `internal/assess.Decode*` function |
| More relationships than `--max-relationships` (default `100000`) would be produced | `internal/assess.Assess`, via `*TooManyRelationshipsError`, naming the count it would have produced |

A present-but-malformed CIDR (a value that is not a canonical IPv4 prefix, for
example one with host bits set) is **not** on this list: it degrades instead,
excluded from every comparison with a `invalid-cidr` note naming the file and
row (`internal/assess.buildAssociations`), the same "report and exclude"
treatment `internal/onboard/plan.go` already gives a malformed CIDR under
`RuleInvalidCIDR`.

## 4. Exit codes, and the two summary templates verbatim

| Exit | Meaning |
| --- | --- |
| `0` | A report was produced; `coverage.complete` is `true` and no conflict is `confirmed` (`Report.Clean()`) |
| `2` | Usage error (a bad flag, an unrecognized `--format`, or no network data named at all) |
| `3` | A report was produced — printed to stdout exactly as it would be for exit `0` — and it is **not** clean: coverage is incomplete, or at least one conflict is `confirmed` |
| `4` | No report exists at all (section 3); nothing is printed to stdout |

Exactly two summary templates exist, selected by `coverage.complete` alone,
never by anything else:

**Complete, no relationship at all** (verbatim, `--stamp` appends `" at
<stamp>"` when given, and is otherwise omitted — never derived from the wall
clock):

> no conflicting relationship was observed in the scope read

**Complete, at least one relationship** (verbatim, `%d` are the summary's own
counts):

> across the AWS network data actually observed, %d VPC/CIDR relationship(s)
> prevent the intended connectivity (%d confirmed, %d potential, %d unknown)

**Incomplete, no VPC-to-VPC or fixed relationship found in what WAS read**
(verbatim):

> this report cannot make a complete statement for %d account(s) and %d
> account-region pair(s) because coverage is missing; no VPC/CIDR relationship
> was found in the scope that was read, but an incomplete scope is not
> evidence that none exist

**Incomplete, at least one relationship found** (verbatim — the two sentences
above are joined with `"; "`, the relationship-count sentence first):

> across the AWS network data actually observed, %d VPC/CIDR relationship(s)
> prevent the intended connectivity (%d confirmed, %d potential, %d unknown);
> this report cannot make a complete statement for %d account(s) and %d
> account-region pair(s) because coverage is missing

There is no third template, and no free-text summary anywhere in the tool. A
test (`internal/assess`'s own `report_test.go`) asserts that the text
produced when `coverage.complete` is `false` contains none of the forbidden
phrases `"conflict-free"`, `"no conflicts"`, `"ready"`, `"clean"`
(`assess.ForbiddenCompletePhrases()`), in either `--format json` or `--format
text`.

## 5. Conflict ids, and why they are stable

A resource identity is `aws:<account>:<region>:<vpc>:<cidr>`, with
`:<association>` appended only when the association id is known. A conflict
id is `c-` followed by the first sixteen hex characters of the SHA-256 of the
relationship kind and the two resource identities, joined by a separator,
**after sorting the two identities as byte strings** — so
`ConflictID(kind, a, b) == ConflictID(kind, b, a)`, and the id does not depend
on which record the sweep happened to visit first.

The id deliberately excludes everything that is not the two resources and the
relationship kind: not the observation time, not the source file or row, not
ownership, not the matrix, not the impact. That means a decision recorded
against `c-1a2b3c4d5e6f7a8b` in `--decisions` survives a new collection run,
survives a matrix being supplied for the first time, and survives the
inventory growing — only the conflicts whose *resources themselves* changed
(a VPC's CIDR changed, an account/region pair changed) mint a new id and let
the old one go stale (section 8).

Traceability is a stronger property than the id alone: every conflict's two
`sides` each carry `source_file` and `source_row`, and those name the *exact*
row of the *exact* input file that produced that side — reading that file at
that row reproduces that side's `account_id` and `cidr` byte for byte
(`TestAssessConflictsAreTraceableToTheirSourceRows`,
`internal/onboardcmd/assess_test.go`; the same property is asserted end to end
against the real collector's own CSV in
`tests/aws/test_assess_from_inventory.sh`, section 10).

## 6. The reviewed matrix: format, worked example, and the quoted-account-id rule

The connectivity matrix is a YAML (or JSON) document with `version`, a list of
`groups` (each an `id` and a list of member entries), a `must_communicate`
list of `[group-id, group-id]` pairs, a `must_stay_isolated` list of the same
shape, and a `shared_services` list of group ids that must communicate with
every other assigned group. A member entry is an account id, a VPC id or a
product name, matched in that order of precedence: an explicit VPC id wins
over a product name, and a product name wins over an account id, so the common
case ("this whole account, except that one VPC") needs no special syntax.

```yaml
# reviewed connectivity matrix -- synthetic, worked example
version: 1
groups:
  - id: g-equal-a
    members:
      - "111111111111"
  - id: g-equal-b
    members:
      - "222222222222"
must_communicate:
  - [g-equal-a, g-equal-b]
must_stay_isolated: []
shared_services: []
```

**The quoted-account-id rule.** An account id (or any other digit string used
as a member entry) MUST be quoted in YAML. An unquoted, all-digit scalar —
`111111111111` without quotes, and *especially* one with a leading zero, which
some YAML resolvers additionally read as octal — decodes as a YAML/JSON
**number**, not a string, and this project's decoders (`encoding/json`'s
strict decode, which never silently coerces a JSON number into a string field)
refuse it with an error naming the offending field, rather than truncating or
misreading it. This is deliberate, not a special case: the same rule applies
to every string-typed field a bare digit sequence could land in, not only
account ids
(`TestAssessMatrixRejectsUnquotedLeadingZeroAccountID`,
`TestAssessOwnershipAcceptsQuotedLeadingZeroAccountID`,
`internal/onboardcmd/assess_test.go`).

A conflict is `confirmed` when both sides are assigned to groups the matrix
requires to communicate (including both sides landing in the *same* group,
which by definition must communicate with itself, and the shared-service
case). It is `potential` when at least one side is assigned and the matrix
does not decide the pair — including a pair the matrix marks
`must_stay_isolated`, which still lists the conflict (isolation is a policy
decision enforced by route tables and security controls, not a property of
the addresses, and every such conflict carries an `isolation_note` saying
exactly that) — or when only one side is assigned at all. It is `unknown` when
no matrix was supplied, when neither side is assigned, or when a VPC matches
more than one group and is therefore ambiguous (every conflict touching that
VPC on either side becomes `unknown`, and one `ambiguous-group-assignment`
note is added per ambiguous VPC).

## 7. Ownership and `--fixed`: formats and worked examples

**`--ownership`** is a YAML (or JSON) list — or an object with an `entries`
key wrapping the same list — of rows: `account_id` (quoted, same rule as
above), an optional `vpc_id` (empty or omitted means "the whole account"),
`product`, `environment`, `owner`. A row naming a specific `vpc_id` wins over
one naming only the account.

```yaml
# reviewed ownership table -- synthetic, worked example
- account_id: "111111111111"
  product: sample-product
  environment: prod
  owner: platform-team
```

Ownership never comes from the AWS `Name` tag or from an account naming
convention — both are carried on a side as evidence, labelled as what they
are, and never used to infer product, environment or owner. A resource with no
matching entry gets the **literal string** `"unknown"` in all three fields,
never an empty string and never an omitted key, so absence cannot be mistaken
for a value a CSV merely lost.

**`--fixed`** is a YAML (or JSON) list of non-AWS ranges — an on-premises
network, a pool container, a partner range — that enter the assessment only
through this explicit table, never through loaded pools configuration:

```yaml
# fixed (non-AWS) ranges -- synthetic, worked example
- cidr: 192.0.2.0/24
  description: on-premises documentation range (RFC 5737)
  owner: network-team
```

**A fixed range's identity is its CIDR**, and only its CIDR — not its row
position in the table. A CIDR written twice in `--fixed` is one range (the
first occurrence in a stable secondary sort, never the row order the operator
happened to type them in), because the first implementation took a fixed
range's identity from its position, which gave one real-world conflict two
different ids depending on the order somebody wrote the table in (see the
dated amendment to [ADR 0014](decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md)).
A relationship with a fixed range on one side is reported with the same two
kinds (`equal-cidr`/`contains`), marked `"fixed": true` on that side and on
the conflict, counted in `summary.fixed_relationships` **separately** from
`summary.total_relationships` so the headline VPC-to-VPC count is never
inflated by it, and can never be `confirmed` — a fixed range has no VPC and so
is never assigned to a connectivity group.

`--fixed`'s own source tracking is thinner than a networks row's: the schema
(`cidr`, `description`, `owner`) has no file-of-its-own concept the way a
merged `--networks` input does, so `internal/onboardcmd/assess.go` supplies
`source_file` as the one `--fixed` path given and `source_row` as the entry's
own ordinal position in that file's decoded list — the closest honest analogue
this format has to a CSV row number. ADR 0014 does not define this; it is a
decision this package made and reports here rather than in the record itself.

## 8. Decisions

`--decisions` is a YAML (or JSON) object keyed by conflict id (section 5),
each value a `decision` (free text — this project imposes no enum), an
optional `remediation` note, `responsible` party, `reviewed_by` and
`reviewed_at`. The command echoes a conflict's matching entry into its
`decision` field unchanged; nothing above it in the report is affected by
having a decision at all.

```yaml
# decisions -- synthetic, worked example
c-afb0635483d9bc8c:
  decision: accepted-risk
  remediation: renumber one VPC before the migration wave
  responsible: network-team
  reviewed_by: alice
  reviewed_at: "2026-09-20T00:00:00Z"
```

`summary.decisions_decided`/`decisions_undecided` count conflicts *in this
report* with and without a matching entry; `summary.decisions_stale` counts
entries in `--decisions` whose conflict id **no longer appears in this
report** — the one place a reviewer, who has been recording decisions for a
month, learns that a conflict was either resolved or that its resources
changed underneath the id. That count is rendered prominently in the text
form (a whole line: `decisions: %d decided, %d undecided, %d stale (conflict
id no longer appears)`), not only in the JSON, exactly because it is easy to
miss.

## 9. The JSON report, field by field, with a real worked example

A `Report` is one value, rendered by two encoders (`--format json`/`--format
text`) that can never disagree because both read the same struct. This is a
real, complete report — an equal-cidr conflict between two accounts, no
coverage gap, no matrix, no ownership — from a run of the actual collector
(`scripts/aws/org-inventory.sh`, a stubbed `aws` CLI) into the actual command;
only the temp-directory paths in `source_file`/`inputs[].path` are trimmed for
readability (they are ordinarily the real, absolute paths the operator gave):

```json
{
  "report_version": 1,
  "inputs": [
    {"path": "inventory/accounts.json", "sha256": "d88588a2…"},
    {"path": "inventory/failures.csv", "sha256": "4a74f351…"},
    {"path": "inventory/networks.csv", "sha256": "8f0c1665…"},
    {"path": "inventory/run.json", "sha256": "f06223c8…"}
  ],
  "input_limits": null,
  "coverage": {
    "complete": true,
    "failed": null, "partial": null, "not_attempted": null, "read_empty": null,
    "run_missing": false, "accounts_missing": false
  },
  "summary": {
    "sentence": "across the AWS network data actually observed, 1 VPC/CIDR relationship(s) prevent the intended connectivity (0 confirmed, 0 potential, 1 unknown)",
    "total_relationships": 1,
    "by_kind": [{"kind": "equal-cidr", "count": 1}, {"kind": "contains", "count": 0}],
    "by_impact": [{"impact": "confirmed", "count": 0}, {"impact": "potential", "count": 0}, {"impact": "unknown", "count": 1}],
    "fixed_relationships": 0, "incomplete_accounts": 0, "incomplete_region_pairs": 0,
    "implicated_vpcs": 0, "duplicate_observations": 0, "unknown_ownership_sides": 2,
    "decisions_decided": 0, "decisions_undecided": 1, "decisions_stale": 0
  },
  "conflicts": [
    {
      "id": "c-afb0635483d9bc8c",
      "kind": "equal-cidr", "impact": "unknown", "matrix_relation": "not_in_matrix",
      "intersection": "10.0.0.0/16", "fixed": false,
      "sides": [
        {
          "identity": "aws:111111111111:eu-central-1:vpc-alpha0000001:10.0.0.0/16:vpc-cidr-assoc-alpha1",
          "account_id": "111111111111", "region": "eu-central-1", "vpc_id": "vpc-alpha0000001",
          "cidr": "10.0.0.0/16", "association_id": "vpc-cidr-assoc-alpha1", "primary": "true",
          "name": "alpha-vpc", "state": "available", "observed_at": "2026-09-20T15:20:34Z",
          "product": "unknown", "environment": "unknown", "owner": "unknown", "fixed": false,
          "source_file": "inventory/networks.csv", "source_row": 2
        },
        {
          "identity": "aws:222222222222:eu-central-1:vpc-beta00000001:10.0.0.0/16:vpc-cidr-assoc-beta01",
          "account_id": "222222222222", "region": "eu-central-1", "vpc_id": "vpc-beta00000001",
          "cidr": "10.0.0.0/16", "association_id": "vpc-cidr-assoc-beta01", "primary": "true",
          "name": "beta-vpc", "state": "available", "observed_at": "2026-09-20T15:20:34Z",
          "product": "unknown", "environment": "unknown", "owner": "unknown", "fixed": false,
          "source_file": "inventory/networks.csv", "source_row": 3
        }
      ]
    }
  ],
  "notes": [
    {"kind": "reachability-disclaimer",
     "message": "no route table, Transit Gateway attachment, propagation or traffic test was read; nothing in this report asserts that any two workloads can or cannot reach each other"}
  ]
}
```

And the same report, `--format text` (unmodified):

```
across the AWS network data actually observed, 1 VPC/CIDR relationship(s) prevent the intended connectivity (0 confirmed, 0 potential, 1 unknown)
relationships: 1 total (1 equal-cidr, 0 contains), 0 fixed; impact 0 confirmed, 0 potential, 1 unknown
coverage: complete=true, 0 account(s) and 0 account-region pair(s) without a complete statement, 0 VPC(s) implicated
duplicate observations collapsed: 0; sides with unknown ownership: 2
decisions: 0 decided, 1 undecided, 0 stale (conflict id no longer appears)

CONFLICT            KIND        IMPACT   ACCOUNT       VPC               CIDR         PRODUCT  OWNER
c-afb0635483d9bc8c  equal-cidr  unknown  111111111111  vpc-alpha0000001  10.0.0.0/16  unknown  unknown
c-afb0635483d9bc8c  equal-cidr  unknown  222222222222  vpc-beta00000001  10.0.0.0/16  unknown  unknown

coverage gaps:
  failed: 0
  partial: 0
  not attempted: 0
  read and empty: 0
notes:
  - [reachability-disclaimer] no route table, Transit Gateway attachment, propagation or traffic test was read; nothing in this report asserts that any two workloads can or cannot reach each other
```

Field reference (every JSON field this version can produce):

| Field | Meaning |
| --- | --- |
| `report_version` | integer, bumped whenever a field is added, renamed or removed |
| `stamp` | the `--stamp` value verbatim, omitted when not given — the *only* field that did not come from an input file; never the wall clock |
| `inputs[].path`, `.sha256` | every input file read, as given, with the SHA-256 of its exact bytes — what makes two reports comparable |
| `input_limits` | the degradations named in section 2, sorted, `null` when none apply |
| `coverage.complete` | `true` **iff** `failed`, `partial` and `not_attempted` are all empty **and** `run.json` **and** `accounts.json` were both supplied |
| `coverage.failed[]` | one entry per `failures.csv` row (account, region — empty means the whole account, stage, error) **plus** any `run.json` attempt whose outcome this version does not recognize |
| `coverage.partial[]` | account/region pairs `run.json` records as `partial` (VPC call succeeded, subnet call failed) |
| `coverage.not_attempted[]` | account/region pairs `run.json` records as `not_attempted`, plus (with no `run.json` at all) every `ACTIVE` account with no attempt recorded anywhere |
| `coverage.read_empty[]` | account/region pairs `run.json` records as `succeeded` with zero rows — "read and empty", the statement nothing but `run.json` can make |
| `coverage.run_missing`, `.accounts_missing` | `true` when `--run`/`--accounts` were not supplied at all |
| `summary.sentence` | the rendered sentence, section 4 |
| `summary.total_relationships` | VPC-to-VPC conflicts only (excludes `fixed_relationships`) |
| `summary.by_kind[]`, `.by_impact[]` | always both enumerated values, including a zero count, so the JSON shape never varies by content |
| `summary.fixed_relationships` | conflicts with a `--fixed` side, counted separately |
| `summary.incomplete_accounts`, `.incomplete_region_pairs` | the union of `failed`, `partial` and `not_attempted` (not `read_empty`, which *is* a complete statement) |
| `summary.implicated_vpcs` | distinct VPCs whose account, or specific account-region pair, falls inside a coverage gap |
| `summary.duplicate_observations` | rows collapsed as the *same* observation (identical account/region/VPC/CIDR/association id) |
| `summary.unknown_ownership_sides` | conflict sides whose resolved owner is the literal `"unknown"` |
| `summary.decisions_decided/undecided/stale` | section 8 |
| `conflicts[].id`, `.kind`, `.impact`, `.matrix_relation`, `.intersection`, `.fixed` | sections 5–6 |
| `conflicts[].sides[]` | exactly two, sorted by `identity`; every field a `ResourceRecord` or `FixedRange` carries, plus `group`/`group_ambiguous` (matrix assignment) and `role` (`"outer"`/`"inner"`, only for a `contains` conflict) |
| `conflicts[].isolation_note` | present only when `matrix_relation` is `must_stay_isolated` |
| `conflicts[].decision` | section 8, omitted when no matching `--decisions` entry exists |
| `notes[]` | `reachability-disclaimer` (always first, always present), plus `invalid-cidr`, `subnet-parent-missing`, `subnet-outside-parent`, `vpc-id-reused-across-accounts`, `high-fanout-cidr`, `ambiguous-group-assignment` as they occur |

## 10. A worked example from the synthetic estate generator

`internal/assess/estategen` (package M1b4) writes a whole synthetic,
collector-shaped inventory directory — `networks.csv`, `accounts.json`,
`failures.csv`, `run.json`, plus a matching `matrix.yaml`, `ownership.yaml`,
`fixed.yaml` and `decisions.yaml` — with known planted facts: a chosen number
of `equal-cidr` and `contains` relationships between named VPCs, a secondary
association, a duplicate observation, a VPC with subnets, one account that
could not be assumed, one region that is partial and populated, one region
read and empty, and one `ACTIVE` account never attempted at all. Its own test,
`TestGappedEstateProducesExactlyThePlantedRelationships`
(`internal/assess/estategen/estategen_test.go`), generates such an estate, runs
it through the real `platform-ipam onboard assess` command (`onboardcmd.Main`),
and asserts every one of those facts comes back out: the exact relationship
count by kind, the named VPC pairs, the three-valued impact against the
generated matrix, and every coverage list entry by entry — `coverage.complete`
is `false` and the command exits `3`. A second, fully-covered estate
(`TestCleanEstateIsCleanAndCompletelyRead`) plants none of those seven gaps and
no conflict at all: `coverage.complete` is `true` and the command exits `0`.

The generator is a test and demonstration tool, not part of the product: it is
not reachable from `cmd/platform-ipam`'s command surface, and it adds no
dependency — the only non-standard-library package it imports is this
repository's own `internal/assess`, for `ResourceRecord.Identity` and
`ConflictID`, so the facts it predicts are computed the *same* way the engine
computes them rather than by a second, possibly drifting formula. Every
account id, VPC id, association id and CIDR it writes is synthetic (RFC 1918
or RFC 5737 space, account ids computed arithmetically from a literal base);
its own test scans every file it writes — not just its Go source — for a
twelve-digit sequence and asserts each one is an id the generator itself
reported planting.

The real end-to-end pipeline (the actual collector script, stubbed `aws` CLI,
into the actual command) has its own separate test,
`tests/aws/test_assess_from_inventory.sh`, described in the next section.

## 11. Known limits

These come from the review notes in `docs/WORK_PLAN.md`, and each is listed
here because an operator reading a report needs to know them:

- **The row-count cross-check counts rows, not meaning.** `run.json`'s
  `row_count` is compared with the source rows present for that account and
  region (section 2): fewer rows than recorded is incomplete coverage
  (`coverage.row_count_short`), more is a data error
  (`coverage.row_count_exceeded`), and either keeps `coverage.complete` false.
  It cannot detect a row that was *edited* rather than removed, a `run.json`
  that was edited to match, or a run record older than the collector's
  `row_count` field (`input_limits` then carries `row-count-missing`). Two
  different files covering one region are summed, so an old and a new export of
  the same estate read together show up as a surplus, not as a clean report.
- **`--sheet` is not exposed** on this command (unlike `onboard plan`): a
  multi-sheet `.xlsx` networks input reads its first sheet only.
- **`account_name` is not carried onto a side.** `internal/onboard.NetworkRow`
  has no dedicated field for it (pre-existing behaviour, unrelated to this
  package); it lands in the row's free-text description for every import, not
  in this command's structured output.
- **An association read once with and once without its association id is two
  observations, not one.** The duplicate-observation collapse (section 9's
  `duplicate_observations`) keys on the full tuple including the association
  id when it is present; an old collector run and a new one covering the same
  VPC and CIDR, one missing the column, therefore produce two distinct
  resource identities rather than collapsing to one.
- **The high-fan-out threshold of 20** (the point at which a `high-fanout-cidr`
  note is added, warning that one CIDR shared by many VPCs is producing a
  combinatorial number of pairwise relationships) **is a placeholder**, not a
  value derived from any measurement or customer requirement.
- **Nothing has run against a real AWS Organization.** `scripts/aws/org-inventory.sh`
  is verified only against a stubbed `aws` CLI
  (`tests/aws/test_org_inventory.sh`, `tests/aws/test_assess_from_inventory.sh`);
  the `jq` filters that produce `networks.csv` and `run.json` have been
  checked against synthetic `describe-vpcs`/`describe-subnets` output only.
  The first real run may find pagination, permission or output shape
  surprises this design does not yet account for.
- Nothing in this package changes the estate-scale claim ADR 0014 makes: ten
  thousand associations across two hundred accounts finishing in under five
  seconds is a target the design believes is comfortable, not a result — no
  measurement at that scale has been taken by this package either, only by
  `internal/assess`'s own scale test over a synthetic, in-memory estate.

## 12. Where this fits

[Organization inventory](AWS_ORGANIZATION_INVENTORY.md) collects the input;
run this command against its output *before* deciding what to import — an
assessment changes nothing and needs no NetBox credentials, so it costs
nothing to run early and often, and its output survives a customer growing
their AWS footprint between assessments (conflict ids are stable, section 5).
[Onboarding import](ONBOARDING_IMPORT.md) is a different question over the
same records: it collapses overlapping rows into one write for the allocator's
sake, which is exactly wrong for a conflict report, and the two tools
deliberately share only their reader, never their validator. Remediation,
waves, approvals and cutover evidence are gap M3 of
[Overlapping AWS networks](IP_OVERLAP_MIGRATION.md), not this tool's.
