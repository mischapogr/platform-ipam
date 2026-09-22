# platform-ipam Helm chart

This chart runs independent API and worker Deployments from one immutable
image digest. The API is exposed through a ClusterIP Service and optional
internal Ingress. NetBox remains an external URL and Secret reference; it is
not a subchart.

Stage and prod values require OIDC, live AWS observation mode, HTTPS NetBox,
separate database/NetBox Secret names, and separate policy/coverage ConfigMap
names. ServiceAccount annotations are the cluster's workload identity hook;
set the AWS role annotation in the environment values owned by the cluster
platform. An optional `identity.existingConfigMap` mounts
`identities.yaml` and sets `IPAM_IDENTITY_FILE` for deployments that use the
explicit identity list.

The migration Job is a `pre-install,pre-upgrade` hook. Its dedicated hook
ServiceAccount is ordered before the Job, and the Job must finish before Helm
creates or updates the application workloads. Migrations are not run by API
or worker containers.

```sh
helm lint deploy/helm/platform-ipam --strict \
  -f deploy/environments/stage/values.yaml
helm template platform-ipam-stage deploy/helm/platform-ipam \
  -f deploy/environments/stage/values.yaml
```

## Optional `uiProxy`

`uiProxy` (work-plan package A7, [ADR 0006](../../../docs/decisions/0006-OPERATOR_UI_AUTHENTICATION.md),
[docs/GUI_AUTHENTICATION.md](../../../docs/GUI_AUTHENTICATION.md)) adds a
Caddy reverse proxy Deployment/Service/Ingress/NetworkPolicy that reproduces
`deploy/compose/ui-proxy/Caddyfile`'s `basic`-mode logic in the cluster:
strips inbound `X-Remote-User`/`X-Remote-User-Group` in both the dashed and
underscore spelling, refuses `/api/*` and `/graphql/*`, verifies an HTTP
Basic credential against a bcrypt hash, and sets `X-Remote-User` to the
verified user before proxying to `uiProxy.upstream`. It is **disabled by
default** and, disabled, changes nothing in the rendered manifests.

This chart does not deploy NetBox (see the top of this README) and cannot
create a NetBox deployment's configuration. The proxy is only meaningful,
and header trust only holds, under two conditions that are entirely the
external NetBox operator's responsibility, not something this chart can
enforce:

1. **The NetBox named in `uiProxy.upstream` must be reachable ONLY through
   this proxy.** If NetBox's own port stays reachable by any other path --
   another Ingress, a NodePort, a Service exposed to more than this proxy's
   pods -- the `X-Remote-User` header this proxy sets can be bypassed
   entirely, and every safeguard below is void.
2. **That NetBox must be configured for remote-header authentication**, per
   `docs/GUI_AUTHENTICATION.md` section 2: `REMOTE_AUTH_ENABLED=true`,
   `REMOTE_AUTH_HEADER=HTTP_X_REMOTE_USER`, `REMOTE_AUTH_AUTO_CREATE_USER=true`,
   and `REMOTE_AUTH_DEFAULT_GROUPS` set to the read-only operator group (for
   example `platform-operators`, package A2). Exact setting names were
   verified against the pinned Compose NetBox image (package A1); confirm
   they still apply to whatever NetBox image/version the external release
   runs.

### Creating the Basic Auth secret

This chart never creates a Secret holding credentials -- `uiProxy.basicAuth.existingSecret`
must name a Secret you create yourself, with keys `username` and
`passwordHash`. `passwordHash` must be a bcrypt **hash**, never a plaintext
password:

```sh
docker run --rm caddy:2.11.4-alpine caddy hash-password --plaintext '<password>' --algorithm bcrypt
kubectl create secret generic platform-ipam-ui-proxy-basic \
  --from-literal=username=operator \
  --from-literal=passwordHash='<hash from the previous command>'
```

### Values

