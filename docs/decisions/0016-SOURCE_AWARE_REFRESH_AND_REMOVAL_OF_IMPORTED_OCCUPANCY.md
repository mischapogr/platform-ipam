# ADR 0016: Imported occupancy names its contributors, and evidence frees it

Status: accepted, 2026-09-22, as a design, and built the same day by packages
M9b1 to M9b4 of the work plan: the contributor field (measured first against the
pinned NetBox), the four plan findings, `onboard apply --refresh` and `onboard
remove`, each demonstrated end to end against the development NetBox — including
the sentence gap M9 is about: with one of two contributing VPCs gone the removal
refuses and the prefix still blocks the space. Nothing has run in stage or
production, where the custom fields have no creator yet. Where building it
showed this record to be wrong or silent, the dated paragraphs at its end say so
and win over the text above them. Proposed by package M9a of the [work
plan](../WORK_PLAN.md) against gap **M9** of the owner's [overlap-migration
analysis](../IP_OVERLAP_MIGRATION.md): "multiple VPCs may share one imported
prefix; skipped reimports do not refresh ownership descriptions; unsafe cleanup
could hide remaining occupancy". It extends [ADR
0007](0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md), whose import keeps
every rule it has, and it consumes what [ADR
0014](0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md) added to the collector: the
association id, the observation time and the run record. It is bounded by [ADR
0009](0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md), which forbids any allocation
or reconciliation decision from reading the NetBox AWS plugin, and it must
survive [ADR 0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md)'s adoption
and [ADR 0012](0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)'s
abandonment unchanged. Everything below is a proposed contract; the "what
happens today" paragraphs are read from the checkout and are the only claims
here about demonstrated behaviour.

## Context

### Today, a prefix shared by several VPCs forgets all but one of them

`planPrefixCandidates` (`internal/onboard/plan.go:365-427`) groups every
candidate by its canonical CIDR string. Two or more rows in one group raise
`RuleDuplicateCIDR` at warning level (`plan.go:390-393`) and produce exactly one
`WriteEntry` carrying the sorted source rows (`plan.go:420`). The rule is right
for its purpose and its comment says why: a duplicate `(vrf, cidr)` fails the
whole inventory snapshot and turns every reservation in the domain into a 503
(`plan.go:42-48`).

What the collapse discards is the identity of the contributors. `WriteEntry`
(`plan.go:158-164`) carries a kind, a CIDR or a start/end pair, and a list of
row numbers -- no account, no region, no resource id. The command re-derives the
rows by CIDR in `networkRowsForCIDR`
(`internal/onboardcmd/onboardcmd.go:980-989`) and then hands them to
`networkAWSFields` (`onboardcmd.go:1034-1045`), which returns an account, a
region and a resource id **only when exactly one of the collapsed rows carries
an account id**. Two VPCs in two accounts therefore land on the prefix with
`platform_aws_account_id`, `platform_aws_region` and `platform_aws_resource_id`
all unset, because `ensureOccupancyPrefix` omits an empty value from the custom
fields it sends (`internal/netbox/occupancy.go:161-166`). The structured place
is deliberately blanked in precisely the case that needs it.

What survives is one free-text sentence. `mergeNetworkDescription`
(`onboardcmd.go:1047-1066`) joins the distinct names, then "accounts A, B", then
"resource ids vpc-a, vpc-b", then each row's own description prefixed by
`sourceLabel` (`onboardcmd.go:1086-1091`) with its source row. That string is
the prefix's `description`, and `EnsureOccupancy` refuses a description longer
than `maxDescription`, 200 runes (`occupancy.go:39`, enforced at
`occupancy.go:101-103`), with `ErrOccupancyInvalid` -- which stops the whole
`apply` run at that entry (`onboardcmd.go:679-683`). A CIDR shared by enough
VPCs does not import at all, and the failure arrives as a length error rather
than as the scale problem it is.

A description is also operator-editable free text in NetBox. Nothing can tell an
operator's sentence from the import's, and nothing parses it back.

The one place that does keep the contributors apart is the optional plugin.
`applyAWSObjects` (`onboardcmd.go:764-933`) walks `table.Networks` rather than
`report.Writes` -- its comment says why (`onboardcmd.go:758-763`) -- so with
`--aws-objects` each VPC becomes its own `AWSVPC` object pointing at the one
prefix, and package N3 demonstrated exactly that on a real stack. But the plugin
is an optional Compose overlay, ADR 0009 confines it to a view that no
allocation or reconciliation decision may read, and that record's own exit-path
paragraph names "the second owner on a collapsed prefix" as one of the things
lost when the plugin is uninstalled.

### Today, a re-import refreshes nothing on the prefix

`alreadyUnmanaged` (`plan.go:533-547`) finds the CIDR already present as
unmanaged occupancy, and `Plan` raises `RuleAlreadyUnmanaged` at info level and
`continue`s (`plan.go:414-418`): the entry never enters the write set. `apply`
therefore makes no NetBox call for it and prints no line, which [the import
contract](../ONBOARDING_IMPORT.md) section 6 states as the idempotency property
it is -- "`0 created, 0 unchanged` on a re-run is the expected result, not an
empty import".

Even if such an entry did reach the adapter, nothing would change.
`ensureOccupancyPrefix` finds the single existing prefix and returns
`existingOccupancyPrefix` (`occupancy.go:155-160`), whose surrounding comment is
explicit: "Whatever it is, the space is already occupied and the import is
complete. Operator-owned fields -- description, tenant, other tags -- are left
exactly as they are." ADR 0012 enumerated every prefix write in the package --
`Ensure`'s create, `EnsureOccupancy`'s create, `Sync`'s patch, `Adopt`'s patch
and `Delete`'s delete -- and no sixth has appeared since. There is no update
path for occupancy at all.

The consequence is that `description`, `platform_import_batch`,
`platform_import_source` and the AWS fields carry the values of the **first**
import for ever. A VPC that appeared since is nowhere on the prefix; a VPC that
was deleted since is still named on it. A fresh collector run changes the
inventory's knowledge and not the inventory.

The asymmetry with the plugin is worth stating, because it is the shape of the
gap: with `--aws-objects`, a re-import *does* refresh the plugin objects and can
report `updated` (`internal/netbox/awsplugin.go:456-461`), while the prefix --
the only object that blocks allocation -- is the one thing left stale.

Start/end ranges are the same story by a different route: `plan` does not
deduplicate them at all, because the snapshot decomposes existing ranges into
anonymous blocks; idempotency comes from `matchOccupancyRange`
(`occupancy.go:283-304`) at apply time, again with no update.

### Today, nothing can remove imported occupancy, and nothing knows when to

