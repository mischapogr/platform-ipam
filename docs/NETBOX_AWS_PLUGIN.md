# NetBox AWS Plugin Evaluation

**Status:** Desk evaluation, 2026-09-18. Nothing was installed or run. This is a read-only research document comparing two candidate NetBox AWS plugins against the pinned NetBox version v4.6.7 (image `netboxcommunity/netbox:v4.6.7-5.0.2`).

## Executive Summary

**Recommendation:** Adopt **netbox-aws-vpc-plugin** (v0.1.0).

This plugin explicitly supports NetBox 4.5+, including 4.6.7, has active recent development (Sept 2026), and ships with the three core AWS models (Account, VPC, Subnet) needed for the platform-ipam integration plan. The alternative, netbox-aws-resources-plugin, has no documented NetBox version constraint, shows compatibility only with 4.0, and while more feature-rich, has had no commits since December 2025 and carries license ambiguity. Both plugins correctly model VPC and Subnet CIDRs as ForeignKeys to NetBox IPAM Prefix objects, respecting the "plugin as view" principle that imported networks must exist as prefixes in the domain VRF.

**Critical finding:** At least one candidate (netbox-aws-vpc-plugin) supports NetBox 4.6.7.

---

## Candidate Comparison

| Aspect | netbox-aws-vpc-plugin | netbox-aws-resources-plugin |
|--------|------------------------|------------------------------|
| **Latest Version** | 0.1.0 (Jan 20, 2025) | 0.1.0 (pyproject.toml); 4.0.0-alpha1 (__init__.py) |
| **NetBox Version Support** | 4.5+ (min_version: "4.5.0") | not established; README states 4.0 only |
| **4.6.7 Compatibility** | ✓ Supported | ✗ Not documented |
| **License** | Apache-2.0 (pyproject.toml line 16) | not established (LICENSE empty) |
| **Python Version** | ≥3.12.0 (pyproject.toml line 29) | ≥3.10.0 (pyproject.toml line 27) |
| **Models** | AWSAccount, AWSVPC, AWSSubnet | AWSAccount, AWSVPC, AWSSubnet, AWSLoadBalancer, AWSLoadBalancerListener, AWSLoadBalancerListenerRule, AWSTargetGroup, AWSEC2Instance, AWSRDSInstance, ECSCluster, ECSTaskDefinition, ECSService |
| **VPC CIDR Handling** | ForeignKey to `ipam.Prefix` (vpc_cidr, nullable) | OneToOneField to `ipam.Prefix` (cidr_block, required) |
| **Subnet CIDR Handling** | ForeignKey to `ipam.Prefix` (subnet_cidr, nullable) | OneToOneField to `ipam.Prefix` (cidr_block, required) |
| **AWS Account Model** | account_id (12 digits, unique); arn; name; description; tenant FK; status | account_id (12 digits, unique); name; tenant FK; parent_account self-FK (org hierarchy) |
| **REST API Endpoints** | `/api/plugins/aws-vpc/aws-{vpcs,subnets,accounts}/` | `/api/plugins/netbox_aws_resources_plugin/aws-{accounts,vpcs,subnets,load-balancers,load-balancer-listeners,load-balancer-listener-rules,target-groups,ec2-instances,rds-instances}/`, `/ecs-{clusters,task-definitions,services}/` |
| **AWS Sync / Import Logic** | Modeling-only; no AWS SDK | Modeling-only; scripts fetch static AWS region/instance data |
| **Migrations Touch Core Tables** | No (plugin tables only; FKs to ipam.Prefix) | No (plugin tables only; FKs to ipam.Prefix, tenancy.Tenant) |
| **Recent Activity** | Latest commit Sept 13, 2026 | Latest commit Dec 31, 2025 (8.5 months ago) |
| **Distribution** | PyPI (netbox-aws-vpc-plugin) | GitHub only; not on PyPI |

### Models and Key Fields

**AWSAccount:**

