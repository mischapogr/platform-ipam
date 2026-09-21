# ADR 0006: One proxy in front of the NetBox UI, with basic, Entra ID and LDAP modes

Status: accepted, 2026-09-18. Implemented for Compose in all three modes and,
for `basic` mode, as an optional part of the Helm chart; see the implementation
note at the end for what was verified and against what.

## Context

The only web interface in the system is the NetBox UI, and it has exactly one
way in. NetBox enforces login by default, so the inventory is not readable
anonymously, but there is a single bootstrapped superuser and no group or
read-only role, which means every person who can log in at all is an
administrator; `CORS_ORIGIN_ALLOW_ALL` is on; and the loopback port binding is the only
thing standing between the UI and a network. The design documents have asked
for an SSO-protected URL and read-only operator roles since the first plan,
as the optional U1 phase, but nothing implements them. The Helm chart exposes
the API only and assumes NetBox is someone else's release.

Operators need to get in under three different circumstances: where Microsoft
Entra ID is the identity provider, where an on-premises Active Directory is
reachable only over LDAP, and where neither is connected yet and a shared
fallback has to work on day one.

The platform API is not part of this problem. It accepts bearer tokens only
and rejects every other scheme before looking at the credential. Teaching a
machine API that already has workload identity to accept HTTP Basic would add
a weaker credential and gain nothing. NetBox's REST API and HTTP Basic also
both claim the `Authorization` header, so they cannot share a path.

## Decision

A reverse proxy becomes the only published path to the NetBox UI, and one
setting, `NETBOX_UI_AUTH_MODE`, selects `basic`, `entra` or `ldap`.

In `basic` mode the proxy verifies a bcrypt-hashed credential and passes the
user name to NetBox in a trusted header. In `entra` mode `oauth2-proxy`
performs the OIDC login against Entra ID and passes user and groups the same
way. In `ldap` mode the proxy only routes and terminates TLS, and NetBox binds
to Active Directory through its own LDAP backend. `basic` is the fallback for
an environment with no identity provider, not a peer of the other two.

Entra goes through `oauth2-proxy` rather than NetBox's built-in social login
so that `basic` and `entra` share one trust path: one NetBox configuration and
one set of security tests cover both.

Header trust is safe only if the proxy is the sole way in, so NetBox's own port
is never published, the proxy deletes client-supplied identity headers before
authenticating, and the proxy refuses `/api/` and `/graphql/`. The platform
adapter continues to reach NetBox on the internal network with its token.

Everyone who authenticates lands in a read-only `platform-operators` group.
Write access is a separate explicit grant, and the platform service account
remains the only routine writer. The local superuser stays as break-glass and
is not reachable through the proxy.

A platform-owned UI is not decided here. It would need browser sessions, CSRF
protection and a role on the principal, and it would reverse the rule against
building a frontend for v1. It gets an options paper and its own record before
any code.

## Consequences

Operators stop sharing a superuser, and the same proxy works in Compose
and in Kubernetes, so what is tested locally is what runs.

Only `basic` can be verified completely on a developer machine. `entra` is
verifiable against a mock OIDC issuer and `ldap` against an OpenLDAP-compatible
server, which proves the wiring but not the behaviour of a real tenant or real
domain controllers: conditional access, group claims, nested groups, LDAPS
trust chains. Those stay onboarding work and must not be reported as verified.

HTTP Basic has no logout, sends the credential on every request, and here
means a static user list with no rotation workflow. That is acceptable for a
fallback and would not be for a target state.

Moving the published port from NetBox to the proxy changes how the end-to-end
harness reaches NetBox's REST API, and the browser smoke test must go through
the proxy to test the real path.

This controls who gets in and with what default role. It does not constrain a
NetBox administrator, who still outranks every viewer. Where NetBox is operated
by another team, the requirement that it be reachable only through the proxy is
theirs to meet, and the chart can only document it.

## Implementation note (2026-09-18)

Packages A1-A8 built this decision as designed, with one deviation from the
Decision above: no file sets or reads the `NETBOX_UI_AUTH_MODE` variable
this decision names. The mode a Compose stack runs in is instead selected
by which optional overlay is layered on top of the shared
`compose.netbox.yaml` -- no overlay for `basic`, `compose.ui-entra.yaml`
for `entra`, `compose.ui-ldap.yaml` for `ldap` -- because each mode needs
NetBox configured differently (`ldap` mode in particular needs
`REMOTE_AUTH_ENABLED=false`, where `basic` and `entra` need it `true`) and
an overlay leaves the files the other modes already use untouched, so the
three never fight over one service definition. `basic` mode is verified
against the real Compose stack, with nineteen end-to-end tests across two
files. Five guards were removed by hand, one at a time: removing the API
block, the credential check or the health path each made a test fail, while
removing either header-strip guard did not, because the proxy's final
assignment of the identity header overwrites whatever arrived. Those two
guards are therefore defence in depth and not what stops a forgery today. `entra`
mode is verified against a mock OIDC issuer only, and `ldap` mode against
an OpenLDAP-compatible test server only; neither has been run against a
real Microsoft Entra ID tenant or a real Active Directory, exactly as the
Consequences above anticipated. An optional `ui-proxy` was also added to
the Helm chart, implementing `basic` mode only, verified by linting and
rendering the chart and by running its rendered Caddyfile under the pod's
exact security constraints, never against a running cluster. Full detail,
including the settings table corrected against what actually shipped, is
in `docs/GUI_AUTHENTICATION.md`.
