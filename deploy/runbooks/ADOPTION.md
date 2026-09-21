# Adoption runbook

`platform-ipam adopt plan|apply` gives an existing, imported network an owner in
the ledger. Read this before running it against a real account. Background:
[ADR 0010](../../docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md)
is the design; this runbook is the operational half of it. It assumes you have
already read [Onboarding import](../../docs/ONBOARDING_IMPORT.md).

## 1. What adoption is, and is not

Adoption converts **one** already-imported, unmanaged NetBox prefix — at the
exact CIDR reviewed, nothing wider or narrower — into a managed allocation with
a tenant, an allocation key and an audit trail. That is all it does.

It never creates, deletes, resizes or tags an AWS resource. `adopt`'s AWS
access is read-only (the same observer the worker uses); nothing in this
codebase gives it `ec2:CreateTags` or any write permission
(`deploy/aws/platform-ipam-readonly-role.yaml`). The VPC or subnet you adopt is
untouched in AWS before, during and after the run. Adoption also never selects
a CIDR: the operator supplies the exact prefix, and every check that would
reject a *chosen* candidate for an ordinary reservation is repeated against the
*pinned* one (`pinnedCIDR`, `internal/service/service.go`) — adoption cannot
widen, narrow or shift what gets adopted.

Adoption is not an API endpoint and not a `client` verb. It is a third process
mode of the service binary, alongside `api` and `worker`
(`cmd/platform-ipam/main.go`), because it acts *as* a tenant without that
tenant's authentication — something the identity model has no way to express
at the API layer (ADR 0010).

## 2. Prerequisites

Before running `adopt` for a network, all of the following must already be
true:

1. **The network was imported.** It is a NetBox prefix in the domain's VRF
   carrying the `platform-ipam-imported` tag, written by
   `platform-ipam onboard apply` (see
   [Onboarding import](../../docs/ONBOARDING_IMPORT.md)). It carries none of
   the four ownership custom fields (`platform_allocation_id`,
   `platform_allocation_key`, `platform_operation_id`, `platform_state`) —
   adoption refuses a prefix that already carries any of them.
2. **The cloud observer sees the resource, completely and currently.** `adopt`
   refuses on an incomplete inventory snapshot or an incomplete, stale or
   untrustworthy cloud observation, exactly as a reservation does. Confirm the
   worker (or, for `apply`, `adopt`'s own one-shot observer call) has a recent,
   complete scan of the target account/region before you run `apply`.
3. **The owning tenant and a covering identity already exist in
   configuration.** `adopt` resolves the acting principal itself, from the
   identities in the configured identity file: it looks for the tenant's
   identities and, among them, **exactly one** whose `accounts`,
   `environments` and `regions` scope covers the record's account,
   environment and region. Zero covering identities, or more than one, is a
   refusal (`internal/adoptcmd/principal.go`) — `adopt` never guesses or
   widens a principal's scope to make a record fit. Add the identity and
   redeploy configuration first if it does not exist yet.
4. **The allocation key is agreed with the owning team, in writing, before you
   run `apply`.** The key is permanent from the moment `apply` commits it —
   there is no renaming an allocation's key. The owning team's *first*
   `POST /v1/allocations` after adoption must use **exactly** this key and
   **exactly** the adopted CIDR's prefix length, or their request either
   replays successfully (same key, same shape) or is refused as a conflict
   (same key, different shape) — see section 8.

## 3. Who may run it, and with what

`adopt` needs ledger credentials, NetBox credentials and cloud read
credentials **at once** — more authority than any other process mode holds. It
acts as the tenant's principal without that tenant ever authenticating, and
the service cannot verify who is really running it: `--operator <subject>` is
an audit string recorded on both `ADOPT_PLANNED` and `ADOPT_COMMITTED` events
and nothing else. It grants no authority by itself and is never looked up in
the identity file (`internal/adoptcmd/adoptcmd.go` help text; ADR 0010,
"Consequences"). **Restricting who may invoke this binary in this mode is a
deployment control, not something the code enforces** — treat the environment
`adopt` runs in (whoever can reach its database, NetBox token and AWS
credentials) as equivalent to an operator with full ledger write access, and
gate it accordingly (a break-glass workstation, a bastion job with its own
approval, or similar — this repository does not prescribe one).

`adopt` is dispatched in `cmd/platform-ipam/main.go` alongside `api` and
`worker`: it opens the same PostgreSQL ledger connection, loads the same pools
and identity configuration, and constructs the same cloud observer (the fake
observer if `IPAM_AWS_MODE=fake`, otherwise a live AWS client). It needs, at
minimum: `IPAM_DATABASE_URL`, `IPAM_CONFIG_FILE`, `IPAM_IDENTITY_FILE`,
`IPAM_NETBOX_URL` and `IPAM_NETBOX_TOKEN`, and either `IPAM_AWS_MODE=fake` with
`IPAM_FAKE_CLOUD_FILE`, or real AWS credentials for the observer's SDK config.
It never migrates the schema and never starts the HTTP server or the worker
loop; it runs its one subcommand and exits.

### Running `adopt` in Kubernetes: `operatorJob`

`deploy/helm/platform-ipam/templates/operator-job.yaml` (work-plan package
H3) is an opt-in Job, disabled by default, that runs `adopt` or `onboard`
in the cluster instead of a hand-built Job. See
[the chart README](../helm/platform-ipam/README.md#optional-operatorjob)
for the full values reference, the least-privilege table per mode, and the
`runId`/immutability rule. The procedure for `adopt`:

1. **Create the ConfigMap holding the reviewed CSV/TSV table** (section 4)
   — this chart never generates it:
   ```sh
   kubectl create configmap platform-ipam-adopt-records \
     --from-file=records.csv=./records.csv
   ```
2. **Set the values** and run `helm upgrade` (a real deployment, not a dry
   run — this is not a Helm hook, so nothing about `helm upgrade --dry-run`
   or `--install` alone runs it):
   ```yaml
   identity:
     existingConfigMap: platform-ipam-identities   # required for command: plan/apply (not abandon -- work-plan package H9)
   operatorJob:
     enabled: true
     mode: adopt
     command: apply
     args: ["--operator", "alice"]
     runId: 2026-09-20-adopt-apply-1               # new for every run
     input:
       existingConfigMap: platform-ipam-adopt-records
       key: records.csv
   ```
   Use `command: plan` first, exactly as section 5 above describes for a
   local run — the Job's report is the pod's log either way.
3. **Read the Job's log** (`kubectl logs job/<release>-operator-<runId>`):
   the JSON report from `plan`/`apply`, and the process exit code as the
   Job's own result (`kubectl get job` shows `Failed` for non-zero — a
   `Failed` Job with exit `3` after adopting a VPC with subnets is the
   *expected* first-run result, section 6 above, not a broken deployment).
4. **Delete or disable the block afterward.** `ttlSecondsAfterFinished`
   (default one day) eventually removes the finished Job on its own, but
   set `operatorJob.enabled: false` (or delete the Job directly) once you
   have read the log, rather than leaving a completed adoption Job sitting
   in the release — a stale `input.existingConfigMap` holding a since-
   corrected table is exactly the kind of drift the paused window in
   section 7 exists to prevent. To run `apply` again, pick a **new**
   `runId` (the chart README explains why the same one changes nothing, or
   is rejected outright, on a re-`upgrade`).

**Running `abandon` through `operatorJob`.** As of work-plan package H7, the
Job template (`deploy/helm/platform-ipam/templates/operator-job.yaml`)
supports `command: abandon` for `mode: adopt` as a third, distinct shape: no
input ConfigMap is mounted, no table path is appended to the container's
arguments, and `--allocation-id`, `--operator` and `--reason` (required) plus
`--operation-id`/`--dry-run` (optional) arrive entirely through
`operatorJob.args`. See section 10 below ("Abandoning a stuck adoption") for
the full procedure, including the cluster form.

**Not verified by this procedure and said so:** it has been checked by
`helm lint`/`helm template` only (`scripts/ai/check-helm`) — never run
against a real cluster, database, NetBox or AWS account.

**Whoever can set these Helm values and create that ConfigMap can adopt
networks under `operatorJob`'s worker-equivalent credentials.** This does
not relax the warning above: `--operator <subject>` is still just an audit
string, so **the release pipeline's own access controls — who can edit
these values, who can `kubectl create configmap` in this namespace, who can
`helm upgrade` this release — are the actual control**, exactly as
restricting who may invoke the binary directly already was. Gate the
pipeline path the same way you would gate the break-glass workstation.

## 4. The input table

`plan` and `apply` both take one CSV or TSV table (`.tsv`/`.txt` extension
selects tab-separated; anything else is comma-separated), one reviewed record
per network, no normalization: every cell is taken literally, unlike
`onboard`'s reader (`internal/adoptcmd/read.go`; `internal/adoptcmd`'s package
comment). `.xlsx` is not supported — export to CSV first. Every column below
is required on the row unless marked otherwise; an unrecognized column
(including `prefix_length`, `address_family`, `pool` or `domain`, which this
tool derives itself) or a missing required one refuses the whole file before
any row is evaluated.