netbox-aws-vpc-plugin (models/aws_account.py):
- account_id: CharField(max_length=12, unique=True)
- arn: CharField(max_length=2000, blank=True)
- name: CharField(max_length=50, blank=True)
- description: CharField(max_length=500, blank=True)
- tenant: ForeignKey(Tenant, blank=True, null=True)
- status: CharField(choices=AWSAccountStatusChoices, default=active)
- comments: TextField(blank=True)

netbox-aws-resources-plugin (models.py lines 12–50):
- account_id: CharField(max_length=12, unique=True, blank=True, null=True)
- name: CharField(max_length=100)
- tenant: ForeignKey(Tenant, blank=True, null=True)
- parent_account: ForeignKey(self, null=True, blank=True) — for parent/child org relationships

**AWSVPC:**

netbox-aws-vpc-plugin (models/aws_vpc.py lines 15–78):
- vpc_id: CharField(max_length=21, unique=True)
- name: CharField(max_length=256, blank=True)
- arn: CharField(max_length=2000, blank=True)
- vpc_cidr: ForeignKey(Prefix, blank=True, null=True, limit_choices_to=IPV4_PREFIXES) — **links to Prefix**
- vpc_secondary_ipv4_cidrs: ManyToManyField(Prefix) — secondary CIDR blocks
- vpc_ipv6_cidrs: ManyToManyField(Prefix) — IPv6 blocks
- owner_account: ForeignKey(AWSAccount, blank=True, null=True)
- region: ForeignKey(Region, blank=True, null=True)
- status: CharField(choices=AWSVPCStatusChoices, default=active)
- comments: TextField(blank=True)

netbox-aws-resources-plugin (models.py lines 165–217):
- vpc_id: CharField(max_length=50, unique=True, blank=True, null=True)
- name: CharField(max_length=255)
- arn: CharField(max_length=2000, blank=True)
- cidr_block: OneToOneField(Prefix, required, validate no parent prefix) — **links to Prefix**
- aws_account: ForeignKey(AWSAccount, required)
- region: CharField(choices=AWS_REGION_CHOICES)
- state: CharField(choices=AWS_VPC_STATE_CHOICES, default=available)
- is_default: BooleanField(default=False)

**AWSSubnet:**

netbox-aws-vpc-plugin (models/aws_subnet.py lines 16–81):
- subnet_id: CharField(max_length=47, unique=True)
- name: CharField(max_length=256, blank=True)
- arn: CharField(max_length=2000, blank=True)
- subnet_cidr: ForeignKey(Prefix, blank=True, null=True) — **links to Prefix**
- subnet_ipv6_cidr: ForeignKey(Prefix, blank=True, null=True) — IPv6 block
- vpc: ForeignKey(AWSVPC, blank=True, null=True, on_delete=CASCADE)
- owner_account: ForeignKey(AWSAccount, blank=True, null=True)
- region: ForeignKey(Region, blank=True, null=True)
- status: CharField(choices=AWSSubnetStatusChoices, default=active)
- comments: TextField(blank=True)

netbox-aws-resources-plugin (models.py lines 219–271):
- subnet_id: CharField(max_length=47, unique=True)
- name: CharField(max_length=255)
- arn: CharField(max_length=2000, blank=True)
- cidr_block: OneToOneField(Prefix, required, validate child of parent VPC prefix) — **links to Prefix**
- subnet_ipv6_cidr: ForeignKey(Prefix, blank=True, null=True)
- aws_vpc: ForeignKey(AWSVPC, required)
- owner_account: ForeignKey(AWSAccount, blank=True, null=True)
- region: CharField(choices=AWS_REGION_CHOICES)
- state: CharField(choices=AWS_SUBNET_STATE_CHOICES, default=available)

---

## Fit with Platform-IPAM Design Principle

Both plugins correctly implement the core design principle: **the plugin is a VIEW, never the allocator's source of truth** (ONBOARDING_IMPORT.md section 1 and ADR 0007).

**Verification:**

