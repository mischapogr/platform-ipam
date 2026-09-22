# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the version is below `1.0.0`, the API and provider contracts may change
between minor versions.

## [Unreleased]

### Added

- Local Compose probes now cover Moto EC2/STS, Samba AD with LDAPS, an exact
  NetBox 4.6.10 candidate and its optional AWS plugin, while a kind probe
  exercises Helm controller behavior. The public provider route has a local
  filesystem-mirror installation and checksum rehearsal.
- M4g skips unchanged NetBox projection PATCHes and refreshes the operator
  observation stamp on a separate bounded cadence. The 1,000-allocation pilot
  timing remains an acceptance gate.
- The development NetBox bootstrap can create a write-enabled, group-limited
  inventory maintainer test account. The e2e role test checks prefix create,
  change, and denied delete, then removes only its verified test prefix (A2).
- New network imports keep contributor identities in `platform_import_contributors`
  and cap the purpose description at NetBox's 200-rune limit. `onboard remove`
  accepts the new description format while preserving the check for prefixes
  imported before it (work-plan package M9c).
- The end-to-end suite documents its per-module allocation quota and checks
  the committed count at the end of its second half (work-plan package T2).
- `platform-ipam seed`, a new process mode that creates or verifies every
  NetBox custom field, choice set and tag `internal/netbox` relies on --
  idempotent and conflict-detecting (an existing definition of another type,
  or with different object types, is reported, never rewritten), reading
  only the NetBox origin and token (no database, no OIDC, no cloud, no
  pools/identity configuration). Closes the gap `deploy/compose/seed-netbox.py`
  left for stage and production, where an import used to fail with a bare
  `400` until an operator created the fields by hand. `internal/netbox/seed.go`
  is now the single source of truth for the field/tag catalogue;
  `deploy/compose/seed-netbox.py`'s own declarations are asserted equal to
  it by a test so the two cannot drift apart silently. The Helm
  `operatorJob` gains `mode: seed` (least privilege: NetBox settings only,
  no subcommand, no flags, no input table). See
  [the seed runbook](deploy/runbooks/SEED.md) (work-plan package N4).
- `internal/storage` gains a differential harness that replays deterministic,
  seeded sequences of ledger transactions against both `PostgresLedger` and
  `MemoryLedger`, comparing reloaded state, all nine tables row by row, and
  returned errors after every step; it ships no production change and is
  gated on `IPAM_TEST_DATABASE_URL` like the rest of the package (ADR 0017,
  package M4c).
- `platform-ipam onboard progress` derives three independent per-move facts
  (subject, target, conflicts) from a reviewed `migration.yaml`, the same
  organization inventory `onboard assess` reads, and an authenticated
  allocation evidence file — never a percentage or a "done". The evidence
  file is produced by a new consumer-CLI verb, `platform-ipam client
  evidence`, which walks every page of `GET /v1/allocations` and infers or
  is told its own scope. See
  [Migration progress](docs/MIGRATION_PROGRESS.md) and
  [ADR 0015](docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md).
- Imported occupancy names its contributors: an imported prefix carries
  `platform_import_contributors`, an unowned NetBox JSON custom field with one
  entry per collapsed source row, so a CIDR shared by several VPCs records all
  of them instead of only a sentence in its description. Written on create
  only; nothing refreshes it yet. See
  [ADR 0016](docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md).
- `onboard plan` reports a re-import's contributor drift, read-only:
  `contributor-new`, `contributor-absent`, `contributor-unknown`,
  `contributor-stale-source` and `contributor-unreadable` findings, computed
  against the prefix's existing `platform_import_contributors` list; `apply`
  still writes nothing for them ([ADR 0016](docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md)).
- `onboard apply --refresh`, occupancy's first update path: additively adds a
  new contributor and refreshes an already-named one's `observed_at`/
  `last_seen_batch`, never removes one a table does not name, writes at all
  only when the contributor set changes, and reconstructs (and permanently
  flags, `platform_import_contributors_reconstructed`) a list on a prefix
  imported before ADR 0016 existed. Without the flag `apply` is unchanged.
  See [ADR 0016](docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md).
- `onboard remove`, the first command in this project that deletes something
  the platform does not own: a dry run by default, one named prefix, four
  independently-refusable evidence rules (every contributor known, coverage
  complete for its accounts and regions, every contributor observed absent,
  a reviewed `--apply --id`), and a JSON report naming exactly why a prefix
  is or is not removable. See
  [ADR 0016](docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md).

- First-party consumer CLI, `platform-ipam client`, shipped inside the service
  image. It refuses to invent an allocation key, derives a stable
  `Idempotency-Key`, polls an accepted operation instead of re-posting, and
  uses a distinct exit code for an operation that did not settle. See
  [ADR 0003](docs/decisions/0003-FIRST_PARTY_CONSUMER_CLI.md).
