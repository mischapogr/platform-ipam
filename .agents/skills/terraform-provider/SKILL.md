---
name: terraform-provider
description: Build or review the platformipam Terraform provider and AWS examples, especially state, retries, import, replacement, and teardown.
---

Read the relevant [client contract](../../../docs/CLIENTS.md) and [API endpoint](../../../docs/API_V1.md). Use Terraform MCP for AWS resource usage and official Plugin Framework documentation for custom-provider behavior.

- Configure/Validate/Plan and data sources are read-only. New CIDRs remain unknown until apply reserves them.
- Recover timed-out creates by stable allocation key/operation. Preserve known identity when later work fails; account for Terraform taint on partial Create failures.
- Treat backend errors and ambiguous authorization-related 404s as diagnostics, not proof that an allocation vanished.
- Replacing sizing/placement needs a distinct allocation key. Do not allow taint or `-replace` to silently destroy/create the same logical identity.
- Delete succeeds on durable quarantine acceptance, not immediate space reuse. AWS resources must be destroyed before allocation release; parent reclamation waits for children.
- Import reads an existing platform ID. It never claims a CIDR or creates inventory.

Run `scripts/ai/check-provider` for formatting and available short provider tests. Add `--plan-json path/to/plan.json` to check the known replacement-key rule against `terraform show -json` output. This guard is limited to allocation replacement; it is not a complete authorization or AWS-binding policy check.

Provider acceptance tests against a real sandbox remain separate. Never set `TF_ACC=1` as part of routine formatting/unit checks.
