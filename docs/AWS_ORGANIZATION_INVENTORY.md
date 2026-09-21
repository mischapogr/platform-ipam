# Inventorying accounts, networks and IP ranges from an AWS Organization

Status: operator procedure, 2026-09-18, extended 2026-09-20 (ADR 0014, work-plan package M1b1) with `association_id`, `observed_at` and `run.json`. **The script [`scripts/aws/org-inventory.sh`](../scripts/aws/org-inventory.sh) is covered by `tests/aws/test_org_inventory.sh` with a stubbed `aws` CLI and has NOT been run against a live organization.** The `jq` filters were checked against synthetic `describe-vpcs`/`describe-subnets` output only. Run them read-only first and compare a sample account by hand before trusting the output.

This is a one-off onboarding task performed by a person with organization credentials. It is deliberately **not** something the service does: the reconciliation worker has no Organizations permissions and must not gain them ([AWS integration](AWS_INTEGRATION.md), sections 3 and 5). The output of this procedure feeds the [onboarding import](ONBOARDING_IMPORT.md).

## What you are collecting

| Table | Source | Used for |
| --- | --- | --- |
| Accounts: id, name, status, OU path, tags | Organizations | `cloud_coverage` cells, pool `eligible_accounts`, identities |
| Networks: every VPC CIDR association and every subnet, per account and region | EC2 in each member account | occupancy that must block allocation |
| Other ranges: on-premises, VPN, Direct Connect, partner networks | your network team, not AWS | occupancy; AWS cannot tell you these |

**A failed lookup is not an empty account.** Every method below must record the accounts and regions it could not read. An inventory with silent gaps is worse than none, because the gap looks like free space.

## 1. Prerequisites

- AWS CLI v2 and `jq`.
- Credentials in the **management account** or a **delegated administrator** account with `organizations:ListAccounts`, `organizations:ListParents`, `organizations:DescribeOrganizationalUnit`, `organizations:ListTagsForResource`.
- A role you can assume in **every member account** with at least `ec2:DescribeVpcs`, `ec2:DescribeSubnets`, `ec2:DescribeRegions`. Options, best first:
  1. Deploy the project's read role (`PlatformIpamReadOnly`, the same three Describe actions the worker uses, plus `ec2:DescribeRegions` for this task) to all accounts with a **service-managed CloudFormation StackSet** targeted at the organization root. It then doubles as the worker's target role. See work-plan package B3.
  2. `AWSControlTowerExecution`, if the organization uses Control Tower.
  3. `OrganizationAccountAccessRole`. It exists only in accounts *created* by Organizations, not in invited accounts, and it is full administrator — use it for discovery only if nothing narrower exists.
- The management account itself usually has none of these roles. Inventory it with its own credentials (section 3, last step).

## 2. Accounts

```bash
mkdir -p inventory
aws organizations list-accounts --output json > inventory/accounts.json

# Human-readable check
jq -r '.Accounts[] | [.Id, .Name, .Status] | @tsv' inventory/accounts.json | column -t -s $'\t'
```

Keep suspended accounts in the file; filter on `Status == "ACTIVE"` only when scanning. The CLI follows pagination by itself.

OU path and tags, which usually carry the environment and owner:

```bash
jq -r '.Accounts[].Id' inventory/accounts.json | while read -r id; do
  parent=$(aws organizations list-parents --child-id "$id" --query 'Parents[0].Id' --output text)
  name=$(aws organizations describe-organizational-unit --organizational-unit-id "$parent" \
           --query 'OrganizationalUnit.Name' --output text 2>/dev/null || echo ROOT)
  tags=$(aws organizations list-tags-for-resource --resource-id "$id" \
           --query 'Tags[].[Key,Value]' --output text | tr '\t\n' '=;')
  printf '%s\t%s\t%s\n' "$id" "$name" "$tags"
done > inventory/account-ou-tags.tsv
```

`list-parents` returns the direct parent only; walk upward with repeated calls if you need the full path.

## 3. Networks: the direct scan (authoritative)

This assumes a role into every active account and reads EC2 directly. It is the only method whose completeness you control, and it matches what the worker will observe later.

The scan script is at [`scripts/aws/org-inventory.sh`](../scripts/aws/org-inventory.sh). Usage:

```bash
ROLE_NAME=PlatformIpamReadOnly \
REGIONS="eu-central-1 us-east-1" \
OUT=inventory \
./scripts/aws/org-inventory.sh
```

For the management account, set `MANAGEMENT_ACCOUNT_ID`:

```bash
MANAGEMENT_ACCOUNT_ID=123456789012 \
ROLE_NAME=PlatformIpamReadOnly \
REGIONS="eu-central-1 us-east-1" \
OUT=inventory \
./scripts/aws/org-inventory.sh
```

Notes that matter:

- **Every associated CIDR of a VPC is a row**, not just the primary one. Secondary CIDRs occupy space too; the worker treats them the same way (`internal/cloud/aws.go`).
- `describe-regions` without `--all-regions` returns the regions enabled for that account. Opt-in regions that are disabled are not listed, which is what you want.
- The `get-caller-identity` check mirrors the worker's own guard: a role that lands in a different account than expected is an error, not data.
- `failures.csv` must be **empty or explained** before the inventory is used. Re-run the failed accounts; do not import around them.
- The management account: run `scan_account <id> <name>` once with the management credentials themselves, outside the loop.
- Session credentials last 15 minutes here. Raise `--duration-seconds` for accounts with many regions.
- Never commit `inventory/`; it contains account names and network layout.

`networks.csv` already uses the column names the [onboarding import](ONBOARDING_IMPORT.md) accepts.

### `networks.csv`'s two trailing columns

Two columns were appended after `primary` (so any consumer that already reads the first eleven columns by position is unaffected):

- `association_id` — the VPC CIDR association's own id, from `CidrBlockAssociationSet[].AssociationId` in the same `describe-vpcs` response the other VPC columns already come from (no new AWS call). Set on every `vpc` row; empty on every `subnet` row, because a subnet is not a CIDR association.
- `observed_at` — the UTC RFC 3339 instant the row's region was read, e.g. `2026-09-20T12:00:00Z`. One instant per region scan, taken before that region's `describe-vpcs` call: a VPC row and the subnet rows from the same region scan share it, so it says when the region was read, not when each individual API call happened to return.

### `run.json`

A fourth output file (ADR 0014): the only place this procedure records *what was attempted*, so "read this account and region, and it was empty" can be told from "never read this account and region" — something `networks.csv` and `failures.csv` alone cannot say, because an account or region that produced zero rows looks identical to one nobody visited.

Top-level fields:

| Field | Meaning |
| --- | --- |
| `script_version` | the collector's own version string (`"2"` as of this extension) |
| `started_at`, `finished_at` | UTC RFC 3339, the whole run's start and end |
| `role_name` | the `ROLE_NAME` assumed in each member account |
| `management_account_used` | `true` when `MANAGEMENT_ACCOUNT_ID` was set for this run |
| `management_account_id` | that account id, or `null` when `management_account_used` is `false` |
| `configured_regions` | the `REGIONS` list as configured, or `null` when `REGIONS` was left empty (region list discovered per account) |
| `accounts` | one entry per `ACTIVE` account in `accounts.json`, described below |