- **VPC and Subnet CIDRs reference Prefix objects:** Both plugins use ForeignKey (or OneToOneField, which is a FK variant) to `ipam.Prefix`, not duplicate CharField fields. This ensures a single source of truth in NetBox IPAM.
  - netbox-aws-vpc-plugin: vpc_cidr, vpc_secondary_ipv4_cidrs (FK/M2M to Prefix; models/aws_vpc.py lines 27–51)
  - netbox-aws-resources-plugin: cidr_block (OneToOneField to Prefix; models.py lines 182–189, 233–236)

- **Imported networks live in the domain VRF as Prefix objects:** The onboarding process (package C4) writes prefixes to the domain's VRF. Plugin objects link to these prefixes; they do not replace or shadow them. `Snapshot` (internal/netbox/client.go) queries only IPAM prefixes, addresses, and ranges in the domain VRF, never the plugin tables. `chooseCIDR` skips overlapping prefixes.

- **Plugin objects cannot block allocation if missing:** A VPC that exists only as a plugin object (without a linked Prefix) cannot prevent allocation because the allocator never queries the plugin. If plugin objects are deleted but prefixes remain, allocation is unaffected. This satisfies the N3 e2e test: "reservation still skips the imported space **with the plugin objects deleted**" (WORK_PLAN.md line 138).

- **Migrations do not touch core tables:** Both plugins create only plugin tables and add ForeignKeys to existing NetBox tables (Prefix, Tenant, Region, VirtualMachine). No migrations alter IPAM, tenancy, or virtualization schema. The plugin can be uninstalled without data loss.

Both plugins satisfy the design principle.

---

## Open Risks and Considerations

### netbox-aws-vpc-plugin

1. **Scope:** Only three core models (Account, VPC, Subnet). Load Balancers, EC2, RDS, ECS not included. If future requirements expand to compute/network resources, a second tool or plugin migration would be needed. *Mitigation:* Onboarding scope is networks only; this scope is appropriate for the current plan (packages C1–C8, N1–N3).

2. **Python 3.12+ Requirement:** Requires Python ≥3.12.0 (pyproject.toml line 29). The pinned NetBox image must carry Python 3.12 or later. Verify via `docker run netboxcommunity/netbox:v4.6.7-5.0.2 python --version` before N2.

3. **API Serializers:** Secondary and IPv6 CIDRs are ManyToMany, not OneToOne. The serializers (api/serializers.py) handle this; no apparent issues observed, but test coverage is needed in N2–N3 e2e tests.

### netbox-aws-resources-plugin

1. **No NetBox 4.6.7 Support Declared:** PluginConfig has no min_version/max_version (lines 11–21). README states compatibility with NetBox 4.0 only (4.0.0-alpha1 in compat table). Installing into 4.6.7 is **not verified to work.** This is a go/no-go blocker for adoption. Testing would require a development NetBox 4.6.7 instance and a trial installation.

2. **License Missing:** LICENSE file is empty; pyproject.toml has no license field. This is a compliance gap for open-source use and distribution. GitHub may infer a license from the repo, but intent is ambiguous.

3. **Maintenance Gap:** Last commit December 31, 2025 (8.5 months ago). No activity in response to NetBox 4.5+ releases or security updates. For production, a more actively maintained plugin is preferred.

4. **Not on PyPI:** Requires `pip install git+https://github.com/zeddD1abl0/netbox-aws-resources-plugin`. This complicates dependency pinning, reproducibility, and audit. A Dockerfile must pin the commit SHA, adding fragility if the repo is deleted or forked.

5. **Complexity:** Seven extra models (LB, LB Listener, LB Listener Rule, Target Group, EC2, RDS, ECS) that are not used by the onboarding plan. They represent code maintenance burden without near-term payoff.

### Shared Risks

1. **Ecosystem Coupling:** Both plugins depend on NetBox internals (PluginConfig, NetBoxModel, API routers, serializers). A breaking change in NetBox 5.0 (e.g., model metaclass, URL routing, or API versioning) could require immediate plugin updates. The work plan (N4) acknowledges this: "the upgrade coupling between a third-party plugin and the pinned NetBox release."

