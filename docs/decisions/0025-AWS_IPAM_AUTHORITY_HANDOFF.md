# ADR 0025: AWS IPAM authority handoff for pilot moves

Status: accepted, 2026-10-21. Implemented and tested against a fake AWS CLI
only; live AWS IPAM behaviour is unverified.

## Context

[ADR 0022](0022-PILOT_ADDRESS_READINESS_AND_POOL_AUTHORITY.md) left the
allocation gate open for pools owned by AWS IPAM: the planner recommends a
pool and size, but nothing reserved from AWS IPAM or verified the result.
The pilot planning gate also required a local CIDR for every move, so a
pilot with an AWS-IPAM-owned pool could never reach the allocation gate.

## Decision

- An approved `aws-ipam` pool names its AWS pool in `aws_ipam_pool_id`. A
  reviewed `REQUEST_FROM_AWS_IPAM` proposal counts as a planning option.
- `aws-ipam-reserve.py` is the only command that writes to AWS. It is a dry
  run unless `--execute` is given. Each reviewed move maps to one custom
  allocation whose description carries the request identity
  `platform-ipam:<tenant>:<allocation_key>` and whose client token is derived
  from the AWS pool, tenant, key and size. Known occupancy is passed as
  `DisallowedCidrs`.
- Recovery relies on the identity, not on local state: before writing, the
  command adopts an existing allocation with the same identity. A response
  that cannot be confirmed is `UNKNOWN_OUTCOME`, and a rerun is safe.
- Nothing releases automatically. A rejected response is reported with its
  release command for an operator.
- The command's journal is not evidence. `verify-reservations.py --aws-ipam`
  reads AWS IPAM again and revalidates the returned CIDR before a move counts
  as verified. AWS's choice of CIDR does not need to match a local
  candidate, because no local candidate exists.
- AWS access uses a dedicated role in the pool-owning account, scoped to the
  named pool ARNs and locale regions. Verification uses a role without
  `AllocateIpamPoolCidr`.

## Consequences

- An operator who edits an allocation's description in AWS can break or
  forge the identity match. The pool account is trusted for this pilot.
- Changing the inputs after an unconfirmed write reuses the same token with
  different parameters. AWS refuses that, and the result stays
  `UNKNOWN_OUTCOME` until the earlier allocation becomes visible and is
  adopted.
- AWS IPAM allocations are not projected into NetBox IPAM or the
  Platform-IPAM ledger; the verification report is the shared record.
- The allocation gate can now pass for AWS-IPAM-owned pools in principle.
  It must not be claimed for a customer until the steps in
  [live AWS evaluation](../LIVE_AWS_EVALUATION.md) have passed.