| Column | Required for | Meaning |
| --- | --- | --- |
| `tenant_id` | every row | owning tenant, exactly as configured in the identity file |
| `allocation_key` | every row | exactly as the owning team will use it in their own `POST` — permanent once adopted |
| `scope` | every row | `vpc` or `subnet` |
| `environment` | every row | matched against the tenant's identity and the pool |
| `region` | every row | matched against the tenant's identity and the pool |
| `account_id` | every row | matched against the tenant's identity and the pool; compared with the observed resource, never trusted on its own |
| `cidr` | every row | the exact, canonical IPv4 CIDR reviewed; its mask supplies `prefix_length`, which is never a column |
| `resource_id` | every row | the AWS resource id observed at that CIDR |
| `netbox_prefix_id` | every row | the NetBox id of the imported, unmanaged prefix at that CIDR |
| `parent_allocation_key` | `scope=subnet` only; **forbidden** for `scope=vpc` | the `allocation_key` of the parent VPC record — already committed in the ledger, or present as its own `vpc` row elsewhere in this same file |
| `availability_zone_id` | `scope=subnet` only; **forbidden** for `scope=vpc` | e.g. `euc1-az1` |

Example (documentation-range addresses, account `000000000000`, so nothing here
resembles a real estate):

```csv
tenant_id,allocation_key,scope,environment,region,account_id,cidr,resource_id,netbox_prefix_id,parent_allocation_key,availability_zone_id
orders,prod-eu-central-1-orders,vpc,prod,eu-central-1,000000000000,198.51.100.0/24,vpc-00000000000000000,482,,
orders,prod-eu-central-1-orders-private-euc1-az1-v1,subnet,prod,eu-central-1,000000000000,198.51.100.0/26,subnet-00000000000000000,483,prod-eu-central-1-orders,euc1-az1
```

## 5. Procedure

### `plan`

```sh
platform-ipam adopt plan records.csv
```

Writes nothing to the ledger or NetBox — it is entirely read-only, including
against NetBox and the cloud observer (`PlanAdoption`,
`internal/service/adopt_plan.go`). It prints one JSON report to stdout, one
entry per record, and a one-line summary to stderr. Exit code `0` means no
record was refused — it does **not** mean every record would be adopted in one
run: `pending`, `deferred_on_parent` and `waiting_for_parent` also exit `0`, so
read the verdicts. Exit `3` means at least one record was refused outright —
resolve those with the data's owners and re-run `plan` before ever running
`apply`.

Every record gets one verdict:

| Verdict | Meaning |
| --- | --- |
| `would_adopt` | no allocation exists under this tenant and key yet; `apply` would create one (`201`) |
| `would_replay` | an allocation already exists and is committed; `apply` would replay it (`200`), writing nothing new |
| `pending` | an allocation exists under this tenant and key but has not committed — a prior `apply` was interrupted, or the record disagrees with what was actually converted; `apply` would report it and stop rather than finish it synchronously |
| `deferred_on_parent` | a subnet whose `parent_allocation_key` resolves to a `vpc` row elsewhere in this same file, not yet adopted; its real admissibility is unknown until `apply` actually runs the parent row |
| `waiting_for_parent` | a subnet whose parent is a committed allocation with **no verified binding yet** — see section 6; `apply` will not attempt it this run |
| `refused` | `apply` would refuse this record outright; see section 13 for what the refusal codes mean |