2. **No AWS Sync:** Neither plugin imports or syncs AWS infrastructure state. Operators must still gather networks via external tools (org-inventory.sh in track B, console, CLI, or APIs). The plugin is a schema and REST interface, not an automation boundary.

3. **Custom Field Dependencies:** Both plugins reference NetBox custom fields (e.g., platform_import_batch, platform_aws_account_id) created by C4 (seed-netbox.py). If those fields are removed or renamed, both plugins remain functional but lose semantic meaning. No hard dependency, but a drift risk.

---

## Recommendation

**Adopt netbox-aws-vpc-plugin v0.1.0.**

**Rationale:**

1. **Explicit NetBox 4.6.7 Support:** PluginConfig min_version="4.5.0" (line 19 of __init__.py) covers 4.6.7. Only this candidate documents compatibility with the pinned version.

2. **Active Development:** Latest commit September 13, 2026 (5 days old). Regular maintenance and dependency updates signal responsiveness to NetBox ecosystem changes and security issues.

3. **Published on PyPI:** Simpler dependency management in Dockerfile and requirements files. No Git SHA pinning; version pinning is atomic and auditable.

4. **Scope Alignment:** Three models (Account, VPC, Subnet) match the onboarding import design. Extra models in the alternative are feature creep for the current plan.

5. **Correct IPAM Integration:** ForeignKey to Prefix (with optional M2M for secondary/IPv6) correctly embeds the "plugin as view" principle. Prefix is the authority; plugin objects are derived.

6. **Verified Design Pattern:** The use of ForeignKey to Prefix and the absence of on_disk CIDR storage confirms that plugin objects link to, not shadow, IPAM prefixes.

**Not Recommended: netbox-aws-resources-plugin**

- No documented support for NetBox 4.6.7 (compatibility table lists 4.0 only).
- Empty LICENSE file; compliance risk for open-source projects.
- No commits since December 2025 (8.5 months); maintenance risk.
- Not on PyPI; adds build complexity and reproducibility burden.
- More models than needed for the current scope.

The alternative may become preferable if future requirements include compute or network infrastructure modeling, but the onboarding plan does not justify that scope today.

---

## Implementation Notes for N2–N4

- **N2 (Plugin-enabled image):** Pin netbox-aws-vpc-plugin to v0.1.0 in deploy/compose/netbox/Dockerfile. Install via `pip install netbox-aws-vpc-plugin==0.1.0` alongside the base image. Enable in deploy/compose/netbox/plugins.py. Run migrations at startup with `./manage.py migrate`.

- **N3 (Import into plugin):** Add `onboard apply --aws-objects` flag to cmd/platform-ipam/main.go C5 mode. Create AWSAccount, AWSVPC, AWSSubnet objects linked to the prefixes written by C4. API endpoints `/api/plugins/aws-vpc/{aws-accounts,aws-vpcs,aws-subnets}/` will serve them. Test: import networks.csv, verify plugin objects in API, delete plugin objects, confirm allocation still skips prefixes.

- **N4 (Decision record):** Document version coupling. NetBox 5.0 may require `netbox-aws-vpc-plugin` version bump. Production must pin both image digest *and* plugin version. Test them together before release. Define exit path: if plugin maintenance ceases, migrate plugin data (Account IDs, VPC IDs) to configuration tables (not to be source of truth, but for audit trail).

---

## Reviewer verification (2026-09-18)

This evaluation was produced by a small model, so its load-bearing claims were re-checked against the plugin's source, PyPI and the GitHub API. They hold: `min_version = "4.5.0"` with **no** `max_version` (`netbox_aws_vpc_plugin/__init__.py:19`); release 0.1.0 on 2026-01-20, Apache-2.0, Python >= 3.12; last commit 2026-09-13 (a dependency bump); `AWSVPC.vpc_cidr` and `AWSSubnet.subnet_cidr` are `ForeignKey`s to `ipam.Prefix`, and secondary CIDRs are a `ManyToManyField` to `ipam.Prefix`; the other plugin's last commit is 2025-12-31.

