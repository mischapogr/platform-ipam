# Development Compose

Create local credentials and start the platform plus the independent NetBox
release:

```sh
cd deploy/compose
./create-env.sh
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml up --build -d
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml \
  --profile bootstrap run --rm netbox-bootstrap
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml \
  --profile bootstrap run --rm seed
```

`netbox-bootstrap` must run before `seed`. It creates the development
superuser and the API token the platform adapter authenticates with; `seed`
needs that token and cannot create one, because a NetBox token can only be
issued from inside NetBox's own Django environment. The image's built-in
`SUPERUSER_*` bootstrap is not a substitute: it issues a v2 token
(`Authorization: Bearer nbt_<key>.<secret>`), while the adapter sends the
legacy v1 header (`Authorization: Token <secret>`). A v1 token's plaintext is
validated at exactly 40 characters, which is why `create-env.sh` generates
`IPAM_NETBOX_TOKEN` at that length and the bootstrap refuses any other.

The bootstrap also creates the `e2e-viewer` and `e2e-maintainer` accounts used
by `tests/e2e/test_e2e_netbox_roles.py`. `create-env.sh` generates separate v1
tokens for them. The viewer token is read-only; the maintainer token permits
writes, while the maintainer group's object permissions limit those writes to
adding and changing prefixes and IP ranges. An existing `.env` can opt into
the maintainer test without replacing its database credentials:

```sh
printf 'NETBOX_E2E_MAINTAINER_TOKEN=%s\n' "$(openssl rand -hex 20)" >> .env
```

Rerun `netbox-bootstrap` before the e2e test so NetBox receives that token.

[`tests/e2e/run-e2e.sh`](../../tests/e2e/README.md) performs all of the above
and then runs the end-to-end suite.

To add the read-only overlap, address planning, migration and topology evidence workspace to the local NetBox UI,
layer `compose.netbox-workspace.yaml` after the two base files. It builds a
small first-party plugin image from the pinned NetBox release and the current
Platform-IPAM binary. The page is `/plugins/platform-ipam/` on the configured
`NETBOX_PORT`; see [the workspace guide](../../docs/MIGRATION_WORKSPACE.md).
The overlay does not enable the separate optional AWS-account plugin.

## A second local development identity

`create-env.sh` also generates `IPAM_OPS_TOKEN`, the same way and at the same
length as `IPAM_LOCAL_TOKEN`. `compose.yaml` passes it to the platform as
`IPAM_LOCAL_EXTRA_CREDENTIALS=ops-observer:${IPAM_OPS_TOKEN}` (package G3c),
a second local-mode identity honoured alongside `IPAM_LOCAL_TOKEN`/
`IPAM_LOCAL_SUBJECT`. It authenticates as `ops-observer`, tenant `ops`
(`fixtures/identities.yaml`), which `fixtures/pools.yaml` lists in no pool's
`eligible_tenants` -- on purpose, so `tests/e2e/test_e2e_second_identity.py`
can exercise the "authenticated but eligible for nothing" path that a single
development identity could never reach. `IPAM_LOCAL_EXTRA_CREDENTIALS` is
honoured only in `local` authentication mode and is never valid outside
`IPAM_ENVIRONMENT=development`; it is development-only, so Helm carries no
equivalent.

`create-env.sh` refuses to touch an `.env` that already exists (see below),
so on an existing `.env` predating this variable, add the one line by hand
instead of regenerating the whole file:

```sh
printf 'IPAM_OPS_TOKEN=%s\n' "$(openssl rand -hex 32)" >> .env
```

## The operator identity

