# Documentation

The implementation is grounded in these contracts. External compatibility, live AWS onboarding, and a deployed Kubernetes environment still need separate validation.

| Document | Purpose |
| --- | --- |
| [Implementation plan](IMPLEMENTATION_PLAN.md) | Boundaries, storage, allocation correctness, deployment, delivery phases, and acceptance gates |
| [API v1](API_V1.md) | Requests, responses, ownership, idempotency, lifecycle, release, and errors |
| [Finding codes](FINDINGS.md) | Every finding the platform raises: severity, scope, who sees it, what raises it, what resolves it and what to do; kept complete by a source-parsing test |
| [Clients](CLIENTS.md) | Terraform provider implementation, usage, imports, REST, Python, AWS CLI, and future clients |
| [AWS integration](AWS_INTEGRATION.md) | VPC/subnet inventory, cross-account IAM, EKS identity, coverage, onboarding, and optional AWS native IPAM |
| [NetBox integration](NETBOX_INTEGRATION.md) | Inventory mapping, adapter, native IPAM UI, access control, and optional visualization extensions |
| [Deployment environments](DEPLOYMENT.md) | Docker Compose for development; Helm/Kubernetes for stage and prod; migration and promotion flow |
| [Local simulation](LOCAL_SIMULATION.md) | Compose-first validation and the next AWS, identity, and Kubernetes simulation gates |
| [Adoption runbook](../deploy/runbooks/ADOPTION.md) | Giving an imported network an owner with `platform-ipam adopt plan\|apply`: prerequisites, the input table, verdicts, `adoption_stuck`, no undo |
| [Work plan](WORK_PLAN.md) | Packaged next steps with the model and effort each one needs |
| [Operator UI authentication](GUI_AUTHENTICATION.md) | Basic, mock Entra ID and OpenLDAP modes implemented in Compose; real identity providers remain unverified |
| [Organization inventory](AWS_ORGANIZATION_INVENTORY.md) | Collecting accounts, VPCs, subnets and ranges from an AWS Organization |
| [NetBox AWS plugin](NETBOX_AWS_PLUGIN.md) | Plugin evaluation and optional Compose image for AWS accounts, VPCs and subnets |
| [Onboarding import](ONBOARDING_IMPORT.md) | Importing existing accounts, networks and ranges from CSV, Excel or a pasted table |
| [Overlap assessment](OVERLAP_ASSESSMENT.md) | `platform-ipam onboard assess`: an offline, resource-aware report of which observed VPC CIDR relationships conflict, impact against a reviewed connectivity matrix, and coverage as a first-class result (ADR 0014) |
| [Migration progress](MIGRATION_PROGRESS.md) | `platform-ipam onboard progress`: derives three independent per-move facts (subject, target, conflicts) from a reviewed `migration.yaml`, the same inventory, and an authenticated allocation evidence export; never a percentage or a "done" (ADR 0015) |
| [Overlapping AWS networks](IP_OVERLAP_MIGRATION.md) | Customer scenario, current allocation/migration boundaries, missing capabilities, proposed pilot and open questions; analysis, not an accepted implementation contract |
| [Licensing and business-model brainstorming](BRAINSTORMING_LICENSING.md) | Preserved licensing notes, corrected assumptions and a proposed small recurring-revenue offer; does not change the licence |
| [End-to-end suite](../tests/e2e/README.md) | Running the local stack and the REST/CLI/Terraform/UI checks against it |
| [Contributing](../CONTRIBUTING.md) | Setup, required checks, and what a change must carry |
| [Security policy](../SECURITY.md) | Private reporting, supported versions, and scope |
| [Changelog](../CHANGELOG.md) | Released and unreleased changes |