Two qualifications to the text above:

- "Supports 4.6.7" means **declared compatible by metadata**. Nobody has installed it into the pinned image. With no `max_version` the plugin will also load into a NetBox it was never tested with, so package N2's first step is an actual install and migration run, and that is the real go/no-go.
- The activity signal is mostly automated dependency bumps. One maintainer, version 0.1.0: treat it as a young single-maintainer project, which is what package N4's exit-path question is for.

Two consequences for package N3 that follow from the models:

- `vpc_cidr` is a plain `ForeignKey`, not one-to-one, so **several VPCs may point at one prefix**. That fits the import exactly: ADR 0007 collapses the same CIDR used in two accounts into a single prefix, because a duplicate CIDR in one VRF takes the domain's reservations down, and the collapsed prefix loses who owns what. Two `AWSVPC` objects, each with its own `owner_account`, pointing at that one prefix restore the per-account picture. The alternative plugin uses `OneToOneField` and could not represent this.
- `region` is a `ForeignKey` to `dcim.Region`, and `AWSAccount.tenant` to `tenancy.Tenant`. N3 must create or look up NetBox regions for AWS region names; it should leave `tenant` empty rather than invent tenants.

## Installed and verified (package N2, 2026-09-18)

`netbox-aws-vpc-plugin==0.1.0` was built into the pinned image and started in an isolated Compose project. This replaces "declared compatible" with an actual result: **go**.

| Check | Result |
| --- | --- |
| Install | `uv pip install` into the image's existing virtualenv; the image has no `pip` binary |
| Migrations | `0001_initial` through `0005_…` applied automatically at first start; `showmigrations` shows all five applied |
| Core tables | `sqlmigrate` on all five: every `CREATE`/`ALTER` targets `netbox_aws_vpc_plugin_*`. `ipam_prefix`, `dcim_region` and `tenancy_tenant` appear only as foreign-key references |
| Models | `AWSAccount`, `AWSVPC`, `AWSSubnet` import and query |
| API root | **`/api/plugins/aws-vpc/`** — the plugin's `base_url`, not its module name. The desk evaluation above had guessed the module name; corrected throughout. `aws-accounts`, `aws-vpcs`, `aws-subnets` answer 200 with a token and 403 without |
| Python | the image runs 3.14; the plugin needs ≥ 3.12 |

Shipped as an optional overlay, `deploy/compose/compose.netbox-plugin.yaml`; without it nothing changes. Not verified: any create/update round trip, the import wiring (package N3), and NetBox 5.0.

## References

