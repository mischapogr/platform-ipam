# Overlapping AWS networks: assessment, migration and prevention

Status: analysis and proposal, 2026-09-20. This records the customer scenario
discussed with the maintainer and a source-code review of the current checkout.
No customer inventory, AWS access, traffic measurements or migration has been
provided or verified. Proposed capabilities and acceptance criteria below are
not implemented contracts or commitments. The [API contract](API_V1.md),
[AWS integration contract](AWS_INTEGRATION.md) and accepted decision records
remain authoritative. A choice that changes their boundaries needs its own ADR
under [decisions/](decisions/).

Related: [licensing and business-model brainstorming](BRAINSTORMING_LICENSING.md),
[organization inventory](AWS_ORGANIZATION_INVENTORY.md),
[onboarding import](ONBOARDING_IMPORT.md),
[adoption runbook](../deploy/runbooks/ADOPTION.md) and [work plan](WORK_PLAN.md).

## 1. Customer problem and intended outcome

The potential customer has approximately 100 AWS accounts. VPC address ranges
overlap between production and non-production accounts of the same product,
and between different products. The customer wants private communication
through Transit Gateways (TGWs) instead of its current internet-facing paths.

The maintainer confirmed on 2026-09-20 that the customer needs **general private
IP connectivity between VPCs**, not only private access to named APIs/services.
The desired outcome is working private communication between the required
workloads, with a process that prevents future conflicting allocations.
PrivateLink alone does not meet this requirement. This confirmation does not
authorize all-to-all access or establish that all approximately 100 accounts
need migration; the actual VPC inventory, communication matrix and security
boundaries remain open.

Replacement VPCs versus unique secondary ranges in existing VPCs is still a
customer/design decision. Current platform-ipam can allocate the replacement
VPCs; managing secondary associations requires new contracts and implementation.

Platform-ipam can supply inventory inputs, controlled replacement allocations
and ongoing allocation policy. It does not change existing workload addresses,
create TGW attachments/routes, migrate data or perform network translation.
An allocation result is not proof that two workloads can communicate.

## 2. AWS constraints and possible remediation paths

