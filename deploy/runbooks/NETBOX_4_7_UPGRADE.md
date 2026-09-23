# NetBox 4.7.1 upgrade and rollback

The qualified local Compose image is
`docker.io/netboxcommunity/netbox:v4.7.1-5.1.1@sha256:59e3e5954d0243d8648f2fa7ba82cf7c7770aaeef392026cca8b21cd67411e74`.
The previous qualified local image is retained in
`deploy/compose/compose.netbox-compat-4_6_10.yaml`. NetBox in stage and
production is a separate release; this runbook does not promote either one.

## Before an upgrade

1. Review the [NetBox 4.7 release notes](https://netbox.readthedocs.io/en/stable/release-notes/version-4.7/). NetBox now requires PostgreSQL 15 or later, the `ltree` extension and database `CREATE` privilege for its installer; the local stack uses PostgreSQL 17. The hierarchy migration can hold locks and take minutes. Schedule a write pause for the affected NetBox, its worker and the platform worker.
2. Record the exact source image, database version, plugins, configuration, secrets and media volumes. Back up the NetBox database (`pg_dump -Fc`) and media separately. Keep the source database and volumes intact while rehearsing on a new project or volume.
3. Export a comparison set of managed NetBox prefixes: ID, CIDR and complete `custom_field_data`, ordered by ID. Record the row count and checksum. Also record the platform ledger's committed allocation IDs and inventory IDs. A database restore by itself does not reconcile later platform writes.
4. Check plugin compatibility and identity settings against the exact new image. A successful image pull or Compose configuration check is not a migration result.

## Rehearse and promote

1. Restore the pre-upgrade database backup into an isolated PostgreSQL database with the same NetBox secrets. Start the exact 4.7.1 image there; never start the old image against a database already migrated by 4.7.
2. Wait for NetBox health, then run `manage.py migrate --check`. Compare the managed-prefix count and checksum to the pre-upgrade export. Read a managed prefix through the authenticated REST API. Confirm the platform adapter can reserve, read, update, release and detect inventory drift without an unexplained prefix.
3. If the optional AWS plugin is used, build it from the exact 4.7.1 base image, apply its migrations, check its worker health and read `/api/plugins/aws-vpc/aws-accounts/` with a scoped token. Test the selected UI identity mode and the operator/maintainer permissions.
4. Run the end-to-end suite against the isolated stack. Promote the same digest through the owned deployment process only after the target environment's backup/restore and identity gates pass. Verify the platform ledger and NetBox still agree after resuming writers.

For a disposable local project with a previously unused name and free loopback
ports, the checked-in runner can perform the fresh-stack gate without touching
the shared development project's volumes:

```sh
COMPOSE_PROJECT_NAME=platform-ipam-e2e-netbox-471 \
IPAM_API_PORT=18190 NETBOX_PORT=18191 \
IPAM_E2E_RUN_ID=netbox471-check \
sh tests/e2e/run-e2e.sh
```

The runner resets allocation state in the named project; choose a new name
before invoking it. The `IPAM_E2E_COMPOSE_OVERLAY` option accepts an extra
Compose file when qualifying a future candidate before changing the default.

The 2026-09-23 local rehearsal restored a 4.6.10 database containing 1,035
managed prefixes into a separate volume. After the 4.7.1 migration NetBox was
healthy, `migrate --check` passed, and the count and ID/CIDR/custom-field
checksum remained `1035` and
`2618f68319fb9cbec42d40550425cf2d3e4f784e3b07aa388aee73027d95e562`.
The fresh isolated 4.7.1 stack passed all 126 end-to-end tests. The optional
AWS plugin built, applied five migrations, started its worker and returned HTTP
200 from its accounts API. Six mock-Entra and seven OpenLDAP UI tests passed
on separate fresh projects. These are local results, not stage or production
upgrade evidence.

## Rollback

Stop NetBox and platform writers. Do not run an older NetBox image on the 4.7
database: the 4.7 hierarchy migration is not practically reversible. Restore
the complete pre-upgrade database, media and matching configuration/secrets to
a separate target, start the recorded 4.6.10 image, and verify managed-prefix
IDs, CIDRs and fields before reopening writes. Reconcile any platform ledger
operations accepted after the backup with the restored inventory; retain holds
or fence allocation until that comparison is complete. Preserve the failed
4.7 database for diagnosis rather than overwriting the only evidence.
