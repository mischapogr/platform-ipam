# platform-ipam implementation plan

Status: implementation foundation, 2026-09-08. The API/worker, adapters, provider, Compose stack, Helm chart, and contract are now present. The remaining delivery phases describe production acceptance work. “NetBox” is the interpretation of “nexbox” in the request.

## 1. Outcome and boundaries

Provide a company allocation capability: an authorized consumer asks for a network of a given size and receives a stable allocation ID and CIDR. The service chooses an eligible pool, enforces ownership and overlap policy, records the reservation in NetBox, and manages its lifecycle. Terraform provisions AWS resources using the returned CIDR. Reconciliation checks the intended inventory against observed AWS resources.

The consumer API uses `allocation`, `pool`, `scope`, `environment`, and `region`. It does not expose NetBox prefix IDs, VRF IDs, serializers, tokens, or endpoint names as required inputs. An optional opaque inventory link is presentation metadata and cannot be needed for provisioning.

| Component | Owns | Does not own |
| --- | --- | --- |
| platform-ipam | Admission policy, allocation identities, idempotency, lifecycle, release decisions, audit events | Creation/deletion of AWS VPCs, subnets, routes, or workloads |
| NetBox | Authoritative network inventory: CIDRs, hierarchy, address domains, inventory ownership and change history | Company allocation workflow or Terraform state |
| Platform PostgreSQL ledger | Transactional operation identity, durable holds, lifecycle history, pending writes, observations | A separately editable network inventory |
| Terraform AWS provider / other provisioner | Cloud resource lifecycle | Selecting a different CIDR after allocation |
| Reconciler | Cloud observations, validated bindings, drift findings, guarded lifecycle transitions | Unconditional AWS-to-NetBox overwrite or AWS remediation |
| NetBox UI | Human inspection of IPAM inventory and allocation metadata | A second allocation entry point for consumers |

These are distinct authorities. NetBox being the source of truth for inventory does not make custom fields a transactional idempotency database. The ledger is necessary for reliable operations across two independent systems; neither database participates in a distributed transaction.

## 2. Scope and working assumptions

The first usable milestone allocates private IPv4 primary VPC CIDRs in explicitly onboarded AWS accounts/regions. The next extends the same API to subnet allocations with explicit parent allocation IDs. One deployment can serve multiple tenants, but tenancy is derived from authenticated identity. Accounts are targets, not tenants, and do not automatically define separate overlap domains.

| Area | v1 decision | Later extension |
| --- | --- | --- |
| Address family | IPv4, RFC1918 company pools | IPv6 after a separate AWS/BYOIP allocation contract |
| Scope | `vpc`, followed by `subnet` | Individual addresses, on-premises networks, Kubernetes CIDRs |
| Cloud | AWS, one bound resource per allocation | Azure/GCP adapters; multiple associations |
| VPC CIDRs | One primary CIDR per allocation | Secondary CIDR associations with association-level identity |
| Pool management | Reviewed configuration plus controlled NetBox bootstrap | Administrative pool API if needed |
| Reservations | Durable until explicit release; aged reservations alert | Expiring leases only with provisioning fencing |
| Allocation size changes | New allocation and key, then workload migration | No in-place CIDR resizing |
| Cloud correction | Findings and operator action | Explicitly scoped remediation workflows |
| UI | Existing NetBox IPAM views, optional user access | Custom capacity widgets or a thin NetBox plugin |

