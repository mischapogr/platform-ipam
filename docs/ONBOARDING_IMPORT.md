# Onboarding import: existing accounts, networks and IP ranges

Status: accepted design, 2026-09-18. **Implemented through package C5**: parsing, validation, the NetBox writer and the `onboard` command exist and were run against a live NetBox in the development stack. Still open in the [work plan](WORK_PLAN.md): the end-to-end import test (C7), the agent skill (C8), plugin objects (N3). The `.xlsx` reader is tested against ten workbooks written by openpyxl 3.1.5, XlsxWriter 3.2.9 and LibreOffice 25.8 (committed under `internal/onboard/testdata/real/` with their generators). **No file written by Microsoft Excel has been read.** Decision record: [ADR 0007](decisions/0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md).

## 1. What the import does, and what it refuses to do

Before platform-ipam may hand out address space from a pool, it has to know what is already in use. The import takes tables that people already have — a spreadsheet, a CSV from the [organization inventory](AWS_ORGANIZATION_INVENTORY.md), a table copied out of Confluence — and turns them into three outputs:

| Output | Lands in | Effect |
| --- | --- | --- |
| Networks and ranges | **NetBox**, as prefixes in the domain's VRF *without* platform allocation markers | the allocator never selects overlapping space |
| Accounts | **YAML fragments for human review**: `cloud_coverage` cells, pool `eligible_accounts`, identity skeletons | nothing, until a person merges them and redeploys |
| Observed VPCs/subnets | a **fake-cloud fixture** for the development stack | the local stack behaves as if those networks existed |

