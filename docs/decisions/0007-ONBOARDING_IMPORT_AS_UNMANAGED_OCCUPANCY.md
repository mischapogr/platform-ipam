# ADR 0007: Existing networks are imported as unmanaged occupancy, not as allocations

Status: accepted, 2026-09-18. Not yet implemented; see the [work plan](../WORK_PLAN.md).

## Context

The service cannot safely allocate from a pool until it knows what is already
in use, and that knowledge lives in spreadsheets, wiki tables and the AWS
accounts themselves. The documents have always named "an explicit
inventory/import workflow" as a precondition for onboarding, and no such
workflow exists.

There are four places imported data could land. The ledger could receive
allocations for existing VPCs. NetBox could receive prefixes. Configuration
could receive accounts. The development stack's fake cloud fixture could
receive observed resources.

Minting allocations is the high-fidelity option and the dangerous one. `Reserve`
is the only place an allocation is constructed and the only path that commits
one. A second constructor would have to reproduce the request hash exactly, or
the owning team's first request under that key is refused as a conflict; create
the matching NetBox prefix with its markers, or projection never settles; and
tag the cloud resource, or the worker raises a critical finding for every
imported row. Raw SQL is not available as a shortcut, because the ledger
rewrites every table on each update. And the API contract forbids any consumer
endpoint that selects a CIDR.

NetBox occupancy needs none of that. The adapter already treats every prefix,
address and range in the domain's VRF as occupied, and the allocator already
refuses to select overlapping space. The existing integration document
prescribes exactly this step before pool admission.

Accounts are configuration and nothing else: coverage cells, pool eligibility,
identities. They are validated strictly at load, read once at start, and
changing coverage invalidates absence evidence by design.

## Decision

Networks and ranges are imported into NetBox as prefixes in the domain's VRF
that carry an import tag and batch but none of the platform's allocation
markers. They block allocation and own nothing.

Accounts are rendered as YAML fragments for a person to review and merge. The
import never edits configuration.

The import is an operator process mode of the service binary,
`platform-ipam onboard`, with separate `parse`, `plan`, `apply` and `render`
steps. It is not a CLI verb, because the CLI is a transport client with no
policy and there is no endpoint for it to call; and it is not an endpoint,
because the contract forbids one. NetBox writes go through the adapter, so the
VRF and marker rules stay in one place.

`plan` validates against the real configuration and a real inventory snapshot,
and `apply` refuses to run over errors. The validation rules are derived from
how the running service fails: a duplicate CIDR in a VRF takes the whole
domain's reservations down; a network that contains a pool is silently ignored
as an ancestor and would block nothing; a prefix outside the VRF is invisible.
These are errors or explicit collapses, never warnings to scroll past.

Input is CSV, TSV, a table pasted from Confluence, and `.xlsx` read with the
standard library. Normalization treats spreadsheet damage as expected: lost
leading zeros in account ids are repaired with a warning, scientific notation
is refused because the digits are gone, and a non-canonical CIDR is an error
rather than something to mask quietly.

Adopting an existing network as an owned allocation is deferred to its own
decision record.

## Consequences

Onboarding can start without any new way to create an allocation, which keeps
the one constructor the safety tests are written against.

Imported space is visible in NetBox and in reduced capacity figures, but it is
invisible to the platform API: there is no owner, no key, no audit event and no
finding for NetBox-only occupancy. Operators see it in the UI; consumers only
see less room. That asymmetry is accepted for now and is the main argument for
the later adoption work.

Existing VPCs cannot be brought under Terraform management through the provider
until adoption exists, because provider import takes a platform allocation id
and imported occupancy has none.

An import can take allocation offline if it is wrong, which is why `plan` is
mandatory and why onboarding pauses the domain before `apply`.

The `.xlsx` reader handles cached values in ordinary sheets and nothing more.
Formulas, merged header cells and multi-table sheets are the operator's to
flatten, and the tool says so rather than guessing.
