# ADR 0024: Browser reservation identity and review authority

Status: accepted for the pilot, 2026-10-14. Browser reservation and tenant SSO
are post-pilot enhancements. This record preserves ADR 0015's reviewed-file
authority and does not authorize a browser allocation write.

## Context

The NetBox migration workspace authenticates a deployment-wide operator. The
allocation API's operator principal has no tenant and cannot reserve or read a
tenant's pending operation. A NetBox session is not an allocation API bearer
token, and the workspace has no reviewed mapping from a human to a tenant.
Saved pilot snapshots are visible to admitted workspace operators; they are
not tenant-private views. Their stored digest detects changes, but is not a
signature or an access grant.

ADR 0015 makes the customer's reviewed `migration.yaml` the authority for
move intent. The browser currently renders that file's derived evidence and
stores immutable report snapshots. An approval stored only in NetBox would
create a second plan authority. A reservation made from a saved snapshot may
also use stale inventory or a stale advisory CIDR. The API selects and holds
the actual CIDR; the suggested CIDR is never a claim on capacity.

## Pilot decision

The browser is an operator-only, read-only projection of uploaded plan,
inventory and reservation verification evidence. It displays advisory
reservation candidates, the stable allocation key, returned allocation ID,
CIDR and verification state, with an authenticated CLI/API handoff. It does
not collect, receive, retain or display tenant tokens; it makes no allocation
API calls and provides no approval, reserve, edit or release action. A saved
snapshot stays immutable. A new CLI verification report and snapshot are
needed to show a later allocation state; the read time and expiry remain
visible. An expired, missing or mismatched report cannot establish a current
hold.

The owning tenant creates or modifies allocations using the existing
authenticated CLI/API. The API derives tenant identity server-side from the
credential; it refuses cross-tenant reads and writes and operator writes.
Keep the CLI/API response, request or operation ID, allocation ID, audit
events and verification report as reservation evidence. Use one stable
allocation key and request identity across retries. A `202` operation is
polled by the same tenant; failures, conflicts and a returned CIDR differing
from the reviewed candidate need explicit owner review. The browser never
runs Terraform or treats reservation as network migration.

Workspace admission is enforced on the server through NetBox authentication
and the `platform-operators` group or superuser role. It does not create
tenant isolation for operator-shared snapshots. Pilot operators must be
authorized to see the whole uploaded pilot estate. Tenant-specific browser
views require a real tenant identity mapping and are outside this pilot.

## Post-pilot enhancement and acceptance

Browser-based reservation requires an approved tenant SSO contract and a real
OIDC provider. Before implementation, specify issuer, audience, subject-to-
tenant mapping, allowed groups, token lifetime, refresh and revocation,
session binding, delegated scopes, logout and audit retention. No mock issuer,
browser-side tenant selector, shared bearer token or NetBox-session shortcut
qualifies as that contract.

Acceptance requires all of the following:

1. The server validates the real OIDC token and derives the tenant and write
   permission from reviewed claims or server-side mapping. A normal user
   cannot read or mutate another tenant's allocations or saved tenant views;
   a deployment operator cannot reserve merely by having workspace access.
2. Every browser write is bound to one reviewed plan digest, move, tenant and
   stable allocation key. Stale plans or inventory require new review.
   Conflicts, concurrent attempts, lost responses, pending and failed
   operations, retries and returned-CIDR mismatches are explicit states.
3. An append-only audit record links the human actor, tenant, plan and move
   digests, request identity, API request or operation ID, allocation ID and
   observed result, without storing bearer credentials. A new verified read
   updates the browser projection without rewriting old evidence.
4. Integration tests exercise the complete browser-to-API path against a
   **real OIDC provider** with positive and denied cross-tenant cases,
   expiration and revocation, operator write refusal, conflict, retry and
   audit assertions. Mock OIDC and static checks remain development evidence
   only. The target deployment must also pass its real identity admission and
   restore checks before production use.
