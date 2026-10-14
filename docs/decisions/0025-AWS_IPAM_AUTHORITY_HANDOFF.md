# ADR 0025: AWS IPAM authority handoff for the first pilot

Status: proposed, 2026-10-14. PILOT-1 remains open. This record authorizes no
AWS writes, pool migration or change to the allocation API.

## Problem

The offline address report can recommend an AWS IPAM pool and prefix length,
but cannot reserve a CIDR there. Platform-IPAM must not select or hold a CIDR
from the same pool. The existing allocation API promises a durable hold before
the consumer provisions a VPC; that promise cannot be carried over merely by
switching the backend name.

AWS supports a manual pool allocation with a client token and a returned
allocation ID. Creating a VPC from an IPAM pool is a separate operation that
causes AWS to create a resource allocation. The current documentation does not
establish a race-free conversion from a manual allocation to that VPC resource
allocation. A manual allocation is therefore **not** evidence that the same
CIDR can be used by a later VPC create request.

## Proposed pilot contract

For an AWS-IPAM-owned pool, use an explicit **provisioning-time handoff**:
the approved move supplies the AWS IPAM pool ID, locale and prefix length to
the customer's VPC provisioner. The provisioner requests its CIDR from that
pool when creating the VPC. Only the returned VPC CIDR and its observed IPAM
resource allocation may become authority evidence. Platform-IPAM stores or
displays that evidence separately from its own allocation ledger and never
reports a pre-provisioning hold for this mode.

The customer and platform owners must accept that the AWS-IPAM path cannot
satisfy a reserve-before-provision requirement under this proposal. If a
durable hold before VPC creation is mandatory, keep this path disabled until
an authorized sandbox demonstrates a safe manual-allocation-to-VPC handoff and
the resulting lifecycle is reviewed. Do not release a manual allocation and
assume the next VPC request will obtain the same CIDR.

## Required identity and evidence

- The approved pool map binds the AWS IPAM pool ID, account, locale, allowed
  prefix lengths and single allocation authority. Its configuration version
  is recorded with each attempt. A mismatched or changed map refuses the
  handoff.
- One immutable move and one stable local attempt identity bind records and
  VPC tags. The current `CreateVpc` request has no client-token parameter, so
  that identity does not make AWS creation idempotent. After a lost response,
  stop writes and reconcile VPCs and IPAM allocations by account, region,
  pool, tags and creation window. An absent, incomplete or ambiguous lookup
  remains `UNKNOWN` and needs operator resolution; never blindly repeat the
  create under either the same or a new local identity.
- Record the caller account, VPC ID, pool ID, allocation ID, returned CIDR,
  region, time and reviewed move hash. Validate the returned CIDR against the
  approved size and pool, current complete occupancy, protected ranges and
  move intent. Any incomplete or conflicting observation remains `UNKNOWN`;
  no report may claim a verified replacement from the create response alone.
- A resource allocation is released through the resource lifecycle, not by
  treating it as a manual allocation. NetBox is a projection of the observed
  result, never a second allocator for the AWS-owned pool. Release and reuse
  remain blocked while resource deletion or observation is uncertain.

## Acceptance before implementation or pilot use

1. Owners choose provisioning-time handoff or require a separately proven
   reserve-before-provision design, and approve the pool/account/role map.
2. In an authorized test pool, exercise pool rules, cross-account grants,
   concurrent creates, a lost response, a blocked repeat attempt, denied
   permissions, a returned-CIDR conflict and deletion with delayed IPAM
   observation. Retain VPC and allocation IDs and evidence of cleanup.
3. Stub the same outcomes before wiring an API/provider path. Verify that
   Platform-IPAM never creates a ledger hold or emits `RESERVED` for an AWS-owned
   pool, and that failed/unknown results cannot pass the pilot allocation gate.
4. Update the pilot gate and consumer contract only after the selected mode
   and its evidence have passed review. Live AWS acceptance remains required.

## AWS references

- [IPAM allocation modes](https://docs.aws.amazon.com/vpc/latest/ipam/allocate-cidrs-ipam.html)
- [Manual allocation](https://docs.aws.amazon.com/vpc/latest/ipam/manually-allocate-ipam.html)
- [Manual allocation request and client token](https://docs.aws.amazon.com/java/api/latest/software/amazon/awssdk/services/ec2/model/AllocateIpamPoolCidrRequest.Builder.html)
- [VPC creation from an IPAM pool](https://docs.aws.amazon.com/vpc/latest/ipam/create-vpc-ipam.html)
- [CreateVpc request parameters](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_CreateVpc.html)
- [Release of manual allocations](https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_ReleaseIpamPoolAllocation.html)