`deferred_on_parent`, `waiting_for_parent` and `pending` are **not** refusals
— only `refused` blocks `apply` from starting (section 6).

### Review

There is no undo (section 12). Treat `plan`'s report as the thing you review,
line by line, against the agreed allocation key, CIDR and prefix length for
every record — this is the only protection against a wrong key or a wrong
CIDR (ADR 0010).

### `apply`

```sh
platform-ipam adopt apply records.csv --operator alice
```

`apply` **re-runs `plan` first, over the whole file, and writes nothing at all
if any single record's verdict is `refused`** — one bad row blocks every row
in the file, not just itself. Once the pre-flight plan reports no refusals,
`apply` adopts records one at a time, `vpc` rows before `subnet` rows
regardless of the file's own order (`internal/adoptcmd/ordering.go`), and
**stops at the first failure, error, or pending (`202`) result** — records
after the stopping point are reported `not_attempted` and are not touched.
There is no "continue on error": an adoption cannot be undone, so nothing is
attempted past the first sign of trouble.

Each attempted record gets one outcome:

| Outcome | Stops the run? | Meaning |
| --- | --- | --- |
| `adopted` | no | committed: `201` for a fresh adoption, `200` for a clean replay |
| `waiting_for_parent` | **no** | a subnet whose parent has no verified binding yet (including a parent this same run just adopted); not attempted, but the run continues to the next record |
| `failed` | **yes** | the service refused the record, an adapter/transport error occurred, or the write came back `202` (pending — the worker's own recovery, or a later `apply`, finishes it; see section 10/11) |
| `not_attempted` | (already stopped) | a record after the point the run stopped |

Exit codes. `2` and `4` mean the same for both commands; `0` and `3` do not:

| Code | `plan` | `apply` |
| --- | --- | --- |
| `0` | no record was refused | every record adopted or replayed cleanly |
| `2` | usage error (bad flags, wrong number of positional args) | the same |
| `3` | bad input, or at least one record refused | bad input, a refusal, a failure, a pending (`202`) result, or a subnet waiting for its parent's binding |
| `4` | the table could not be opened, or a ledger/NetBox/cloud failure occurred | the same |

A run that ends with any `waiting_for_parent` outcomes exits `3` even though
nothing failed — the file is not *fully* adopted yet. Re-run `apply` later;
see section 6.

## 6. The two runs a VPC with subnets takes

> **Demonstrated end to end on 2026-09-20 (package F7).** Until that day this
> section described a procedure that had never worked: a VPC whose subnets were
> already imported and observed was refused (`adoption_refused`), because the
> overlap rule exempted only the one reviewed prefix and the one reviewed
> resource and the VPC's own subnets counted as "another network" and "another
> cloud resource"; and a VPC adopted first made `onboard` refuse its subnets as
> `overlaps-managed`. ADR 0010's 2026-09-20 amendment exempts the reviewed
> VPC's own observed children, and the onboarding import now admits a row
> strictly inside a managed vpc-scoped prefix. `tests/e2e/test_e2e_adopt.py`'s
> `AdoptVPCWithSubnetsE2ETest` runs the whole sequence below against the
> development stack: a VPC and one nested subnet imported and observed together,
> `plan` reporting the subnet `deferred_on_parent`, the first `apply` adopting
> the VPC and reporting the subnet `waiting_for_parent` with exit `3`, the
> subnet's own `unmanaged_occupancy` finding staying open while the VPC's
> closes, the owning team's tags promoting the VPC to `ACTIVE`, the second
> `apply` replaying the VPC and adopting the subnet with its
> `parent_allocation_id` set, and a third `apply` leaving both NetBox prefixes'
> `last_updated` untouched. It also imports a subnet created *after* the VPC was
> adopted, which is the case step 1 below cannot cover.

A VPC and its subnets cannot be adopted in the same `apply` invocation. The
service exempts a subnet's own parent VPC from the overlap rule only on the
evidence of the parent's **verified binding** — the same rule an ordinary
subnet reservation uses (`parentResourceMatches`) — and an adopted VPC is born
`RESERVED` with **no** binding, because the platform cannot tag a VPC it does
not own (ADR 0010, dated 2026-09-18 paragraph). So a subnet attempted right
after its own parent is adopted would be refused as overlapping "another cloud
resource" — true, and useless to an operator. `adopt` reports this case
explicitly as `waiting_for_parent` instead of a refusal, and it does not stop
the run.

The sequence:

1. `adopt apply` with the VPC row (and, if you like, its subnet rows in the
   same file — they will report `waiting_for_parent` and the run still exits
   non-zero, but nothing is broken by including them). The VPC becomes
   `RESERVED`, committed, unbound.
   - Check: `platform-ipam client get --id <allocation-id>` under the
     **owning tenant's own credentials** returns `"state": "RESERVED"`,
     `"binding": null`. `adopt`'s own operator credentials cannot read this —
     `GET`/`client get` only ever returns a *committed* allocation to its own
     tenant, and the operator has no tenant.
   - Or check NetBox directly: `GET /api/ipam/prefixes/{netbox_prefix_id}/`
     — `status` is `"reserved"`, `custom_fields.platform_state` is
     `"RESERVED"`, and `custom_fields.platform_allocation_id` now names the
     new allocation.
2. **The owning team tags the VPC** with both platform tags —
   `platform-ipam:allocation-id` and `platform-ipam:allocation-key` — using
   **their own provisioning credentials**, not the operator's. Adoption
   itself never does this and cannot: `adopt`'s AWS access is read-only.
3. **The worker observes it.** On its next pass, `reconcileAllocations`
   (`internal/service/worker.go`) sees a resource whose id, type, account,
   region, primary CIDR and both tags match the allocation, promotes it to
   `ACTIVE`, and records a `binding_verified` event.
   - Check: `client get --id <allocation-id>` now returns `"state": "ACTIVE"`
     with a populated `"binding"` object; or the NetBox prefix's `status`
     becomes `"active"`.
4. **Re-run `adopt apply` with the same file.** The VPC row replays (`200`,
   nothing written); the subnet rows are now attempted for real, since the
   parent's binding is verified, and they adopt normally.

A subnet created after its VPC was adopted needs one extra step at the front:
import it first. `onboard plan` admits a row that lies **strictly inside** a
platform-managed vpc-scoped prefix and reports it as `inside-managed-vpc`, an
informational finding naming the parent allocation, so the occupancy `adopt`
later requires can still be written. Only that shape is admitted — a row equal
to a managed network, a row containing one, a row partly overlapping one, and
any row overlapping a managed **subnet** are still refused as
`overlaps-managed`. Then continue from step 4 above with the new subnet's row
in the file.

## 7. The paused window

Onboarding already prescribes pausing new consumer reservations in the domain
before writing to NetBox (`docs/ONBOARDING_IMPORT.md`, section 8); the same
pause applies while `adopt apply` runs, for the same reason and one more:

- The snapshot and the observation `adopt` reasons over are read **before**
  the write, not re-checked afterward except by the re-read the adapter itself
  performs. Anything that changes the ground truth between that read and the
  commit — the AWS resource being modified, another import touching the same
  space, someone editing the NetBox prefix by hand — is exactly the class of
  drift the guards exist to catch, and catching it produces a refusal or a
  stuck operation, not a silent wrong adoption. Don't manufacture that drift
  on purpose by running imports or manual NetBox edits against the same VRF
  while `apply` is running.
- **A pending `ADOPT` operation fences the whole overlap domain.** While one
  adoption is uncommitted, *every* reservation in that domain — consumer
  `POST /v1/allocations` included — is refused `503 domain_busy`
  (`pendingDomain`, checked in both `reserve` and `PlanAdoption`). Bulk
  adoption is therefore serial by construction: the global ledger lock and the
  full-table rewrite make every adoption a stop-the-world write, and the cost
  grows with the size of the whole ledger, not just the batch being adopted.
- The conditional `PATCH` to the NetBox prefix (`If-Match` on the read's
  ETag, on NetBox releases that return one) closes the **NetBox-side** window
  only — the moment between `adopt`'s own read of the prefix and its write. It
  does nothing for the ledger side or the AWS side; those are what the paused
  window and the domain-wide fence are for.

## 8. What the owning team does next

The team's first `POST /v1/allocations` after adoption, using the agreed key:

- **Same key, same CIDR/prefix length as adopted:** the tenant-and-key scan
  (the same one that makes any reservation idempotent) answers it directly —
  `200`, the same allocation, even though adoption itself never wrote an HTTP
  idempotency record under that key (ADR 0010).
- **Same key, a different prefix length (or any other immutable field):**
  refused `409 allocation_key_conflict` — permanently, for that key. There is
  no way to change an adopted allocation's shape after the fact; get the key
  right in the reviewed record before `apply`, not after.

**Terraform import** works exactly as it does for any other committed
allocation — adoption adds nothing provider-specific.
`ImportState` (`providers/terraform/allocation_resource.go`) is a plain
passthrough of the platform allocation id; it is followed by an ordinary
`Read` that populates the rest of the resource's schema from the API. There is
no adoption-aware import flow in the provider — do not expect one:

```sh
terraform -chdir=examples/terraform/vpc import platformipam_allocation.vpc <allocation-id>
terraform -chdir=examples/terraform/vpc plan
```

As with any import (`docs/CLIENTS.md`, section 5), the Terraform configuration
must describe the same immutable request the allocation actually holds
(scope, environment, region, account, prefix length, parent/AZ for a subnet),
and the AWS VPC/subnet itself is imported into its own `aws_vpc`/`aws_subnet`
resource separately — `platformipam_allocation` import never touches AWS.

## 9. What the warnings mean while the resource is untagged

Between adoption (step 1 above) and the owning team's tagging (step 2), the
allocation is `RESERVED`, committed, unbound, exactly like an ordinary
reservation the owning team hasn't yet tagged — with one deliberate
difference in what the reconciler reports about it:

- **`reservation_aged` (`WARNING`)** fires once the allocation has been
  unbound past the configured age threshold
  (`Lifecycle.ReservationAgeAlertHours`), exactly as it would for any
  untagged reservation. This is the one signal that measures the wait —
  use it to judge how long "waiting for the team to tag" has actually been
  going on.
- **`unmanaged_occupancy` is deliberately *not* raised for the resource this
  adoption reviewed**, even though it is still untagged. That finding would
  otherwise be fanned out to every eligible tenant of every pool in the
  domain and would tell them nothing they could act on — the platform, not
  they, is the reason it's untagged. The reconciler recognises the specific
  resource an adoption's durable record names (its type, account, region and
  *primary* CIDR still matching) and passes over it
  (`adoptedResources`, `internal/service/worker.go`).
