# ADR 0020: A read-only migration workspace inside NetBox

Status: accepted for the local development workflow, 2026-09-23. Stage and
production promotion require their own identity, upgrade and load gates.

## Context

NetBox's native Prefix view exposes occupancy and allocation metadata, but
`onboard assess` and `onboard progress` produce offline reports with no browser
view. The customer workflow needs operators to review conflicts, coverage and
planned moves beside the inventory. The assessment engine deliberately reads
neither NetBox nor the ledger, and the migration plan remains a reviewed file
outside the service.

## Decision

Provide an optional, read-only NetBox plugin at `/plugins/platform-ipam/`.
An authenticated `platform-operators` member or NetBox superuser uploads the
collector's input files and, for progress, the reviewed plan and allocation
evidence export. The plugin runs the existing `platform-ipam onboard assess`
or `progress` binary with `--format json`, a bounded timeout, fixed temporary
file names and no inherited service credentials. Exit `3` is a valid report
with a conflict or incomplete evidence, not an execution failure. Files are
removed after the request; the plugin stores neither reports nor decisions by
default. ADR 0023 adds explicit, immutable derived-report snapshots.
The plugin can also display the separate `topology.json` report from the
read-only AWS collector. It keeps that report separate from the intended
connectivity assessment; it does not join or reinterpret them.
An address-planning view runs the separate offline pilot planner with an
operator-approved pool plan and displays advisory candidates. It does not
reserve them; ADR 0022 governs its address and network readiness labels.
A pilot overview may additionally display an operator-uploaded report from
the read-only Platform-IPAM reservation verifier. It checks the report's
pilot digest, coverage, counts and expiry before projecting its per-move
results; it still receives no Platform-IPAM credential and makes no API call.
The imported report is an operator-supplied artifact, not a signed proof.

The browser uses NetBox's session and CSRF handling. The local proxy remains
the only published UI path, and retains its REST/GraphQL block. The plugin
does not add an allocation write path, import records, collect AWS data, or
claim route or workload reachability. NetBox remains the inventory UI; the
offline engines remain the authority for report contents.

## Consequences

- A report can be inspected in the browser and downloaded as JSON without
  granting the browser Platform-IPAM ledger or NetBox API credentials.
- The operator must still obtain inventory, review the connectivity matrix,
  maintain the migration plan and provision from committed allocations.
- The plugin image derives from the exact qualified NetBox 4.7.1 digest and
  bundles the current service binary. The plugin and NetBox must be qualified
  together on each subsequent exact release. The optional Compose overlay
  keeps the base stack and its upgrade path intact.
- Browser evidence from the local Basic-auth stack does not validate real
  Entra/LDAP identity, a live AWS Organization, 100-account scale or a TGW
  readiness verdict. Those gates remain explicit in [the workspace guide](../MIGRATION_WORKSPACE.md).
