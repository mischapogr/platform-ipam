---
name: aws-reconciliation
description: Implement or review platform-ipam AWS inventory collection, cross-account IAM, binding verification, and CIDR reuse evidence.
---

Read the relevant sections of [AWS integration](../../../docs/AWS_INTEGRATION.md) and [release gates](../../../docs/API_V1.md). Use current official AWS references for the API/identity behavior being changed.

- Observe the explicitly onboarded account/region set for an overlap domain. Include every page, associated VPC CIDR, relevant subnet, and untagged occupancy; tag filtering cannot establish absence.
- A denied, incomplete, or stale scan is UNKNOWN. Coverage-generation changes invalidate earlier absence evidence.
- Tags correlate records; verify account, region, type, CIDR, and parent independently before binding.
- Parent VPC containment is permitted for subnet reclamation. Peer overlap and unreclaimed children remain blockers.
- Keep worker observation credentials separate from Terraform provisioning credentials. Routine development uses fake AWS observations; live sandbox tests must identify their target explicitly.

Use `scripts/ai/inspect-allocation ID --input saved-allocation.json` for a compact saved-response summary, or omit `--input` to GET the configured platform API. The helper does not query AWS/NetBox directly or authorize reuse. Preserve its UNKNOWN/NOT_EVALUATED fields.

Validate changed collector logic with pagination, denied-role, missing-tag, eventual-consistency, and coverage-change cases. Run `scripts/ai/check-contract` if consumer-visible evidence changes.