`create-env.sh` also generates `IPAM_OPERATOR_TOKEN`, the same way and at the
same length as `IPAM_LOCAL_TOKEN` and `IPAM_OPS_TOKEN`. `compose.yaml` adds it
as a second `IPAM_LOCAL_EXTRA_CREDENTIALS` pair,
`ops-operator:${IPAM_OPERATOR_TOKEN}` (package G3b1, [ADR 0011](../../docs/decisions/0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md)).
It authenticates as `ops-operator`, which `fixtures/identities.yaml` gives
`role: operator` and nothing else -- no `tenant_id`, `accounts`,
`environments` or `regions`. An operator is a principal with **no tenant**,
so every existing tenant comparison in `internal/service` refuses it before
any new code runs: `tests/e2e/test_e2e_operator_role.py` shows the resulting
deny-by-default state end to end. The role now grants five cross-tenant
reads, one at a time, each in its own named place (packages G3b2-G3b4):
`GET /v1/pools` returns every configured pool rather than only an eligible
subset; `GET /v1/pools/{id}/capacity` answers for any pool id; `GET
/v1/allocations` and `GET /v1/allocations/{id}` return committed allocations
across every tenant, carrying an additional `tenant_id` field a tenant's own
response never gains; and `GET /v1/findings` returns the whole estate, with
the per-tenant fan-out behind a domain-level code such as
`unmanaged_occupancy` collapsed to one row per resource, carrying
`domain_id`, `resource_type` and `resource_id`, and `tenant_id` only where
the row names an allocation. `GET /v1/operations/{id}` stays refused by
design, not by omission (ADR 0011). Every write — `POST`, `PATCH`, `PUT
.../binding`, `DELETE` — is refused for the role exactly as for any
principal with no tenant, and each operator read is logged with `slog` at
the transport layer (subject, method, path, status, rows returned), never
into the ledger's own audit trail. `tests/e2e/test_e2e_operator_role.py` and
`tests/e2e/test_e2e_operator_gate.py` show these reads, the write refusals,
and the `client findings --fail-if-open` gate judged on the whole estate,
end to end. `IPAM_LOCAL_EXTRA_CREDENTIALS` is honoured only in `local`
authentication mode and only in development, exactly as for `ops-observer`
above; Helm carries no equivalent.

On an existing `.env` predating this variable:

```sh
printf 'IPAM_OPERATOR_TOKEN=%s\n' "$(openssl rand -hex 32)" >> .env
```

A `role` key in the identity file is a forward-incompatible configuration
change: a binary built before `domain.Principal.Role` existed rejects a file
that carries it (strict decoding), rather than silently ignoring the field.
Deploy the binary before a file that uses `role`; roll the file back before
rolling the binary back.

The API is published only on `127.0.0.1:8080` (`IPAM_API_PORT` moves it when
another project already holds that port). NetBox's own port is **not**
published by anything; `scripts/ai/check-compose` fails the build if it ever
is again. The only published path to the NetBox UI is `ui-proxy`
(`deploy/compose/ui-proxy/`), on `127.0.0.1:18000` by default -- `NETBOX_PORT`
can override that loopback port, for both `ui-proxy` and the internal target
it proxies to. This is package A3 of
[the operator UI authentication work](../../docs/GUI_AUTHENTICATION.md)
([ADR 0006](../../docs/decisions/0006-OPERATOR_UI_AUTHENTICATION.md)), in its
`basic` mode:

* Sign in at `http://127.0.0.1:18000/` with an HTTP Basic prompt, using
  `NETBOX_UI_USER` (default `operator`) and `NETBOX_UI_PASSWORD` from `.env`.
  `create-env.sh` generates both; only the plaintext password lives in
  `.env` -- `ui-proxy/entrypoint.sh` hashes it with `caddy hash-password`
  (bcrypt) at container start, so a rotated password takes effect on the
  service's next recreate with no separate hash to regenerate.
* ui-proxy deletes any client-supplied `X-Remote-User`/`X-Remote-User-Group`
  header before authenticating, then tells NetBox who signed in through that
  same header, trusted only because ui-proxy is the sole way in. The
  authenticated person lands in NetBox's read-only `platform-operators` group
  (`deploy/compose/netbox/bootstrap-groups.py`, package A2) -- there is no
  self-service write access through this path.