| Decision record | Subject |
| --- | --- |
| [ADR 0001](decisions/0001-IMPLEMENTATION_FOUNDATION.md) | Go API and worker with a durable PostgreSQL allocation ledger |
| [ADR 0002](decisions/0002-LOCAL_DEVELOPMENT_AND_E2E_ENVIRONMENT.md) | Containerized local environment and a multi-client end-to-end suite |
| [ADR 0003](decisions/0003-FIRST_PARTY_CONSUMER_CLI.md) | A first-party consumer CLI shipped in the service binary |
| [ADR 0004](decisions/0004-LOCAL_TERRAFORM_PROVIDER_DISTRIBUTION.md) | Development builds of the Terraform provider install through a CLI development override |
| [ADR 0005](decisions/0005-PUBLIC_RELEASE_LICENCE_AND_DISTRIBUTION.md) | Apache-2.0, public release, and the limits of controlling reuse |
| [ADR 0018](decisions/0018-PUBLIC_TERRAFORM_PROVIDER_RELEASE_ROUTE.md) | Public Terraform Registry route; namespace and real-release gates remain open |
| [ADR 0019](decisions/0019-EXACT_NETBOX_RELEASE_SUPPORT.md) | Qualify exact NetBox images; 4.6.10 is a candidate, 4.7 a separate gate |
| [ADR 0006](decisions/0006-OPERATOR_UI_AUTHENTICATION.md) | One proxy in front of the NetBox UI, with basic, Entra ID and LDAP modes |
| [ADR 0007](decisions/0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md) | Existing networks are imported as unmanaged occupancy, not as allocations |
| [ADR 0008](decisions/0008-PLATFORM_OWNED_OPERATOR_UI.md) | Proposed: no platform-owned operator UI, and a NetBox plugin if that changes |
| [ADR 0009](decisions/0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md) | Proposed: the NetBox AWS plugin is a view, never an allocation input |
| [ADR 0010](decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md) | Existing networks are adopted by a pinned reservation (built and demonstrated end to end, including a VPC with its subnet in two runs; amended 2026-09-20 to exempt a reviewed VPC's own observed subnets) |
| [ADR 0014](decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md) | Overlaps are assessed per resource from the organization inventory, impact is three-valued against a connectivity matrix, and coverage is a result (accepted as a design 2026-09-20; built through packages M1b1-M1b4 -- `platform-ipam onboard assess`, see [Overlap assessment](OVERLAP_ASSESSMENT.md) -- against a stubbed collector only, not a real AWS Organization) |
| [ADR 0015](decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md) | A migration plan is a reviewed file in the customer's repository with no CIDR field anywhere; its progress is derived offline from the assessment and an allocation evidence file, as three independent facts per move and never a percentage or a "done" (accepted as a design 2026-09-22; built through packages M3b1-M3b4 -- `platform-ipam onboard progress` and `platform-ipam client evidence`, see [Migration progress](MIGRATION_PROGRESS.md) -- against a synthetic estate only, not a real plan or a real AWS Organization) |
| [ADR 0016](decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md) | Imported occupancy names its contributors in an unowned NetBox field; a re-import writes only when the contributor set changes; a removal is one named prefix, explicit, and refused without complete coverage for every contributing account and region (accepted as a design 2026-09-22; built through packages M9b1-M9b4 against the development NetBox. The N4 `seed` mode can create the fields for stage and production, but has not run in either environment) |
| [ADR 0017](decisions/0017-PERSISTING_ONLY_WHAT_A_LEDGER_TRANSACTION_CHANGED.md) | The ledger port and whole-state closures stay. Audit writes (M4e), the remaining table diff and on-demand audit reads (M4f), and M4g's projection `PATCH` skip are built and locally tested; the 1,000-allocation pilot timing remains open. Per-aggregate row locks remain the target. |
| [ADR 0013](decisions/0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md) | A reservation that can never complete is cancelled by its own consumer, only while the platform itself calls it stuck (built through packages H8a-H8d; demonstrated end to end against the real ledger and NetBox in `tests/e2e/test_e2e_reservation_stuck.py`'s `ReservationCancelE2ETest`, including the case where the reservation's prefix was already created before it got stuck) |
| [ADR 0012](decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md) | An adoption that never committed may be abandoned by an operator; a committed one never (built and demonstrated end to end) |
| [ADR 0011](decisions/0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md) | No eligible pool is a refusal, and an operator is a principal with no tenant (built and demonstrated end to end) |

| Example | Status |
| --- | --- |
| [Terraform VPC](../examples/terraform/vpc/main.tf) | Provider configuration and VPC allocation usage |
| [Terraform subnets](../examples/terraform/subnets/main.tf) | Parent-aware subnet allocation usage |
| [REST allocation request](../examples/rest/allocate.json) | API request body |
| [Python reservation client](../examples/python/reserve.py) | Standard-library client, including operation polling |
| [Pool and policy configuration](../examples/config/pools.yaml) | Illustrative schema; addresses, account, and NetBox IDs are placeholders |

The implementation plan is the delivery authority. The API document is the behavioral authority for consumer integrations. The OpenAPI contract is at [`api/openapi.yaml`](../api/openapi.yaml); NetBox field names and IDs belong only in adapter/configuration documentation.

## Validation status

Updated 2026-09-23. Alongside the Go unit tests and the static OpenAPI,
documentation-link, Compose and Helm checks, the local Compose stack now runs
end to end and is verified by [`tests/e2e`](../tests/e2e/README.md): reservation
through REST, through the first-party CLI, and through the Terraform provider,
with the resulting allocation asserted in the NetBox inventory an operator
reads, plus an opt-in browser check of the NetBox UI itself. Address-space
behaviour covered there includes alignment, pool containment, disjointness
across mixed prefix lengths, idempotent replay of an allocation key, and
capacity arithmetic.

Running the stack for the first time surfaced defects that no static check
could reach: a NetBox API token pepper below NetBox's minimum length, no path
that created a usable NetBox API token at all, a non-idempotent seed, an
inventory adapter that cancelled its request context before reading the
response body, and two Terraform provider faults that made every real plan or
apply fail. These are fixed; see the decision records for the reasoning behind
the environment they were found in.

Still unverified: live AWS, a stage/production Kubernetes rollout, published
provider distribution and installation, and stage/production promotion of the
exact NetBox support image. The local Compose stack now pins the qualified
4.6.10 digest after full-suite, identity and restore rehearsals; 4.7 remains
untested. The optional
[`run-provider-mirror.sh`](../tests/e2e/run-provider-mirror.sh) now exercises
local filesystem-mirror installation, checksums and a lock file; the main
development suite still uses a CLI development override.
