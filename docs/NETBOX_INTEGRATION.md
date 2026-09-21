# NetBox inventory and optional IPAM UI

Status: implemented adapter and local UI integration, 2026-09-08; operator UI access path updated 2026-09-18. Docker Compose pins a local NetBox release, and the HTTP adapter projects managed prefixes into its native IPAM views. The UI is reachable only through `ui-proxy` (`deploy/compose/compose.netbox.yaml`; NetBox itself publishes no port), and the development bootstrap creates read-only `platform-operators` and scoped `platform-inventory-maintainers` groups (`deploy/compose/netbox/bootstrap-groups.py`) -- see [GUI authentication](GUI_AUTHENTICATION.md) for the full design, the three modes, and what is and is not verified. Production SSO against a real identity provider remains onboarding work. [NetBox IPAM](https://netbox.readthedocs.io/en/stable/features/ipam/).

## 1. Use the native UI first

Reuse NetBox as the operator inventory UI. Deploy or connect its API for core allocation; enabling user access to its UI is a separate optional milestone. The UI should help an operator answer: which pool contains this VPC, who requested it, which AWS account/region uses it, what subnets belong to it, and why a released range remains held?

Do not build a new frontend or require an embedded iframe for v1. Offer a normal SSO-protected NetBox URL, optionally deep-linked from platform allocation responses and internal documentation. The API/provider must work when these links are absent. This is an integration of NetBox's existing application, not a standalone frontend package mounted into the platform API.

## 2. Inventory mapping

| Platform concept | Proposed NetBox representation | Important boundary |
| --- | --- | --- |
| Address domain | VRF with uniqueness enforced, or explicitly configured global table | Routing overlap policy spans AWS accounts when they can interconnect |
| Allocatable regional/environment pool | Parent Prefix with status `container` | Platform eligibility/quotas remain in policy configuration |
| Excluded space | Reserved child Prefix marked as policy exclusion | Cannot be selected even if not associated with AWS infrastructure |
| VPC allocation | Child Prefix with allocation metadata, role `aws-vpc` | One allocation per primary IPv4 CIDR in initial v1 |
| Subnet allocation | Nested Prefix, role `aws-subnet` | Explicit parent identity remains in the platform ledger |
| Tenant/team | Tenant where suitable, plus immutable platform tenant metadata | Tenant assignment alone is not an authorization rule |
| AWS account/region/AZ | Custom fields | Platform `scope: vpc` is not NetBox's geographic `scope` field |
| Allocation binding | AWS resource ID plus verified observation timestamp in custom fields | Worker verifies cloud evidence; users cannot activate via metadata edits |

Do not set NetBox's `is_pool` merely because a prefix is an allocation pool: that option affects whether boundary IP addresses are considered usable. Prefix nesting and `container` serve the proposed pool model. [NetBox Prefix fields](https://netbox.readthedocs.io/en/stable/models/ipam/prefix/).

Use these platform-prefixed custom fields on managed Prefixes; names are a proposed schema:

| Field | Type / purpose |
| --- | --- |
| `platform_allocation_id` | Text, exact-match lookup; absent on parent pool containers |
| `platform_allocation_key` | Text, exact-match lookup; consumer's permanent key |
| `platform_operation_id` | Text; recover uncertain inventory writes |
| `platform_parent_allocation_id` | Text; explicit parent for a subnet |
| `platform_tenant_id`, `platform_environment` | Text; ownership and policy context |
| `platform_pool_id`, `platform_policy_version` | Text; chosen platform pool and recorded admission policy |
| `platform_state` | Selection: RESERVED, ACTIVE, QUARANTINED; RELEASED lives in ledger history |
| `platform_aws_account_id`, `platform_aws_region` | Text; account remains a string |
| `platform_aws_resource_id`, `platform_aws_az_id` | Text; verified resource and optional AZ |
| `platform_quarantine_until`, `platform_last_observed_at` | Datetime; earliest reuse and cloud freshness |
| `platform_drift_status` | Selection; summary for filtering, with full findings in platform API |

Set exact matching for identity fields, group them as “Platform allocation”, and show operator-relevant fields in object views. UI-only editing restrictions on custom fields are useful presentation controls, but backend authorization must independently prevent mutation. NetBox supports custom-field grouping, filtering, and visibility. [NetBox custom fields](https://netbox.readthedocs.io/en/stable/customization/custom-fields/).

Custom-field values are recovery markers, not database uniqueness guarantees. The platform ledger enforces unique tenant/key identity. NetBox prefix uniqueness and the adapter's identity checks provide a separate protection against exact duplicate inventory records.

## 3. Lifecycle projection

| Platform state | NetBox Prefix status | Meaning |
| --- | --- | --- |
| Pool container | `container` | Organizational parent for child allocations |
| RESERVED | `reserved` | CIDR is committed and unavailable to others |
| ACTIVE | `active` | Cloud binding verified |
| QUARANTINED | `reserved`, with `platform_state=QUARANTINED` | Still unavailable; no custom NetBox status required |
| RELEASED | Managed Prefix removed after safety gates | Historical identity/events remain in platform ledger and retained audit history |

Confirm these status values with the selected NetBox release/configuration. Keeping quarantined Prefixes as real inventory objects ensures that ordinary free-space calculation does not treat them as available. Never just mark a released prefix “available” while leaving it as an allocatable inventory child.

NetBox container utilization measures child-prefix occupancy, while ordinary Prefix utilization relates to recorded IP addresses/ranges. An ACTIVE AWS VPC can therefore show little host-IP utilization even though the entire CIDR is allocated. Label those meanings separately. Do not infer available AWS subnet addresses from NetBox's host-IP percentage, or mark all VPCs as containers just to change the chart. [NetBox utilization rules](https://netbox.readthedocs.io/en/stable/features/ipam/).

## 4. Adapter boundary and version gate

The adapter exposes methods such as read domain inventory, find by allocation marker, create exact prefix, update owned metadata, and delete verified managed prefix. It translates those calls to the pinned NetBox REST API. Business logic never imports NetBox response structs outside this boundary.

For each supported release, verify prefix create/read/update/delete, VRF behavior, pagination, custom-field lookup, validation errors, token format, timeout behavior, and audit logging. Record compatibility as tested release pairs, not “latest supported”. Stable documentation may describe newer features than the selected deployment. [NetBox REST API](https://netbox.readthedocs.io/en/stable/integrations/rest-api/).

NetBox has an available-prefix operation, but the platform's reservation uses the exact persisted candidate/recovery protocol in the [implementation plan](IMPLEMENTATION_PLAN.md). Avoid retrying an unconstrained “allocate next” write after a lost response. Test alternate write paths too: managed-domain access must not permit another actor to bypass the platform's holds or create partially overlapping prefixes.

Bootstrap custom fields, roles, VRFs, and pool containers through a separately scoped administrative job. Runtime credentials only need relevant inventory operations. Bootstrap is idempotent by stable identifiers, refuses conflicting pre-existing definitions, and produces a reviewable change report. It never edits or removes unowned objects automatically.

On updates, modify only owned fields and preserve unrelated description/tags/metadata unless the field's ownership contract says otherwise. Treat manual changes to CIDR, VRF, parent pool, or identity markers as drift requiring investigation; do not silently overwrite them. A deleted managed prefix is an incident and cannot make a held range free.

## 5. Access and UI setup

Create separate NetBox groups for platform operators with view permissions and restricted inventory maintainers for bootstrap. The platform service account is the only normal writer in managed domains. Existing unrelated NetBox workflows may continue in separate inventory domains.

NetBox supports object-based permissions and combines granted constraints. A broad existing group grant can defeat intended isolation, so validate each effective user/service account through UI and REST reads/writes. Hiding Add/Edit buttons or making custom fields read-only is insufficient. [NetBox permissions](https://netbox.readthedocs.io/en/stable/administration/permissions/).

Native UI setup steps:

1. Connect the selected NetBox deployment to the organization's supported SSO mechanism for production; test logout/session expiry. The development bootstrap already creates the read-only `platform-operators` and scoped `platform-inventory-maintainers` groups the operator UI needs ([GUI authentication](GUI_AUTHENTICATION.md) section 5, package A2); a production SSO connection (a real Entra ID tenant or Active Directory) is still onboarding work, not yet built or verified against a real identity provider.
2. Seed the managed domain hierarchy and custom fields. Provide entry links to the relevant VRF/pool Prefixes and filterable allocation lists.
3. Configure useful table columns where the pinned release supports them: CIDR, role, tenant, state, account, region, resource ID, and observed time. Keep detail views available when a field cannot be displayed as a list column.
4. Provide saved/operator filter examples for RESERVED, QUARANTINED, unbound, and drifted allocations; verify their exact query parameter syntax on the deployed release.
5. Link from allocation `links.inventory` to the corresponding NetBox Prefix view. Never include bearer credentials or require clients to parse the URL.
6. Optionally add a NetBox Custom Link back to an existing authorized platform portal record. Only enable it when that destination exists; the JSON allocation API is not a promised browser portal.

NetBox Custom Links can use object data/custom fields to produce links. A future portal template could use `https://portal.example.com/ipam/allocations/{{ object.cf.platform_allocation_id }}` with conditional display and validated/escaped IDs. Its hostname is illustrative. [NetBox custom links](https://netbox.readthedocs.io/en/stable/customization/custom-links/).

A NetBox administrator can always have more power than an ordinary viewer. Break-glass inventory changes require an audited maintenance procedure that freezes affected allocation domains, performs the correction, reconciles, and explicitly resumes admission.

## 6. Optional visualization milestone

First validate the native hierarchy/filter/detail workflow with operators. Add only views that answer questions the native inventory does not answer clearly:

| View | Data and interpretation |
| --- | --- |
| Capacity by allowed block size | Platform capacity API; free `/20` counts and fragmentation, not raw host utilization |
| Held space by lifecycle | Reserved/active/quarantined/pending counts without double-counting parent and children |
| AWS domain health | Current coverage generation, missing account/region scans, conflicts, stale observations |
| Quarantine queue | Earliest reuse, blocking evidence, owner, and related findings |

A small NetBox plugin may supply these read-only widgets. NetBox documents a dashboard-widget extension mechanism; it does not provide these platform-specific dashboards automatically. [NetBox dashboard widgets](https://netbox.readthedocs.io/en/stable/plugins/development/dashboard-widgets/).

Plugin backend calls must preserve tenant authorization, use short timeouts, cache with a visible freshness timestamp, and degrade to an unavailable state without blocking core NetBox pages. Re-check permissions before serving cached data; a single unrestricted service token must not expose cross-tenant details. A plugin is independently versioned and disabled during upgrades if incompatible. It must not become an allocation writer.

Release U1 when an operator can navigate pool → VPC → subnets, identify AWS ownership/lifecycle, see stale observations, and inspect quarantine without gaining write access. Release U2 only after its permissions, accessibility, cache isolation, and pinned NetBox upgrade tests pass. Neither release blocks the Terraform/API delivery.