| Key | Purpose |
| --- | --- |
| `uiProxy.enabled` | Off by default. |
| `uiProxy.image.{repository,tag,digest}` | Same pinned Caddy tag as Compose (`2.11.4-alpine`); `digest` is optional and takes precedence over `tag` when set. |
| `uiProxy.mode` | Only `"basic"` is implemented; `"entra"` (A4) and `"ldap"` (A5) are separate, not-yet-built packages. |
| `uiProxy.upstream` | The external NetBox's reachable URL. Required, and must be `https://`, when enabled in stage/prod. |
| `uiProxy.basicAuth.existingSecret` | See above. |
| `uiProxy.ingress.*` | `enabled`, `className`, `host`, `tlsSecretName`, `annotations`. **TLS is required**: rendering fails with a clear error if `ingress.enabled` is true and `tlsSecretName` is empty -- Basic credentials must never cross the wire in plaintext (ADR 0006 rule 7). |
| `uiProxy.networkPolicy.ingress` | Selector pair for the ingress controller, same shape as `networkPolicy.apiIngress`. |
| `uiProxy.networkPolicy.egress` | CIDR + port list for the upstream NetBox; an empty list denies non-DNS egress. |

`deploy/environments/stage/values.yaml` and `deploy/environments/prod/values.yaml`
ship this block present but disabled, with the rest commented out as a
starting point. `deploy/helm/platform-ipam/ci/ui-proxy-values.yaml` is a
CI-only overlay (used by `scripts/ai/checks.py`'s `helm()` check) that
additively validates the enabled path on every run; it is not a deployment
environment and is not referenced by the application.

## Optional `operatorJob`

`operatorJob` (work-plan packages H3 and N4) is an opt-in Job that runs
`platform-ipam adopt`, `platform-ipam onboard` or `platform-ipam seed`
inside the cluster -- today's only supported way to run any of the three
process modes in stage/prod without hand-building a Job of your own. It is
**disabled by default** (`operatorJob.enabled: false`) and, disabled,
renders nothing: no Job, no extra ServiceAccount, no extra NetworkPolicy.
See [the adoption runbook](../../runbooks/ADOPTION.md) section 3 for the
`adopt`/`onboard` end-to-end procedure, [the seed runbook](../../runbooks/SEED.md)
for `seed`'s, and [docs/DEPLOYMENT.md](../../../docs/DEPLOYMENT.md) section 4
for how it fits the rest of the chart.

**This chart never generates the reviewed table.** Create the ConfigMap
holding it yourself, e.g.:

```sh
kubectl create configmap platform-ipam-adopt-records \
  --from-file=records.csv=./records.csv
```

### Example: `onboard plan`

```yaml
identity:
  existingConfigMap: ""   # onboard needs none -- see the least-privilege table below
operatorJob:
  enabled: true
  mode: onboard
  command: plan
  args: ["--domain", "core"]
  runId: 2026-09-20-onboard-plan-1
  input:
    existingConfigMap: platform-ipam-onboard-table
    key: table.json
  networkPolicy:
    egress:
      - cidr: 10.0.20.0/24          # NetBox's address, only
        ports: [{protocol: TCP, port: 443}]
```

### Example: `adopt apply`

```yaml
identity:
  existingConfigMap: platform-ipam-identities   # REQUIRED for mode: adopt
operatorJob:
  enabled: true
  mode: adopt
  command: apply
  args: ["--operator", "alice"]
  runId: 2026-09-20-adopt-apply-1
  input:
    existingConfigMap: platform-ipam-adopt-records
    key: records.csv
```

### Example: `adopt abandon`

`abandon` (ADR 0012) withdraws an **uncommitted** adoption that can never
commit; it takes no reviewed table at all, only `--allocation-id`,
`--operator` and `--reason` -- through `operatorJob.args`, exactly as they
would be typed on the command line. `operatorJob.input` is left at its
chart defaults (both fields empty): rendering **fails** if either is set,
because a table handed to a command that takes none is a mistake worth
stopping. **Dry run first**, then the real run under a **new** `runId`:

`identity.existingConfigMap` is **not required for `abandon`** (work-plan
package H9), unlike `plan`/`apply` below: `abandon` resolves no acting
principal at all (`internal/adoptcmd/abandon.go`'s `runAbandon` never reads
the identity file, and `Service.AbandonAdoption` takes no `domain.Principal`,
ADR 0012), and the chart mounts and demands nothing for it even when
`identity.existingConfigMap` happens to be set chart-wide for another Job.

```yaml
operatorJob:
  enabled: true
  mode: adopt
  command: abandon
  args:
    - "--allocation-id"
    - "alloc_01abc"
    - "--operator"
    - "alice"
    - "--reason"
    - "reviewed CIDR was wrong; ground truth cannot be made to match"
    - "--dry-run"
  runId: 2026-09-20-adopt-abandon-1-dry-run
```

Read the pod log, confirm `inventory.claim` and `would_do` are what you
expect (see [the adoption runbook](../../runbooks/ADOPTION.md) section 10),
then drop `--dry-run` and pick a **new** `runId` for the real run:

```yaml
operatorJob:
  enabled: true
  mode: adopt
  command: abandon
  args:
    - "--allocation-id"
    - "alloc_01abc"
    - "--operator"
    - "alice"
    - "--reason"
    - "reviewed CIDR was wrong; ground truth cannot be made to match"
  runId: 2026-09-20-adopt-abandon-1
```

### Example: `seed`

`platform-ipam seed` (work-plan package N4) creates or verifies the
platform's NetBox custom fields, choice sets and tags -- see
[the seed runbook](../../runbooks/SEED.md) for the full procedure. Unlike
`adopt`/`onboard` it takes **no subcommand and no flags at all**:
`operatorJob.command` and `operatorJob.args` must both be left empty, and
`operatorJob.input` must name no table (both empty), or the render fails --
the same "a table handed to a mode that reads none is a mistake worth
stopping" rule `adopt abandon` already applies.

```yaml
operatorJob:
  enabled: true
  mode: seed
  runId: 2026-09-20-seed-1
  networkPolicy:
    egress:
      - cidr: 10.0.20.0/24          # NetBox's address, only
        ports: [{protocol: TCP, port: 443}]
```

`identity.existingConfigMap` is not required and, like `adopt abandon`, is
never mounted for `seed` even when set chart-wide for another Job's own
`plan`/`apply` -- `seed` resolves no principal and loads no identity file at
all.

### The `runId`/immutability rule

`runId` is **required** whenever `operatorJob.enabled` is true; rendering
fails with a clear message without it. It becomes part of the Job's name
(`<release>-operator-<runId>`), so every run is a brand-new, immutable
Kubernetes object:

- A `helm upgrade` that leaves the block enabled with the **same** `runId`
  changes nothing -- Kubernetes refuses to mutate an existing Job's pod
  template, so the upgrade either no-ops on this resource or, if you also
  changed the input table or arguments, is rejected outright by the API
  server (a Job's `spec.template` is immutable after creation).
- Changing the input table (or `command`/`args`) **without** changing
  `runId` therefore fails loudly at the next `helm upgrade`, rather than
  silently re-running the same Job against different data.
- To run `apply` again -- for a new batch, a corrected table, or simply a
  retry after fixing an environment problem -- pick a **new** `runId`.
  `adopt apply` and `onboard apply` are themselves idempotent (re-running
  the same table replays cleanly, per the adoption/onboarding runbooks), so
  a fresh Job over the same already-applied table is safe; it is the *Job
  object* that is immutable, not the underlying operation.

### NOT a Helm hook

`operatorJob`'s Job carries **no** `helm.sh/hook` annotation, unlike
`migration-job.yaml`. A hook re-runs on every `helm upgrade` that touches
the release and its result gates that release; `adopt apply`/`onboard
apply` are one-shot, human-reviewed operator actions that must never
silently re-run because an unrelated value (an ingress host, a replica
count) changed in the same upgrade. `backoffLimit: 0` and
`restartPolicy: Never` mean it is never retried automatically either -- a
person reads the pod log and decides whether to run a new Job under a new
`runId`.

### Reading the result

The report is the pod's log (stdout): **one JSON document** from `plan`,
`apply`, `abandon` or `seed`, preceded by any `slog` JSON lines the process
emits before it (`onboard`, `adopt` and `seed` route through the same
`cmd/platform-ipam/main.go` process, which logs to stdout). `seed`'s report
lists every custom field, choice set and tag as `created`, `present` or
`conflict` (see the seed runbook); its own exit codes are `0` nothing to
do/created, `2` usage (an argument was somehow passed -- this template never
sends one), `3` a conflict, `4` adapter (NetBox unreachable or the settings
misconfigured) -- distinct from, but the same shape as, `onboard`/`adopt`'s
codes below. `abandon` prints
its document -- the `AbandonReport`'s own fields on success, or an
`{"error": {"code": ..., "message": ...}}` object -- in **every** outcome
that reached the service, so a pipeline reading stdout learns why without
parsing stderr; a refusal before the service is ever reached (a usage
mistake in `operatorJob.args`, or one of this template's own render-time
`fail` guards) prints none and never creates the Job at all. The **exit code
is the Job's result** -- `kubectl get job` shows `Failed` for a non-zero
exit. `onboard`, `adopt plan`/`apply` and `adopt abandon` share the same
exit-code shape: `0` ok, `2` usage, `3` validation -- a refusal the operator
must act on (bad input, a policy refusal, a pending or `waiting_for_parent`
outcome for `apply`; for `abandon`, an unknown allocation, a committed
allocation, the wrong or an already-terminal operation, an abandon already
in progress, or the inventory's own refusal -- see `error.code`), `4`
adapter -- an infrastructure or uncertain outcome (NetBox/ledger/cloud
failure; for `abandon`, re-running the same abandon is always safe after
this exit). **A `Failed` Job with exit `3` is a refusal to read, not an
outage** -- after `adopt apply` of a VPC with subnets it is the *expected*
first-run result (see the adoption runbook section 6); after `adopt abandon`
it means the operator must act (wrong allocation id, already committed,
already abandoned) before trying again. Read the log before assuming
something broke.

### Least privilege per mode

| | `mode: onboard` | `mode: adopt` (`plan`, `apply` and `abandon` alike) | `mode: seed` (work-plan package N4) |
| --- | --- | --- | --- |
| Opens the ledger database | No (`IPAM_DATABASE_URL` is never set) | Yes -- same `IPAM_DATABASE_URL` as `worker` | No |
| Builds a cloud observer | No | Yes -- same `IPAM_AWS_MODE`/live AWS credentials as `worker` | No |
| NetBox URL/token | Yes | Yes | Yes |
| Pools configuration (`policy` ConfigMap) | Yes | Yes | **No** -- `seed` loads no pools/identity configuration at all; `internal/netbox.New` is constructed with no `Domains`/`Pools` for this mode |
| Auth/listen settings (`IPAM_OIDC_ISSUER`, `IPAM_OIDC_AUDIENCE`, `IPAM_AUTH_MODE`, `IPAM_LISTEN_ADDR`) | No (never set for onboard) | **No** (work-plan packages H5/H7) -- `adopt` authenticates no HTTP caller and never listens, in any of its three subcommands, so these four are omitted from the Job's environment even though the `worker` Deployment (which shares most of this environment) still carries them | No |
| Identity file | Only if `identity.existingConfigMap` is set chart-wide (not required; `onboard plan`/`apply` never read it) | `plan`/`apply`: **Required** -- rendering fails without it: `adopt` resolves its acting principal from the identity file. `abandon`: **not required and never mounted** (work-plan package H9) -- it resolves no principal at all, so it neither demands `identity.existingConfigMap` nor mounts it even when set chart-wide for `plan`/`apply`'s own Jobs | **Not required and never mounted**, like `abandon` -- `seed` resolves no principal at all |
| ServiceAccount | Dedicated `<release>-operator-job`, no annotations, `automountServiceAccountToken: false` | The **worker's own** ServiceAccount -- whatever cloud-role annotation it carries | The **same** dedicated `<release>-operator-job` ServiceAccount `onboard` uses |
| NetworkPolicy egress | DNS + `operatorJob.networkPolicy.egress` only (NetBox's own destination -- fill this in yourself) | DNS + `networkPolicy.egress` (the same chart-wide list `worker` gets) | DNS + `operatorJob.networkPolicy.egress` only, like `onboard` |
| Table/subcommand/flags | `command`/`input` required (see below) | `command`/`input` required (except `abandon`, whose flags come from `args`) | **None accepted** -- `command`, `args` and `input` must all be left empty; the render fails otherwise |

Both modes share the API/worker Deployments' pod and container
`securityContext` (read-only root filesystem, all capabilities dropped, non-root)
and mount the same `policy` ConfigMap and a writable `/tmp` `emptyDir`
(neither `adopt` nor `onboard` is known to write anywhere besides stdout,
but this mirrors the Deployments' own defensive mount, which has never been
proven necessary there either).

Rendering **fails with a clear message** rather than silently under-
provisioning when `mode: adopt` is requested with `command: "plan"` or
`"apply"` and no `identity.existingConfigMap` is configured (work-plan
package H9: `"abandon"` has no such requirement -- see the least-privilege
table above and the `adopt abandon` example above), when `operatorJob.mode`
is anything other than `"adopt"`/`"onboard"`/`"seed"`, when `enabled: true`
is set without `runId`, when a `command`/`input.existingConfigMap`/
`input.key` a command needs is missing, or when `mode: seed` is given a
`command`, non-empty `args`, or an `input` table (work-plan package N4:
`seed` takes none of these).

`operatorJob.command` is validated against an explicit **allow-list per
mode** (work-plan package H7, `templates/operator-job.yaml`'s `fail`
guards): for `mode: adopt`, `"plan"`, `"apply"` or `"abandon"`; for
`mode: onboard`, `"plan"` or `"apply"`; for `mode: seed` (work-plan package
N4), the list is **empty** -- `seed` takes no subcommand at all, so
`operatorJob.command` must stay `""` and no second argument is ever
appended to the container's `args`. Anything else fails the render with a
message naming the supported set. `plan` and `apply` take exactly one
positional table-path argument, which this chart mounts from `input` and
appends automatically; `abandon` ([ADR 0012](../../../docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md),
work-plan package H2c) and `seed` alike take **no** table at all --
`abandon`'s `--allocation-id`/`--operator`/`--reason` (required) and
`--operation-id`/`--dry-run` (optional) flags arrive entirely through
`operatorJob.args`, `seed` takes no flags at all, and the render fails if
`operatorJob.input` is set for either (a table handed to a mode/command
that takes none is a mistake worth stopping). Every subcommand outside the
allow-list -- `onboard drift`, `onboard parse`, `onboard render-config`,
`onboard render-fixture` -- is **not** supported by this template, because
this chart cannot know in general whether an unlisted subcommand takes a
table or not; run one of those as a one-off `kubectl run`/local invocation
instead. See [the adoption runbook](../../runbooks/ADOPTION.md) section 3
for the `adopt abandon` cluster procedure and
[the seed runbook](../../runbooks/SEED.md) for `seed`'s.

### Values

| Key | Purpose |
| --- | --- |
| `operatorJob.enabled` | Off by default. |
| `operatorJob.mode` | `"adopt"`, `"onboard"` or `"seed"`. Anything else fails the render. |
| `operatorJob.command` | The subcommand. Required when enabled and `mode` is `"adopt"`/`"onboard"`; validated against an allow-list per mode -- `"plan"`, `"apply"` or `"abandon"` for `mode: adopt`, `"plan"` or `"apply"` for `mode: onboard`. Anything else fails the render. **Must be left empty (`""`) for `mode: "seed"`** (work-plan package N4): `seed` takes no subcommand at all -- the render fails if this is set. |
| `operatorJob.args` | Extra arguments appended after the mode, the subcommand and (for every command except `"abandon"`) the mounted table path -- `--operator <subject>` (`adopt apply`), `--domain <id>` (`onboard plan`/`apply`), `--batch <name>` (`onboard apply`). For `"abandon"` this is the **only** place its flags come from: `--allocation-id`, `--operator` and `--reason` are **required** here (each followed by a non-empty value; the render fails otherwise), `--operation-id`/`--dry-run` are optional. **Must be empty for `mode: "seed"`** -- the render fails if this is non-empty. |
| `operatorJob.runId` | **Required** when enabled. See the immutability rule above. |
| `operatorJob.input.existingConfigMap` / `.key` | The reviewed table's ConfigMap and the key (and mounted file name) inside it. **Required for every command except `"abandon"` and every `mode: "seed"` invocation** (neither takes a table -- leave both empty; the render fails if either is set); this chart never generates the table. |
| `operatorJob.ttlSecondsAfterFinished` | Default `86400` (1 day). |
| `operatorJob.activeDeadlineSeconds` | Default `1800` (30 minutes). |
| `operatorJob.resources` | Same shape as `api.resources`/`worker.resources`. |
| `operatorJob.networkPolicy.egress` | `mode: onboard` or `mode: seed` only -- NetBox's destination CIDR/port. (`mode: adopt` reuses the chart-wide `networkPolicy.egress`.) An empty list denies all non-DNS egress. |

`deploy/helm/platform-ipam/ci/operator-job-{onboard-plan,onboard-apply,adopt-plan,adopt-apply,adopt-abandon,adopt-abandon-dry-run,adopt-abandon-no-identity,seed}-values.yaml`
are CI-only overlays (used by `scripts/ai/checks.py`'s `helm()` check) that
additively validate all eight combinations -- `adopt-abandon-no-identity`
(work-plan package H9) proves `abandon` renders, and mounts no identity
ConfigMap or `IPAM_IDENTITY_FILE`, with `identity.existingConfigMap` left
unset; `seed` (work-plan package N4) proves the least-privilege render
succeeds with `command`/`args`/`input` all left empty -- and the must-fail
renders (unknown mode; enabled without `runId`; enabled without an input
ConfigMap; `mode: adopt` with `command: plan` without an identity ConfigMap;
`mode: seed` with a `command`, non-empty `args`, or an input ConfigMap set),
on every run; none is a deployment environment and none is referenced by
the application.

**This has been validated by `helm lint` and `helm template` only and has
NEVER run in a real cluster.** Not verified: that the rendered Job actually
starts and completes successfully against a live database, NetBox and AWS;
that the dedicated `operator-job` ServiceAccount behaves as expected under
the cluster's actual admission/PSA policies; that `/tmp` is genuinely
unneeded (or genuinely sufficient) for either binary under
`readOnlyRootFilesystem: true`.

**Not verified on a cluster.** Everything above has been checked by

What *was* verified without a cluster: the rendered Caddyfile was run in the pinned Caddy image under the Deployment's own constraints — read-only root filesystem, UID/GID 10001, `no-new-privileges`, all capabilities dropped except `NET_BIND_SERVICE` — and answered 200 on `/healthz`, 401 without or with a wrong credential, and 403 on `/api/` and `/graphql/`. That one capability is required, not optional: the image's `caddy` binary carries the file capability `cap_net_bind_service`, and with a bare `drop: [ALL]` the kernel refuses to execute it (`exec /usr/bin/caddy: operation not permitted`). The proxy therefore does not reuse `containerSecurityContext`; the API and worker still drop everything.
`helm lint --strict`/`helm template` (schema and rendering, including the
enabled path and the ingress-without-TLS failure) and by running
`caddy validate` against the rendered Caddyfile. No Kubernetes cluster,
Ingress controller, or real NetBox has exercised this path end to end.