TGW attachment and working connectivity are different. AWS permits attachment
operations but does not normally propagate the new VPC's overlapping CIDR
routes. Ordinary routing cannot distinguish two required destinations with the
same address. Separate TGW route tables can maintain isolation; they do not by
themselves resolve that ambiguity when the overlapping networks must communicate.
See [AWS TGW attachment limits](https://docs.aws.amazon.com/vpc/latest/tgw/tgw-vpc-attachments.html).

An existing VPC's primary IPv4 CIDR cannot be removed. Adding a secondary CIDR
does not renumber any existing workload. Readdressing requires new subnets or
VPCs and service-specific migration work; changing an IPAM entry cannot perform
it. See [AWS CIDR management](https://docs.aws.amazon.com/vpc/latest/userguide/add-ipv4-cidr.html).

| Required outcome | Possible AWS approach | Fit with current platform-ipam |
| --- | --- | --- |
| General private communication and a clean final address plan | Create non-overlapping VPCs/subnets, migrate selected workloads, configure TGW and retire conflicting networks | Existing primary-VPC/subnet allocation is the closest fit; migration and routing remain external |
| Retain existing VPCs while moving communicating workloads | Add unique secondary CIDRs, create subnets, move those workloads and explicitly route the unique ranges; leave conflicting ranges unadvertised | Secondary-CIDR allocation and binding are not supported by v1; this requires a new contract |
| Private access to selected services | Publish services through PrivateLink endpoints, which can connect consumers and providers with overlapping ranges | Does not satisfy the confirmed general-connectivity goal alone; may support a selected transitional service flow |
| Bridge selected application traffic during transition | Use unique routable ranges with private NAT and a load balancer over TGW | A separate network design; platform-ipam does not configure NAT or load balancers, and secondary-range management is missing |

AWS describes the alternatives in
[Connecting networks with overlapping IP ranges](https://aws.amazon.com/blogs/networking-and-content-delivery/connecting-networks-with-overlapping-ip-ranges/)
and its [private NAT guidance](https://docs.aws.amazon.com/whitepapers/latest/building-scalable-secure-multi-vpc-network-infrastructure/private-nat-gateway.html).
PrivateLink is service access, not general bidirectional VPC routing. The
private-NAT/load-balancer pattern is also for selected flows, not universal
address-conflict removal. Compare migration effort, recurring infrastructure
cost, protocols, DNS, source-address requirements and operations before choosing.

## 3. What the implementation can do today

These are source-backed capabilities, not evidence of operation across the
customer's 100 accounts. Some older document status paragraphs lag the code;
the component and function references below identify the inspected behavior.

| Capability | Current behavior and limit | Evidence |
| --- | --- | --- |
| Collect organization inventory | Operator script collects accounts and VPC/subnet CIDRs, including secondary VPC ranges, with failure reporting; live organization use remains unverified | [Script](../scripts/aws/org-inventory.sh), [procedure](AWS_ORGANIZATION_INVENTORY.md) |
| Record existing occupancy | `onboard` imports unmanaged space; it does not mint allocations or alter AWS | [Import contract](ONBOARDING_IMPORT.md), [planner](../internal/onboard/plan.go) |
| Handle identical imported CIDRs | One prefix represents the occupied range, with duplicate-source warnings and combined descriptions; this is not a structured collision graph | `planPrefixCandidates` in [planner](../internal/onboard/plan.go), `networkAWSFields` and `mergeNetworkDescription` in [command](../internal/onboardcmd/onboardcmd.go) |
| Allocate clean replacement networks | Chooses candidates outside inventory occupancy, observed primary/secondary CIDRs, exclusions and durable holds; old unmanaged VPCs overlapping each other elsewhere do not alone block every new candidate | `chooseCIDR` in [service](../internal/service/service.go) |
| Apply shared policy | Resolves eligible pool/account/environment/region from authenticated identity and configuration; retries preserve allocation identity | [API contract](API_V1.md), [service](../internal/service/service.go) |
| Adopt an existing network | Adds ownership to a reviewed imported range without changing its CIDR; another overlapping VPC in the domain causes refusal | `reviewedOccupancy` in [service](../internal/service/service.go), [runbook](../deploy/runbooks/ADOPTION.md) |
| Observe and report drift | Reports coverage and per-resource unmanaged occupancy, binding and inventory problems; it does not enumerate all conflicting legacy VPC pairs or prove reachability | [Worker](../internal/service/worker.go), [AWS observer](../internal/cloud/aws.go) |
| Delay reuse | Managed allocations remain held through explicit release, quarantine and complete evidence checks; this is not automatic cleanup of imported unmanaged prefixes | [Lifecycle gates](API_V1.md#7-lifecycle-and-release-gates) |

Important details for interpreting the inventory:

- Keep the original account/region/resource/CIDR records. Collapsing identical
  ranges is appropriate for occupancy but loses structured per-resource
  relationships in the core prefix view. The optional AWS plugin can preserve
  separate resource objects; the allocator does not consume it as an authority.
- Reimporting an already-present unmanaged prefix skips it; this is not a
  continuous refresh of its descriptive provenance.
- Unequal unmanaged overlaps are allowed by import. Some are legitimate VPC
  and child-subnet relationships; others are cross-VPC conflicts. Import success
  is therefore not a TGW-readiness result.
- `unmanaged_occupancy` means that AWS resource is not managed by a platform
  allocation. It can already be present in NetBox; the finding does not by
  itself mean it was omitted from the import.
- A row equal to or containing a configured pool is refused by import. Pool
  containers and legacy occupancy must be planned together; do not bypass this
  guard or remove occupied ranges to make an import pass.

## 4. Proposed migration workflow using the existing allocator

### 4.1 Agree the connectivity and inventory boundary

Record required source/destination applications, environments, protocols and
owners. Distinguish intended connectivity from current routes. Include shared
services, on-premises, partner, VPN and Direct Connect address space. Inventory
the management account and every relevant account/region, not just accounts
currently eligible to request new networks. Unreadable coverage remains unknown.

Use an `overlap_domain` for the address space that must remain unambiguous under
the intended connectivity. An AWS account, product or environment label alone
does not establish isolation. Separate domains are appropriate only for reviewed
isolation; never split domains merely to hide conflicts or failed observations.
Unique addressing also does not authorize communication: route tables and
security policy must still isolate environments where required.

### 4.2 Build and review a conflict assessment

Use original resource records to identify equal or contained CIDRs belonging to
different VPCs. Distinguish those from a subnet contained in its own VPC and from
duplicate observations of the same resource. Evaluate each conflict against the
agreed communication requirements. The complete report described in section 5
is proposed work; current import warnings are only inputs.

Choose which networks stay and which move based on dependencies, stateful
services, maintenance windows and migration cost. Do not assume every account
needs renumbering. The network team must confirm the replacement pool is unused
throughout the relevant connected estate and has capacity for old/new coexistence.

### 4.3 Reserve replacements and migrate one product first

Keep legacy ranges recorded as occupancy. Configure reviewed pools, identities
and coverage, then use the existing API/provider to reserve replacement VPCs
and their subnets. Complete inventory and current AWS observations, eligible
policy, sufficient space and absence of unresolved domain operations are still
required. Use distinct allocation identities for replacement resources.

Illustrative final state, not a proposed customer address plan:

| Workload network | Existing CIDR | Reviewed target |
| --- | --- | --- |
| Product A production | `10.0.0.0/20` | Keep existing VPC |
| Product A non-production | `10.0.0.0/20` | New VPC at `10.64.0.0/20` |
| Product B production | `10.0.0.0/20` | New VPC at `10.64.16.0/20` |

The example addresses require full availability and sizing checks. Keeping
Product A production does not mean it can immediately be adopted: conflicting
old VPCs still visible in its domain must first be resolved. It may remain
unmanaged occupancy during the transition.

Terraform or another separately authorized provisioner creates the new AWS
resources from committed allocations. Application/network owners handle data
migration, DNS, access controls, TGW configuration, cutover and rollback. Test
both permitted and forbidden traffic paths and the required private DNS names.
Do not change an existing Terraform VPC CIDR blindly and treat resource
replacement as an application migration plan.

### 4.4 Retire old occupancy and prevent recurrence

Keep old and new ranges occupied throughout the agreed rollback window. Retire
an old range only after every resource using it has been accounted for; one
deleted duplicate VPC does not make a range unused if another still uses it.
Imported unmanaged prefixes require a reviewed cleanup procedure, not the
managed-allocation DELETE lifecycle. The source-aware cleanup support in
section 5 is missing today. For managed allocations, preserve the existing
release/quarantine gates and permanent allocation-key retirement.

Have approved provisioning modules request ranges through platform-ipam and
review direct AWS provisioning permissions. The runtime observer is read-only:
it cannot prevent independent authorized writers from choosing conflicting
CIDRs. Company pipeline and IAM controls are an external part of the solution;
tags alone cannot prove that a CIDR was validly allocated.

## 5. Missing capabilities and evidence

The priorities below are proposals. Customer choices may remove some work;
they do not authorize changes to existing safety contracts.

| ID | Gap | Why it matters here | Proposed completion evidence |
| --- | --- | --- | --- |
| M1 | Resource-aware overlap assessment | Current prefix collapse and per-resource findings do not identify conflicting VPC pairs, affected owners or communication requirements | Deterministic read-only report from original records; equal and contained cross-VPC conflicts detected; legitimate parent/child containment and duplicate observations excluded; missing coverage explicit |
| M2 | Connectivity context | Collector does not read TGW attachments, route tables, associations or propagations; desired connectivity is not modeled | Start with a reviewed customer matrix; add read-only topology import only if needed. Keep configured routes, intended access and tested reachability distinct |
| M3 | Reviewed migration plan and progress | No old-to-new resource mapping, wave tracking, owner approval or cutover evidence exists | Versioned report linking old resources, proposed targets, later committed allocations, owners, dependencies, blockers, rollback and verification results; drafts never reserve space |
| M4 | Proven observation and storage performance at scale | Each new reservation observes its domain synchronously; account/region scans are sequential; ledger operations use a global lock and rewrite state tables | Representative account/region/resource counts; measured request latency, scan age, retry/throttling behavior and concurrent allocations within agreed limits; incomplete evidence still blocks admission/reuse |
| M5 | Supported deployment and recovery evidence | Live AWS/IAM behavior, Kubernetes rollout, normal provider distribution/install and broader NetBox compatibility remain unverified | One documented supported configuration exercised in a real sandbox, including denied-role/pagination cases, restart, restore and upgrade without losing holds |
| M6 | Complete operator recovery for stuck adoption | A pending adoption can fence its domain. Adapter abandonment work is in progress, but a supported service/CLI workflow is not established | Finish and review work-plan H2a–H2c; demonstrate interrupted and ambiguous cases through the supported command. Follow ADR 0012; no manual ledger deletion shortcut |
| M7 | Secondary VPC CIDR allocation/binding, conditional | Needed only if the chosen design adds managed secondary ranges to existing VPCs | New ADR and API/provider contract for association identity, parent relationships, observation and release; end-to-end evidence. Current primary-CIDR binding must not be misused |
| M8 | Provisioning adoption and bypass controls | Allocator correctness does not stop conflicting resources created outside the workflow | Customer-approved module/pipeline rollout, scoped AWS permissions and exception ownership; demonstrate new standard provisioning uses committed CIDRs and detects unmanaged exceptions |
| M9 | Source-aware refresh and cleanup of imported occupancy | Multiple VPCs may share one imported prefix; skipped reimports do not refresh ownership descriptions; unsafe cleanup could hide remaining occupancy | Preserve contributing resources and freshness; require complete evidence and reviewed removal; demonstrate deleting one duplicate resource keeps its range occupied |

M4 evidence: `Reserve`/`observe` in [service](../internal/service/service.go),
`Observe`/`cell` in [AWS collector](../internal/cloud/aws.go),
`View`/`Update`/`persistState` in [ledger](../internal/storage/postgres.go),
and HTTP timeouts in [entrypoint](../cmd/platform-ipam/main.go).
The current API write timeout is 60 seconds. No measured 100-account latency is
available; account count alone cannot establish performance. If changes are
needed, bounded parallel collection or reusable complete observations are
design candidates, not permission to weaken freshness, coverage or concurrency
guarantees. Record a design before changing the observation authority.

M6 snapshot: on 2026-09-20 the working tree contains an `AbandonAdoption`
inventory port and adapter work; the [work plan](WORK_PLAN.md) tracks the
remaining service and command work. Recheck before scheduling a pilot. The
[abandonment ADR](decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)
applies to uncommitted adoption, not general undo of a completed migration.

### Minimum useful assessment artifact (proposed)

Produce JSON for subsequent tooling and a readable table for owners. Do not
introduce a new API/schema commitment or CLI command name through this document.
Each reported conflict should contain:

- Both resource identities: account, region, VPC, CIDR association and CIDR;
  primary/secondary status; source and observation time.
- The intersecting range and relationship; the enclosing VPC for a subnet.
- Product/environment/owner where supplied, with unknown ownership explicit.
- Intended connectivity group or path, current topology evidence if supplied,
  and whether the impact is confirmed, potential or unknown.
- Coverage gaps, proposed remediation, responsible owner and decision status.

Keep the raw evidence separately from allocator occupancy. A CIDR collision is
a mathematical fact; whether it blocks a required connection needs topology and
intent; actual reachability requires testing. A report with missing coverage
must never claim the estate is conflict-free or ready for TGW migration.

## 6. Confirmed answers and open questions

Confirmed through the maintainer on 2026-09-20: Q1's broad requirement is general
private connectivity between VPCs. Its detailed protocols/directions and the
remaining customer questions are open. The maintainer's business constraints
are also confirmed: EUR 500–1,500/month recurring revenue before costs/tax, and
paid onboarding/scheduled support only with a strict time limit. Exact support
hours, prices and customer commitments are not agreed; see the
[business-model notes](BRAINSTORMING_LICENSING.md).

| ID | Question | Needed from / consequence |
| --- | --- | --- |
| Q1 | General private IP connectivity is confirmed. Which protocols, connection directions and source-address properties must be supported? | Application/network owners; defines what the routed migration must demonstrate; service-only access is insufficient |
| Q2 | Which products/environments must communicate, which must remain isolated, and which shared services need to reach both? | Security/network owners; defines routing and overlap domains without assuming all-to-all access |
| Q3 | How many VPCs, regions, CIDR associations and subnets are involved, and where are the actual conflicts? | Organization inventory; determines remediation volume and scale tests |
| Q4 | Can every relevant account/region be read, including the management account, and who owns denied/unknown coverage? | AWS organization/IAM owner; determines whether any allocation-safety conclusion is possible |
| Q5 | What connected on-premises, partner, VPN, Direct Connect and other-cloud ranges must be preserved? What free space and growth headroom remain? | Network/address-plan owner; bounds replacement pools and temporary coexistence |
| Q6 | Which workloads can move to new VPCs? Which require existing VPC IDs, secondary ranges, stable IPs, original source IPs or special DNS behavior? | Application owners; chooses remediation and whether M7 is necessary |
| Q7 | Who owns stateful-service migration, traffic cutover, rollback and deletion approval? What downtime is acceptable? | Application/change owners; separates IPAM deliverables from migration execution |
| Q8 | Does the customer already operate NetBox, Kubernetes, PostgreSQL, Terraform and AWS native IPAM? Who operates them? | Platform owner; establishes integration fit and whether existing AWS tooling already solves enough |
| Q9 | What request latency, scan freshness, outage behavior, account growth and support response are acceptable? | Platform owner and maintainer; supplies measurable M4/M5 acceptance criteria |
| Q10 | Who can currently create VPCs/subnets outside approved pipelines, and how will exceptions and new accounts enter the process? | IAM/platform owners; determines practical prevention of recurrence |
| Q11 | Is a reviewed connectivity matrix sufficient initially, or must the tool discover TGW topology? | Network owner; keeps M2 bounded and avoids promising a network reachability simulator |
| Q12 | What evidence and retention are required before removing a contributing resource or imported prefix? | Network/audit owners; governs M9 and prevents premature reuse |
| Q13 | What is the smallest paid pilot, who controls its budget, and what specific result would trigger an ongoing support contract? | Customer sponsor; tests willingness to pay and defines commercial scope |
| Q14 | Given the maintainer's strict support-time limit, what are the per-customer and total hour budgets, response windows, incident ownership and supported-version policy? | Customer operator and maintainer; bounds ongoing commitments without assuming a managed-availability service |

## 7. Suggested next packages and acceptance gates

1. **Scope and collect evidence.** Complete the open details of Q1–Q5 and Q8 with the customer. Obtain
   an authorized read-only export, coverage/failure list and a proposed
   connectivity matrix. No allocation or migration is needed for this stage.
2. **Deliver an assessment.** Implement M1 with customer-supplied context for
   M2 and a minimal reviewed M3 report. Check representative source accounts by
   hand. Every relevant resource has an owner or an explicit unresolved owner;
   each reported conflict is traceable to evidence. Keep customer inventory out
   of Git and public examples.
3. **Prove one migration path.** Select a product with named owners and agreed
   rollback. Resolve the relevant recovery/deployment gaps, measure the intended
   domain's scan behavior, reserve clean space and demonstrate permitted and
   forbidden private traffic after the externally executed cutover. Compare
   replacement VPCs with the secondary-range option under Q6. A service-only
   demonstration does not establish the confirmed general-connectivity goal.
4. **Expand only on measured results.** Agree a supported operating envelope,
   repeatable onboarding and upgrade/recovery procedures, provisioning controls
   and occupancy cleanup. Complete M7 only if the chosen path requires it.

The potential commercial offer is a paid inventory/conflict assessment and a
bounded pilot, followed by supported allocation and reconciliation. Application
migration and TGW implementation need explicit scope and ownership. Pricing,
recurring support effort and customer willingness to pay remain hypotheses;
see [the business-model notes](BRAINSTORMING_LICENSING.md).

## 8. Evidence limits

This document records inspected implementation and existing local test reports,
plus official AWS references consulted during the discussion. It does not claim
a new test run, a customer inventory, an AWS deployment, measured scale,
successful application migration or confirmed demand. Documentation/link checks
can validate this artifact's consistency, not any of those runtime outcomes.

## 9. Product directions under consideration

The maintainer supplied two further product proposals on 2026-09-20. They are
recorded here for evaluation, not adopted as a new scope or architecture.

| Direction | Buyer and outcome | Implication for this project |
| --- | --- | --- |
| Simplified NetBox-based IPAM for SMBs/MSPs | Small infrastructure teams discover networks, reconcile documentation and reserve space through a simpler interface | Requires a broader device/host model, heterogeneous connectors, packaging and support, and possibly a new UI/customer-isolation model; demand has not been established |
| AWS overlap assessment and migration planning | A platform/network team identifies what blocks its intended TGW connectivity, assigns owners, reviews replacement ranges and tracks migration evidence | Directly addresses the identified customer's problem and can reuse current AWS inventory/allocation work; the reporting, topology context and migration planner remain proposed |

The recommended experiment is the second direction, starting with a paid,
read-only assessment and one migration pilot. A simpler interface can support
that workflow later. A report generated from authorized exports should not need
a production allocator deployment merely to establish whether the customer has
a problem worth solving. That offline report is proposed work, not an existing
`onboard plan` capability: the current import planner needs a NetBox snapshot.

### 9.1 Existing products already cover much of generic reconciliation

The broad proposal is a usability/packaging hypothesis, not evidence of an empty
feature market. [NetBox Assurance](https://netboxlabs.com/docs/assurance/)
already describes multi-source drift, reviewed remediation and history; its
availability documentation distinguishes it from Community Edition.
[Nautobot SSoT](https://docs.nautobot.com/projects/ssot/en/latest/user/app_use_cases/)
provides source/target synchronization, dry-run diffs and change tracking.
[phpIPAM](https://phpipam.net/documents/features/) already lists subnet scanning,
status checks, free-space views and import/API workflows. These are current
documented capabilities, not hands-on competitive acceptance tests.

A potential differentiator is the completed customer task: identify the address
conflicts that affect a specified future connectivity design, explain the
evidence and unknowns, and produce a reviewed migration plan with accountable
owners. Neither uniqueness nor willingness to pay has been demonstrated.
FortiGate, Cato, Cloudflare, Proxmox and VMware are suggested integration
candidates in the proposal, not verified dependencies of this customer. Start
with reviewed exports/manual dependency records until an actual paid workflow
justifies a specific connector.

### 9.2 A proposed first release with six outcomes

1. **Inventory with coverage:** retain resource identities, all observed VPC
   CIDRs, timestamps and failed account/region observations.
2. **Connectivity and ownership:** accept a reviewed target communication
   matrix, isolation boundaries and accountable owners. Environments label
   policy; they are not evidence of routing isolation by themselves.
3. **Conflict explanation:** identify relevant equal/contained cross-VPC
   ranges, legitimate parent/child relationships, and conflicts introduced by
   joining previously isolated address spaces.
4. **Reviewed target plan:** propose replacements, capacity and old/new
   coexistence; explain assumptions. Draft targets are not reserved addresses.
5. **Migration record:** track owners, dependencies, waves, approvals, actual
   allocations, cutover/rollback evidence and remaining unknowns. Execution is
   owned by the customer's existing infrastructure workflow.
6. **Repeat assessment:** compare later observations and proposed connections
   with the reviewed plan, detecting newly introduced conflicts and coverage
   regressions. This is a candidate continuing service after the initial project.

An initial JSON plus readable report can demonstrate these outcomes before
committing to a standalone dashboard. These are proposed product outcomes,
not six small implementation tickets or six already implemented features.

### 9.3 Evidence and user-interface promises

The proposed "find me a safe subnet" screen must not equate silence with
availability. The absence of an ICMP reply, DNS record, route or DHCP response
can reflect filtering, missing visibility, dormant equipment or reserved-but-unused space.
A route for an aggregate such as `10.0.0.0/8` is also not proof that every address
in that aggregate is occupied. Firewall objects, route references, reservations
and actual interfaces represent different facts; preserve their meanings.

Report "no observed conflict within this scope as of this time" with coverage
and blockers. Do not use a confidence score to bypass incomplete inventory,
authorization, durable holds or release gates. A discovered discrepancy should
produce a reviewed change; accepting observed state indiscriminately as intended
state can erase the drift the product is meant to reveal.

Likewise, qualify "TGW readiness" as **address compatibility for a specified
target design** until additional evidence exists. Show separate counts for
complete/expected account-region observations, required connection pairs with
conflicts/unknowns, approved/completed migration waves, and traffic paths tested.
Any percentage needs a defined denominator and snapshot time; missing accounts
or untested paths must not disappear and improve the score. Conflict-free CIDRs
do not prove routes, security controls, DNS or application connectivity.

### 9.4 AWS IPAM backend and UI choices are separate decisions

AWS already supports organization-wide IPAM monitoring and shared pools through
[Organizations integration](https://docs.aws.amazon.com/vpc/latest/ipam/enable-integ-ipam.html).
AWS also documents [SCPs requiring IPAM allocation](https://docs.aws.amazon.com/vpc/latest/ipam/scp-ipam.html);
sharing a pool alone is not a prohibition on all alternative provisioning.
Using native AWS allocation can be sensible if it fits the customer's operating
model. There are three different product commitments:

| Option | Allocation authority | Work needed before claiming support |
| --- | --- | --- |
| Assessment/planning with execution handed to the customer | Customer's existing reviewed workflow, potentially AWS IPAM | Export/mapping contract, revalidation and execution evidence; the report itself grants no reservation |
| Current platform allocation workflow | Platform chooses CIDRs; its PostgreSQL ledger owns identity/lifecycle; NetBox owns intended inventory; AWS supplies observations | Existing live deployment and scale acceptance gates |
| Native AWS IPAM behind the platform API | AWS selects CIDRs for designated pools under a newly defined platform lifecycle | Dedicated ADR for reservation/provisioning handoff, retries, permanent keys, release/reuse, permissions, provider compatibility and NetBox projection |

Do not run two independent allocators for the same ranges. The
[existing AWS backend boundary](AWS_INTEGRATION.md#8-optional-aws-native-vpc-ipam-integration)
already requires this decision. Replacing allocation mechanics is not necessary
to validate a read-only assessment offer, and the current code should not force
a customer to replace a working native allocation process just to buy a report.

A standalone SMB/MSP frontend would revisit the v1 boundary in
[NetBox integration](NETBOX_INTEGRATION.md). [ADR 0008](decisions/0008-PLATFORM_OWNED_OPERATOR_UI.md)
is proposed; some rationale predates the implemented operator role and findings
CLI. Do not treat those as still missing, or the proposal as an accepted ban on
future UI work. An MSP console additionally needs a demonstrated customer
authorization/isolation model: tenant/VRF labels alone do not supply that security,
and the existing deployment-wide operator role is not a customer-scoped MSP role.

These decisions remain open. They do not change the current allocator, licence,
API or support commitments.