`Client.Delete` (`internal/netbox/client.go:828-868`) reads the prefix, requires
its `platform_allocation_id` to equal the allocation's, and otherwise returns
"refusing to delete a non-owned NetBox prefix". `EnsureOccupancy` only creates.
`AbandonAdoption` clears ownership fields back to imported occupancy (ADR 0012)
and removes nothing. So an imported prefix can leave NetBox only through the
NetBox UI or its REST API by hand, outside every guard this project has. [The
import contract](../ONBOARDING_IMPORT.md) section 6 says `--batch` "lets a whole
import be listed or removed later by tag and batch"; the listing is a NetBox
filter and the removal is not implemented anywhere.

Nor does anything notice that a source has gone. `unmanaged_occupancy` is raised
per observed **cloud resource**, not per prefix: the worker's occupancy loop
keys it `domain:tenant:account:region:type:resourceID`
(`internal/service/worker.go:455-479`) and [the findings
reference](../FINDINGS.md) records that it is reset every pass. When the
resource disappears the finding simply closes, nothing records that it closed,
and the prefix is untouched. The inventory's memory of the resource is the
description nobody parses.

What *does* exist is the template for an absence argument, on the managed side.
`reclaimEligible` (`internal/service/service.go:1738-1785`) frees a quarantined
allocation only after `RequiredAbsenceScans` (at least two) trusted
observations, spaced by `MinScanSpacing`, every one of them started after the
release request and none of them conflicting; and `trustedObservation`
(`service.go:1204-1206`) requires configured coverage, the domain's current
`CoverageGeneration`, `Complete`, and freshness. That is what "complete
evidence" means in this project, and imported occupancy has no counterpart for
any of it.

### What ADR 0014 made sayable, and what it deliberately refused

M1b gave the collector an `association_id` column, an `observed_at` column and a
fourth output file, `run.json`, which records for every account and region
whether it was `succeeded` (with a `row_count`, so "read and empty" is finally
distinguishable from "not read"), `partial`, `failed` or `not_attempted` ([the
inventory procedure](../AWS_ORGANIZATION_INVENTORY.md) section 3). `NetworkRow`
carries `AssociationID`, `ObservedAt`/`ObservedAtParsed`, `SourceFile` and the
two column-presence flags (`internal/onboard/types.go:77-129`), and
`stampSourceFile` (`onboardcmd.go:352-357`) records which input each row came
from because row numbers restart at 1 in every file.

ADR 0014's amendment of 2026-09-20 settled one thing this record must not undo:
the two new columns are **known** columns and are not folded into the
description, "because an observation time in every description would turn every
re-import of a fresh collector run into a drift on every prefix". A refresh
design that writes on every collection is the failure mode that amendment
already named.

That record also left this gap explicitly to this one: "Gap M9 keeps refresh and
cleanup: the per-resource records here are the evidence M9 will need to prove a
range is still occupied after one duplicate VPC is deleted, and this tool writes
nothing and refreshes nothing."

## Decision

An imported prefix carries a structured list of the cloud resources that
contribute to it, in a NetBox custom field of its own. A re-import adds
contributors and refreshes their provenance and never removes one; a contributor
the table does not name is reported and left alone. An imported prefix is
removed only by an explicit, reviewed operator command, per named prefix, and
only when every one of its contributors is known, is observed absent, and lies
in an account and region the run record calls `succeeded`. Anything less is
UNKNOWN, and UNKNOWN never frees address space.

### Where a contribution is recorded

A new custom field on the prefix, `platform_import_contributors`, holding a JSON
array. NetBox is the inventory; the thing that blocks allocation is the prefix;
the record of why it blocks belongs on the prefix.

The three alternatives are rejected for reasons that are properties of this
checkout, not preferences.

**Not the description.** It is capped at 200 runes (`occupancy.go:39`), it is
free text an operator may edit, and nothing can tell an edit from data. A
contributor list whose capacity is a function of how many VPCs share a CIDR
cannot live in a field whose overflow is an `apply` failure.

**Not a ledger table.** `persistState` (`internal/storage/postgres.go:322-330`)
clears nine whole tables and re-inserts every row of them inside one
`pg_advisory_xact_lock` transaction on every `Update` (`postgres.go:135-158`). A
contributor table there would rewrite the entire estate's contributor rows on
every allocation write -- exactly the per-pass cost gap **M4** exists to
measure, multiplied by the import. And `onboard` is dispatched before
`storage.NewPostgresLedger` is constructed (ADR 0007, ADR 0014); teaching the
import to open the ledger would give up the one structural property that keeps
it out of the allocation path.

**Not the plugin objects.** ADR 0009 makes the plugin a view and forbids every
allocation and reconciliation decision from reading it; deciding that a range
may be freed is such a decision. The plugin is an optional overlay, its objects
require the prefix to exist already (`resolveOccupancyPrefixID`,
`awsplugin.go:355-371`), and ADR 0009's own exit path treats losing "the second
owner on a collapsed prefix" as an accepted cost of uninstalling it. When
`--aws-objects` is given the same facts continue to be written there; the plugin
stays the operator's view and is never the evidence.

**Not a side file.** A file beside the inventory is not read by anything that
decides, is not the inventory, and diverges the first time somebody re-imports
from another machine or another directory.

The AWS custom fields already on the prefix are not an alternative either, and
for a reason worth recording: `ownedFields` (`client.go:667-679`) **owns**
`platform_aws_account_id`, `platform_aws_region` and `platform_aws_az_id`, so an
adoption overwrites the import's values with the allocation's, and ADR 0012's
dated paragraph records that an abandonment had to learn to restore them from
`PriorAccountID` and `PriorRegion`. Those keys belong to ownership. The
contributor field must not, which is the next rule.

`platform_import_contributors` is an **unowned** custom field. `ownedFields`
never names it, so `Adopt`'s merge preserves it and `AbandonAdoption`'s clear
leaves it alone, exactly as the import tag, batch and source already survive
both (ADR 0010, ADR 0012). That is an automatic consequence of the key not being
in a list -- and ADR 0012 is the precedent that says to test it anyway, because
the AWS account and region were believed to survive an adoption until review
measured that they did not.

What is not known: whether the pinned NetBox `v4.6.7-5.0.2` exposes a JSON
custom-field type the adapter can write and read through its existing request
path, and what size limit it imposes. The 200-rune figure in this checkout is
`maxDescription`, the description's own limit, measured by this project; nothing
here has measured a custom field. The first package confirms the type against
the pinned image, and the named fallback is a long-text field holding the same
JSON document, decoded by the adapter rather than by NetBox.

### What a contributor is, and what its freshness is