- End-to-end suite in `tests/e2e`, covering reservation through REST, the CLI,
  and the Terraform provider, with results asserted in the NetBox inventory an
  operator reads, plus an opt-in browser check of the NetBox UI. See
  [ADR 0002](docs/decisions/0002-LOCAL_DEVELOPMENT_AND_E2E_ENVIRONMENT.md).
- `netbox-bootstrap` Compose service, which creates the development superuser
  and the v1 API token the NetBox adapter authenticates with.
- Apache-2.0 licence, `NOTICE`, and the community documentation required for a
  public repository.

- Designs, decision records ([ADR 0006](docs/decisions/0006-OPERATOR_UI_AUTHENTICATION.md),
  [ADR 0007](docs/decisions/0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md)) and a packaged
  [work plan](docs/WORK_PLAN.md) for operator UI authentication and the onboarding import. These
  are plans; none of it is implemented.
- [Procedure](docs/AWS_ORGANIZATION_INVENTORY.md) for inventorying accounts, VPCs, subnets and
  ranges from an AWS Organization.

- `internal/onboard`: canonical import tables, header aliases and spreadsheet-damage normalization
  (no readers or command yet). `netbox.Client.EnsureOccupancy`: writes imported networks as unmanaged
  occupancy and refuses any object carrying an allocation marker.
- NetBox bootstrap creates a view-only `platform-operators` group and a narrowly scoped
  `platform-inventory-maintainers` group.
- `scripts/aws/org-inventory.sh` with a stubbed-CLI test, and a CloudFormation StackSet template for
  the cross-account read role under `deploy/aws/`.

- `ui-proxy` (Caddy) is now the only published path to the NetBox UI: HTTP Basic against a bcrypt
  hash, identity passed to NetBox in a trusted header, client-supplied identity headers stripped in
  both spellings, `/api/` and `/graphql/` refused. Authenticated users land in the view-only group.
- `internal/onboard`: readers for CSV, semicolon CSV, TSV, pasted tables and `.xlsx` (standard library
  only); `Plan`, the import validator; config-fragment and fake-cloud fixture renderers.
- Optional Compose overlay building NetBox with `netbox-aws-vpc-plugin` 0.1.0, verified to install and
  migrate on the pinned NetBox.

- `platform-ipam onboard`: `parse`, `plan`, `apply`, `render-config`, `render-fixture` and the read-only
  `drift` check. Imports CSV, semicolon CSV, TSV, pasted tables and `.xlsx` as unmanaged NetBox
  occupancy; verified end to end against a live NetBox and against workbooks written by openpyxl,
  XlsxWriter and LibreOffice (not Microsoft Excel).