- [netbox-aws-vpc-plugin on PyPI](https://pypi.org/project/netbox-aws-vpc-plugin/)
- [netbox-aws-vpc-plugin GitHub Repository](https://github.com/dmaclaury/netbox-aws-vpc-plugin)
- [netbox-aws-vpc-plugin Releases](https://github.com/dmaclaury/netbox-aws-vpc-plugin/releases)
- [netbox-aws-vpc-plugin Documentation](https://dmaclaury.github.io/netbox-aws-vpc-plugin/)
- [netbox-aws-vpc-plugin __init__.py (PluginConfig)](https://github.com/dmaclaury/netbox-aws-vpc-plugin/blob/main/netbox_aws_vpc_plugin/__init__.py)
- [netbox-aws-vpc-plugin Models](https://github.com/dmaclaury/netbox-aws-vpc-plugin/tree/main/netbox_aws_vpc_plugin/models)
- [netbox-aws-vpc-plugin Migrations](https://github.com/dmaclaury/netbox-aws-vpc-plugin/tree/main/netbox_aws_vpc_plugin/migrations)
- [netbox-aws-resources-plugin GitHub Repository](https://github.com/zeddD1abl0/netbox-aws-resources-plugin)
- [netbox-aws-resources-plugin PluginConfig](https://github.com/zeddD1abl0/netbox-aws-resources-plugin/blob/main/netbox_aws_resources_plugin/__init__.py)
- [netbox-aws-resources-plugin Models](https://github.com/zeddD1abl0/netbox-aws-resources-plugin/blob/main/netbox_aws_resources_plugin/models.py)
- [netbox-aws-resources-plugin pyproject.toml](https://github.com/zeddD1abl0/netbox-aws-resources-plugin/blob/main/pyproject.toml)
- [ONBOARDING_IMPORT.md](ONBOARDING_IMPORT.md)
- [WORK_PLAN.md — Track N](WORK_PLAN.md)
- [ADR 0007: Onboarding Import as Unmanaged Occupancy](decisions/0007-ONBOARDING_IMPORT_AS_UNMANAGED_OCCUPANCY.md)

## Drift check (package E2)

`platform-ipam onboard drift --domain ID` is a read-only operator command
(`internal/onboardcmd/drift.go`) that compares the plugin's `AWSAccount`
objects against the account sets that already live in configuration. It never
writes to NetBox, and it deliberately lives in the operator process mode, not
in the worker or `internal/service`: [ADR 0009](decisions/0009-NETBOX_AWS_PLUGIN_AS_ACCOUNT_VIEW.md)
holds that the plugin is a view, so no allocation or reconciliation decision
may ever read it, and drift is the one place allowed to.

For the domain named by `--domain`, drift reads:

- **P** — account ids of the plugin's `AWSAccount` objects (`GET
  /api/plugins/aws-vpc/aws-accounts/`, paged).
- **C** — account ids in that domain's `cloud_coverage`.
- **E** — the union of `eligible_accounts` over the pools that belong to that
  domain.
- **I** — the union of `accounts` over every identity mapping, regardless of
  domain.

and reports:

| Rule id | Level | Condition |
| --- | --- | --- |
| `uncovered-account` | error (info if the plugin object's status is `INACTIVE`) | in P but not in C: the account exists in NetBox but the worker never observes it, so networks there are invisible to reconciliation and could be handed out again |
| `unknown-to-plugin` | warning | in C but not in P: covered and observed, but operators cannot see the account in the NetBox view |
| `eligible-without-coverage` | error | in E but not in C: `internal/config.Validate` already refuses to load a configuration shaped this way, so this rule cannot fire through `onboard drift`'s own config-loading path; it exists as defence in depth for a configuration this process did not load itself, and is exercised directly against the pure comparison function in tests |
| `identity-without-coverage` | warning | in I but not covered by *any* domain's `cloud_coverage` (not just the one named by `--domain`): an identity may legitimately target a different domain |
| `invalid-plugin-account-id` | warning | the plugin returned an `AWSAccount` whose `account_id` is empty (including NetBox's JSON-null representation of an unset field) or not twelve digits; such an object is excluded from every rule above rather than compared against configuration |

The `INACTIVE` status read for `uncovered-account` is
`AWSAccountStatusChoices.STATUS_INACTIVE` from the plugin's own source
(`netbox_aws_vpc_plugin/choices.py`, fetched from
[GitHub](https://github.com/dmaclaury/netbox-aws-vpc-plugin/blob/main/netbox_aws_vpc_plugin/choices.py)
on 2026-09-18): `STATUS_ACTIVE = "ACTIVE"`, `STATUS_INACTIVE = "INACTIVE"`,
`STATUS_PENDING_ACTIVATION = "PENDING_ACTIVATION"`. Only `INACTIVE` reads as
"decommissioned" in the sense the work plan means; `PENDING_ACTIVATION` is not
yet usable rather than retired, so it still raises `uncovered-account` at
error level like any other status would.

Output is a JSON report on stdout (`domain`, `counts`, and `findings` sorted
by rule then account id, so it is deterministic across runs against the same
inputs) plus a one-line summary on stderr. Exit codes:

- `0` — no error-level finding.
- `2` — usage: `--domain` missing, or naming a domain absent from
  configuration.
- `3` — validation: at least one error-level finding (`uncovered-account` or
  `eligible-without-coverage`).
- `4` — adapter: NetBox is unreachable, returns a server error, or the plugin
  is not installed (a 404 on `aws-accounts`). An unreachable plugin is never
  reported as "no drift" — no report is printed on stdout in this case, and
  the stderr message names this document and the optional
  `deploy/compose/compose.netbox-plugin.yaml` overlay.

Not verified: any run against a real NetBox or a real plugin install — every
test uses an `httptest` fake, in the style of the rest of this package's
tests.

## Import into the plugin (package N3)

`onboard apply --aws-objects` (docs/WORK_PLAN.md Track N) additionally creates AWS Account, VPC and Subnet objects in netbox-aws-vpc-plugin, linked to the prefixes `apply` writes as occupancy. Without the flag, `apply`'s behaviour is byte-for-byte unchanged, so an installation that never enables `compose.netbox-plugin.yaml` is unaffected. Adapter methods live in `internal/netbox/awsplugin.go`: `ProbeAWSPlugin`, `EnsureAWSAccount`, `EnsureAWSRegion`, `EnsureAWSVPC`, `EnsureAWSSubnet`, `LookupAWSVPC`.

### Usage

```sh
platform-ipam onboard apply table.json --domain <id> --batch <name> --aws-objects
```

`--aws-objects` probes the plugin **before any occupancy write** (`ProbeAWSPlugin`, a GET against `/api/plugins/aws-vpc/aws-accounts/`) and exits with the adapter code (4) and a message naming `netbox-aws-vpc-plugin` if it answers 404 — a missing plugin leaves zero NetBox writes, occupancy included. If the probe succeeds, `apply` runs its normal occupancy write loop first (unchanged from the no-flag path), then walks every row of a `networks` table (or every row of an `accounts` table) a second time to ensure plugin objects. It prints one JSON line per plugin object it touches — `{"kind":"aws-account"|"aws-region"|"aws-vpc"|"aws-subnet","key":...,"action":"created"|"updated"|"unchanged"}` — after the occupancy result lines, and a second summary line to stderr (`aws objects: N created, N updated, N unchanged, N total`).

A `vpc`-typed row needs `account_id`, `region` and `cidr` to get a linked AWSVPC; a `subnet`-typed row additionally needs `parent_id` naming a `resource_id` from a `vpc` row *either in the same table or already present in the plugin* (looked up via `LookupAWSVPC`) — otherwise it is skipped with a warning on stderr, never a hard failure. A row with no `resource_id` at all is skipped with an info line: there is nothing to key a plugin object on. This walk is over the table's rows directly, not over the occupancy write set, so a row whose prefix already existed and was therefore dropped by `onboard.Plan`'s `RuleAlreadyUnmanaged` still gets its plugin objects ensured (and found unchanged) on a repeat run.

Because `AWSVPC.vpc_cidr` is a plain `ForeignKey` to `ipam.Prefix`, not one-to-one (`docs/NETBOX_AWS_PLUGIN.md`'s "Reviewer verification"), two `vpc` rows that collapse to the same CIDR under `onboard.Plan`'s duplicate-CIDR rule (ADR 0007) each get their **own** `AWSVPC` object, both pointing at the single surviving prefix — this is ADR 0009's central scenario and the reason the plugin was adopted.

### What is written

- **AWSAccount**: `account_id` (the lookup key), and `name` when known (only an `accounts`-table row carries one; a `networks`-table row's account gets an account_id-only object rather than a blank name overwriting one already set). The import tag (`platform-ipam-imported`) is added if missing, alongside any tags already there.
- **dcim.Region**: looked up or created by slug from the AWS region name (e.g. `eu-central-1`, already a valid NetBox slug) via `EnsureAWSRegion`; there is no owned field to update once it exists — the AWS region name *is* the lookup key.
- **AWSVPC**: `vpc_id` (the lookup key), `name`, `owner_account` (required — refused locally if it cannot be resolved), `region` (optional), `vpc_cidr` — the NetBox prefix id resolved by an **exact CIDR + VRF lookup** against the domain's own inventory (never created here), and `vpc_secondary_ipv4_cidrs` when the row supplies secondary CIDRs (added to, never removed from, the existing set).
- **AWSSubnet**: `subnet_id` (the lookup key), `name`, `vpc` (required, resolved from `parent_id`), `owner_account` (required), `region` (optional), `subnet_cidr` (same exact-lookup rule as `vpc_cidr`).
- Every plugin object carries the import tag, added without ever removing an existing one.

`Ensure*` is idempotent by the plugin's own unique key: a repeat run with the same input compares every field the import owns against what is already there and writes nothing (`action: "unchanged"`) when they already match; only a differing owned field triggers a `PATCH`, and never a delete of any relationship, secondary CIDR, or tag.

### What is never written

- **Prefixes.** `internal/netbox/awsplugin.go` never creates or modifies an `ipam.Prefix`. `PrimaryCIDR`/`SecondaryCIDRs`/`CIDR` on `AWSVPCSpec`/`AWSSubnetSpec` must already exist as a prefix in the domain's VRF (normally written one step earlier, in the same `apply` run, by `EnsureOccupancy`); if one does not, `EnsureAWSVPC`/`EnsureAWSSubnet` return an error naming the exact CIDR and nothing is written for that row.
- **`AWSAccount.tenant`.** Left empty on every write, per ADR 0009 and `docs/NETBOX_AWS_PLUGIN.md`'s "Reviewer verification": accounts are configuration (`cloud_coverage`, `eligible_accounts`, identities), and an `AWSAccount` row in the plugin grants none of it.
- **Availability zone.** `AWSSubnetSpec.AvailabilityZone` exists so a caller can pass an import table's `az_id` column through without a wiring error, but it is never sent to NetBox: `netbox-aws-vpc-plugin` 0.1.0's `AWSSubnet` model has no availability-zone field at all (`models/aws_subnet.py` carries only a `# TODO: Availability Zone` comment). An operator reading the plugin will not see the AZ an import table recorded; it survives only in the source table (`onboard parse`'s output) and, for the underlying prefix, in the occupancy description if the row's own description text happened to mention it.
- Any managed marker (`platform_allocation_id` and friends) — `--aws-objects` only ever touches unmanaged occupancy written by `EnsureOccupancy`, which already refuses to touch a managed prefix.

### Verified vs not verified

**Verified** (`internal/netbox/awsplugin_test.go`, httptest fakes covering every method: create, unchanged-on-repeat with zero writes, the plugin-not-installed sentinel, the missing-prefix error, and two `AWSVPC` objects sharing one prefix; `internal/onboardcmd/onboardcmd_test.go`'s `apply --aws-objects` tests: flag absent makes zero `/api/plugins/` requests, a missing plugin exits 4 with zero occupancy writes, and a happy-path run followed by a repeat run that is entirely `"unchanged"`) and, against a real, throw-away Compose stack built from `deploy/compose/compose.netbox-plugin.yaml` (`tests/e2e/run-plugin-import.sh` + `tests/e2e/plugin_mode_import.py`): importing a networks table with one VPC CIDR shared by two AWS accounts and one subnet of it produces exactly two `AWSAccount` objects, two `AWSVPC` objects pointing at the single resulting NetBox prefix, and one `AWSSubnet` linked to its VPC; a second `apply --aws-objects` run is entirely unchanged; a reservation against the pool afterwards skips the imported space; and — after every plugin object is deleted through the plugin's own REST API — the imported prefixes are still present, the pool's capacity is unchanged by the deletion, and a further reservation still avoids the imported space, proving the prefixes, not the plugin, do the blocking (ADR 0009).

**Not verified**: any plugin behaviour beyond its REST API (no admin UI coverage); NetBox 5.0 (package N2's own caveat); a real multi-account AWS Organization inventory feeding this path; and `AWSAccount.status`/`AWSVPC.status`/`AWSSubnet.status`, which are left at the plugin's own default (`ACTIVE`) rather than derived from an import row's `state`/`environment` columns — no case in the current design calls for setting them, so this file does not.