Never present: a credential, a session token, or any account id beyond what `accounts.json` already holds — in particular, an identity-mismatch (assumed role landed in an unexpected account) is recorded in `run.json` only as the reason `identity-mismatch`, never with the account id actually observed (that id, which `failures.csv`'s `error` column already carries as `got <id>`, can be a real AWS account beyond anything `accounts.json` lists).

Each entry of `accounts`:

| Field | Meaning |
| --- | --- |
| `account_id`, `account_name` | as in `accounts.json` |
| `credential_source` | `"assumed-role"` or `"management-account"` |
| `regions_attempted` | `"known"` when a region list was obtained (or configured), `"unknown"` when it never was — never a guessed or invented list |
| `not_attempted_reason` | `null`, or one of `assume-role-failed`, `identity-mismatch`, `region-list-unavailable` when the whole account was never scanned |
| `region_source` | `"configured"` (from `REGIONS`) or `"discovered"` (from that account's own `describe-regions`) |
| `regions` | one entry per region, below |

Each entry of an account's `regions`:

| Field | Meaning |
| --- | --- |
| `region` | the region name |
| `outcome` | `succeeded`, `partial`, `failed`, or `not_attempted` |
| `stage` | `"describe"`, present for `failed` and `partial` |
| `row_count` | present for `succeeded` and `partial`: how many rows that region contributed to `networks.csv`. `0` is a legitimate value — it means the region was read and had nothing in it, not that it was skipped |
| `reason` | present for `not_attempted`: why (mirrors the account-level `not_attempted_reason`) |
| `observed_at` | present for `succeeded` and `partial`: the same instant as the matching rows in `networks.csv` |

**`partial` means "not complete", not "no data".** `scan_region` runs `describe-vpcs` before `describe-subnets`; if `describe-vpcs` succeeds and `describe-subnets` then fails, the VPC rows it already produced stay in `networks.csv` (and stay useful — a VPC's own CIDR associations are all there), but that region's subnet population is unknown, and the same `describe,failed` row that has always gone into `failures.csv` for this case is still written, unchanged. A `partial` region's `row_count` is exactly the number of rows written before the failure (the VPC rows), not the (unknown) total the region would have produced. Do not read "some rows are present" as "this region is done" — check `outcome`, not row presence, before trusting a region's coverage.

An account whose role could not be assumed, whose identity check failed, or whose region list could not be obtained gets `regions_attempted: "unknown"` and an empty `regions` list when its intended region set is genuinely unknown (`REGIONS` was left empty for the whole run), or `regions_attempted: "known"` with every configured region listed as `not_attempted` when `REGIONS` was set (the intended set was known even though nothing in it was reached). Either way, nothing is invented: a region never appears in `run.json` unless it was actually configured or actually discovered.

`run.json` is written once, after every account has been processed. **An interrupted run (the process killed partway through) leaves no `run.json` at all** — a consumer that finds `networks.csv` and `failures.csv` but no `run.json` should treat the whole run's coverage as unknown, exactly as it would treat a missing file.

## 4. Alternatives when you cannot assume a role everywhere

| Method | Gives you | Does not give you | Trust |
| --- | --- | --- | --- |
| **AWS Config aggregator** (organization-wide) | VPC and subnet CIDRs across accounts in one query | anything in accounts or regions where Config recording is off; can lag | good cross-check |
| **Amazon VPC IPAM** with Organizations integration | monitored resource CIDRs, overlap and compliance status | regions outside IPAM's operating regions; needs IPAM already set up by the org owner | good if already deployed |
| **Resource Explorer** multi-account search | which VPCs and subnets exist, and where | **no CIDRs** — it returns ARNs and tags | discovery only |

Config advanced query (one call per resource type):

```bash
aws configservice select-aggregate-resource-config \
  --configuration-aggregator-name <aggregator> \
  --expression "SELECT accountId, awsRegion, resourceId, configuration.cidrBlock, configuration.cidrBlockAssociationSet
                WHERE resourceType = 'AWS::EC2::VPC'" --output json

aws configservice select-aggregate-resource-config \
  --configuration-aggregator-name <aggregator> \
  --expression "SELECT accountId, awsRegion, resourceId, configuration.cidrBlock, configuration.vpcId, configuration.availabilityZoneId
                WHERE resourceType = 'AWS::EC2::Subnet'" --output json
```

Each `Results[]` entry is a JSON **string**; decode it with `jq '.Results[] | fromjson'`.

VPC IPAM, if it exists:

```bash
aws ec2 describe-ipam-scopes --query 'IpamScopes[].[IpamScopeId,IpamScopeType]' --output text
aws ec2 get-ipam-resource-cidrs --ipam-scope-id <private-scope-id> --resource-type vpc --output json
aws ec2 get-ipam-resource-cidrs --ipam-scope-id <private-scope-id> --resource-type subnet --output json
```

Resource Explorer, to find accounts and regions you did not know had networks:

```bash
aws resource-explorer-2 search --query-string 'resourcetype:ec2:vpc' --output json
```

Use an alternative to **cross-check** the direct scan, or as a stopgap. An account that appears in Config or Resource Explorer but not in `networks.csv` is a finding to resolve before import.

## 5. What AWS cannot tell you

On-premises ranges, VPN and Direct Connect customer networks, ranges promised to a project that has not built anything yet, and ranges used in another cloud. Collect those from the network team as a table of `cidr, description, owner`. Transit Gateway route tables (`aws ec2 search-transit-gateway-routes`) are a useful hint for which external ranges are actually routed.

## 6. Before you import

1. `failures.csv` is empty or every row is explained.
2. Row counts for two or three accounts match the VPC console.
3. Overlapping CIDRs across accounts are listed and understood — they are common, and the import reports them rather than hiding them.
4. Every account you intend to make eligible for allocation has the read role installed, because pool eligibility requires a matching coverage cell (`internal/config/config.go`).

For point 3, `platform-ipam onboard assess` reads this procedure's own output directory directly and reports exactly which VPC CIDR associations conflict, with coverage as a first-class result — see [Overlap assessment](OVERLAP_ASSESSMENT.md).