One contributor entry is one observed cloud resource that occupies this CIDR:

- `identity` -- the string ADR 0014 defines for a resource,
  `aws:<account>:<region>:<vpc>:<cidr>` with `:<association>` appended when the
  association id is known. The same string, not a similar one: a reviewer
  holding an `onboard assess` conflict and a prefix's contributor list must be
  able to join them without a mapping table, and the sides of a conflict are the
  contributors of the prefixes it is about.
- `account_id`, `region`, `type`, `resource_id`, `parent_id`, `association_id`
  -- the row's own fields, so the identity never has to be re-parsed.
- `observed_at` -- the collector's per-region instant for the row, verbatim, or
  `null`.
- `first_seen_batch` and `last_seen_batch` -- the `--batch` names of the import
  that first wrote the entry and of the last one that refreshed it.
- `source_file` and `source_row` -- ADR 0014's traceability property, applied to
  the inventory: a contributor names the exact input record that put it there.

Identity is the account, region, VPC id and CIDR, with the association id when
present. `association_id` is populated for VPC rows only (M1b1), so a subnet
contributor and any row from a file written before M1b1 carries `null` there,
and ADR 0014 already records the consequence: without it, two associations of
one VPC at one CIDR cannot be told from one association read twice.

Freshness is `observed_at` and nothing else. A contributor whose `observed_at`
is `null` -- a file written before M1b1, which `ObservedAtColumnPresent`
(`types.go:102-129`) can distinguish from a present-but-empty cell -- is a
contributor whose provenance is unknown, and the prefix carries a limit flag
saying so. **A contributor with a null observation time can never take part in a
removal.** This is ADR 0014's degradation rule applied to a write instead of a
report.

### What a re-import refreshes, and what it only reports

The rule has one sentence: **a write happens when the contributor set changes,
and never because an observation time changed.**

A re-import over an estate that has not changed produces no NetBox write at all.
That is the property `apply` already has (`ONBOARDING_IMPORT.md` section 6), it
is what ADR 0014's amendment protected when it refused to put `observed_at` into
descriptions, and it is what M4 will measure elsewhere. Losing it would turn
every collector run into an estate-wide write.

Concretely, `Plan` gains four findings, computed from the contributor lists the
snapshot now returns, in the style of the existing `Rule*` constants:

- `contributor-new` -- this table names a contributor the prefix does not carry.
  Info. This is the only condition that causes a write.
- `contributor-absent` -- the prefix carries a contributor this table does not
  name. **Warning, and never a write.** An import table is a scope, not a
  census: `--networks` may cover one account, the operator may have filtered the
  file, and package M1c exists precisely because a truncated `networks.csv`
  beside an intact run record is possible. Absence from one table proves nothing
  about the estate, so a re-import may only ever add.
- `contributor-unknown` -- the prefix carries no contributor list at all,
  because it was imported before this record. Info. A refresh may populate it,
  and the prefix then carries a flag recording that the list was reconstructed
  from a later table rather than written by the import that created the prefix
  -- the two cannot be distinguished afterwards, and a removal must not treat
  them alike.
- `contributor-stale-source` -- an entry has a null `observed_at`. Info, and the
  flag that makes the entry unusable as removal evidence.

`RuleDuplicateCIDR`'s message gains the contributor identities; today it says
only how many rows collapsed (`plan.go:391-392`).

The write itself is `onboard apply --refresh`, and it is the first update path
`EnsureOccupancy` has ever had. Its rules:

- It writes `platform_import_contributors` and, on the same call,
  `platform_import_source` and `last_seen_batch` values inside the entries. It
  does not touch the description, the tags, the status, the tenant or any other
  operator-owned field.
- It is additive. An entry is added or has its `observed_at` and
  `last_seen_batch` refreshed. No entry is ever deleted by a refresh.
- It refuses any prefix carrying an ownership field, through the check
  `existingOccupancyPrefix` already makes (`occupancy.go:270-278`), and it still
  passes `refuseManagedFields` (`occupancy.go:308-315`). The import tag is not
  the test: an adopted prefix keeps it (ADR 0010, ADR 0012).
- It is conditional, carrying the read's ETag as `If-Match`, on the measurement
  ADR 0010 recorded and ADR 0012 reused. A `412` is a refusal, not a retry,
  because the prefix may have just been adopted.
- Without `--refresh`, `apply` behaves byte-for-byte as it does today. An
  installation that never passes the flag sees no change, which is the shape N3
  already established for `--aws-objects`.

There is a pleasant result in the existing rules: a table row for an adopted
network never reaches any of this. `overlapsManaged` (`plan.go:473-491`) raises
`RuleOverlapsManaged` as an **error** for a row whose CIDR equals a managed
network -- `childOfManagedVPC` (`plan.go:503-509`) requires strictly longer
bits, so equality is not a child -- and `apply` refuses to write over any error
(`onboardcmd.go:666-670`). The adopted prefix is already unreachable from a
re-import, and `--refresh` inherits that refusal rather than needing its own.

### What a removal requires

A new operator subcommand, `platform-ipam onboard remove`, in the shape of
`adopt abandon` (package H2c): a dry run by default, one named target, a report,
and no automatic path anywhere. Nothing in the worker, nothing in `apply`,
nothing on a timer may ever delete imported occupancy.

It names exactly one prefix, by domain and CIDR, and it verifies the NetBox id
the dry run reported. There is no `--all` and no batch-wide sweep. The reason is
partly evidence -- a sweep would have to decide for prefixes nobody reviewed --
and partly mechanical: whether NetBox can filter prefixes by a value inside a
JSON custom field is not known here, so a candidate list cannot be assumed to be
producible at all.

Four pieces of evidence, each independently refusable:

1. **Every contributor is known.** The prefix carries a contributor list, no
   entry is flagged reconstructed (`contributor-unknown`), and no entry has a
   null `observed_at`. A prefix imported before this record cannot be removed
   until a refresh has given it a list, and a list built from a pre-M1b1 file
   never qualifies.
2. **Coverage is complete for every contributing account and region.** For each
   distinct `(account_id, region)` among the contributors, the `run.json` of the
   collection being acted on records `succeeded`. `partial`, `failed`,
   `not_attempted`, an outcome word this version does not know, a missing
   `run.json`, a missing `accounts.json`, or a contributing account the run
   record does not list at all are each UNKNOWN and each a refusal. This is ADR
   0014's amended coverage rule -- "`failed`, `partial` and `not_attempted`
   empty, `run.json` present and the account list present" -- narrowed to the
   accounts and regions this prefix actually depends on, and it is the statement
   M1b's run record exists to make. Package M1c's row-count cross-check must
   hold too: a `succeeded` region whose `row_count` disagrees with the rows
   present is not complete.
