# Live AWS evaluation

Status: runbook, 2026-10-21. **Not yet run.** Every AWS check in this
repository so far has used a stubbed or fake AWS CLI
(`scripts/ai/check-aws` reports `live-aws-organization: NOT_CHECKED`). This
runbook turns the remaining open items in [pilot gates](PILOT_GATES.md) into
dated evidence: live read-only discovery at scale, the
[AWS IPAM handoff](AWS_IPAM_HANDOFF.md), and the IAM templates.

Run it in a **sandbox AWS Organization** first, never in a customer
organization. Steps 1–4 are read-only. Step 5 allocates from an AWS IPAM
pool, which changes AWS state. Step 6 releases what step 5 allocated.

## 0. Sandbox and record

Use an organization with at least:

- a management or delegated administrator account and two member accounts;
- two regions, e.g. `eu-central-1` and `eu-west-1`;
- two overlapping VPCs in different member accounts, e.g. `10.20.0.0/22` in
  both, plus one VPC with a secondary CIDR;
- one region left without the read-only role in one account, to produce a
  real failed cell;
- an Amazon VPC IPAM with one private-scope pool, locale `eu-central-1`,
  provisioned with `10.64.0.0/16`, owned by the IPAM administrator account.
  IPAM's Free Tier is enough for this test.

Create `evaluation/<date>/` outside the repository and keep every output
there. For each step record the start/end time, the exact command, the exit
code, the caller identity (`aws sts get-caller-identity`) and the result
against the expectation below.

## 1. Deploy the IAM roles

```sh
aws cloudformation validate-template --template-body file://deploy/aws/platform-ipam-readonly-role.yaml
aws cloudformation validate-template --template-body file://deploy/aws/platform-ipam-ipam-handoff-role.yaml
```

Deploy the read-only role as a StackSet with `EnableRegionDiscovery=true`
([deploy/aws/README.md](../deploy/aws/README.md)). Deploy the handoff role
twice in the IPAM account: `PlatformIpamIpamVerify` with
`EnableAllocation=false` and `PlatformIpamIpamHandoff` with
`EnableAllocation=true`. In both, set `IpamPoolArns` to the sandbox pool and
`AllowedRegions=eu-central-1`.

**Expect:** stacks complete, and the least-privilege boundaries hold:

```sh
# as the verify role: must fail with UnauthorizedOperation
aws ec2 allocate-ipam-pool-cidr --region eu-central-1 --ipam-pool-id <pool> --netmask-length 28 --dry-run
# as the read-only role: must fail
aws ec2 create-tags --resources <vpc-id> --tags Key=x,Value=y --dry-run
# as the handoff role in a region outside AllowedRegions: must fail
aws ec2 get-ipam-pool-allocations --region eu-west-1 --ipam-pool-id <pool>
```

Also check that the `--dry-run` allocate call **succeeds as a dry run**
(`DryRunOperation`) with the handoff role. This confirms the
`ipam-pool-allocation` resource ARN in the template is sufficient.

## 2. Live discovery

```sh
ROLE_NAME=PlatformIpamReadOnly REGIONS="eu-central-1 eu-west-1" OUT=evaluation/<date>/inventory \
  scripts/aws/org-inventory.sh
```

**Expect:**

- `run.json` lists every active account × region. The cell without the role
  is `failed` or `not_attempted` with a reason, and every other cell is
  `succeeded` with `observed_at`.
- `networks.csv` holds both overlapping VPCs and the secondary CIDR as a
  separate row.
- Reconcile a sample: for at least three VPCs, compare
  `aws ec2 describe-vpcs --vpc-ids ...` with the CSV rows, including
  `association_id` and state.
- Rerun immediately. The result is the same apart from timestamps.

## 3. Scale and timing

Rerun step 2 with `REGIONS` empty (all enabled regions). Then, if a larger
organization is available read-only, run it against a representative number
of accounts. Record the duration, the account × region cells, the cells per
minute and any throttling in `failures.csv`. This is the "representative
scale measurement" required by the discovery gate. If no larger organization
is available, record that the scale measurement is still open; do not
extrapolate from the sandbox.