- Adoption of an imported network as an owned allocation (ADR 0010): `platform-ipam adopt plan|apply`, worker recovery of a crashed adoption, the `adoption_stuck` finding, and an [adoption runbook](deploy/runbooks/ADOPTION.md). A VPC's own observed subnets no longer block its adoption, and a VPC with subnets is adopted in two runs with the owning team tagging the VPC in between. `onboard` can import a subnet created inside an already managed VPC.
- `platform-ipam adopt abandon` (ADR 0012): an operator withdraws an adoption that never committed — the half-converted NetBox prefix is cleared back to imported occupancy, the hold is deleted, the allocation key is free again and the audit events remain; a committed allocation is always refused.
- A `reservation_stuck` finding for a pending reservation that fences its overlap domain and cannot complete, which was silent before; a [reference of every finding code](docs/FINDINGS.md); inventory errors that are no answer at all (a timeout, a `5xx`, a `429`) are retried without a finding, for reservations and adoptions alike.
- Helm: an opt-in `operatorJob` that runs `platform-ipam adopt` (`plan`, `apply`, `abandon`) or `onboard` (`plan`, `apply`) in the cluster as a one-shot, never-retried Job with least privilege per mode; `abandon` takes no table and its flags through `args`. Validated by `helm lint` and `helm template` only.
- `adopt` and `worker` no longer refuse to start in stage and production without OIDC settings they never read; `api` is unchanged.
- A read-only `operator` role in the identity file (ADR 0011): an identity with no tenant that reads pools, capacity, allocations and a de-duplicated, domain-wide findings list across tenants, is refused every write by the existing tenant checks, and sees `tenant_id`, `domain_id` and the resource identity that no tenant response carries. Operator reads are written to the application log. `client findings --fail-if-open` run as an operator judges the whole estate.
- `GET /v1/pools` answers `403 no_eligible_pool` for an identity whose tenant is eligible for no pool (ADR 0011 stage one); the API warns about such identities at start-up, and an empty `eligible_tenants` entry fails configuration load.
- `IPAM_LOCAL_EXTRA_CREDENTIALS` (development only) for additional local identities; the Compose stack gains an `ops-observer` identity eligible for no pool, and `IPAM_API_PORT` moves the API's host port.
- `platform-ipam client findings`, with `--fail-if-open` as a pipeline gate that walks every page and first checks `GET /v1/pools`: an identity eligible for no pool exits `8` instead of passing over a list it could never see into.
- Organization inventory collector: `networks.csv` gains `association_id` and `observed_at`, and the run writes `run.json`, recording every account and region as `succeeded`, `partial`, `failed` or `not_attempted` — the only way "read and empty" can be told from "not read" (ADR 0014). The import reads the new columns and is otherwise unchanged; no new IAM permission.
- `platform-ipam onboard assess`: an offline, resource-aware overlap assessment of an organization inventory (ADR 0014). It reads the collector's files and optional reviewed inputs and nothing else — no configuration, no NetBox, no ledger — lists every VPC CIDR relationship with both source rows, and never reports complete coverage it cannot show. Exits `0` clean, `3` not clean, `4` no report.
- `onboard assess` cross-checks `run.json`'s own `row_count` against the rows actually present for each account and region (ADR 0014, M1c): fewer rows than recorded is a new `coverage.row_count_short` gap, counted like `partial`/`not_attempted`; more rows than recorded is `coverage.row_count_exceeded`, a data-error note that also forces `coverage.complete: false`. A truncated or hand-filtered `networks.csv` beside an intact `run.json` is now caught instead of silently reporting `complete: true`.
- Inventory port `CancelReservation` (ADR 0013): deletes the single prefix a stuck reservation's own marker names, conditionally and confirmed by read-back. Nothing calls it yet.
- `DELETE /v1/operations/{operation_id}` (ADR 0013): a tenant cancels their own pending reservation once the platform has declared it stuck, and `platform-ipam client cancel` calls it; `client`'s `awaitOperation` now prints a `FAILED` operation's own reason instead of chasing its (possibly now-gone) allocation, and the Terraform provider's diagnostic for a `FAILED` reservation no longer reads "HTTP 0" and now mentions the cancel.
- Operator UI security test matrix; optional `ui-proxy` in the Helm chart; an agent skill for the
  onboarding import; proposed decision records 0008 and 0009; decision records 0010 (adoption)
  and 0011 (what an operator who is not a tenant may see).

- Operator UI sign-in through Microsoft Entra ID (`compose.ui-entra.yaml`, `oauth2-proxy`) and through
  LDAP (`compose.ui-ldap.yaml`), each an optional Compose overlay. Verified against a mock OIDC issuer
  and an OpenLDAP test server respectively, not against Microsoft services.
- `onboard apply --aws-objects`: also records AWS Account, VPC and Subnet objects in the optional
  NetBox AWS plugin, linked to the imported prefixes. The plugin is a view; prefixes do the blocking.

### Changed

- `scripts/ai/check-aws` runs `shellcheck` from a pinned image (`koalaman/shellcheck:v0.10.0`) over the shell scripts in `scripts/aws/`, `tests/aws/` and `deploy/compose/`, locally and in CI.
- Go module paths are now `github.com/mischapogr/platform-ipam` and
  `github.com/mischapogr/platform-ipam/providers/terraform`, so both modules
  resolve for external consumers.
- Go and Terraform run from pinned container images. Neither host toolchain is
  a prerequisite for development or for `scripts/ai/check-provider`.
- `deploy/compose/fixtures/pools.yaml` raises
  `max_unreclaimed_allocations_per_tenant` from 8 to 32, so a full end-to-end
  run does not exhaust the development quota.
- The worker's `syncProjections` now records a pass's NetBox projection results in one batched
  `ledger.Update` instead of one per allocation, skipping that write entirely once nothing has
  changed; measured at 100/1,000/5,000 committed allocations, this cut pass wall time by 3.4x/5.5x/
  ~16x (docs/DEPLOYMENT.md's dated measurement section) by removing the O(N^2) full-ledger rewrite
  `internal/storage`'s `persistState` paid on every one of those calls -- the PATCH itself still
  runs once per allocation per pass, unchanged.
- `internal/storage` gains an opt-in measurement test, `TestMeasureStatementsPerUpdate` (skipped
  unless `IPAM_MEASURE=1`), that counts `persistState`'s round trips per `Ledger.Update` against a
  counting fake of `pgx.Tx`, confirming ADR 0017's formula `9 + 3A + O + P + R + D + F + C + E`
  exactly at every swept size; it ships no production change (ADR 0017, package M4d).
- `internal/storage`'s `PostgresLedger.Update` no longer re-offers or decodes the ledger's full audit
  history: `persistState` inserts only the events a transaction's own closure appended, and `Update`'s
  `loadState` call skips `audit_events` entirely, removing a write and read cost that grew with the
  ledger's age rather than its size. `View` is unchanged, still loading events eagerly, so this is a
  partial read-side win, roughly half of a reservation's audit-decode cost; measured before/after at
  100 and ~1,000 committed allocations in `docs/DEPLOYMENT.md`'s dated measurement section (ADR 0017,
  package M4e).