3. **Every contributor is observed absent.** In the networks table of that same
   collection, no row carries the contributor's resource id in its account and
   region. Absence in an account-region the run record calls `succeeded` is
   evidence; absence anywhere else is not evidence of anything. Each
   contributor's stored `observed_at` must also be older than that collection's
   `finished_at`, so a removal cannot be argued from a collection that predates
   the evidence that created the contributor.
4. **A reviewed, explicit invocation.** The operator runs the dry run, reads the
   report -- which prints the contributor list verbatim, the run record's start
   and finish, the per-account-region outcomes and the absence argument for each
   contributor -- and then re-runs with `--apply` and the NetBox id. The
   freshness of the collection is the operator's judgement, stated as such: the
   tool refuses only what it can decide mechanically.

Refused outright, whatever the evidence says:

- A prefix carrying any ownership field, tested with `prefix.owned()`
  (`client.go:158-165`) and not with the import tag, because an adopted prefix
  keeps the tag.
- A prefix without the `platform-ipam-imported` tag: not ours to remove, which
  is the rule `Adopt` already applies in the other direction.
- A prefix that equals a configured pool's own CIDR or contains one. Both shapes
  are already known to `refusePoolPrefix` (`occupancy.go:355-370`), and this
  command must not become the way somebody deletes a pool container.
- A prefix whose description is not the one the import would generate from its
  own contributor list. An operator wrote something there, and removing the
  prefix destroys it. Report it and stop; clearing the edit is a deliberate act.
- A prefix whose contributor list is empty. An empty list is not an argument
  that nothing contributes; it is a list nobody wrote.

Deliberately **not** required, with the reason: the repeated, spaced absence
scans `reclaimEligible` demands of a managed allocation. Those exist because the
platform is about to delete a resource it owns and then re-issue its address
space under a permanently retired key. Here the platform deletes nothing in AWS,
the prefix is reproducible by re-running `apply` from the same table, and a
named operator with a dry run is the fence. This is a weaker guarantee than the
managed path's and is stated as such rather than dressed up.

The removal writes a report -- to stdout, and to the one `--out` path if given,
in ADR 0014's shape: every input with its SHA-256, the prefix and its NetBox id,
the contributor list verbatim, the run record's identity and the per-contributor
absence argument. It writes no tombstone into NetBox: a deleted prefix cannot
carry one, and a "removed" marker on a surviving object would be a lie. The
recovery from a wrong removal is to re-run `apply` from the table the report
names, and saying which table is most of what the report is for.

The delete itself reuses the confirmation shape `Client.Delete` already has
(`client.go:851-867`): delete, re-read, require a 404, and on an uncertain
delete re-list rather than assume.

### Where it lives, and the ledger it cannot see

All of this stays in the `onboard` family, which never opens the ledger. That is
consistent with ADR 0007 -- occupancy is written without the ledger today -- and
it keeps the removal out of the allocation path.

The cost is real and is named here rather than discovered later: `onboard`
cannot see pending operations or the worker's observations, so a removal cannot
refuse "while an adoption is fenced in this domain" the way a ledger-aware
command could. Three things bound the exposure. A half-converted adoption's
prefix carries ownership fields and is refused (ADR 0012 records that `Adopt`
writes `ownedFields` before the commit). `Reserve` takes a fresh snapshot for
every request, so a reservation cannot act on a pre-removal view. And the
residual risk of a removal is removing occupancy that is still real, which is
what the four evidence rules address, not a race with the ledger.

The alternative -- making removal an `adopt`-family command that opens the
ledger, checks for pending operations and reads the worker's latest observation
-- buys a real guard at the price of putting a database URL and the observer
into the one workflow that today needs neither. This record chooses `onboard`
and flags the choice as the one most likely to be revisited first, particularly
if question **Q12** comes back demanding an audited, ledger-backed removal
record.

### Relation to the records and gaps around it

ADR 0007 is untouched in substance: the import still writes occupancy that owns
nothing, still collapses by CIDR for the allocator's sake, still refuses a
pool's own space, and still never mints an allocation. What changes is that the
collapse stops being lossy.

ADR 0010 and ADR 0012 gain one invariant each, both already true by construction
and both needing a test: an adoption preserves the contributor list because the
field is unowned, and an abandonment restores a prefix that carries it. A
removal can never touch an adopted prefix, because ownership fields refuse it
and `overlapsManaged` refuses the table row that would reach it.

ADR 0009 is untouched: the plugin stays a view, keeps receiving the same objects
under `--aws-objects`, and is never read by the refresh or the removal. `onboard
drift` is untouched and stays the accounts-versus-coverage comparison it is
(`internal/onboardcmd/drift.go:16-27`); the contributor question is not added
there, because `drift` reads the plugin and this reads the VRF's prefixes, and
merging them would put a plugin read on a path that decides about occupancy.

ADR 0014 is the supplier and the sibling. `onboard assess` answers the conflict
question over the same records and writes nothing; this record lands the same
facts in the inventory and writes rarely. Neither reads the other's output, and
the shared resource identity string is what lets a person join them.