- **A same-shaped *twin* is still reported.** If a second, different resource
  at the same account/region/type/CIDR shape shows up, or the reviewed
  resource's primary CIDR changes, or a resource carries somebody else's
  allocation claim, `unmanaged_occupancy` fires for it normally — only the
  specific reviewed resource, exactly as reviewed, is exempted. This is by
  design, narrowed in the F3 review from an earlier, broader version that
  would have suppressed the finding for *any* committed allocation and hid
  genuinely unreviewed occupancy from its owners.

Neither warning is actionable by anyone but the owning team (go tag the
resource) and, eventually, you (decide whether "waiting" has become "nobody is
ever going to tag this" — the code has no signal for that judgment call; it is
yours to make).

## 10. `adoption_stuck` (CRITICAL)

This is the finding that matters most operationally, because **while it is
open, the pending `ADOPT` operation fences the entire overlap domain: every
reservation there — consumer and operator alike — answers `503 domain_busy`**
until it is resolved. It appears on the allocation the stuck adoption belongs
to (`AllocationID` on the finding), severity `CRITICAL`
(`flagStuckAdoption`, `internal/service/worker.go`).

### Every cause the code raises it for

The worker's crash-recovery path (`recoverAdoption`) runs on a pending
`ADOPT` operation on every worker tick. It raises `adoption_stuck` when:

1. **The pending operation carries no durable adoption record** (should not
   happen for anything adopted after package F3, but is the fallback if it
   ever does).
2. **The allocation's own CIDR does not parse** (should not happen; a defensive
   guard).
3. **The reviewed cloud resource is no longer alone at the adopted CIDR** —
   `reviewedOccupancy` fails: the resource has disappeared, changed shape, or
   a second resource now overlaps the CIDR.
4. **The reviewed NetBox prefix has gone, become owned by something else, or
   lost its import tag** — `reviewedNetwork` fails against a fresh snapshot.
5. **The adapter refuses outright** — any `internal/netbox` `Adopt` error
   that is not a timeout or a cancellation: the prefix now belongs to another
   operation, another prefix already claims this allocation, the read-back
   after the `PATCH` didn't come back as expected, or the conditional
   `PATCH`'s `If-Match` was rejected because the prefix changed between the
   read and the write (`412 Precondition Failed` is treated as a **definite**
   refusal here, not uncertainty).
6. **The inventory converted a prefix that is not the one reviewed** —
   `Inventory.Adopt` locates a prefix by CIDR and VRF, not by id, so a
   mismatch between the answer and the operation's own durable record
   (`adoptedTheReviewedObject`) is a stuck adoption, never an overwrite.

**Not raised** — retried silently on the next worker pass, with no finding at
all — for a context deadline, a cancellation, or a network timeout talking to
the adapter (`uncertainInventoryError`), and for an unavailable or incomplete
snapshot/observation (the pass is simply skipped). The domain stays fenced
either way; only the *finding* is withheld while the cause is merely
"try again."

### Diagnosing which cause you have

A `202`/pending outcome in the `adopt apply` JSON report carries the
`allocation_id` and `operation_id` of the hold it left behind — keep that
report: the allocation is invisible to `GET` until it commits, and the finding
names it. If the report is gone, diagnose from the reviewed record itself (its
`tenant_id`, `allocation_key`, `netbox_prefix_id`, `resource_id`) and the
finding.

1. Ask the **owning tenant** to run `platform-ipam client findings` (or
   `GET /v1/findings` with their own token) — findings are tenant-scoped, and
   `adoption_stuck` is filed under the allocation's own tenant even though the
   allocation itself is invisible to `GET`/`client get` while uncommitted.
   Confirm the `allocation_id` on the finding matches the stuck record.
2. Re-check NetBox directly:
   `GET /api/ipam/prefixes/{netbox_prefix_id}/` — does it still carry the
   `platform-ipam-imported` tag and no ownership fields (cause 4)? Does its
   `custom_fields.platform_allocation_id` already equal this allocation's id,
   meaning the write landed and only the read-back or the operation's own
   agreement failed (cause 5/6)?
3. Re-check the cloud resource directly (console or CLI, read-only) at the
   `resource_id` the record names: is it still there, alone, matching the
   reviewed account/region/type/primary CIDR (cause 3)?
4. If NetBox and the cloud both still match the reviewed record exactly,
   suspect a transient adapter failure that outlived the "uncertain" window,
   or an operation record that is missing its durable adoption details (cause
   1/2) — these need code-level investigation, not operator repair.

### Who resolves it, and how

The finding is resolved automatically, by `resolveFinding`, at the moment a
later worker pass's `recoverAdoption` call succeeds — there is no manual
"resolve" action and none is needed once the underlying disagreement is gone.
Practically: if the cause was that the reviewed ground truth (the NetBox
prefix or the cloud resource) drifted from what was reviewed, restoring it to
match what the operator actually reviewed lets the next worker pass converge
and close the finding on its own.

**Abandoning a stuck adoption ([ADR 0012](../../docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)).**
When the reviewed record turns out to have simply been wrong — the wrong
CIDR, the wrong resource id, the wrong prefix id — and the ground truth
cannot be made to match what was reviewed, recovery (above) will never
converge on its own. `platform-ipam adopt abandon` is the third `adopt`
subcommand, and it exists for exactly this case. It acts only on an
**uncommitted** adoption (`internal/adoptcmd/abandon.go`,
`internal/service/abandon.go`) — never on a committed allocation; see section
12 — in a forced order: *fence* (the pending `ADOPT` operation becomes
terminal, so no commit can follow it and the overlap domain stops being
fenced), *clear* (any NetBox prefix carrying this allocation's **and** this
operation's own markers is returned to imported occupancy — its ownership
fields are emptied, and the import tag, batch and source survive
untouched), *delete* (the allocation row and the adoption's own idempotency
record are removed, and the `adoption_stuck` finding is resolved). There is
no cancel and no delete-pending-operation for anything else in this codebase
(ADR 0010's "No package may add a delete or 'un-adopt'" still holds for a
committed allocation, absolutely).

The procedure:

1. **Dry run first.**
   ```sh
   platform-ipam adopt abandon --allocation-id <id> --operator <subject> \
     --reason "<why this adoption cannot be finished>" --dry-run
   ```
   This runs every refusal check the fence would and the same inventory
   search the clear would, writes nothing to the ledger or to NetBox, and
   prints the same JSON report shape a real run does. Read
   `inventory.claim`:
   | `inventory.claim` | What it means |
   | --- | --- |
   | `unclaimed` | Nothing on NetBox names this allocation — either the adapter write never happened, or an earlier, interrupted abandon already cleared it. A real run's clear step would write nothing. |
   | `this_operation` | A prefix carries exactly this allocation's and this operation's own markers — the half-converted case abandon exists for. A real run's clear step would empty its ownership fields. |
   | `another_operation` | A prefix claims this allocation under a **different** operation id. Something else may still be finishing it (a concurrent recovery pass, a race) — do not proceed; this needs engineering involvement, not this command. |
   | `ambiguous` | More than one prefix claims this allocation — needs engineering involvement; the inventory refuses to clear when this is true. |
   | `unknown` | The inventory snapshot could not be read, or is incomplete. Wait for a fresh, complete snapshot and retry the dry run. |

   `would_do` states, in the order a real run performs them, exactly what
   fencing, clearing and deleting would do for this allocation.
