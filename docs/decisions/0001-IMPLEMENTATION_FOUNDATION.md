# ADR 0001: Go API and worker with a durable PostgreSQL allocation ledger

Status: accepted, 2026-09-08

## Context

Platform IPAM must issue one stable CIDR for a tenant allocation key through
lost responses, duplicate requests, process restarts, NetBox timeouts, and
eventually consistent AWS observations. NetBox is the network inventory
authority, but it cannot provide the transactionally durable idempotency,
operation, hold, audit, and coverage records required across both systems.
The API contract requires asynchronous operations, revision preconditions,
permanent allocation-key tombstones, and explicit quarantine/reclamation
gates.

## Decision

Implement the API and reconciliation worker in Go 1.26.8. Use
`internal/domain` for shared plain-string IDs/enums and allocation value types,
`internal/service` for use cases and ports, `internal/storage` for the pgx
ledger, `internal/netbox` and `internal/cloud` for adapters,
`internal/transport` for HTTP, and `cmd/platform-ipam` for process wiring.
Keep the API schema at `api/openapi.yaml` as the wire contract and map its
snake_case DTOs at the transport boundary. Domain code does not accept tenant,
owner, or role data from request bodies. The authenticated identity maps to a
server-side tenant and eligible account/environment/region targets.

Use PostgreSQL as the durable allocation ledger. It stores allocations,
allocation-key tombstones, HTTP idempotency records, operations, CIDR holds,
outbox work, bindings, observations, findings, and append-only audit events.
Admission and lifecycle decisions run in short ledger transactions with the
lock order **overlap domain, parent allocation, allocation**. A durable domain
operation barrier and exact candidate CIDR are committed before an external
NetBox mutation. No SQL transaction spans an HTTP call.

The storage port must support tenant-scoped reads and a transaction boundary
that can atomically persist the allocation/operation/hold/barrier/audit/outbox
records. Database uniqueness protects allocation identity and idempotency
keys; domain locking and CIDR intersection checks protect peer allocation.
The ledger may never release a hold merely because a client retries, a worker
restarts, or a timer fires.

NetBox is a real HTTP inventory adapter behind a narrow port. It reads every
inventory page relevant to an allocation domain, creates a persisted exact
CIDR with the allocation and operation markers, recovers a timed-out create by
marker plus exact CIDR, and deletes only the stored managed backend identity
after validating its marker and CIDR. It never calls a retryable “next
available prefix” operation. NetBox custom-field names, VRF IDs, tokens, and
release-specific semantics remain adapter configuration, outside consumer API
types.

Cloud observation is also a port. Local development and deterministic tests
use a fake observer. The AWS implementation runs in the worker and is
read-only: it enumerates paginated VPCs, every VPC CIDR association, subnets,
tags, account/region coverage generation, and scan completion. Failed,
partial, stale, or inaccessible observation is `UNKNOWN`, never evidence of
absence. The worker alone dispatches durable operations/outbox rows,
verifies bindings, records findings, and evaluates guarded reclamation; it
does not create or delete AWS resources.

Development runs the API, worker, PostgreSQL, and a local NetBox stack under
Docker Compose. Stage and production deploy the API and worker separately in
Kubernetes through Helm, with PostgreSQL migrations as a single controlled
job and workload identity for AWS read roles.

## Consequences

The ledger becomes the source for allocation identity and lifecycle while
NetBox remains the source for network inventory. A committed `RESERVED` or
`ACTIVE` response is the only usable CIDR result. Uncertain external outcomes
return a durable pending operation; retrying retains the same identity and
candidate.

This foundation requires a PostgreSQL integration suite and a pinned NetBox
compatibility test before a service is considered ready. It also requires
exact configuration for OIDC tenant mapping, pool/routing domains, AWS
coverage targets, NetBox version/VRF/custom fields, and migration tooling.
Those are Phase 0 inputs, not defaults that handlers may invent.