* ui-proxy refuses `/api/` and `/graphql/` outright (403), on any credential.
  The platform adapter is unaffected: it keeps talking to
  `http://netbox:8080` on the internal Compose network, and never crosses
  this proxy. `tests/e2e/harness.py`'s `netbox()`/`netbox_as()` do the same
  for the end-to-end suite; its `ui()` helper is the one that goes through
  `ui-proxy` instead.
* The local NetBox superuser (`NETBOX_SUPERUSER_NAME`/`_PASSWORD`) remains as
  break-glass and is reachable only from inside the Compose network (for
  example `docker compose exec netbox ...`), never through `ui-proxy` or any
  published port.

`entra` and `ldap` are the other two modes ([GUI authentication](../../docs/GUI_AUTHENTICATION.md)),
each an optional overlay layered on top of the commands above -- `basic` mode is what the stack runs
with neither:

* `entra` mode (package A4) puts [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/) in
  front of `ui-proxy`, logging a person in through OIDC instead of a Basic prompt. Layer
  `-f compose.ui-entra.yaml` after `compose.netbox.yaml` to start it, or run
  [`tests/e2e/run-ui-entra.sh`](../../tests/e2e/README.md), which brings the whole thing up in its
  own throw-away Compose project against a development-only mock OIDC issuer and tears it down when
  done. **Verified against that mock issuer only -- not against a real Microsoft Entra ID tenant.**
* `ldap` mode (package A5) drops `basic_auth` from `ui-proxy` entirely and has NetBox's own login
  form bind to a directory through its LDAP backend instead. Layer `-f compose.ui-ldap.yaml` after
  `compose.netbox.yaml`, or run [`tests/e2e/run-ui-ldap.sh`](../../tests/e2e/README.md) against its
  own throw-away project and a bundled OpenLDAP test directory. **Verified against that test
  directory only -- not against a real Active Directory.**

For a closer local AD protocol test, use `tests/e2e/run-ui-samba-ad.sh`
with its `up`, `wait`, `bootstrap`, `test` and `stop` phases. It layers
`compose.ui-samba-ad.yaml`, pins a Samba AD image by digest, and tests nested
groups and LDAPS. The Samba service is privileged as its upstream image
requires; it publishes no host port. Its isolated volumes are retained by
`stop`. `tests/e2e/run-aws-moto.sh up|test|stop` separately exercises the
real AWS SDK EC2/STS read path against an isolated, pinned Moto service.
Neither overlay changes the default fake-cloud development stack. See
[local simulation](../../docs/LOCAL_SIMULATION.md) for the evidence boundary.

`platform-db` is the application ledger. NetBox has its own PostgreSQL and
Valkey data volumes, and its image is pinned to exact NetBox/netbox-docker
`v4.7.1-5.1.1` digest `sha256:59e3e5954d0243d8648f2fa7ba82cf7c7770aaeef392026cca8b21cd67411e74`.
The qualified 4.6.10 source image remains available through
`compose.netbox-compat-4_6_10.yaml` for isolated upgrade rehearsals; see
[ADR 0019](../../docs/decisions/0019-EXACT_NETBOX_RELEASE_SUPPORT.md) and the
[4.7 upgrade runbook](../runbooks/NETBOX_4_7_UPGRADE.md).
The fake cloud observation file is `fixtures/cloud.json` and is mounted
read-only into the platform services.

The `seed` service runs the repeatable development-only Python bootstrap for
NetBox custom fields, the development VRF, and the stable `pool_dev_euc1`
container. The same script can be run directly when the stack is already
running:

```sh
NETBOX_URL=http://127.0.0.1:18000 NETBOX_TOKEN=local-netbox-development-token \
  python3 seed-netbox.py
```

Ordinary `docker compose down` preserves all named volumes. To inspect
readiness and migration completion:

```sh
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml ps
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml logs --since=10m migrate api worker
```

Use a deliberately named project before removing development data:

