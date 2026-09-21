---
name: ipam-contract
description: Change or review platform-ipam allocation API contracts, idempotency, ownership, and lifecycle semantics.
---

Read [API v1](../../../docs/API_V1.md) for the affected endpoint/state and [transaction design](../../../docs/IMPLEMENTATION_PLAN.md) when changing concurrency or persistence. Treat these as proposed contracts until implemented; preserve the user's current decisions.

- Consumer inputs describe allocation intent, not NetBox objects. Derive tenant authorization from authenticated identity.
- Keep allocation identity distinct from an HTTP retry key. Lost responses must recover the same durable operation/CIDR.
- A reservation is usable only after confirmed inventory commit and a durable hold. Reads and retries cannot release space.
- Reclamation requires explicit release, complete current evidence, child-state checks, and confirmed inventory removal. Timers alone do not prove absence.
- When editing the contract, update the applicable OpenAPI/example/client behavior together. Do not invent another schema if the planned OpenAPI is still missing.

Run `scripts/ai/check-contract`. It distinguishes syntax/link checks from missing OpenAPI/schema tooling. For lifecycle implementation, also run targeted concurrency and failure-recovery tests in the actual component; this static helper cannot prove those properties.