2. **Run it for real, with a reason.** Drop `--dry-run`. `--reason` is
   mandatory free text recorded in the audit trail (`ADOPT_ABANDONED`)
   beside `--operator`; both are unverified audit strings, exactly as
   `--operator` is on `apply` (section 3) — restricting who may run this
   command is the same deployment control, not something the code enforces.
   `--operation-id` is optional and, when given, must name this allocation's
   own `ADOPT` operation, or the command refuses.
3. **Confirm the finding resolved and the domain answers reservations
   again.** The report's `finding_resolved` is `true` when an open
   `adoption_stuck` finding existed and was just resolved by this run;
   `client findings` (or `GET /v1/findings`) for the owning tenant, or the
   operator role of ADR 0011, no longer shows it `OPEN`. A reservation
   elsewhere in the same overlap domain no longer answers
   `503 domain_busy`.
4. **Re-review the record**, exactly as before the first `apply` (section 2
   above): confirm the correct CIDR, resource id and NetBox prefix id
   against what is actually there now — the point of this whole procedure is
   that the original review was wrong about one of them.
5. **Adopt again under the SAME key.** The allocation key is freed, not
   retired, by an abandon (ADR 0012) — the owning team's written agreement
   for that key (section 2, point 4) still holds. Re-run `apply` with the
   corrected reviewed record.

### Running `abandon` in Kubernetes: `operatorJob`

