# AWS IPAM authority handoff

Status: implemented and tested against a stateful fake of the AWS CLI,
2026-10-21. **No real AWS IPAM pool has been allocated from or read.** The
live evaluation steps are in [live AWS evaluation](LIVE_AWS_EVALUATION.md).
Decision: [ADR 0025](decisions/0025-AWS_IPAM_AUTHORITY_HANDOFF.md).

For a pool whose sole authority is AWS IPAM, the pilot planner recommends a
pool and size but never a CIDR ([ADR 0022](decisions/0022-PILOT_ADDRESS_READINESS_AND_POOL_AUTHORITY.md)).
This handoff asks AWS IPAM to allocate for each reviewed move and then
verifies the returned CIDR from a fresh AWS read, closing the
[allocation gate](PILOT_GATES.md#4-allocation-gate) for AWS-IPAM-owned pools.

## Inputs

The approved address plan names the AWS pool for each `aws-ipam` pool:

```json
{"id": "pilot-euc1", "cidr": "10.64.0.0/16", "authority": "aws-ipam",
 "aws_ipam_pool_id": "ipam-pool-0123456789abcdef0", "region": "eu-central-1",
 "allowed_prefix_lengths": [22], "eligible_accounts": ["111111111111"]}
```

`region` must be the pool's **locale**: AWS IPAM allocates and lists
allocations only in that region. `cidr` must equal, or lie inside, the space
provisioned to that AWS pool; the planner cannot check this offline, so
confirm it when the plan is approved.

The pilot report must have complete discovery, a passing planning gate, and
approved moves whose proposal is `REQUEST_FROM_AWS_IPAM` for the reviewed
target. An AWS IPAM request now counts as a reviewed option for the planning
gate, alongside a local advisory CIDR.

## Credentials

Deploy [`platform-ipam-ipam-handoff-role.yaml`](../deploy/aws/platform-ipam-ipam-handoff-role.yaml)
in the account that owns the pools. It grants
`ec2:GetIpamPoolAllocations` on the named pool ARNs and, only with
`EnableAllocation=true`, `ec2:AllocateIpamPoolCidr`. It never grants
`ReleaseIpamPoolAllocation`. Use a role with `EnableAllocation=false` for
verification. The commands use the ambient AWS CLI credentials.

## Allocate

```sh
python3 scripts/aws/aws-ipam-reserve.py \
  --pilot pilot-evidence.json --approved-plan approved-plan.json \
  --protected protected.json --networks inventory/networks.csv --run inventory/run.json \
  --max-inventory-age-seconds 3600 --out handoff-plan.json          # dry run: no AWS call
python3 scripts/aws/aws-ipam-reserve.py ... --execute --out handoff.json
```

Each move becomes one `AllocateIpamPoolCidr` call with:

- `Description` `platform-ipam:<tenant>:<allocation_key>`, the request
  identity a later read matches;
- `ClientToken` derived from the AWS pool, tenant, key and size, so a retry
  of the same request cannot allocate twice while AWS honours the token;
- `NetmaskLength` from the reviewed target;
- `DisallowedCidrs`: protected ranges, observed inventory, pool exclusions
  and other moves' advisory CIDRs that overlap the pool.

Before allocating, the command lists the pool's allocations and **adopts** an
existing allocation with the same identity, so a rerun after a crash or an
expired token does not allocate again. Transient errors retry with the same
token. Each result is one of:

| Status | Meaning | Operator action |
| --- | --- | --- |
| `ALLOCATED` / `ADOPTED` | AWS returned an allocation that passed the immediate checks | Verify |
| `REJECTED` | The response fails a check, or AWS holds several allocations for one identity | Review; release with the printed command if unwanted |
| `UNKNOWN_OUTCOME` | The write may or may not have happened | Rerun later; it adopts or retries safely |

Exit `0` means every request ended `ALLOCATED` or `ADOPTED`, `3` means at
least one did not, and `4` means invalid input; nothing was sent for `4`.
The command never releases anything. `handoff.json` is an operator journal,
not evidence.

## Verify

```sh
python3 scripts/aws/verify-reservations.py --aws-ipam \
  --pilot pilot-evidence.json --approved-plan approved-plan.json \
  --protected protected.json --networks inventory/networks.csv --run inventory/run.json \
  --max-inventory-age-seconds 3600 --out verification.json
```

Add `--url` when the pilot also has Platform-IPAM-owned pools; one report
then covers both authorities (`"authority": "mixed"`). For each AWS IPAM move
the verifier reads the pool's allocations again and requires exactly one
`custom` allocation carrying the move's identity in the approved AWS pool,
of the reviewed size, inside the approved pool range, and clear of
exclusions, protected ranges, observed inventory and other returned CIDRs.
Unlike Platform-IPAM, there is no advisory CIDR to match: AWS chose the
CIDR, and the revalidation is what makes it acceptable. Upload
`verification.json` to the workspace or pass it to `pilot-evidence.py
--verification` as before.

`GetIpamPoolAllocations` is eventually consistent. A just-made allocation
can be missing for a short time, which reads as `UNKNOWN`, never `VERIFIED`;
rerun the verifier.

## Not covered

- **NetBox projection.** The verified allocation ID and CIDR appear in the
  workspace through the uploaded verification report. Nothing writes an AWS
  IPAM allocation into NetBox IPAM or the Platform-IPAM ledger.
- **Release.** Releasing a rejected or abandoned allocation is a manual
  operator decision.
- **Pool configuration.** The handoff does not check the AWS pool's
  provisioned CIDRs, allocation rules or locale; the approved plan asserts
  them.