- `internal/storage`'s `persistState` now writes only what a `Ledger.Update` closure actually changed
  across the remaining eight tables (added / changed / removed per row, deletion always computed from
  key-set membership, never from content), instead of an unconditional full rewrite. An allocation
  whose tenant or key changes clears its old derived rows before the new identity is written; `View` stops
  decoding `audit_events` too, and the additive `domain.Ledger.Events(ctx, allocationID)` method is now
  the store's only reader of the audit table. A thousand-row ledger with one row changed per table now
  costs eight statements instead of six thousand, and idle reservation latency at ~1,000 allocations
  stopped growing with the ledger's size at all in a re-measurement, both in `docs/DEPLOYMENT.md`'s
  dated measurement section (ADR 0017, package M4f).

### Fixed

- `networkRowsForCIDR` ordered a CIDR's collapsed networks rows by `SourceRow` alone with an unstable `sort.Slice`; since each input file restarts its own row numbering at 1 (package M1b1), a multi-file import whose row numbers collided at one CIDR could produce a different merged description, AWS-field choice, and contributor order depending on which file was named first on the command line. Now sorted by `SourceFile`, then `SourceRow`, then `ResourceID`/`AccountID` for a genuine tie, with `sort.SliceStable` (work-plan package C10, found by M9b1's review).
- `GET /v1/allocations/{id}` now answers `409 allocation_pending` (with the pending operation's id in
  `error.details.operation_id`) for the owning tenant's own uncommitted allocation while its `RESERVE`
  or `ADOPT` is still `PENDING`, as `docs/API_V1.md` had promised since v1 but `Service.Get` never
  built; found end to end by H8d's review of ADR 0013. Every other caller — another tenant, an unknown
  id, an operator, or a hold whose operation is not `PENDING` — keeps the `404` it always had. The CLI's
  `get` and `awaitOperation`, and the Terraform provider's `Read`, data source and metadata refresh, are
  unaffected or made to name the pending operation rather than report the allocation as absent.
- `DELETE /v1/operations/{operation_id}` now carries a failed cancel's own progress in
  `error.details` (`allocation_id`, `operation_id`, `fenced`, `already_fenced`, `removed`, `deleted`)
  whenever the fence had already succeeded before the refusal; previously the transport discarded the
  service's partial report and sent only the error (work-plan package H9).
- Helm `operatorJob` no longer demands or mounts `identity.existingConfigMap` for `mode: adopt,
  command: abandon`, which authenticates nobody and resolves no principal from it; `plan`/`apply` are
  unchanged (work-plan package H9).
- The NetBox adapter read an unset (JSON `null`) custom field as the string `"<nil>"`, so every
  unmanaged prefix, including the pool container, looked as if it carried an allocation id. Found only
  by running the import against a live NetBox; every fake had omitted the field.
- Go module file listed six directly imported dependencies as `// indirect`; `go.sum` was not tidy.

- The NetBox seed compared list-valued fields position by position, so its second run reported a
  conflict with records its first run had created. Plain-value lists now compare as sets.

Defects found by running the stack, none of which any static check or unit test
could reach:

- The NetBox adapter cancelled its request context before the response body was
  read, so inventory reads failed once a response outgrew the transport buffer.
  The failure appeared only as inventory grew.
- `prefix.status` was decoded as a string, but NetBox 4.x returns a choice
  object. One unusable field made the entire inventory snapshot fail, which
  blocked every reservation.
- The Terraform provider treated a nil `ProviderData` as an error. Terraform
  passes nil during validation, so every real plan failed with "Provider is not
  configured" and no consumer configuration could fix it.
- The Terraform provider declared `address_family` as `Optional` without
  `Computed`, so the server-side default made every apply that omitted it fail
  with "Provider produced inconsistent result after apply" — including the
  shipped example.
- The NetBox API token pepper default was below NetBox's 50-character minimum,
  so the container exited during startup while reporting an unrelated database
  wait timeout.
- The NetBox seed was not idempotent, despite being documented as repeatable:
  it compared submitted payloads against NetBox's rendered representation. It
  also used an invalid custom-field type and created selection fields without
  the required choice set.
- A `503` from an unusable inventory snapshot discarded the underlying cause,
  leaving no way to diagnose it. The response stays generic; the reason is now
  logged.
- `scripts/ai/check-compose` asserted a fixed number of NetBox secret defaults
  rather than the length property it cares about, so adding a service broke the
  check. It now also validates API token pepper length.

[Unreleased]: https://github.com/mischapogr/platform-ipam/commits/main