It does **not** create allocations. An imported network has no allocation key, no tenant and no lifecycle; it is occupied space, nothing more. Turning an imported network into an owned allocation is a separate step, `platform-ipam adopt` — see [section 9](#9-import-then-adopt) and the [adoption runbook](../deploy/runbooks/ADOPTION.md) — because it must mint ledger records and NetBox markers consistently, and no consumer endpoint may ever do that (`docs/API_V1.md:30`).

Before deciding what to import, `platform-ipam onboard assess` (ADR 0014, [Overlap assessment](OVERLAP_ASSESSMENT.md)) can report which VPC CIDR associations in the same inventory conflict with each other, offline and without NetBox credentials. It is a different question over the same records — it collapses nothing, where `plan` below collapses overlapping rows into one write — and running it costs nothing: `platform-ipam onboard assess --inventory inventory`.

Why NetBox occupancy is sufficient: `Snapshot` turns every prefix, IP address and IP range in the domain's VRF into occupied space (`internal/netbox/client.go`), and `chooseCIDR` skips anything that overlaps it (`internal/service/service.go`). The existing docs already prescribe this: "import or exclude existing occupancy in NetBox … before enabling pool admission" (`docs/AWS_INTEGRATION.md:83`).

## 2. Interface

An operator process mode in the service binary, next to `api|worker|migrate|client`. It needs the pools configuration and, for `apply`, NetBox credentials. It never opens the ledger database.

```
platform-ipam onboard parse   <input>...  --out table.json    # any format -> canonical table
platform-ipam onboard plan    table.json  --domain <id>       # validate; print report; write nothing
platform-ipam onboard apply   table.json  --domain <id> --batch <name> [--refresh]  # write NetBox occupancy
platform-ipam onboard render-config  table.json               # YAML fragments to stdout
platform-ipam onboard render-fixture table.json --domain <id> # fake-cloud JSON to stdout
platform-ipam onboard remove  <cidr> --domain <id> [--inventory <dir>] [--apply --id <netbox-id>]
                                                                # remove one imported prefix (ADR 0016, section 10)
```

`apply` refuses to run unless `plan` reports zero errors for the same table. `render-*` never write into the repository; a person reviews and merges.

It is a process mode, not a `client` verb: the CLI is a transport client that holds no policy (`internal/cli/cli.go:1-7`), and there is no API endpoint for it to call.

In development this runs as a local binary invocation, shown above. In stage/prod, `deploy/helm/platform-ipam`'s optional `operatorJob` block (work-plan package H3) can run `onboard plan`/`apply` as an opt-in Kubernetes Job instead — disabled by default, needing only the NetBox credentials and pools configuration above (no database URL, no cloud role), with the reviewed table mounted read-only from a ConfigMap the deployer creates. See the [chart README](../deploy/helm/platform-ipam/README.md#optional-operatorjob) for the values and the [adoption runbook](../deploy/runbooks/ADOPTION.md) section 3 for the procedure (written for `adopt`, but the same Job mechanism). Validated by `helm lint`/`helm template` only; never run against a real cluster. `onboard drift`, `parse`, `render-config` and `render-fixture` are not supported by this Job template — it assumes exactly one positional table-path argument, which only `plan`/`apply` take.

## 3. Input formats

| Format | Detection | Notes |
| --- | --- | --- |
| CSV | `.csv`, or comma wins the delimiter count on the header line | RFC 4180 quoting |
| TSV | `.tsv`/`.txt`, or tab wins | what a browser produces when a table is copied |
| Confluence paste | a text file of the pasted table, or `-` for stdin | tab-separated; cells may hold several values |
| Excel | `.xlsx` | first sheet, or `--sheet`; cached cell values only, formulas are not evaluated |

`.xls`, `.ods` and PDF are out of scope; export to CSV first. The `.xlsx` reader uses the Go standard library (`archive/zip`, `encoding/xml`): shared strings, inline strings, numeric cells. No new dependency.

One file may hold one table. The table type is decided by which columns resolve:

| Type | Required | Optional |
| --- | --- | --- |
| `accounts` | `account_id` | `account_name`, `environment`, `tenant_id`, `regions`, `role_arn`, `owner`, `notes` |
| `networks` | `cidr`, `account_id`, `region` | `type` (`vpc`\|`subnet`), `resource_id`, `parent_id`, `az_id`, `name`, `environment`, `state`, `primary` |
| `ranges` | `cidr` *or* `start_address`+`end_address` | `description`, `owner`, `source` |

Headers are matched case-insensitively after stripping punctuation, through an alias table, because real tables say "Account ID", "Account-Nr.", "AWS Konto", "CIDR Block", "IP Range", "Netz", "Network". An unresolvable required column is an error naming the headers that were seen. Unknown columns are kept and appended to the description; they are never dropped silently.

## 4. Normalization

Spreadsheets and wikis damage data in predictable ways. Each rule below is a test case in package C1.

| Problem | Rule |
| --- | --- |
| NBSP, zero-width space, BOM, smart quotes | stripped before anything else |
| Several CIDRs in one cell (`10.1.0.0/16, 10.2.0.0/16`, or separated by newline or `;`) | one output row per CIDR |
| Decoration (`10.1.0.0/16 (prod)`, `VPC: 10.1.0.0/16`) | the CIDR token is extracted; the rest goes to the description |
| Non-canonical CIDR (`10.1.2.3/16`) | **error**, not silently masked: the row might be a host address typed by mistake |
| Excel dropped leading zeros from an account id (`12345678`) | left-padded to 12 digits with a **warning** naming the row |
| Excel scientific notation (`1.23457E+11`) | **error**: digits are already lost |
| Account id with dashes or spaces (`1234-5678-9012`) | separators removed |
| Range written as `10.0.0.1 - 10.0.0.50` | parsed as `start_address`/`end_address` |
| IPv6 | **warning**, row skipped: v1 allocates IPv4 only |
| Empty rows, repeated header rows from page breaks | skipped |
| A value with no header above it (the blank cell a merged header leaves behind, or a row longer than the header) | kept in the description under its spreadsheet letter, `column E: …` |
| Blank rows in a workbook | row numbers in diagnostics are the spreadsheet's own, taken from each row's number in the file — real writers omit blank rows from the XML, so counting positions would point every later diagnostic at the wrong line |

## 5. Validation (`plan`)

`plan` loads the real pools configuration and takes a NetBox snapshot, then classifies every row. The error rules exist because each one corresponds to a way the running service breaks:

| Rule | Level | Why |
| --- | --- | --- |
| Two rows with the same CIDR (same VPC range in two accounts; a subnet equal to its VPC) | collapsed to **one** prefix listing every source; **warning** | a duplicate `(vrf, cidr)` fails the whole inventory snapshot, and every reservation in the domain returns 503 |
| Row equals a configured pool CIDR | **error** | the pool must match exactly one prefix, its own container |
| Row *contains* a configured pool | **error**: "pool lies inside an existing network" | the adapter ignores ancestor prefixes, so this row would block nothing while the pool is in fact in use |
| Row overlaps a prefix that carries `platform_allocation_id` | **error**: the platform already allocated space that was in use | needs a human, not an import |
| Row already present in NetBox as unmanaged | **info**, skipped | makes `apply` repeatable |
| Row outside every pool | **info**, still imported | harmless, and useful to operators |
| Overlapping but unequal rows (VPC and its subnets) | allowed | both are occupied space |
| Range (start/end) that spans or contains a configured pool | **error** | same reason as a network that contains a pool |
| Inventory snapshot not complete | **error**, nothing planned | an unreadable inventory is not an empty one; every "already present" and "overlaps managed" check would pass vacuously. `Reserve` holds itself to the same rule |
| Account id not 12 digits; role ARN not `arn:aws:iam::<id>:role/…` | **error** | mirrors `internal/config/config.go` |
| Eligible account without a coverage cell for the pool's region | **error** in `render-config` | config validation would reject the deployment |

An earlier draft listed a warning for a range expanding to more than 4096 blocks, mirroring the adapter's `maxRangeBlocks` cap. Package C3 showed that it cannot happen: splitting an IPv4 range into aligned blocks yields at most about 62 of them, so the cap is unreachable and the adapter never substitutes a covering prefix. The validator keeps the check as defensive parity with the adapter, but it is not a rule an operator will ever see.

### Contributor findings (ADR 0016, package M9b2)

For a **networks** row whose CIDR matches a prefix the snapshot already carries as unmanaged occupancy, `plan` additionally compares the contributor list that prefix already holds (`platform_import_contributors`) against the contributor entries this table's own rows for that CIDR would write, built exactly as `apply` would build them on create. There is nothing to compare for a CIDR the snapshot does not carry yet, so a brand-new import raises none of these:

| Rule | Level | Why |
| --- | --- | --- |
| `contributor-new`: this table names a contributor the prefix does not carry yet | **info** | the only one of these findings a later refresh (`apply --refresh`, package M9b3) writes on |
| `contributor-absent`: the prefix carries a contributor this table does not name | **warning**, never a write | an import table is a scope, not a census -- `--networks` may cover one account, or the operator may have filtered the file, so absence from one table is never evidence a resource is gone |
| `contributor-unknown`: the prefix carries no contributor list at all | **info** | it was imported before ADR 0016 (package M9b1); a refresh would reconstruct one from this table and flag it as reconstructed |
| `contributor-stale-source`: a would-be contributor entry has a null `observed_at` | **info** | no `observed_at` column, or the cell was blank -- the entry could never be removal evidence even if it were written |
| `contributor-unreadable`: the prefix's `platform_import_contributors` field could not be decoded | **warning**, its own case | not the same as `contributor-unknown` (an operator's hand edit, or a shape a later version wrote, not an absent field), and nothing is silently reconstructed from it |

