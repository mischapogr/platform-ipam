# Seed runbook

`platform-ipam seed` (work-plan package N4, `internal/seedcmd`) creates or
verifies every NetBox custom field, choice set and tag `internal/netbox`
relies on. It is the mode `docs/DEPLOYMENT.md` and `docs/NETBOX_INTEGRATION.md`
used to describe only as a procedure -- run by hand before this package, or
covered only for development by `deploy/compose/seed-netbox.py`.

## 1. What seed is, and is not

`seed` bootstraps the **schema** NetBox needs before it can hold any of the
platform's data: the custom fields, their choice sets, and the
`platform-ipam-imported` tag. It creates or verifies these objects only --
never the development tenant, VRF, pool container or sample inventory, which
stay `deploy/compose/seed-netbox.py`'s own development-only bootstrap.

It is idempotent and conflict-detecting, never destructive: an object that
already exists and matches is left exactly as it is (`present`); one that
already exists but disagrees -- a custom field of a different `type`, or
with a different set of `object_types` -- is reported as a `conflict` and
**never rewritten**; only a genuinely missing object is created. Nothing it
does is reversible by re-running it with different arguments, because it
takes none: `platform-ipam seed` is the whole invocation.

Run it:

- **Before the first onboarding import** against a new NetBox instance
  (stage, prod, or any environment other than the development stack, where
  `deploy/compose/seed-netbox.py` already covers it).
- **After any upgrade that adds a field** -- a new platform release that
  reads or writes a custom field this NetBox does not yet have will fail
  every write with a bare `400` until `seed` has run.

## 2. Prerequisites

- A NetBox origin and a token with permission to read and create custom
  fields, custom-field choice sets and tags (`/api/extras/custom-fields/`,
  `/api/extras/custom-field-choice-sets/`, `/api/extras/tags/`). No other
  NetBox permission is required -- `seed` never touches a prefix, a VRF or
  any other IPAM object.
- Nothing else. `internal/config.Settings.Validate("seed")` requires only
  `IPAM_NETBOX_URL` and `IPAM_NETBOX_TOKEN` (plus a valid `IPAM_ENVIRONMENT`)
  -- no `IPAM_DATABASE_URL`, no OIDC settings, no AWS mode, no
  `IPAM_CONFIG_FILE`/`IPAM_IDENTITY_FILE`. `seed` opens no database and never
  builds a cloud observer (`cmd/platform-ipam/main.go` dispatches it before
  `storage.NewPostgresLedger` is even constructed, exactly like `client` and
  `onboard`).

## 3. Who may run it, and with what

Anyone who can set the two NetBox settings above can create or verify these
fields -- there is no authentication, no reviewed input, and no ledger
write, so the operational control is simply who may set
`IPAM_NETBOX_URL`/`IPAM_NETBOX_TOKEN` for this process (an environment
variable locally, or the `operatorJob`'s NetBox Secret reference in
Kubernetes) and who may run the binary or the Job.

### Running locally or via Compose

```sh
IPAM_ENVIRONMENT=development \
IPAM_NETBOX_URL=http://localhost:18000 \
IPAM_NETBOX_TOKEN=local-netbox-development-token \
  ./platform-ipam seed
```

Against the development stack (`deploy/compose/compose.yaml` and
`compose.netbox.yaml`), run it through the built image the same way the
`seed` Compose service already runs `seed-netbox.py`:

```sh
docker compose -f deploy/compose/compose.yaml -f deploy/compose/compose.netbox.yaml \
  run --rm api seed
```

### Running `seed` in Kubernetes: `operatorJob`

`deploy/helm/platform-ipam/templates/operator-job.yaml` (work-plan package
N4) is the same opt-in Job `adopt`/`onboard` already use, disabled by
default. See [the chart README](../helm/platform-ipam/README.md#optional-operatorjob)
for the full values reference and the `runId`/immutability rule. `seed`'s
shape is deliberately the simplest of the three: no subcommand, no flags, no
input table -- `operatorJob.command` and `operatorJob.args` must both be
left empty, and `operatorJob.input.existingConfigMap`/`key` must both be
empty too, or the render is refused with a clear message (an operator table
handed to a mode that reads none is a mistake worth stopping, exactly as
`adopt abandon`'s own guard already treats it).

1. **Set the values** and run `helm upgrade` (a real deployment, not a dry
   run -- this is not a Helm hook):
   ```yaml
   operatorJob:
     enabled: true
     mode: seed
     runId: 2026-09-22-seed-1               # new for every run
     networkPolicy:
       egress:
         - cidr: 10.0.20.0/24                # NetBox's destination CIDR
           ports: [{protocol: TCP, port: 443}]
   ```
   Leave `command`, `args` and `input` at their empty defaults.
2. **Read the Job's log** (`kubectl logs job/<release>-operator-<runId>`):
   the JSON report, one line, listing every object as `created`, `present`
   or `conflict`. The process exit code is the Job's own result (`kubectl
   get job` shows `Failed` for non-zero): `0` nothing to do (or everything
   just created), `2` a usage error (an argument was somehow passed), `3` a
   conflict -- an existing field or tag disagrees with what `seed` expects,
   and needs a person to decide whether to fix it by hand or fix
   `internal/netbox/seed.go` -- `4` NetBox itself was unreachable or the
   settings above were misconfigured.
3. **Delete or disable the block afterward**, exactly as the adoption
   runbook's own procedure recommends: `ttlSecondsAfterFinished` (default
   one day) eventually removes the finished Job, but set
   `operatorJob.enabled: false` once you have read the log rather than
   leaving it enabled in the release.

Least privilege: `mode: seed` runs under the same dedicated, annotation-free
ServiceAccount `mode: onboard` uses (no cloud-role annotation, no mounted
service-account token -- `automountServiceAccountToken: false`), and its
NetworkPolicy carries only NetBox's destination CIDR and DNS -- never the
chart-wide egress list the `api`/`worker`/`adopt` Deployments use for the
database, AWS and the OIDC issuer.

**Not verified by this procedure and said so:** it has been checked by
`helm lint`/`helm template` only (`scripts/ai/check-helm`) -- never run
against a real cluster.

## 4. The one source of truth for the field list

`internal/netbox/seed.go`'s Go constants are the single source of truth for
every custom field, choice set and tag `seed` creates or verifies.
`deploy/compose/seed-netbox.py`'s own `FIELDS`, `CHOICE_SETS`,
`SELECT_CHOICE_SETS`, `FIELD_OBJECT_TYPES` and `TAGS` declarations mirror it
by hand, for the development stack's own convenience; a test
(`internal/netbox/seed_test.go`'s `TestSeedFieldsMatchPythonScript`) parses
the Python source and asserts the two are byte-for-byte the same catalogue,
so a change to one without the other fails `go test` rather than drifting
silently. Add a new field or tag to `internal/netbox/seed.go` first, run the
test, and fix `seed-netbox.py` until it passes again.

## What this runbook does not cover

Rotating the NetBox token, creating the token itself, or any other NetBox
administration outside custom fields, choice sets and tags. Bootstrapping
the development tenant, VRF, pool container and sample inventory --
`deploy/compose/README.md` and `deploy/compose/seed-netbox.py` cover that.
Onboarding an import once the fields exist -- see
[the onboarding import design](../../docs/ONBOARDING_IMPORT.md) and the
[adoption runbook](ADOPTION.md).
