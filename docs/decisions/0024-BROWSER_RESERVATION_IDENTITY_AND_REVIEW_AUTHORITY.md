# ADR 0024: Browser reservation identity and review authority

Status: proposed, 2026-09-29. This record does not authorize a browser write
path or supersede ADR 0015. PILOT-3 remains open pending the customer and
platform owner identity decision.

## Context

The NetBox migration workspace authenticates a deployment-wide operator. The
allocation API's operator principal has no tenant and cannot reserve or read a
tenant's pending operation. A NetBox session is not an allocation API bearer
token, and the workspace has no reviewed mapping from a human to a tenant.

ADR 0015 makes the customer's reviewed `migration.yaml` the authority for
move intent. The browser currently renders that file's derived evidence and
stores immutable report snapshots. An approval stored only in NetBox would
create a second plan authority. A reservation made from a saved snapshot may
also use stale inventory or a stale advisory CIDR. The API selects and holds
the actual CIDR; the suggested CIDR is never a claim on capacity.

## Proposed decision

Keep `migration.yaml` in the customer's repository as the reviewed move plan.
The browser may collect draft comments, but a move is approved only after the
reviewed plan is updated and a new snapshot is derived from its exact bytes.
The browser must display the plan approval, snapshot hash and current evidence
limits before offering any reservation action.

If browser reservation is required, use a tenant-scoped identity obtained
through a reviewed SSO delegation flow. The allocation API must authenticate
the token and derive the tenant server-side. The NetBox operator session alone
must never supply tenant identity, and the workspace must not keep a shared
tenant bearer token. The exact issuer, token audience, subject-to-tenant
mapping, allowed groups, token lifetime and revocation behavior require an
owner-approved identity contract before implementation. Until then, the
existing owning-tenant CLI/API handoff remains the supported reservation path.

For each browser attempt after that contract is accepted:

1. Bind the action to one immutable snapshot digest, one reviewed move
   subject, its target tenant and permanent `allocation_key`. Recompute or
   revalidate the proposal against current inputs before requesting a hold;
   a changed plan or stale evidence requires a new review.
2. Send `POST /v1/allocations` as the tenant with the plan's immutable target
   fields. Use one stable `Idempotency-Key` for retries of this attempt. Never
   choose a CIDR in the browser or retry a conflict under a random key.
3. Persist an append-only attempt record with the human actor, tenant identity,
   snapshot and move hashes, idempotency key, request time, API request ID,
   operation/allocation ID and observed result. Do not store the bearer token.
4. Handle `201`/`200` as a returned allocation to validate, `202` as a pending
   tenant-only operation to poll, and conflict/failure/timeouts as explicit
   states. A lost response retries the same request identity. A returned CIDR
   different from the reviewed candidate requires owner review before it is
   counted as verified.
5. Keep reservation verification separate from migration execution. The
   browser never runs Terraform, releases a hold as an implicit rollback, or
   marks network reachability ready.

## Alternatives needing an owner choice

- Keep the current CLI/API handoff permanently. This avoids browser tenant
  credentials and makes PILOT-3's browser reservation requirement a deliberate
  scope change rather than an implementation claim.
- Use a dedicated service identity with explicit tenant grants. This needs a
  new authorization and audit contract and cannot reuse the read-only
  deployment-wide operator role.

## Acceptance before implementation

Record the chosen identity pattern and plan approval authority. Test denied
cross-tenant access, operator write refusal, stale proposal, plan change,
concurrent conflict, lost response, pending operation, failed operation,
retry and returned-CIDR mismatch with the actual API. Qualify the selected
SSO path with a real identity provider before production admission.
