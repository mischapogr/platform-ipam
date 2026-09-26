# ADR 0019: Qualify exact NetBox release images

Status: accepted as a support policy, 2026-09-23. The qualified 4.7.1 image is now the local Compose default; stage and production require their own rollout evidence.

## Context

The original Compose stack pinned `netboxcommunity/netbox:v4.6.7-5.0.2`. An isolated Compose probe of `v4.6.10-5.0.2` first passed 14 allocation/projection, three role and six UI-proxy tests after bootstrap and seed. The candidate image's observed registry digest is `netboxcommunity/netbox@sha256:91b823a05cb51004f07acc2228ccc2993f38b0f0bf711b403b0fdf85e51277e8`. The later full suite, identity and restore gates below qualified that exact image for local use. Two tested patch images do not establish compatibility with every 4.6 patch.

[NetBox's upgrade guidance](https://netboxlabs.com/docs/netbox/installation/upgrading/) calls for reviewing release notes, backups and plugin compatibility. [NetBox 4.7](https://netboxlabs.com/blog/netbox-4-7-is-ga/) changes dependencies and has a substantial database migration; it is a separate qualification target.

## Decision

- The v1 support claim names an exact NetBox and netbox-docker image version and digest. The first qualified local image is `v4.6.10-5.0.2`. Keep 4.6.7 as a proven upgrade source for future rehearsals; do not promise all `4.6.x` or 4.7.
- Promote 4.6.10 only after the full development-stack suite, `seed`, basic/Entra-shaped/LDAP-shaped UI gates, optional AWS-plugin build and migration, and backup/restore upgrade rehearsal from the pinned 4.6.7 data. Compare managed-prefix identities and custom fields before and after. Record the exact image digest in deployment values.
- Qualify 4.7 separately with the same gates and its own upgrade/rollback runbook. Do not automatically float the image tag across minor releases. An unsupported or untested image is not an allocation-safety input.
- Re-run this matrix for each proposed NetBox image promotion. Keep the adapter's fail-closed behavior on unknown or incomplete inventory responses.

## Qualification and current limit

The 4.6.10 targeted probe passed on a clean ledger. A later repeat on its reused LS4 ledger hit the test quota; the fresh `platform-ipam-e2e-m4g` project passed all 126 end-to-end tests. The optional AWS plugin image built from the candidate digest, all five plugin migrations applied on the isolated LS4 database, its worker became healthy, and its accounts API returned HTTP 200. On separate 4.6.10 projects, the six mock-Entra UI tests and seven OpenLDAP UI tests passed; the latter also verified both LDAP users were not superusers. These are local identity simulations, not real Entra or Windows AD validation.

A `pg_dump -Fc` backup of the stopped 4.6.7 `platform-ipam-e2e-20260923a` NetBox database was restored into a fresh isolated database. Before upgrade, all 31 managed prefixes matched the source by NetBox ID, CIDR and the complete custom-field JSON. The 4.6.10 image started on that restored database, reported healthy and no unapplied migrations, and the same 31 records matched byte-for-byte at those fields afterward. A restored API token read one managed prefix with HTTP 200. The source and rehearsal project volumes were retained. This exercises local restore and forward upgrade; it does not validate a stage/production backup system or rollback.

The exact `v4.6.10-5.0.2@sha256:91b823a05cb51004f07acc2228ccc2993f38b0f0bf711b403b0fdf85e51277e8` reference was pinned in the Compose NetBox services and optional plugin build before the 4.7.1 qualification below. `compose.netbox-compat-4_6_7.yaml` retains the earlier image as an explicit upgrade source. The external NetBox release used by stage and production is outside this platform Helm chart; neither environment has been upgraded or restore-tested here.

## 4.7.1 local qualification (2026-09-23)

The local Compose default and optional plugin base are now pinned to
`v4.7.1-5.1.1@sha256:59e3e5954d0243d8648f2fa7ba82cf7c7770aaeef392026cca8b21cd67411e74`.
The NetBox 4.7 release changes selection custom-field reads from a raw string
to a `{value, label}` object. The adapter now compares the raw `value` for
the two selection fields, and the affected operator-inventory checks accept
both shapes. The 4.6.10 image remains in
`compose.netbox-compat-4_6_10.yaml` as a qualified upgrade source.

An isolated 4.7.1 stack passed all 126 end-to-end tests after the adapter
change, including REST, CLI, Terraform, import/adoption, roles and UI proxy
paths. A separate rehearsal restored a 4.6.10 database with 1,035 managed
prefixes into a new volume. NetBox reached healthy on 4.7.1,
`migrate --check` passed, and the count and ordered ID/CIDR/custom-field
checksum were unchanged:
`2618f68319fb9cbec42d40550425cf2d3e4f784e3b07aa388aee73027d95e562`.
The optional AWS plugin built from the exact image, applied all five plugin
migrations on that database, started its worker and served its accounts API
with HTTP 200. Separate fresh 4.7.1 projects passed six mock-Entra UI tests
and seven OpenLDAP UI tests; the LDAP viewer and maintainer were both
non-superusers. These are local identity simulations, not real Entra or
Active Directory validation. [The upgrade runbook](../../deploy/runbooks/NETBOX_4_7_UPGRADE.md)
records the separate restore-based rollback route; the 4.7 database must not
be opened with a 4.6 image. Stage and production remain unpromoted.