A **degenerate identity** -- the identity string a row with no `resource_id` column produces (`aws:<account>:<region>::<cidr>`, the VPC segment empty) -- can match only itself and is never treated as evidence in either direction: it never raises `contributor-new` (a different resource could be hiding behind the same collapsed identity, so "not carried yet" cannot be shown) and an existing entry with a degenerate identity never raises `contributor-absent` (the table's own rows could be the same resource under the same collapsed identity, so "gone" cannot be shown either).

`RuleDuplicateCIDR`'s message now names the contributor identities the collapse produces, not just how many rows collapsed -- for every networks-table duplicate group, new or already present, not only a re-import.

Nothing writes. `apply` is unchanged by this package, and the plan's JSON gains only these findings.

Start/end ranges are not deduplicated by `plan`: the snapshot decomposes existing ranges into anonymous blocks, so `plan` cannot tell that a range was imported before. Idempotency for ranges comes from `apply`, where `EnsureOccupancy` looks the range up in NetBox.

The report is JSON plus a readable summary, and exits non-zero on any error, following the `scripts/ai/check-*` convention.

## 6. What `apply` writes

One NetBox prefix per surviving CIDR, in the domain's VRF (a prefix in any other VRF is invisible to the allocator):

- status `active`; description built from name, account, resource id and any preserved columns
- tag `platform-ipam-imported`
- custom fields `platform_import_batch`, `platform_import_source`, and the existing `platform_aws_account_id`, `platform_aws_region`, `platform_aws_resource_id` where known
- custom field `platform_import_contributors`, the structured record of which cloud resources occupy this prefix (see below)
- **never** `platform_allocation_id`, `platform_allocation_key`, `platform_operation_id` or `platform_state`. Those mark ledger-owned prefixes; the end-to-end suite asserts that every prefix carrying an allocation id is known to the API.

### Contributors (ADR 0016)

Rows that resolve to one CIDR collapse into one prefix, and that collapse used to discard everything but a sentence in the description. Since package M9b1 the prefix also carries `platform_import_contributors`: a JSON array with **one entry per collapsed source row**, so a prefix shared by several VPCs says which ones. An entry carries:

| Field | What it is |
| --- | --- |
| `identity` | ADR 0014's resource identity, `aws:<account>:<region>:<vpc>:<cidr>` with `:<association>` appended when the association id is known. The same string `onboard assess` puts on a conflict, so the two join without a mapping table |
| `account_id`, `region`, `type`, `resource_id`, `parent_id` | the row's own fields, so the identity never has to be re-parsed |
| `association_id` | the VPC CIDR association's id, or `null` — the collector writes it for VPC rows only, and a file written before that column existed has none |
| `observed_at` | the collector's per-region instant for the row, verbatim, or `null` |
| `first_seen_batch`, `last_seen_batch` | the `--batch` of the import that wrote the entry. Equal on a create |
| `source_file`, `source_row` | the exact input record that put the entry there. Row numbers are the ones an operator sees in the file, so the header counts |

A `null` `observed_at` means the provenance is unknown, and ADR 0016 makes such an entry unusable as evidence for any later removal. Nothing is guessed to fill it: a column that was absent and a cell that was blank both record `null`.

The same honesty applies to a table with no `resource_id` column, which the import has always accepted: the entry is still written, with an empty `resource_id` and an identity whose VPC segment is empty (`aws:<account>:<region>::<cidr>`). Two such rows at one CIDR therefore carry the *same* identity, and neither can be checked for absence later. Anything reading contributors as evidence must treat an empty `resource_id` the way it treats a null `observed_at`.

**`apply` alone never updates the list.** It is written on create only. A plain re-import over a CIDR that is already unmanaged occupancy makes no NetBox call at all (the idempotency property below), so a VPC that appeared since is not added and a VPC that disappeared is not removed.

### `apply --refresh` (ADR 0016, package M9b3)

`--refresh` is the first update path occupancy has ever had, and it is strictly additive: it adds a `contributor-new` entry, and refreshes an already-named entry's `observed_at` and `last_seen_batch` — never its `first_seen_batch`, and never any other field. **It never removes an entry a table does not name**, for the same reason `contributor-absent` is only ever a warning: a table is a scope, not a census, so absence from one table is never evidence a resource is gone. That asymmetry is deliberate: adding a contributor can only make the platform more careful with address space, removing one can only make it less careful, and ADR 0016 gives removal to its own, far stricter package (M9b4, `onboard remove`).

It writes at all only for a CIDR `plan` already reports `already-unmanaged`, and only when the contributor **set** changes — a new identity, or the prefix carrying no list at all. A re-import whose every row was already named, even with a strictly newer `observed_at` in the table, makes no NetBox write: freshness alone is never a reason to write, the same rule ADR 0014's amendment protects for the description. When it does write, the one call also refreshes `platform_import_source`; nothing else — not the description, the tags, the status, the tenant, or any other operator-owned field — is ever touched, and the write is conditional on the read's `ETag` (`If-Match`), so a prefix that changed between the read and the write (a `412`) is a refusal, never a retry.

A prefix with **no** contributor list at all — imported before ADR 0016 existed — gains one reconstructed from the table, and the prefix carries a permanent flag, `platform_import_contributors_reconstructed`, recording that fact: a reconstructed list and one an import actually wrote are indistinguishable afterwards, and a later removal (package M9b4) must refuse a reconstructed list rather than trust it.

`--refresh` refuses a prefix carrying **any** ownership field — not the import tag, because an adopted prefix keeps it (ADR 0010, ADR 0012) — and a prefix whose contributor list this version cannot decode, rather than overwrite what it cannot read. In practice a table row naming an owned CIDR never reaches either refusal: `plan`'s existing `overlaps-managed` rule already refuses it as a **plan error** before `apply` writes anything, exactly as an ordinary re-import over managed space always has, so `--refresh` inherits that protection instead of needing its own.

A **degenerate identity** — the one no `resource_id` column produces — can only ever match itself: several table rows sharing one collapse to at most one stored entry, never several, exactly as `plan`'s own comparison already treats it as evidence of nothing.

**Without `--refresh`, `apply` is unchanged.** An installation that never passes the flag makes not one additional NetBox request.

The field is **unowned**: `ownedFields` never names it, so an adoption preserves it and an abandonment leaves it alone, exactly as the import tag, batch and source already survive both.

The description is unchanged by this package and still names the accounts and resource ids, so it still grows with every sharer and a CIDR shared by enough VPCs still fails the import at the 200-rune limit. ADR 0016 separates that fix into its own package because it changes golden files and end-to-end assertions written against today's string.

`platform_import_contributors` must exist in NetBox before an import runs, exactly like the `platform-ipam-imported` tag: `deploy/compose/seed-netbox.py` creates it as a `json` custom field on `ipam.prefix`, and an import against a NetBox that lacks it fails at the first entry with a NetBox `400` and writes nothing.

Ranges given as start/end become NetBox IP ranges. Writes go through a new adapter method beside `Ensure`, never through raw HTTP from the command, so the VRF and marker rules live in one place. `apply` is idempotent, and more strictly than "reports unchanged": it re-runs `plan` first, and `plan` drops every row that already exists as unmanaged occupancy, so a repeated `apply` makes no NetBox call for those rows and prints no line for them. A summary of `0 created, 0 unchanged` on a re-run is the expected result, not an empty import. `--batch` lets a whole import be listed or removed later by tag and batch.

## 7. Accounts

Accounts exist only in configuration today: coverage cells, pool eligibility and identities. There is no accounts table and the process reads configuration once at start. `render-config` therefore prints fragments and stops:

```yaml
# cloud_coverage (merge under the right overlap domain, then advance coverage_generation)
- account_id: "123456789012"
  role_arn: arn:aws:iam::123456789012:role/PlatformIpamReadOnly
  regions: [eu-central-1]
```

Two traps the output states explicitly: changing coverage requires advancing `coverage_generation`, which invalidates absence evidence (`docs/AWS_INTEGRATION.md:84`); and `IPAM_IDENTITY_FILE` **replaces** the identity list wholesale, so a partial identities file silently removes existing principals.

## 8. Order of operations for a real onboarding

1. Inventory the organization ([procedure](AWS_ORGANIZATION_INVENTORY.md)); collect non-AWS ranges from the network team.
2. Optionally, `onboard assess` ([Overlap assessment](OVERLAP_ASSESSMENT.md)) against that same inventory, to see which VPC CIDR associations conflict before deciding what to import.
3. `onboard parse`, then `onboard plan`. Resolve every error with the owners of the data.
4. Pause allocation for the domain. `onboard apply`.
5. Review and merge `render-config` output; advance `coverage_generation`; redeploy.
6. Start the worker and wait for a complete observation. Remaining `unmanaged_occupancy` findings are networks AWS sees that the import did not contain — resolve them.
7. Resume allocation with one canary tenant and pool.

## 9. Import, then adopt

Import and adoption are two separate, sequential steps, not one operation, and each has its own tool: `onboard` never opens the ledger, and `adopt` never runs without it (ADR 0010). Import alone **never creates an allocation** — an imported network stays occupied space with no owner, no key and no lifecycle for as long as nobody adopts it, and that is a valid, permanent end state, not a half-finished one. Most imported space should probably stay that way: adopt only the networks a tenant actually needs under platform management (typically so they can be brought under Terraform, since provider import requires a platform allocation id that unmanaged occupancy does not have). A network nobody has asked to adopt costs nothing to leave as unmanaged occupancy — it still blocks the allocator from selecting overlapping space, which is the whole point of importing it in the first place.

Adopt only once, per network, after: the import has landed in NetBox (steps 1–3 above; the prefix carries the `platform-ipam-imported` tag), the worker has a complete, current cloud observation of the target account/region, and the owning tenant has agreed the allocation key it will use in its own first `POST /v1/allocations` — that key is permanent from the moment adoption commits it. Then run `platform-ipam adopt plan`, review its report line by line, and run `platform-ipam adopt apply`. The full procedure — including the two-run shape a VPC with subnets requires, what the transitional warnings mean, and why there is no undo — is in the [adoption runbook](../deploy/runbooks/ADOPTION.md).

## 10. `onboard remove` (ADR 0016, package M9b4)

Imported occupancy is permanent by default — the whole point of an import is that a prefix stays blocked until an operator decides otherwise. `onboard remove` is that decision, made explicit: the only command in this project that deletes something the platform does not own, built like `adopt abandon` — a dry run by default, one named prefix, one JSON report, no `--all` and no sweep — but stricter still, since the removal itself needs an explicit `--apply` (unlike `abandon`'s `--dry-run` opt-*in*, remove opts *out* of writing).

```
platform-ipam onboard remove <cidr> --domain <id> \
    [--inventory <dir> | --networks <in>... --failures <f> --accounts <a> --run <r>] \
    [--apply --id <netbox-id>] [--out <path>]
```

It reads the collection being acted on exactly as `onboard assess` does — `--inventory DIR` (shorthand for `DIR/networks.csv`, `DIR/failures.csv`, `DIR/accounts.json`, `DIR/run.json`) or the individual flags — through the very same decoders, so a `networks.csv`/`accounts.json`/`run.json` that `assess` already reads needs no second look to be handed to `remove`. It never opens the ledger, like every other `onboard` subcommand.

### The four evidence rules

Each is independently refusable, and the dry run evaluates all of them (and every outright refusal below) in one pass, so the report shows the whole picture rather than one refusal at a time:

1. **Every contributor is known.** The prefix carries a contributor list; it is not `contributor-unreadable`, not flagged reconstructed (a refresh populating a pre-ADR-0016 prefix can never prove its list is the whole truth), no entry has a null `observed_at`, and no entry has a degenerate identity (no `resource_id` — it can never be proven absent, the same rule `plan`'s own findings apply).
2. **Coverage is complete for every contributing account and region.** For each distinct `(account_id, region)` among the prefix's own contributors — not the whole collection — the run record must call it `succeeded`, with package M1c's row-count cross-check holding too. `partial`, `failed`, `not_attempted`, an outcome this version does not know, a missing `run.json`, a missing `accounts.json`, or an account/region the run record never mentions at all are each UNKNOWN and each a refusal, naming the account and region.
3. **Every contributor is observed absent.** No row in the collection's networks table carries the contributor's `resource_id` in its `account_id` and `region`. Absence in an account/region the run record calls `succeeded` is evidence; absence anywhere else is not evidence of anything. Each contributor's stored `observed_at` must also be older than the collection's `finished_at`, so a removal can never be argued from a collection that predates the evidence that created the contributor.
4. **A reviewed, explicit invocation.** `--apply` must carry the exact NetBox id a dry run reported; a mismatch (the review is stale, or the wrong prefix) is refused rather than silently re-resolved.

### Refused outright, whatever the evidence says

- Any ownership field (`prefix_owned`) — an adopted prefix is never touched, tested against `owned()` and not the import tag, because an adopted prefix keeps the tag.
- No `platform-ipam-imported` tag (`not_imported`) — not ours to remove.
- A configured pool's own CIDR, or a prefix that contains one as an ancestor (`pool_prefix`).
- An empty (but present) contributor list (`empty_contributor_list`) — "an empty list is not an argument that nothing contributes; it is a list nobody wrote" (ADR 0016).
- A description that does not contain what the import would generate from the contributor list (`description_edited`) — an operator wrote something there. `domain.Contributor` carries no `Name` and no per-row free-text description, so this check reconstructs and verifies only the `account(s) …`/`resource id(s) …` segments of `mergeNetworkDescription`'s output are still present verbatim; it cannot see an edit confined to the name or free-text portions, and says so in its own refusal message. This is the narrowest honest rule the stored data supports, and it errs toward refusing, never toward silently allowing.

### The delete

`--apply` re-derives every mechanical refusal above from a **fresh** NetBox read — never trusting the dry run's copy — reads the prefix by the id the dry run reported, verifies the CIDR still matches, deletes conditional on the read's `ETag` (a `412` is a refusal, never a retry), and confirms by re-reading that the prefix now answers 404. Never by CIDR, never a filter: package M9b1 measured that the pinned NetBox silently ignores a filter on `platform_import_contributors`, so a delete-by-filter could not be trusted to name one object — which is also why removal takes one named prefix and has no `--all`.

### The report

One JSON document, to stdout and to `--out` if given: the prefix and its NetBox id, its description, the contributor list verbatim with rule 3's per-contributor absence argument (`observed_absent`, `reason`), the per-account-region coverage outcome, the run record's `started_at`/`finished_at`, every input's SHA-256 (reused from `internal/assess`'s own digesting, never computed twice), the refusal list (`code`, `message`), and `removable`/`removed` booleans. Exit codes match `adopt abandon`'s: `0` ok, `2` usage, `3` a refusal (the report is still written in full), `4` an uncertain or infrastructure failure (a transport error, or a delete whose outcome NetBox never confirmed).

### What a removal does not attempt

The repeated, spaced absence scans a managed allocation's reclaim requires are deliberately not asked for: this command deletes nothing in AWS, the prefix is reproducible by re-running `apply` from the same table, and a named operator with a dry run is the fence. That is a weaker guarantee than the managed path's, stated as such rather than dressed up. `unmanaged_occupancy` findings are untouched by any of this — they are raised per cloud resource from the worker's own observation, and a removal here settles nothing about them.
