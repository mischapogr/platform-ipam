# ADR 0008: No platform-owned operator UI, and a NetBox plugin if that changes

Status: proposed, 2026-09-18. See the [work plan](../WORK_PLAN.md), package A9.

## Context

[ADR 0006](0006-OPERATOR_UI_AUTHENTICATION.md) gives operators an authenticated
browser path, but only to NetBox. It leaves open whether the platform should
serve a surface of its own, and names three things such a surface would need
that do not exist: browser sessions, CSRF protection and a role on the
principal.

The API has never had a browser as a client: thirteen routes that answer JSON or
plain text, no HTML, no template, no static mount
(`internal/transport/http.go:46-77`). The authenticator requires exactly one
`Authorization` header and only the `Bearer` scheme
(`internal/transport/auth.go:55-63`); in `oidc` mode it is a resource-server
verifier that checks an access token obtained elsewhere and refuses one whose
`token_use` is not `access` (`internal/transport/auth.go:46-53`, `71-82`). There
is no cookie, no session store, no CSRF token and no CORS header anywhere in the
service.

Authorization is by tenant, not by role: `domain.Principal` carries a subject, a
tenant, accounts, environments and regions and nothing else
(`internal/domain/types.go:31-37`), and every read compares the caller's tenant
with the object's: allocations at `internal/service/service.go:282` and `295`,
findings at `596`, and capacity, which also requires the pool to list that
tenant as eligible (`internal/service/capacity.go:54`). An operator who is not a
tenant gets no error from those endpoints. They get an empty list, which reads
as "nothing allocated". The identity file cannot express "read everything"
(`deploy/compose/fixtures/identities.yaml`).

What the platform already offers a browser is a link: with
`ui.inventory_links_enabled` set, every allocation response carries
`links.inventory` into the NetBox prefix view
(`internal/transport/http.go:455-456`). `docs/NETBOX_INTEGRATION.md:9` forbids a
new frontend for v1 or one mounted into the platform API, and section 6 names
the four questions the native views answer badly: capacity by allowed block
size, held space by lifecycle, AWS domain health, and the quarantine queue. All
four come from `GET /v1/pools/{id}/capacity` and `GET /v1/findings`, which
NetBox never receives.

## Decision

Doing nothing isolates perfectly: no new process can take allocation down, and
NetBox behind the ADR 0006 proxy already answers pool → VPC → subnet, ownership
and lifecycle. Its price is those four questions. The CLI's `capacity` command
answers one (`internal/cli/cli.go:110-126`); findings have no client command at
all, so the quarantine queue and AWS domain health are a `curl` and a terminal.

A read-only UI over the existing `GET` endpoints, served separately from the
API, looks cheapest and is not. It needs a session cookie with a store, an
expiry and a logout; CSRF protection, because the moment that origin has a
logout or a filter-saving POST an ambient cookie is exploitable; and an OIDC
authorization-code client with a redirect URI, a client secret, state and nonce
— a different program from the verifier in `internal/transport/auth.go`, not a
setting on it. It needs a role on `domain.Principal`, a cross-tenant read path
because an operator who is not a tenant sees nothing, and CORS if it is not
same-origin. Only the session and the CSRF token live in the UI; the role and
the cross-tenant read live in `internal/service`, on the allocation path. That
is where its isolation is weaker than a separate deployment suggests: the UI can
fall over harmlessly, but the change that makes it useful loosens the
authorization of the allocator itself. It also reverses
`docs/NETBOX_INTEGRATION.md:9` outright.

A NetBox plugin with custom views needs none of the browser machinery. It
inherits NetBox's session, login, CSRF and the read-only `platform-operators`
group that package A2 builds, so ADR 0006 pays for its authentication once. It
still needs the platform side: `docs/NETBOX_INTEGRATION.md:104` requires a
plugin to preserve tenant authorization and forbids one unrestricted service
token from exposing cross-tenant detail, so the role returns, now as a mapping
from NetBox users to platform principals. It couples Python to the pinned NetBox
image's release, and inverts the isolation: allocation cannot be hurt by it, but
NetBox pages can, which is why that paragraph demands short timeouts, cached
data with a freshness stamp, and degrading to unavailable rather than blocking.

The recommendation is to build no platform-owned UI. NetBox behind the ADR 0006
proxy, with the read-only group and the existing `links.inventory` deep link, is
the operator surface for v1, and the cheapest real improvement is a `findings`
command in the CLI, not a frontend. If a UI later becomes necessary it should be
the NetBox plugin rather than the standalone application, because the plugin
reuses browser authentication that is already being built instead of inventing a
second one.

The trigger to revisit is one event: the first time someone must answer the
capacity or the quarantine question for a tenant they do not belong to. No
option here serves that without a role on `domain.Principal` and a cross-tenant
read, so when it arrives the expensive part must be built regardless and only
the rendering is still open.

## Consequences

The "no new frontend for v1" rule survives intact, and the release pipeline
keeps building one static Go binary and a chart with no asset step
(`.github/workflows/release.yml`). A frontend would add a build system nobody
here maintains. The price is that two of the four section 6 questions stay in a
terminal. That is accepted because no operator has used this stack yet: the
browser check is opt-in (ADR 0002) and the read-only group is not built. The
claim that the native views are insufficient is inherited from the design
documents, not observed.

What is unknown is stated plainly. Whether NetBox 4.6.7's dashboard-widget
mechanism can host these views has not been checked — that is package N1, and if
it cannot, the fallback here is weaker than it appears. Whether an identity
provider will permit an authorization-code client is onboarding work in every
environment, as ADR 0006 records for Entra. How many tenants will exist is
unknown, and with one the deferred role field is nearly free while with many it
is the whole problem. The Helm values schema is closed with `auth.mode` limited
to `oidc` and `local` (`deploy/helm/platform-ipam/values.schema.json:31-34`), so
either UI option is a schema change too.

If the plugin is ever built, the rule from `docs/NETBOX_INTEGRATION.md:104`
binds it: it is a view, never an allocation writer.