Gap **M4** is respected rather than aggravated: the refresh writes only on a
changed contributor set, so the estate-wide write M4a is measuring does not gain
a second instance. Gap **M3**'s migration plan is the natural consumer of a
removal report -- retiring an old range is one of its waves -- but this record
commits nothing to it. Question **Q12** ("what evidence and retention are
required before removing a contributing resource or imported prefix") is the
owner's to answer with the network and audit owners; what is written here is a
floor, and Q12 may raise it and may add retention rules this record does not
decide.

### The cut of M9b

Four packages, each independently reviewable.

**M9b1, tier X -- the record and its writer.** The custom field, confirmed
against the pinned NetBox image, in `deploy/compose/seed-netbox.py`;
`netbox.Occupancy` gains `Contributors`; `EnsureOccupancy` writes them on
create; `domain.Network` gains the list so `Snapshot` returns it;
`entryOccupancy` (`onboardcmd.go:944-974`) builds the entries from the collapsed
rows. No update path, no new command, no behaviour change for an import that
produces one contributor. It touches `internal/netbox`, which is why it is X.

**M9b2, tier L with an X review -- the comparison, read-only.** `Plan` gains the
four contributor findings from the snapshot's lists. Nothing writes. This
package is where the "a table is a scope, not a census" rule is encoded and
tested.

**M9b3, tier L with an X review -- the additive refresh.** `apply --refresh`,
the conditional update path in `EnsureOccupancy`, the refusals, and the property
that an unchanged estate writes nothing.

**M9b4, tier M -- the removal.** `onboard remove`, its four evidence rules, its
refusals, its report, a runbook section beside [the adoption
runbook](../../deploy/runbooks/ADOPTION.md), and the end-to-end demonstration
gap M9 itself asks for.

A fifth, deliberately separated: the description of a collapsed prefix should
become a short human sentence once the contributors live in their own field, so
that its length stops being a function of how many VPCs share a CIDR. That
changes `mergeNetworkDescription`'s output, and therefore golden files and
end-to-end assertions written against today's string; it is a real cost and it
belongs in its own package rather than hidden inside M9b1.

### The evidence that moves this record to accepted

Two properties head the list because neither may ever regress.

**No path frees address space without complete coverage for every contributing
account and region.** A table-driven test over every route to incompleteness --
a contributing region recorded `partial`; one recorded `failed`; one recorded
`not_attempted`; an outcome word the code does not know; a missing `run.json`; a
missing `accounts.json`; a contributing account the run record never lists; a
contributor with a null `observed_at`; a reconstructed contributor list --
asserts that `onboard remove` refuses, that it deletes nothing, and that its
message names the account and region that made the answer unknown.

**A re-import over an unchanged estate writes nothing.** A test counts the
NetBox requests of a second `apply --refresh` over the same table and asserts
that no write request is made, including when every row's `observed_at` is newer
than the stored one. This is ADR 0014's amendment expressed as a test.

Then the gap's own sentence, end to end against a real NetBox in the development
stack, in the shape `tests/e2e/test_e2e_import.py` already has: import a
networks table in which two VPCs of two accounts share one CIDR; show one prefix
with two contributors; delete one VPC from the estate and re-collect; show
`onboard remove` **refuses** because the second contributor is still observed,
and that the prefix still blocks a reservation that would otherwise choose that
space; then remove the second VPC, re-collect with complete coverage for both
accounts and regions, and show the removal succeeds, that exactly that prefix is
gone, that no other prefix changed, and that a reservation may now select the
space.

Then the unit list. A collapsed group of three rows produces three contributor
entries with the correct identities, and a group of one produces one and leaves
today's AWS custom fields exactly as they are now. A re-import that names a new
VPC at an existing CIDR raises `contributor-new` and, with `--refresh`, writes
exactly one prefix. A re-import over a filtered table raises
`contributor-absent` and writes nothing. A prefix imported before this record
raises `contributor-unknown`, gains a reconstructed list under `--refresh`, and
is refused by the removal. A pre-M1b1 file produces `contributor-stale-source`
and null observation times, and the removal refuses. An `If-Match` `412` is a
refusal that wrote nothing. A prefix carrying an ownership field is refused by
both the refresh and the removal, and the table row that would reach it is still
`overlaps-managed`. An adopt-then-abandon round trip leaves the contributor list
byte-identical -- the test ADR 0012's dated paragraph earns. A prefix whose
description was edited is refused by the removal and named in the report. A pool
container and an ancestor of a pool are refused. And an uncertain delete is
resolved by re-reading, never by assuming.

**Built on 2026-09-22 by package M9b4.** `platform-ipam onboard remove`
exists, in the shape of `adopt abandon`: a dry run by default, one named
prefix (domain and CIDR), one JSON report always, no `--all` and no sweep.
Unlike `abandon`, nothing is deleted unless `--apply` is given together with
the NetBox id a dry run just reported (ADR 0016's own fourth evidence rule
made literal, not `abandon`'s `--dry-run` opt-in). It reads the evidence
collection exactly as `onboard assess` does -- `--inventory` or the
individual flags, through assess's own decoders and its own
`internal/assess.Assess` call for `Coverage` and the input digests -- never a
second reading of either. `internal/netbox` gained two calls that read or
delete the ONE prefix a removal names, the same way `RefreshOccupancy`,
`Adopt`, `AbandonAdoption` and `CancelReservation` each read their own single
target rather than through `Snapshot`: `ReadOccupancy`, purely a read with no
refusal of its own (the dry run needs every rule's outcome in one report, not
just the first failure), and `DeleteOccupancy`, which re-derives every
refusal this package can decide alone from a FRESH read immediately before
the delete -- owned, not imported, a pool's own space, an unreadable or
reconstructed list, an empty list -- independent of whatever the dry run
found a moment earlier. The `prefix` type gained a `Description` field
(decoded, never sent) since neither `Snapshot` nor any existing read carried
it and removal's own outright refusals need it.

The four evidence rules, each independently refusable and each evaluated
every time -- the dry run never stops at the first refusal:

1. **Every contributor known.** A list present, not `contributor-unreadable`,
   not flagged reconstructed, no null `observed_at`, and no degenerate
   identity (no `resource_id` -- the M9b2 rule that it can never be proven
   absent, applied here as a rule-1 refusal rather than a rule-3 one, since a
   degenerate contributor is unknowable, not merely unproven).
2. **Coverage complete for every contributing account and region.** Answered
   from `internal/assess.Report.Coverage` -- computed once, over the whole
   collection -- narrowed to the `(account_id, region)` pairs the prefix's OWN
   contributors depend on, never the collection's global `Complete` flag,
   which is the wrong question for one prefix. `RunMissing`, `AccountsMissing`,
   `Failed`, `Partial`, `NotAttempted` and (package M1c) `RowCountShort` are
   read directly off that result; an account/region the run record never
   mentions at all is caught by a positive check against the decoded
   `RunAttempt` list, because none of `Coverage`'s gap lists are built to name
   an account it is simply silent about.
3. **Every contributor observed absent.** No row in the collection's networks
   table carries the contributor's `resource_id` in its `account_id` and
   `region`, AND the contributor's stored `observed_at` is older than the
   collection's `finished_at` -- both checked; either failing refuses with its
   own reason in the report.
4. **A reviewed, explicit invocation.** `--apply`'s `--id` must equal a FRESH
   read's id, checked once in the command's own evidence pass and re-checked
   again inside `DeleteOccupancy` itself.

Refused outright, whatever the evidence says, exactly as listed: any
ownership field; no import tag; a pool's own CIDR or an ancestor of one; an
empty (but present) contributor list; a description that does not match what
the import would generate. That last one is where the record's own text left
a gap this package had to close on its own evidence: `domain.Contributor`
carries no `Name` and no per-row free-text description (the record's own
field list for a contributor entry has neither), so the check cannot
reconstruct the whole of `mergeNetworkDescription`'s output -- only the
`account(s) …`/`resource id(s) …` segments. The first implementation compared
for exact equality and refused every real-world import that had a VPC name in
it (which is most of them), discovered live against the development stack in
this package's own end-to-end run. The decision made here, reported rather
than hidden: check CONTAINMENT, not equality -- the reconstructed segments
must appear verbatim inside the stored description. This still refuses
precisely when the evidence-bearing text (the account and resource ids a
removal's argument is built on) has been altered or removed, and it is
provably unable to detect an edit confined to the name or free-text portions,
which the refusal message says outright. Over-refusing was the design intent;
under-verifying an edit to text nothing here has a copy of is the honest
limit of what a five-field struct can prove.

The delete itself: `DeleteOccupancy` reads the prefix by the id `--apply`
was given, refuses if the id a fresh list-then-detail read finds differs,
re-checks owned/imported/pool/unreadable/empty/reconstructed from that fresh
read, deletes conditional on the read's `ETag` (a `412` is
`ErrOccupancyConflict`, never a retry), and confirms absence by re-reading for
a `404` -- never by CIDR, never a filter, exactly as ADR 0016 requires and as
package M9b1 measured NetBox would silently ignore.

**Decided where the record was silent, and why, beyond the description
rule above:** "a reservation may now select the space" (the gap's own e2e
sentence) is demonstrated through `GET /v1/pools/{id}/capacity`'s
`by_prefix_length` buckets before and after removal, not through an actual
reservation pinned to the freed CIDR -- `api/openapi.yaml`'s allocation
request has no candidate-CIDR field to pin one to, so no mechanism exists to
prove selection of the exact block without either exhausting the pool up to
it (impractical under the suite's shared 32-allocation tenant quota) or
adding an API surface this record never asked for. Capacity is the same
aggregate measure this suite's own `test_01` already uses for the converse
claim ("occupying capacity"), read here in reverse, and it spends no
reservation quota at all.

Mutation-tested on a scratch copy including `docs/`: 30 of 30 mutants killed,
covering every evidence rule, every outright refusal, `DeleteOccupancy`'s own
re-checks (owned, not imported, pool, unreadable, empty, reconstructed, the
`--apply` id match, the 412-is-a-refusal branch, the read-back confirmation),
and the top-level `removable` verdict itself.

End to end, in `test_e2e_import.py`'s own range, keeping every existing test
(1-21) unchanged: two new tests borrow the one slot left anywhere in the
pool -- `test_e2e_adopt.py`'s own quadrant had exactly one unclaimed index
after its existing lending to `test_e2e_reservation_stuck.py`, named
`SLOT_REMOVAL` and lent the same way -- split into two `/24` sub-blocks since
removal imposes no minimum prefix length. `test_22` imports two VPCs at one
CIDR, then walks the gap's own progression against synthetic evidence
collections written straight into the `api` container (no pre-parsed
`table.json`; the same raw-file shape `assess` reads): refused while the
second VPC is still observed present (`not_observed_absent`, naming it);
refused again once both are gone but one region reads `partial`
(`coverage_incomplete`); reported removable once both are absent with
complete coverage, with the dry run itself writing nothing (`removed` false,
prefix still present); then `--apply` with the dry run's own id removes
exactly that prefix, no other prefix changes, and the /22 block the /24
sub-block sits inside returns to allocatable capacity. `test_23` simulates an
adopted prefix by writing ownership markers directly through the NetBox REST
API (this module's own established technique, `test_21`'s and `test_02`'s),
and shows the removal -- dry run and `--apply` alike -- refuses it, writing
nothing, cleaning the simulated markers up itself. Both halves under one run
id: **70 + 53 = 123 passing**. Pool quota after the run: 31 of 32 `/22`
reservations, 8 quarantined, matching the wave's own prior figure --
`test_22`/`test_23` spend none of it, by design.

**Not verified:** whether the containment-based description check accepts
every real-world description shape a live collector's own `name`/free-text
columns can produce, beyond this package's own synthetic fixtures; a race
between two concurrent `onboard remove --apply` invocations against the same
prefix (the `412`/id-mismatch paths are unit- and mutation-tested, not raced
against a real NetBox); and, as every package before it in this record has
said, whether the evidence floor this removal enforces is the one the
customer's audit owners will ultimately accept.

## Consequences

The inventory acquires a second structured fact about an imported prefix, and
the first thing that is true of an imported prefix and not of an allocation: it
has several owners. Every part of the project that has so far been able to say
"a prefix, and the one thing that put it there" now has a list to consider. The
narrow cost is a new custom field; the broad cost is that `onboard remove` is
the first command in this repository that deletes something the platform does
not own, and no amount of evidence design makes that a small thing.

The refresh is deliberately one-directional, and that will feel wrong the first
time an operator watches a deleted VPC stay listed on a prefix. It is the right
asymmetry: adding a contributor can only make the platform more careful with
address space, and removing one can only make it less careful, so the two need
different evidence and only one of them may be a side effect of a re-import.

A prefix imported before this record can never be removed by the tool without a
refresh first, and a refresh cannot prove that the list it reconstructs is the
whole truth. That is why the reconstructed flag exists and why it is a permanent
refusal rather than a warning. The practical consequence is that an estate
imported today and cleaned up next year needs one refresh pass per prefix before
any cleanup is possible, and that pass needs a collection whose coverage is
complete -- which for the customer in the analysis document may never be
available for every account.

The removal will be asked to grow an `--all` or a `--batch` sweep, because
retiring an old range after a migration wave means many prefixes and nobody
wants to type them. The answer this record gives in advance is the same one ADR
0014 gave about an override flag: the honest form already exists, which is to
run the dry run for each prefix and read it. If a sweep is ever added it must
produce the same per-prefix report and refuse the whole batch when any member
refuses, because a partial sweep is the failure mode that hides remaining
occupancy.

`unmanaged_occupancy` gets no quieter. It is raised per cloud resource from the
worker's observation, so importing and refreshing contributors changes none of
it, and a resource whose prefix exists still raises it until the resource is
tagged, adopted or removed ([the findings reference](../FINDINGS.md)). Anybody
reading this record hoping that a contributor list will resolve findings should
stop here: it will not, and the two are not the same question.

What is unknown is stated plainly. Nothing here is built. Whether the pinned
NetBox exposes a usable JSON custom-field type, what it costs to read one back
on every `Snapshot`, and whether NetBox can filter on a value inside it are all
unmeasured; the last of those is why the removal takes a named prefix. Whether
AWS can return two associations with the same CIDR for one VPC is still
unconfirmed (ADR 0014 records it), so a contributor identity without an
association id may not be unique. How many VPCs actually share a CIDR in the
customer's estate is question **Q3** and decides whether a contributor list is
three entries or three hundred. `scripts/aws/org-inventory.sh` has still never
run against a live AWS Organization, so every claim about what a real `run.json`
contains is a claim about a stubbed fixture. And whether the evidence floor
proposed here is the one the customer's network and audit owners will accept is
question **Q12**, which the owner's analysis reserves for them and which this
record must not answer on their behalf.

**Measured on 2026-09-22 by package M9b1, before any adapter code, against the
development NetBox `v4.6.7-5.0.2` (NetBox 4.6.7, Django 6.0.7).** The pinned
image does offer a custom field of type `json` on `ipam.prefix`: `OPTIONS
/api/extras/custom-fields/` advertises `text, longtext, integer, decimal,
boolean, date, datetime, url, json, select, multiselect, object, multiobject`,
and `seed-netbox.py`'s existing `ensure()` mechanism creates one unchanged --
the payload `{"name", "type": "json", "object_types": ["ipam.prefix"]}` answered
`201`, and a second run found it by `?name=` and compared clean, because
`matches()` already handles NetBox echoing the type as `{"value": "json",
"label": "JSON"}`. The adapter's existing request path carries it as structured
JSON and not as a string: an array of contributor entries in the shape defined
above was written and read back as a JSON array whose entries were byte-equal to
what was sent, and an unset field read back as the key present with `null`,
which is the shape `stringCF` was taught to survive in package C5. Size is not
the constraint this record feared. Entry counts of 1, 10, 100 and 1 000 were
each accepted `200` and read back equal, at 418, 4 180, 41 980 and 421 450
bytes; pushing past the record's own list found no NetBox refusal anywhere, with
2 000, 5 000, 10 000, 20 000 and finally 50 000 entries all answering `200`, the
last being 19 967 350 bytes written in 4.06 seconds and read back intact. The
binding limit is therefore this project's own and not NetBox's:
`createOccupancy` reads a create response through an `io.LimitReader` of 4 MiB
and `page` through one of 16 MiB, so a list beyond roughly ten thousand entries
of this shape would fail decoding the adapter's own response rather than be
refused by the server -- far above any plausible answer to question Q3, and
recorded here so that a later sweep does not rediscover it as a decode error.
Reading the field back costs no measurable time and about two thirds more
bytes: over a VRF holding 298 prefixes, 248 of them carrying three contributors
each, five interleaved walks of the same paged list call `Snapshot` makes gave a
median of 302.8 ms populated against 308.7 ms empty, which is inside the run to
run noise, while the transferred payload grew from 414 078 to 688 614 bytes. The
one answer worse than unknown is filtering. Every `cf_` query against the JSON
field -- an exact document, `__ic`, `__contains` and `__empty` in both
directions -- returned the unfiltered count and matched even the planted prefix
that carried no contributors at all, which is precisely what a deliberately
nonsensical `cf_definitely_not_a_field__ic` control did, so the pinned NetBox
silently ignores such a filter instead of refusing it and a candidate list built
from one would quietly be the entire VRF; `?q=` does not reach custom fields
either, answering zero. On the named long-text fallback an exact whole-document
match does discriminate, returning only the planted prefix, while `__ic` is
ignored just as silently. Nothing here is changed because of that, exactly as
this record instructed: the removal takes a named prefix. M9b1 therefore uses
the `json` field and leaves the long-text fallback unused. Every object the
measurement created lived in 198.51.100.0/24 in the development VRF, each was
deleted by an id read back together with its CIDR, and the range and both
throw-away probe fields were confirmed gone afterwards.

Built on 2026-09-22 by package M9b2, read-only. `Plan`
(`internal/onboard/plan.go`) gains the four findings this record defines --
`contributor-new`, `contributor-absent`, `contributor-unknown`,
`contributor-stale-source` -- plus a fifth this record's M9b2 block named but
did not spell out: `contributor-unreadable`, raised in place of
`contributor-unknown` when `Snapshot`'s `ContributorsUnreadable` is set, and
never accompanied by any other contributor finding for that prefix, since an
unreadable list gives no basis for "new" or "absent" either. This is the
package's own decision where the record was silent, at warning level because
it names a data-quality problem an operator can act on. Each finding fires
only where a table's row matches a prefix the snapshot already carries as
unmanaged occupancy: a brand-new import raises none of them, because there is
nothing yet to compare against. The contributor construction `EnsureOccupancy`
needs on create (package M9b1) and `Plan`'s comparison both need is now one
function, `onboard.ContributorsForRows`, moved out of `internal/onboardcmd`
and into `internal/onboard` so the two paths cannot drift; `internal/onboard`
importing `internal/assess` for the identity string is one-directional and
safe, and `internal/onboardcmd` keeps thin, same-named wrappers so none of its
own tests needed to change. A degenerate identity -- the string a row with no
`resource_id` column produces -- is excluded from both `contributor-new` and
`contributor-absent` in either direction, exactly as docs/WORK_PLAN.md's M9b1
review note asked: "match only itself... never counting as evidence".
`RuleDuplicateCIDR`'s message now names the contributor identities a collapse
produces, for every networks-table duplicate group and not only a re-import.
Nothing writes: `apply` is unchanged, and every existing golden and
`plan_byte_identity_test.go` fixture is untouched, because none of them
carries a snapshot network at the fixture's own CIDR. Eleven of twelve scratch
mutants were killed; the twelfth is an equivalent mutant (a ranges table's
`Table.Networks` is always nil by the type's own invariant, so passing it or a
literal `nil` into `planPrefixCandidates` cannot differ in any reachable
case). End to end against the real development NetBox: re-planning the
M9b1-created two-VPC prefix from a table naming one VPC raises
`contributor-absent` for the other and writes nothing; from a table naming a
third VPC raises `contributor-new` and still writes nothing, because M9b3's
`apply --refresh` does not exist yet.

**Measured on 2026-09-22 by package M9b3, before any adapter code, against
the same development NetBox `v4.6.7-5.0.2`.** The reconstructed flag needed a
second custom field: `OPTIONS /api/extras/custom-fields/` already named
`boolean` alongside `json`, and `deploy/compose/seed-netbox.py`'s existing
`ensure()` mechanism created `platform_import_contributors_reconstructed`
unchanged on the first run and matched it clean on the second, exactly as it
did for the `json` field in M9b1's own measurement -- confirmed by reading
the definition back (`GET /api/extras/custom-fields/?name=...`): type
`boolean`, `object_types: ["ipam.prefix"]`. No further measurement was
needed: this record already establishes that NetBox 4.6.7 honours `If-Match`
on a prefix `PATCH` (ADR 0010) and that `custom_fields` merges key by key
rather than replacing the whole map (ADR 0010, ADR 0012), and both are what
the refresh needs.

**Built on 2026-09-22 by package M9b3.** `RefreshOccupancy`
(`internal/netbox/refresh.go`) is `onboard apply --refresh`'s write path, and
it is the only thing in the package that updates an already-existing prefix.
It reads the one prefix at the CIDR with `adoptRead` -- the same
ETag-carrying detail read `Adopt` and `AbandonAdoption` already share -- and
refuses on two conditions this record names and a third it does not: **any**
ownership field, tested with `prefix.owned()` rather than
`existingOccupancyPrefix`'s narrower `platform_allocation_id`-only check,
because the record's own words are "any ownership field" and `owned()` is
the function that already means that everywhere else in the package; an
unreadable contributor list, unconditionally; and, where the record is
silent, absence of the import tag itself -- defence in depth for a
hand-created NetBox prefix that happens to sit at a CIDR Plan calls
unmanaged, which `TestRefreshOccupancyRefusesAPrefixWithNoImportTag`
pins. In practice this third refusal is unreachable through `apply
--refresh` today: every unmanaged prefix `EnsureOccupancy` has ever created
carries the tag. The merge (`mergeContributors`) is a plain map keyed on
`Identity`: an identity in the table's own would-be list but not in the
prefix's existing one is added whole; one present in both keeps every field
of the STORED entry except `observed_at` and `last_seen_batch`, which move to
the table's current values -- so a table that names an existing contributor
again, in the SAME call that adds a genuinely new one, refreshes that
existing entry's freshness too, and `first_seen_batch` is the one field nothing
ever moves once written. A degenerate identity -- no `resource_id` column --
is exactly one map key like any other, so several want-rows sharing it
collapse to the one stored entry that identity owns and can never multiply,
proven by `TestRefreshOccupancyDegenerateIdentitiesNeverMultiply`, which
names it new once and then unchanged. The write happens only when the
identity set grew against the existing list or the list was absent
(`reconstruct`); a call whose want list is empty and finds no existing list
either writes nothing, matching `EnsureOccupancy`'s own "no list nobody
wrote" rule, pinned by its own test naming exactly that case.
When it writes, the PATCH carries exactly `platform_import_contributors` and
`platform_import_source` -- the record's own sentence, quoted: "It writes
`platform_import_contributors` and, on the same call, `platform_import_source`
and `last_seen_batch` values inside the entries" -- and, once only, the
reconstructed flag; `platform_import_batch` at the top level is never
touched, because the record names only those two fields and `last_seen_batch`
lives inside the entries, not beside them. A `412` classifies as
`ErrOccupancyConflict`, the same sentinel `Adopt`'s own conflict carries,
and is never retried. **Without `--refresh`, `apply` is byte-for-byte
today's**: `applyRefresh` (`internal/onboardcmd/onboardcmd.go`) is reached
only behind the flag and returns immediately for anything but a
`KindNetworks` table, so an installation that never passes it issues not
one additional NetBox request --
`TestApplyWithoutRefreshFlagMakesNoAdditionalRequestsEvenWhenRefreshWouldWrite`
proves it with a request-recording fake against a table that would
otherwise write. Its mutation table, on a scratch copy: 17 of 17 mutants
killed, covering every refusal (owned, unreadable, `412`), every clause of
the set-change rule (the no-growth guard, `observed_at`, `last_seen_batch`,
`first_seen_batch`, the reconstruct branch, the degenerate-identity map key,
the "grew" flag itself), and the two onboardcmd gates (the `--refresh` flag,
the `RuleAlreadyUnmanaged`/`KindNetworks` filters); one survived on first
pass (a reconstruction with nothing to reconstruct) and a direct unit test
was added to close it. End to end, in `test_e2e_import.py`'s own range and
both halves under one run id, **68 + 53 = 121 passing**: naming a third VPC
on the M9b1-created two-VPC prefix (test_13's shape) writes exactly once,
touches neither the description nor NetBox's own `last_updated` for the two
already-known entries beyond their `last_seen_batch`, and moves
`last_updated` once; a second `--refresh` over the identical table, with
every `observed_at` made strictly newer, makes no PATCH and leaves
`last_updated` unchanged; a prefix seeded directly through the REST API with
no contributor field at all -- exactly what an import before M9b1 leaves --
gains a reconstructed one-entry list and the permanent flag, and
`platform_import_batch` stays the pre-M9b1 value; and a prefix whose
ownership fields were written directly through the REST API (simulating an
adoption without running the full `adopt` pipeline, cleaned up by the test
itself afterwards so the marker never outlives the test) shows the "pleasant
result" this record names: the table row never reaches `RefreshOccupancy` at
all, because `overlapsManaged` already refuses it as a `plan` error before
`apply` writes anything.

**Decided where the record was silent, and why:** the merge's exact
per-field behaviour for an identity present in both lists (only
`observed_at`/`last_seen_batch` move; everything else, including
`source_file`/`source_row`, is the original sighting's, not the newest
table's); the refusal for a prefix without the import tag (added beyond the
record's named list, and never observed to fire against a real `apply
--refresh` invocation); and that a reconstruction producing an empty list
writes nothing, mirroring `EnsureOccupancy`'s create-path rule rather than
inventing a new one. **Not verified:** two refreshes racing each other
against the same prefix (the `412` path is unit-tested, not raced against a
real NetBox); a contributor list large enough to approach the 4 MiB/16 MiB
response-reader limits M9b1 measured, under a refresh's own read-then-merge
path; and, as ADR 0016 itself says of every package before removal, whether
the evidence this refresh accumulates is what the customer's audit owners
will accept.

### 2026-09-22: M9c description format (end-to-end verified 2026-09-23)

Package M9c moves the account and resource-ID roll-up out of a newly created
network prefix's description. The structured contributor field already stores
those identities. The description now contains the input's name and descriptive
text, capped at 200 runes with an ellipsis, followed by an eight-hex-digit
SHA-256 fingerprint of the stored body. A prefix with many contributors no
longer fails creation because of the description's length. Existing descriptions
are untouched on re-import and refresh. `onboard remove` checks the fingerprint
on a new-format prefix and retains its account/resource-ID containment check
for an older prefix. This detects ordinary edits to the stored text and avoids
treating import-time truncation as an edit. It does not authenticate the text:
someone who recalculates the suffix can bypass the check, so the removal
report and the operator's review remain necessary. Unit and fake-NetBox tests
cover the format and the fifty-contributor case. On 2026-09-23 the real-stack
end-to-end import and removal case for a CIDR shared by fifty VPCs passed in
the isolated development project, as part of the first half of the full suite
(71 tests); the second half also passed (55 tests). No real AWS Organization
was used.