The same procedure runs through `deploy/helm/platform-ipam/templates/operator-job.yaml`
(work-plan package H7) as an alternative to a local invocation — see
[the chart README](../helm/platform-ipam/README.md#example-adopt-abandon)
for the full values reference. Unlike `plan`/`apply`, `abandon` mounts no
input ConfigMap: its flags arrive entirely through `operatorJob.args`, and
the render fails if `operatorJob.input.existingConfigMap`/`.key` is set for
it (a table handed to a command that takes none is a mistake worth
stopping).

`identity.existingConfigMap` is **not required for `abandon`** (work-plan
package H9), unlike `plan`/`apply`: `abandon` resolves no acting principal
at all (`internal/adoptcmd/abandon.go`'s `runAbandon` never reads the
identity file, and `Service.AbandonAdoption` takes no `domain.Principal`,
ADR 0012), and the chart neither demands nor mounts it for this command even
if it happens to be set chart-wide for a `plan`/`apply` Job.

1. **Dry run first**, under its own `runId`:
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
   `helm upgrade` this in (a real deployment — this is not a Helm hook, so
   `helm upgrade --dry-run` does not run it), then read the Job's log
   (`kubectl logs job/<release>-operator-<runId>`) exactly as step 1 above
   describes.
2. **Run it for real, with a reason**, dropping `--dry-run` and picking a
   **new** `runId` (the same one changes nothing on a re-`upgrade`, per the
   chart README's immutability rule):
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
3. **Read the Job's log and confirm**, exactly as steps 3–5 above describe;
   `kubectl get job` shows `Failed` for a non-zero exit code, with `3`
   meaning a refusal to act on (an unknown allocation, a committed
   allocation, the wrong or an already-terminal operation, an abandon
   already in progress) and `4` an uncertain or infrastructure outcome where
   re-running the same Job under a new `runId` is always safe. Delete or
   disable the block afterward, as section 3 above describes for `plan`/`apply`.

**Whoever can set these Helm values can abandon adoptions under
`operatorJob`'s worker-equivalent credentials** — the same warning as
section 3 above for `plan`/`apply`: the release pipeline's own access
controls are the actual control, not `--operator`, which remains just an
audit string.

**Not verified by this procedure and said so:** it has been checked by
`helm lint`/`helm template` only (`scripts/ai/check-helm`) — never run
against a real cluster, database, or NetBox.

Exit codes follow the same shape as `plan`/`apply`: `0` ok (the report's
`deleted` is `true`, or, for `--dry-run`, the dry run completed); `2` usage
(a missing or blank `--allocation-id`/`--operator`/`--reason`, or any
positional argument — abandon takes none); `3` a refusal the operator must
act on (section 13 lists the codes — an unknown allocation, a committed
allocation, the wrong or an already-terminal operation, an abandon already in
progress); `4` an uncertain or infrastructure outcome, where **re-running the
same `abandon` command is always safe**. A second `abandon` of an allocation
already abandoned refuses with `abandon_unknown_allocation` — the row is
gone, which is convergence, not an error to chase.

## 11. Re-running is always safe

Every `adopt apply` re-run over the same file converges: `plan`'s pre-flight
pass reports already-committed records as `would_replay`, and `apply`'s write
phase replays them (`200`, nothing new written) rather than re-adopting.
Re-running is the correct response to a `202`/pending outcome, to a
`waiting_for_parent` file once the parent is `ACTIVE`, and to any transient
adapter failure — it never risks a duplicate adoption, because a committed
adoption's identity is the allocation key, checked before anything is
written, on both the pre-flight `plan` pass and the write itself.

## 12. There is no undo

Release (`DELETE /v1/allocations/{id}`, `platform-ipam client release`) treats
an adopted allocation exactly like any other: it moves it to `QUARANTINED` and
starts the quarantine clock (`Service.Release`, `internal/service/service.go`
— no branch for how the allocation was created). Once `QUARANTINED`, the
allocation key is **permanently retired**: any future request under that key,
adoption or ordinary reservation, is refused forever with
`409 allocation_key_retired`. Nothing in this codebase resurrects a retired
key — `allocation_keys` is regenerated from the allocations map on every
ledger write, so there is no tombstone to selectively erase without deleting
the allocation row itself, which nothing here does (ADR 0010).

Reclamation (freeing the CIDR by deleting the NetBox prefix) needs a run of
trusted "absence" observations confirming the cloud resource is *gone*
(`reclaimEligible`, `internal/service/service.go`). Adoption never deletes,
and cannot delete, the underlying AWS resource, so as long as that VPC or
subnet still exists in AWS, the observation keeps finding it present and
reclaim never becomes eligible — the wrongly adopted allocation sits in
`QUARANTINED`, its CIDR held, its key dead, indefinitely. If reclaim does ever
run (because the resource really was deleted), `Client.Delete`
(`internal/netbox/client.go`) issues an HTTP `DELETE` on the whole NetBox
prefix object after verifying it still carries this allocation's markers —
removing the object outright, taking the `platform-ipam-imported` tag,
`platform_import_batch` and `platform_import_source` with it. That is correct
for a real allocation being reclaimed and wrong for occupancy that this
adoption should never have claimed — the occupancy record is simply gone, and
restoring it means re-running the import for that CIDR from scratch.

**There is no procedure that returns a *committed*, wrongly adopted network to
unmanaged occupancy.** `plan` and the review before it are the only
protection ADR 0010 offers, deliberately: a committed adoption is corrected
forward — a new, correctly reviewed adoption under a different key, once the
old one is understood to be wrong — never reversed.

`adopt abandon` (section 10, [ADR 0012](../../docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md))
is not an exception to this — it applies **only** to an adoption that never
committed. It refuses a committed allocation absolutely, in every state a
committed allocation can be in (`abandon_committed`, section 13), and nothing
in this codebase returns a *committed* wrongly adopted network to unmanaged
occupancy: an uncommitted hold is a reservation of intent the code itself
still calls incomplete in several places, so withdrawing it removes an
intent, not a fact, while a committed allocation is ownership, and release
into `QUARANTINED`, above, is the only thing that ever happens to a mistake
once it has committed.

Its mirror is the consumer's own `DELETE /v1/operations/{operation_id}` /
`platform-ipam client cancel` ([ADR 0013](../../docs/decisions/0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md)),
which this runbook does not otherwise cover because it has nothing to do
with `adopt`. The same boundary holds in both directions: a cancel applies
**only** to a `RESERVE` that never committed, and it is the *consumer's* own
write, authenticated as the tenant that holds the reservation, refused for a
committed allocation exactly as `abandon` is; an abandon applies only to an
`ADOPT` that never committed, and it is the *operator's* act. Neither exit
ever returns a committed allocation to unmanaged occupancy or to a pending
state — release into `QUARANTINED` remains the only thing that happens to a
mistake once it has committed, whichever kind of operation created it.

## 13. Refusal and error codes

| Code | Status | What it means for `adopt` | What to do |
| --- | --- | --- | --- |
| `invalid_request` | 422 | A structural problem in the reviewed record or the run itself: missing `--operator`, a non-canonical CIDR, a CIDR whose mask doesn't match the request's prefix length, a `vpc` row with a forbidden parent/AZ column, a `subnet` row missing one | Fix the CSV row or the command line; not a ground-truth problem |
| `invalid_parent` | 409/422 | The named parent isn't a usable VPC allocation for this tenant/account/region, or (from `resolvePrincipal`/the parent lookup) the parent key resolves to nothing in the ledger or this file | Check `parent_allocation_key` spelling; confirm the parent row is present (this file or already adopted) |
| `adoption_conflict` | 409 | The allocation key already holds a different CIDR than reviewed, or is already committed on a different inventory object | The key was already used for something else — do not force it; pick the correct key or investigate with the owning team |
| `allocation_key_conflict` | 409 | The key belongs to a different **immutable** request (scope, environment, region, account or prefix length disagree) | The reviewed record must match the owning team's intended request exactly, character for character |
| `allocation_key_retired` | 409 | The key was adopted (or reserved) and then released — permanently dead | A new key is required; this one can never be reused for anything |
| `idempotency_mismatch` | 409 | The same tenant+key adoption was attempted twice with a different request body | The reviewed CSV row changed between runs, or two rows share a key by mistake |
| `domain_busy` | 503 | Another pending operation (a reservation, another adoption, or crash recovery) is fencing this overlap domain | Wait for it to resolve; if it never does, check for an `adoption_stuck` finding (section 10) |
| `quota_exceeded` | 409 | The pool's allocation limit or the parent's child limit is already reached | Not something `adopt` can bypass; raise the limit in configuration or defer this record |
| `policy_violation` | 422 | The pool configuration is missing, or the adopted CIDR lies outside the pool (or, for a subnet, outside its parent) | The CIDR or the pool assignment is wrong in the reviewed record |
| `adoption_refused` | 409 | The safety-critical bucket: the CIDR overlaps excluded address space, another ledger allocation already holds it, or — most often — the **named NetBox prefix or cloud resource wasn't found alone at exactly this CIDR** | Re-verify `netbox_prefix_id` and `resource_id` against what is actually there right now; the ground truth may have changed since import/observation, or the ids in the record are wrong |
| `dependency_unavailable` | 503 | The inventory snapshot or the cloud observation is incomplete, stale or missing | Wait for a fresh, complete NetBox snapshot and cloud scan; check NetBox/AWS connectivity |
| `not_found` | 404 | (Outside adoption itself, on `client get`/`list`) an uncommitted or foreign-tenant allocation is invisible by design | Not a bug: a pending/stuck adoption cannot be read back this way (section 10) |
| `adapter_error` | — | `adoptcmd`'s catch-all for a non-`APIError` failure (context deadline, transport failure) reaching the ledger, NetBox or the cloud observer | Infrastructure problem, not a record problem — check connectivity; `apply` exits `4` for this, not `3` |

### `adopt abandon` codes (section 10, ADR 0012)

| Code | Status | What it means | What to do |
| --- | --- | --- | --- |
| `abandon_allocation_required` / `abandon_operator_required` / `abandon_reason_required` | 422 | `--allocation-id`, `--operator` or a non-blank `--reason` is missing | `internal/adoptcmd`'s own usage check catches all three before the ledger is even opened (exit `2`); these codes are the service's backstop, reachable only by calling it directly |
| `abandon_unknown_allocation` | 404 | No allocation with this id exists in the ledger | Check the id; a *second* `abandon` of an allocation already abandoned answers this too — that is convergence, not an error |
| `abandon_committed` | 409 | The allocation is committed and therefore owned: it can never be abandoned, in any state | See section 12 — `DELETE /v1/allocations/{id}` (release) is the answer there, not abandon |
| `abandon_operation_mismatch` | 409 | The given `--operation-id` does not name this allocation's operation | Check the id, or omit `--operation-id` and let it be resolved automatically |
| `abandon_no_operation` | 409 | The uncommitted allocation has no operation recording what was reviewed | This code cannot produce that state during normal operation — engineering involvement, not an operator step |
| `abandon_ambiguous_operation` | 409 | More than one operation names this allocation | Engineering involvement: name the exact one with `--operation-id` only once it is clear which is meant |
| `abandon_not_an_adoption` | 409 | The allocation's operation is a `RESERVE`, not an `ADOPT` | `adopt abandon` never touches a reservation; if the platform has raised `reservation_stuck` for it, its own tenant withdraws it with `DELETE /v1/operations/{operation_id}` or `platform-ipam client cancel` (ADR 0013, section 12 above), not this command |
| `abandon_operation_succeeded` | 409 | The operation already succeeded — the commit won the race while this abandon was starting | The allocation is now committed; see section 12 |
| `abandon_operation_terminal` | 409 | The operation is already terminal in some other way this abandon did not write | Investigate what terminated it before retrying |
| `abandon_state_changed` | 409 | Something claimed or changed the allocation between the fence and the delete | Re-run `abandon` — a re-run converges (ADR 0012) |
| `adoption_abandoning` | 409, retryable | An abandon of this adoption is between its fence and its delete; a concurrent `apply` or the owning team's own `POST` under the same key met this window | Retry once the in-progress abandon has finished (an interval, not a mystery) |
| `abandon_inventory_refused` | 409 | The inventory refused to clear the network (ambiguous claim, the wrong operation's marker, or a `412` conflict) | Nothing has been deleted; re-running is safe once the cause named in the message is resolved |
| `abandon_uncertain` | 503 | The inventory did not say whether it cleared the network (a timeout, no answer) | Nothing has been deleted; **re-running this abandon is always safe** |
| `abandon_incomplete` | 503 | The ledger fence or delete transaction could not be completed | **Re-running this abandon is always safe** — every interruption of the sequence converges (ADR 0012) |

## What this runbook does not cover

Live AWS onboarding and a Kubernetes rollout of `adopt` are not verified —
see section 3's gap.

**Demonstrated end to end** against the development stack — the real ledger and
a real NetBox, with the fake cloud observer — by `tests/e2e/test_e2e_adopt.py`:
`plan` writing nothing; `apply` adopting a VPC that has no subnets; the
NetBox prefix gaining the ownership fields while keeping the import tag, batch
and source; no `unmanaged_occupancy` for the adopted resource while a decoy
still has one; the owning team's `POST` replaying `200` and a different prefix
length conflicting; tagging making the allocation `ACTIVE`; a further `apply`
writing nothing; NetBox and the API agreeing in both directions; release
retiring the key; the worker committing a pending `ADOPT` operation (seeded
into the ledger, not produced by a real interruption); and five refusals. The
same module demonstrates the two runs of section 6 for a VPC with one subnet:
the first `apply` adopts the VPC and reports the subnet `waiting_for_parent`
(exit `3`); after the VPC is tagged and `ACTIVE` the second `apply` replays the
VPC and adopts the subnet under the VPC's allocation id (exit `0`); a third
writes nothing; a stranger's subnet or an imported prefix that no observed
subnet explains still refuses the VPC; and a subnet created after the VPC was
adopted can still be imported.

**`adopt abandon` (section 10, ADR 0012), demonstrated end to end** by the
same module's `AdoptAbandonE2ETest`, against the real ledger and a real
NetBox: a half-converted, genuinely stuck adoption is seeded (the pending
`ADOPT` operation's durable record names a NetBox prefix id that does not
exist at the adopted CIDR, so `reviewedNetwork` itself refuses and the worker
raises `adoption_stuck`, never reaching `Inventory.Adopt`) on a prefix
carrying this allocation's and this operation's own markers, written
directly to simulate the adapter write an interrupted `Adopt` would have
made; the domain answers a fresh reservation `503 domain_busy` while it is
open. `abandon --dry-run` reports `inventory.claim: "this_operation"` and
writes nothing (prefix fields, ledger row count, and the finding all
unchanged). A real `abandon` clears every ownership field from the prefix
while keeping the import tag, batch, source, and the AWS account and region
the import wrote (`prior_account_id`/`prior_region`), restores `status:
active`, deletes the allocation row and its `ADOPT` idempotency record,
resolves the `adoption_stuck` finding, and the domain answers reservations
again. A second `abandon` of the same id refuses with
`abandon_unknown_allocation` and writes nothing. The freed key adopts the
same network correctly (`adopt apply`, a fresh allocation id, not a replay).
Abandoning that now-committed allocation refuses with `abandon_committed`
and leaves the prefix's markers untouched. `adopt abandon` with no `--reason`
exits `2` before any dependency is built. `test_netbox_holds_no_managed_prefix_the_api_does_not_know`
and its converse both still pass throughout.

**Not demonstrated:** a VPC with many subnets, or one whose subnets are only
partly imported (the rule is unit-tested, not run end to end); reclamation after
release (the quarantine is longer than a test run); a real interruption between
the adapter write and the commit (both the ordinary adoption scenarios above and
the abandon scenario seed the state a real interruption would leave, rather than
racing one); `abandon` running through `operatorJob` against a real cluster
(section 3/10: supported by the chart as of work-plan package H7, checked
only by `helm lint`/`helm template`, never run against a real cluster,
database or NetBox); and anything against live AWS.