AWS permits IPv4 VPC blocks from `/16` to `/28`; subnet blocks have their own containment and size rules. The initial policy is deliberately narrower, for example VPC `/20` or `/22`, subnet `/24` or `/26`. Validate the technical bounds as well as company rules. [AWS VPC CIDRs](https://docs.aws.amazon.com/vpc/latest/userguide/vpc-cidr-blocks.html), [AWS subnet CIDRs](https://docs.aws.amazon.com/vpc/latest/userguide/subnet-sizing.html).

Assume all allocation writers for managed address space go through this service. Enforce this in NetBox permissions and in the provisioning operating model. IPAM alone cannot prevent an actor with independent AWS permissions from later creating an arbitrary CIDR. Production onboarding must define how stale pipelines are canceled and how untracked infrastructure is discovered before reuse.

## 3. Repository shape and technology decision

Keep the capability-oriented layout. The implementation uses the following paths (package names are `internal/<component>` where appropriate):

```text
platform-ipam/
├── api/                    # OpenAPI, transport, authentication, error mapping
├── allocation/             # Policy, allocation identities, lifecycle
├── netbox/                 # REST adapter and version compatibility tests
├── reconciliation/         # AWS observations and guarded repair jobs
├── storage/                # Ledger repositories, migrations, transactional outbox
├── config/                 # Validated pool/policy schema and environment loading
├── cmd/                    # API, worker, and optional CLI entry points
├── providers/terraform/    # Independently versioned platformipam provider
├── clients/                # Shared/generated API client packages
├── tests/                  # Contract, integration, recovery, and end-to-end tests
├── deploy/                 # Dev Docker Compose, stage/prod Helm, runbooks
├── docs/
├── examples/
├── Dockerfile
└── README.md
```

Freeze the [API contract](API_V1.md) and failure semantics before selecting the backend framework. Working recommendation: Go for the API/worker and provider, PostgreSQL for the ledger, and a small HTTP client for the NetBox adapter. This allows API client reuse and a single operational toolchain. Python remains viable if it better matches the owning team's expertise; its use would not require moving allocation logic into NetBox.

Implement the Terraform provider in Go using the Terraform Plugin Framework, which HashiCorp recommends for new providers. The backend language is independent of this choice. Record the language/framework decision and exact dependency versions in Phase 0. [Terraform Plugin Framework](https://developer.hashicorp.com/terraform/plugin/framework).

## 4. Allocation model and policy

Resolve a request using `(tenant, scope, environment, region, account_id, address_family)`. For subnets, the parent fixes the tenant, account, region, and address domain. Only eligible pools are considered; a caller cannot bypass policy by guessing a pool ID. Reject zero matches or ambiguous matches. If several pools are intentionally allowed, their priority and overflow behavior must be explicit in configuration.

The [configuration example](../examples/config/pools.yaml) demonstrates a production region pool and subnet policy. It is not a statement about existing company networks.

Policy validation must cover:

1. Tenant membership, allowed account/region/environment, workload ownership, and authorized scope.
2. IPv4 family, allowed prefix length, canonical CIDR representation, configured exclusions, and address-domain membership.
3. Quotas per tenant/environment/account, counting reservations, active allocations, quarantine, and pending holds.
4. Pool CIDR/VRF consistency with NetBox and absence of conflicting unmanaged allocations.
5. Subnet containment in an unreleased VPC allocation; sibling subnets must be disjoint. Parent-child containment is intentional and must pass overlap validation.
6. A VPC parent must not be released while any child is RESERVED or ACTIVE, or while a child operation is unresolved. Quarantined children may coexist with a quarantined parent; parent reclamation waits for every child to be RELEASED.

An `overlap_domain` represents a routing space in which peer allocations must be disjoint. For example, VPCs that will connect through a shared transit network should share a domain across accounts and regions. Mapping each AWS account to an independent VRF by default would hide conflicts that matter to routing.

Capacity reports distinguish allocatable, reserved, active, quarantined, pending, and excluded space. Compute counts of allocatable blocks for allowed sizes; a raw free-address percentage cannot show fragmentation. Avoid double-counting parent VPC space and its subnets in domain totals.

## 5. Ledger and transaction design

| Logical table | Required data and constraints |
| --- | --- |
| `allocations` | Opaque ID, tenant, allocation key, normalized immutable request hash, selected pool, CIDR, parent, lifecycle, revision, policy version, backend reference, timestamps |
| `allocation_keys` | Unique `(tenant_id, allocation_key)` retained as a tombstone after release; never silently reused |
| `operations` | Operation ID/type, allocation ID, exact intended external mutation, candidate CIDR, step, retry/recovery information, terminal result |
| `idempotency_requests` | Unique tenant/method/path/key, normalized request hash, associated allocation/operation/result |
| `holds` | Durable CIDR claims for operations and unreclaimed allocations, domain and parent/peer scope, fencing generation |
| `outbox` | Transactionally recorded NetBox projection updates and reconciliation work; workers may deliver more than once |
| `bindings` | Expected cloud target, verified resource ID and observed CIDR, binding status and timestamps |
| `observations` | Account/region scan coverage, coverage generation, pagination completion, errors, inventory findings, last successful observation |
| `audit_events` | Append-only actor, request ID, transition, reason, revisions, policy version, cloud evidence references |

Use short PostgreSQL transactions for admission and transition decisions. Serialize candidate planning per overlap domain initially; correctness and explicit contention are preferable to premature distributed partitioning. Persist a domain operation barrier before an external inventory mutation. Other allocators must honor unresolved barriers even after a process restarts or a lock is lost. Lock ordering is domain, parent, then allocation.

Use database uniqueness for identity and exact duplicate protection. Check CIDR intersections among peer holds under the domain lock. Do not apply a flat exclusion rule to every CIDR: it would reject valid parent/child allocations. A later database overlap constraint must preserve the explicit hierarchy and be integration-tested.

### Reservation algorithm

1. Authenticate, authorize, normalize the request, and look up its permanent allocation key. Return an existing matching allocation or operation before attempting new selection. Changed immutable input is a conflict.
2. Under the domain barrier, read a fresh NetBox inventory for the relevant domain/pool and verify the pinned pool configuration. Include relevant ancestors, descendants, unmanaged prefixes, address ranges, and individual addresses that policy treats as occupied; do not infer free space only from platform-tagged objects. Fetch all pages. Require complete trusted AWS observations for the current coverage generation, at most ten minutes old, and no unresolved domain suspension. Incomplete/inaccessible inventory or unexplained AWS occupancy stops admission. A request may enqueue a fresh scan, but must not select while waiting.
3. Select a deterministic aligned CIDR from eligible free space, excluding ledger holds and relevant observed AWS occupancy. Explicitly allow the declared containing VPC when selecting a subnet; peer subnet occupancy remains excluded. Persist that exact candidate, request identity, operation, and durable hold before any NetBox create. Commit the database transaction; do not hold a SQL transaction across HTTP calls.
4. Create that exact prefix through the adapter with the allocation ID and operation marker. Use NetBox uniqueness enforcement in the managed VRF/global table. A direct writer outside this protocol is an access-control defect, not another supported allocator.
5. Verify the returned prefix, domain, ownership marker, and CIDR. Atomically publish the allocation as RESERVED and record the event. Clear the domain operation barrier but retain its CIDR hold. Only now return a usable CIDR.
6. If the response is lost, recover by allocation marker and exact CIDR/domain. One matching object resumes the operation. A conflicting or duplicate object freezes that domain for investigation. Retrying a create uses the persisted exact candidate and identity, never a newly selected CIDR.
7. On definite rejection with no committed prefix, terminally fail the operation and release its planning hold. On uncertain outcome, retain the barrier/hold and return a pending operation. An empty lookup immediately after a timeout does not prove that the original write cannot still complete.

NetBox offers an available-prefix operation, but a timed-out retry of “choose next free” must not allocate a second range. The proposed v1 adapter uses an explicit persisted candidate plus recovery. Its use of NetBox availability reads and exact-create uniqueness must be proven against the selected release. Upstream code is implementation evidence, not an end-to-end transaction guarantee. [NetBox IPAM API source](https://raw.githubusercontent.com/netbox-community/netbox/main/netbox/ipam/api/views.py).

Lifecycle projection updates and release deletion use the same durable operation discipline. Release removes the managed NetBox prefix only after the reuse gates pass; the ledger hold remains until deletion is confirmed. Delete by stored backend identity, validate its marker/CIDR, and never delete an arbitrary replacement found at the same address.

## 6. Lifecycle and Terraform behavior

The space lifecycle remains `AVAILABLE → RESERVED → ACTIVE → QUARANTINED → AVAILABLE`. AVAILABLE describes free space. An individual allocation record becomes RELEASED after reclamation and remains queryable for audit; its ID never represents a new owner.

Reservations do not automatically expire. A Terraform apply can reserve a range, create AWS infrastructure, and fail before recording all state or before the worker sees the resource. A timer alone is insufficient evidence to recycle that range. Aged reservations generate findings and can be released explicitly after investigation.

Terraform first creates `platformipam_allocation`, then an `aws_vpc` referencing its CIDR. The cloud resource carries the allocation ID tag. The worker verifies the account, region, resource type, CIDR, and tag before marking ACTIVE. Consumers may also submit a binding candidate through the API to accelerate verification. The API does not trust a client's `ACTIVE` assertion.

On ordinary Terraform destruction, references make AWS deletion precede allocation deletion. Provider Delete requests quarantine; Terraform may forget the allocation once that intent and its hold are durable. It does not wait days for address reuse. Quarantine still blocks allocation if cloud deletion failed, tags disappeared, observations are incomplete, or NetBox is unavailable. No separate binding Terraform resource is required, avoiding a destroy dependency cycle.

See [API lifecycle and release gates](API_V1.md) and [provider lifecycle](CLIENTS.md) for normative details.

## 7. Reconciliation

Run API and worker as separate processes from the same release. Begin with a full scan every five minutes plus per-allocation verification jobs. These are proposed operational settings, to be tuned from measurements. Events/webhooks may accelerate scans later; correctness must survive missing events.

Maintain an explicit account/region coverage matrix per overlap domain. Assume only approved read roles. Enumerate VPCs, all associated CIDRs, and subnets with pagination; compare with allocation records and NetBox inventory. Observe unmanaged CIDRs too. A tag-filtered list is useful for discovery but cannot prove absence or detect untagged overlaps. [AWS DescribeVpcs](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_DescribeVpcs.html).

Version the coverage matrix with a `coverage_generation` recorded on every scan. Changing membership invalidates qualifying absence streaks and requires complete fresh scans before new admission/reclamation. Removing an account/region requires audited evidence that its remaining occupancy cannot affect the domain; removing an inaccessible target to manufacture complete coverage is forbidden. The [AWS integration plan](AWS_INTEGRATION.md) specifies roles and onboarding.

| Finding | Required action |
| --- | --- |
| RESERVED and one matching cloud resource | Verify identity/CIDR/target; bind and transition ACTIVE |
| RESERVED with no resource | Retain reservation; alert after configured age |
| Multiple resources claim the same allocation | Report conflict; inhibit reuse and new conflicting allocations |
| ACTIVE resource absent in one observation | Record suspected drift; retain allocation |
| Cloud CIDR or parent differs from allocation | Critical finding; no automatic CIDR change |
| Managed NetBox prefix missing/modified | Block affected admission/release; operator repair using ledger evidence |
| NetBox projection metadata lags a committed transition | Replay owned metadata update after verifying object identity; preserve unrelated metadata |
| AWS resource without allocation or with missing tags | Report unmanaged occupancy; include in conflict/reuse checks |
| Account access denied, scan incomplete, or API throttled | Mark coverage UNKNOWN, not absent; inhibit affected-domain new admission and reuse |
| QUARANTINED, complete evidence of absence, time gate satisfied | Reclaim through a durable NetBox deletion operation |

AWS APIs are eventually consistent. Repeat complete observations with a spacing interval, use bounded backoff, and reset the absence streak when data becomes stale or a scan fails. [AWS eventual consistency](https://docs.aws.amazon.com/ec2/latest/devguide/eventual-consistency.html).

Reconciliation does not authorize arbitrary cloud creation in managed pools. Onboarding unmanaged infrastructure requires an explicit inventory/import workflow. Pause the affected domain when unexplained occupancy could invalidate allocation selection.

## 8. Deployment, security, and operations

Development uses Docker Compose with the API, worker, platform PostgreSQL, and a local NetBox stack. Stage and prod use Kubernetes deployments packaged as versioned Helm charts. The [deployment environment contract](DEPLOYMENT.md) defines service topology, configuration isolation, migrations, and promotion gates.

Deploy the platform API and worker with an independent Helm release in both stage and prod. Reuse an existing supported NetBox where available; otherwise deploy NetBox separately with its own upgrade cadence. An upstream NetBox Helm chart exists, but its exact version and values need validation in Phase 0. [NetBox Helm chart](https://github.com/netbox-community/netbox-chart).

Production topology:

- Internal HTTPS ingress for API clients, with short-lived workload identity credentials and explicit audience/tenant/account authorization. A local development token mode must be disabled in production.
- Two API replicas and worker concurrency protected by the shared ledger, operation barriers, and fencing. Scaling pods alone does not provide allocation correctness.
- Managed PostgreSQL for the platform ledger; separate database/user and migration ownership from NetBox. NetBox retains its own required PostgreSQL/Redis dependencies. No direct access to NetBox tables.
- Kubernetes workload identity for AWS read roles, a dedicated scoped NetBox service token, and external secret references. Consumer provider credentials are platform API credentials.
- Network policies permitting only required DB/NetBox/AWS/identity traffic. Separate browser access to NetBox from machine API access.
- Startup schema/version checks, `/livez` for process health, `/readyz` for ledger/schema readiness, and dependency health metrics. NetBox outages block reservation commits and final reclamation; healthy-ledger reads, metadata updates, and durable quarantine requests remain available with pending inventory projection. Avoid a liveness restart loop.
- A single migration job per release, backward-compatible schema expansion before binary rollout, then later cleanup migrations. Pin image digests and dependency versions; ship an SBOM and signed provider artifacts.

Structured logs carry request/operation/allocation IDs and caller identity without bearer tokens. Metrics cover request failures/latency, duplicate replay, quota/capacity, pool fragmentation, pending operations, quarantine age, NetBox latency, worker lag, and incomplete AWS coverage. Do not put allocation IDs or keys in metric labels.

Proposed initial targets are read latency p95 below 300 ms, allocation p95 below 3 seconds while dependencies are healthy, API availability 99.9%, and complete reconciliation within 10 minutes. These are acceptance targets, not measured promises; Phase 0 must agree expected allocation count, peak write rate, domain count, and account/region scale.

Back up both inventory and ledger, policy versions, and necessary secret recovery material. Proposed recovery targets: RPO 15 minutes, RTO 4 hours, confirmed by restore drills. After restoring either database, disable allocation and reclamation until ledger holds, NetBox inventory, and AWS scans agree. An old NetBox backup must never turn occupied addresses into allocatable space.

Required runbooks: lost create response, orphan reservation, missing NetBox prefix, cloud drift, account access loss, pool exhaustion, frozen domain recovery, token rotation, provider state recovery, safe release, and restore. Operator interventions require an audited reason and evidence; there is no consumer `force_release` switch.

## 9. Delivery phases and acceptance gates

Owner labels describe responsibilities to assign, not existing teams or staffing commitments. Estimates are indicative engineering effort for one experienced implementer plus normal review; they exclude infrastructure procurement and organizational waiting time.

| Phase | Deliverables | Exit evidence | Indicative effort |
| --- | --- | --- | --- |
| 0 — Contract and compatibility | OpenAPI from this API document; lifecycle/authority ADRs; identity mapping; pool policy schema; exact NetBox/DB/Terraform matrix; framework decision; failure prototypes | API examples validate; parallel allocation and lost-response prototypes pass on selected NetBox; owners agree release/coverage assumptions | 3–5 days |
| 1 — Reservation service | Development Docker Compose stack, ledger migrations, auth, pool loader, NetBox adapter, durable operations, VPC create/read/list, idempotency, audit | Compose startup/seed; real PostgreSQL/NetBox tests show one allocation per key and disjoint peer CIDRs across replicas and restarts; unauthorized requests cannot reserve | 5–8 days |
| 2 — Safe cloud lifecycle | AWS read adapter, binding verification, quarantine/reclamation, findings, release runbooks | AWS sandbox create/delete; incomplete scans cannot free space; cloud use during quarantine blocks reuse; restore rehearsal | 4–7 days |
| 3 — Terraform MVP | Versioned provider, VPC example/module, import, timeouts, private registry/mirror packaging, CI guard rejecting same-key replacements | Plan has zero allocation writes; repeated apply is stable; interrupted/tainted creates recover without releasing their identity; normal destroy quarantines after AWS deletion | 5–8 days |
| 4 — Subnets and client coverage | Parent-aware subnet policy, Terraform subnet examples, REST/Python documentation, supported CLI if needed | Concurrent sibling allocation disjointness; correct parent ownership; child-before-parent destruction and reclamation | 4–6 days |
| 5 — Production rollout | Stage/prod Helm deployments, identity integration, alerts, backups, load test, staged pool onboarding | Stage acceptance then exact-artifact prod promotion; canary create/import/destroy; operational targets met; restore and rollback drills; unresolved risks recorded | 4–7 days |
| U1 — Optional native NetBox UI | SSO/read roles, managed IPAM views, custom fields/links, role walkthrough | Operator can locate owner, lifecycle, parent, and drift without write access; API works with UI access disabled | 1–3 days |
| U2 — Optional visualization | Capacity-by-size, fragmentation, quarantine, and drift widgets/plugin | Permissions, accessibility, upgrade compatibility, and UI failure isolation verified | 3–5 days after scope selection |

Phases 1–3 produce the VPC consumer MVP. Production use requires Phase 5 and its safety gates. U1 can start after Phase 1; U2 follows demonstrated operator needs. Neither UI phase is a prerequisite for the API/provider release. Phase 4 may follow the VPC production rollout if subnet allocation is not required by the first tenant.

## 10. Test and rollout matrix

| Level | Essential cases |
| --- | --- |
| Policy/unit | CIDR alignment, containment, sibling overlap, pool exclusions, ambiguous pool selection, cross-account routing domains, quota contention, invalid parent |
| API contract | Unknown fields, stable error codes, auth scoping, pagination, idempotency hash conflicts, async operation recovery, released-key conflict, revision preconditions |
| PostgreSQL + NetBox integration | Many same-key requests produce one prefix; mixed-size different-key requests never overlap; concurrent parent/child release; lost DB connection and worker takeover |
| Failure injection | Crash before/after NetBox commit, delayed external create, duplicate outbox delivery, NetBox delete timeout, manually modified backend object, one restored stale database |
| AWS adapter | Paged VPC/subnet inventory, associated CIDRs, invalid credentials, throttling, untagged use, tag forgery, resource moved/mismatched, eventual consistency |
| Terraform acceptance | Fresh plan, create, no-op second apply, unknown CIDR propagation, import, lost create response, partial Create error/taint recovery, saved-plan rejection of same-key replacement, ForceNew/key diagnostics, destruction order, provider cancellation, API unavailable during Read |
| UI | Tenant-constrained reads, absence of consumer write grants, custom-field filtering, correct utilization labeling, stale observations visible |
| End to end | Reserve VPC/subnets, apply AWS, activate, destroy, quarantine, repeated complete absence checks, reclaim, allocate space again under a new identity |

For production onboarding, inventory existing managed and unmanaged space first, resolve overlaps, seed pools and references, start the reconciler in report mode, then enable one canary tenant/pool. Expand only after the complete lifecycle succeeds. Import existing Terraform allocations by platform allocation ID after an operator has registered and verified their inventory; do not have import allocate new space.

Rollback disables new allocations/reclamation and returns to a compatible API/provider release while preserving all holds. It does not delete NetBox prefixes or rewind allocation identities. Provider versions and the API's supported compatibility window must remain available through the rollback period.

## 11. Decisions to resolve during Phase 0

| Decision | Working default | Why it matters |
| --- | --- | --- |
| Existing NetBox and release | Reuse if supported; otherwise independent installation | Adapter/auth/UI compatibility and operational ownership |
| Backend language | Go after contract review | Maintenance skills and client reuse |
| Identity and account mapping | OIDC/workload identity with server-side tenant mapping | Prevent cross-tenant or unauthorized account allocation |
| Routing domains and pool CIDRs | Centrally reviewed, shared-domain overlap checks | Avoid conflicts across connected accounts and regions |
| Reservation/reuse policy | No automatic reservation expiry; 7-day quarantine and complete absence evidence | Interrupted applies and late provisioning |
| Reuse coverage | Enumerate all authorized accounts/regions per domain | Absence in one account does not prove global absence |
| Provider distribution | Private registry or approved mirror; address still unassigned | Client install, updates, signing, and rollback |
| Scale and retention | Measure in prototype; permanent compact key tombstones | Lock contention, ledger growth, audit costs |
| Native UI access | Optional SSO read-only operator views | Visualization without bypassing allocation policy |

These decisions are implementation inputs. No deployment, registry publication, selected NetBox version, or production IP pool is implied by this plan.