## 4. Assessment and planning against live inventory

Follow [migration workspace: run a report](MIGRATION_WORKSPACE.md#run-a-report)
with the live `inventory/`, a connectivity matrix marking the two overlapping
VPCs `must_communicate`, a `migration.yaml` replacing one of them, and an
approved plan whose pool is:

```json
{"id": "sandbox-euc1", "cidr": "10.64.0.0/16", "authority": "aws-ipam",
 "aws_ipam_pool_id": "<pool>", "region": "eu-central-1", "allowed_prefix_lengths": [22]}
```

**Expect:** `onboard assess` reports the overlap as confirmed. The address
plan shows `REQUEST_FROM_AWS_IPAM` with no CIDR. `pilot-evidence.json` has
`planning: PASS`, `aws_ipam_requests: 1` and `allocation: UNKNOWN`. Uploading
the same files to the local workspace shows the same result.

## 5. AWS IPAM handoff

As the handoff role:

```sh
python3 scripts/aws/aws-ipam-reserve.py <pilot inputs> --max-inventory-age-seconds 3600 \
  --out evaluation/<date>/handoff-plan.json              # no AWS call
python3 scripts/aws/aws-ipam-reserve.py <pilot inputs> --max-inventory-age-seconds 3600 \
  --execute --out evaluation/<date>/handoff-1.json
python3 scripts/aws/aws-ipam-reserve.py <pilot inputs> --max-inventory-age-seconds 3600 \
  --execute --out evaluation/<date>/handoff-2.json
```

**Expect:** run 1 is `ALLOCATED` with a `/22` inside `10.64.0.0/16`. Run 2
is `ADOPTED` with the same allocation ID. AWS lists exactly one allocation
with the identity description. Also exercise these:

- **Same token:** call `aws ec2 allocate-ipam-pool-cidr` directly with run
  1's `client_token`, description and disallowed CIDRs. AWS returns the same
  allocation. Record how long AWS honours the token, if it can be observed.
- **Changed parameters:** call it with the same token and different
  parameters. Record the exact error code. The handoff assumes
  `IdempotentParameterMismatch`; adjust `aws_ipam.py` if AWS returns another
  code.
- **Locale:** run once with a wrong `region` in a copy of the plan and record
  the AWS error. Do not keep any resulting allocation.

Then, as the verify role:

```sh
python3 scripts/aws/verify-reservations.py --aws-ipam <pilot inputs> \
  --max-inventory-age-seconds 3600 --out evaluation/<date>/verification.json
```

**Expect:** exit `0`, `allocation_gate: PASS`, and the allocation ID and CIDR
from run 1. If it runs within seconds of the write it may report `UNKNOWN`
(eventual consistency); record that and rerun. Upload the report to the
workspace and confirm the move shows `VERIFIED`.

**Negative check:** in AWS, allocate a second CIDR by hand with the same
description. The verifier must report `FAIL` and the handoff must report
`REJECTED`. Release that manual allocation afterwards.

## 6. Clean up

Release every allocation made in step 5 with `aws ec2
release-ipam-pool-allocation`, and confirm `get-ipam-pool-allocations` no
longer lists them. Delete the handoff stacks, and delete the read-only
StackSet unless the worker still needs it.

## 7. Record the result

Write `docs/qualification/LIVE_AWS_<date>.md` (no account IDs, ARNs or CIDRs
from a customer; sandbox IDs may be redacted) with a pass/fail line for
each step, the timing numbers and every deviation. Then update:

- [pilot gates](PILOT_GATES.md): the discovery and allocation gate notes;
- [AWS IPAM handoff](AWS_IPAM_HANDOFF.md) and
  [ADR 0025](decisions/0025-AWS_IPAM_AUTHORITY_HANDOFF.md): status lines;
- `scripts/ai/checks.py`: the `live-aws-organization` note, to point at the
  dated record (it stays a manual check, not something CI runs).

A sandbox pass shows that the tooling works against real AWS APIs. It does
not satisfy a customer's pilot gates, which need that customer's approved
scope and live evidence.
