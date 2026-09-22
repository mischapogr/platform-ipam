# ADR 0019: Qualify exact NetBox release images

Status: accepted as a support policy, 2026-09-23. Promotion of the candidate image is pending its remaining gates.

## Context

The default Compose stack pins `netboxcommunity/netbox:v4.6.7-5.0.2`. An isolated Compose probe of `v4.6.10-5.0.2` passed 14 allocation/projection, three role and six UI-proxy tests after bootstrap and seed. The locally pulled candidate reports `netboxcommunity/netbox@sha256:91b823a05cb51004f07acc2228ccc2993f38b0f0bf711b403b0fdf85e51277e8`; this is evidence of the local image, not yet a promoted deployment pin. That probe does not cover the full suite or the optional AWS plugin, whose Dockerfile still builds from 4.6.7. Two tested patch images do not establish compatibility with every 4.6 patch.

[NetBox's upgrade guidance](https://netboxlabs.com/docs/netbox/installation/upgrading/) calls for reviewing release notes, backups and plugin compatibility. [NetBox 4.7](https://netboxlabs.com/blog/netbox-4-7-is-ga/) changes dependencies and has a substantial database migration; it is a separate qualification target.

## Decision

- The v1 support claim names an exact NetBox and netbox-docker image version and digest. The first candidate is `v4.6.10-5.0.2`. Keep 4.6.7 as a proven upgrade source while qualifying the candidate; do not promise all `4.6.x` or 4.7.
- Promote 4.6.10 only after the full development-stack suite, `seed`, basic/Entra-shaped/LDAP-shaped UI gates, optional AWS-plugin build and migration, and backup/restore upgrade rehearsal from the pinned 4.6.7 data. Compare managed-prefix identities and custom fields before and after. Record the exact image digest in deployment values.
- Qualify 4.7 separately with the same gates and its own upgrade/rollback runbook. Do not automatically float the image tag across minor releases. An unsupported or untested image is not an allocation-safety input.
- Re-run this matrix for each proposed NetBox image promotion. Keep the adapter's fail-closed behavior on unknown or incomplete inventory responses.

## Current limit

The 4.6.10 targeted probe passed on a clean ledger. A later repeat on its reused LS4 ledger hit the test quota; the fresh `platform-ipam-e2e-m4g` project passed all 126 end-to-end tests. The optional AWS plugin image built from the candidate digest, all five plugin migrations applied on the isolated LS4 database, its worker became healthy, and its accounts API returned HTTP 200. Entra-shaped and LDAP-shaped UI runs on this exact image and a 4.6.7 backup/restore upgrade rehearsal remain promotion gates. Stage and production still need their own deployment and restore evidence.