```sh
docker compose --env-file .env -p platform-ipam-dev -f compose.yaml -f compose.netbox.yaml down -v
```

The stack never enables live AWS mode or production identity. To use an
external development NetBox, start only `compose.yaml` and set
`IPAM_NETBOX_URL` in `.env` to a reachable development endpoint.

`create-env.sh` refuses to create a new `.env` when it detects an existing
NetBox database volume. NetBox database credentials are persisted with that
volume; restore its matching `.env` rather than generating new credentials.
If the current `.env` must become authoritative for an existing local volume,
first take a database backup and then update only the local `netbox` role
password from inside the database container before restarting NetBox:

```sh
set -a; . ./.env; set +a
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml exec -T \
  netbox-db psql -U netbox -d netbox -v ON_ERROR_STOP=1 \
  -c "ALTER ROLE netbox PASSWORD '$NETBOX_DB_PASSWORD';"
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml up -d netbox
```

The command changes only the development database login used by NetBox; it
does not remove inventory data. If it reports a database connection failure,
the prior credentials are required for a non-destructive recovery.

When NetBox only reports that it is waiting for the database, expose the
underlying exception for one restart without editing `netbox.env`:

```sh
NETBOX_DB_WAIT_DEBUG=1 docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml up -d --force-recreate netbox
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml logs --tail=80 netbox
```

Remove `NETBOX_DB_WAIT_DEBUG` from the shell or leave its `.env` value at `0`
after collecting the diagnostic output.

For a deliberately disposable environment, first take any required export and
then use the explicitly named `down -v` command above before creating a fresh
environment file.

## Optional: NetBox with the AWS plugin (package N2)

`compose.netbox-plugin.yaml` is an optional overlay that builds `netbox`,
`netbox-worker` and `netbox-bootstrap` from
[`netbox/plugin/Dockerfile`](netbox/plugin/Dockerfile), which adds
`netbox-aws-vpc-plugin` (pinned version; see
[`docs/NETBOX_AWS_PLUGIN.md`](../../docs/NETBOX_AWS_PLUGIN.md)) to the pinned
NetBox image, and mounts [`netbox/plugin/plugins.py`](netbox/plugin/plugins.py)
to `/etc/netbox/config/plugins.py`. Without this file layered in, the stack is
unchanged and pulls the published `netboxcommunity/netbox` image:

```sh
cd deploy/compose
./create-env.sh
docker compose --env-file .env \
  -f compose.yaml -f compose.netbox.yaml -f compose.netbox-plugin.yaml \
  up --build -d
docker compose --env-file .env \
  -f compose.yaml -f compose.netbox.yaml -f compose.netbox-plugin.yaml \
  --profile bootstrap run --rm netbox-bootstrap
docker compose --env-file .env \
  -f compose.yaml -f compose.netbox.yaml -f compose.netbox-plugin.yaml \
  --profile bootstrap run --rm seed
```

For an isolated candidate build, set `NETBOX_PLUGIN_BASE_IMAGE` to an exact
NetBox image digest and `NETBOX_PLUGIN_IMAGE_TAG` to a separate local tag before
running the same overlay's `build netbox`. The defaults above use the qualified
4.6.10 digest and `platform-ipam/netbox-plugin:local`. The local candidate
build, migrations and API probe are recorded in ADR 0019; stage/production
need separate checks.

The plugin's migrations run automatically at NetBox startup, the same as
NetBox's own; verified against a throwaway Compose project on 2026-09-18 (five
migrations applied, `manage.py showmigrations netbox_aws_vpc_plugin` all
`[X]`). Its REST API is served under `/api/plugins/aws-vpc/` (the plugin's
`base_url`, not its Python module name
`netbox_aws_vpc_plugin`) --
`/api/plugins/aws-vpc/{aws-accounts,aws-vpcs,aws-subnets}/`. The plugin is a
view over IPAM prefixes, never the allocator's source of truth: imported
networks are always written as prefixes (ADR 0007), and plugin objects only
link to them.
