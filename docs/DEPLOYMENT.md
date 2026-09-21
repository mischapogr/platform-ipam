# Development, staging, and production deployment

Status: implemented deployment artifacts, 2026-09-08. Development uses Docker Compose; stage and production use Kubernetes with Helm charts. Local container readiness and a Kubernetes rollout remain separate verification gates.

## 1. Environment matrix

| Concern | Development | Staging (`stage`) | Production (`prod`) |
| --- | --- | --- | --- |
| Runtime | Docker Compose on a developer machine | Kubernetes | Kubernetes |
| Platform packaging | Locally built API/worker image | Versioned platform Helm chart | Same chart version and image digest promoted from stage |
| Platform database | Dedicated PostgreSQL Compose service and named volume | Isolated external PostgreSQL with backups | Isolated production PostgreSQL with HA/backups |
| NetBox | Local Compose NetBox stack by default | Staging NetBox; separate Helm release or existing instance | Production NetBox; separate Helm release or existing instance |
| NetBox dependencies | Separate NetBox PostgreSQL, Redis services, and worker | Dependencies managed by NetBox deployment owner | Dependencies managed by NetBox deployment owner |
| AWS access | Fake cloud adapter by default; optional sandbox read role | Dedicated staging/sandbox account roles | Explicit production account/region read-role matrix |
| Identity | Explicit local-only token mode | Real workload identity/SSO integration | Real workload identity/SSO integration |
| Networking | Loopback-published API port; NetBox itself publishes no port -- `ui-proxy` is the only published path to its UI ([GUI authentication](GUI_AUTHENTICATION.md)), private Compose network | Internal TLS ingress and network policies; optional Helm `uiProxy` in front of an external NetBox | Internal TLS ingress and network policies; optional Helm `uiProxy` in front of an external NetBox |
| Pool inventory | Disposable synthetic fixture ranges | Independent test pools | Reviewed company address plan |
| Scale | One API and one worker, plus a multi-replica test | Production topology at reduced capacity | Capacity sized from measurements |
| Updates | Rebuild and recreate services | Migration and rollout rehearsal | Promotion after stage evidence |

Deployment environment and allocated network `environment` are separate concepts. For example, the production platform service might manage both development and production network pools. Do not derive consumer authorization from a namespace or Helm release name. Local/staging services cannot allocate from production inventory or use production credentials.

## 2. Deployment files

```text
deploy/
├── compose/
│   ├── compose.yaml              # Local platform API, worker, ledger, cloud fake
│   ├── compose.netbox.yaml       # Local NetBox stack and its dependencies
│   ├── compose.dev.yaml          # Optional source mounts/watch commands
│   ├── .env.example              # Non-secret defaults and variable names
│   └── README.md                 # Setup, seed, reset, and recovery commands
├── helm/
│   └── platform-ipam/
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── values.schema.json
│       └── templates/            # Deployments, Services, Jobs, configuration
├── environments/
│   ├── stage/values.yaml
│   └── prod/values.yaml
└── runbooks/
    ├── MIGRATIONS.md
    ├── RESTORE.md
    └── RELEASE_AND_ROLLBACK.md
```

Environment configuration may live in a dedicated GitOps repository later. Preserve the same values schema and versioned application configuration whichever repository owns deployment state.

## 3. Docker Compose development design

Use the same API/worker entry points and database migrations as Kubernetes. The multi-stage Dockerfile provides development and release targets; bind mounts/reload are optional overrides, while integration tests use the release target.

| Compose service | Purpose |
| --- | --- |
| `api` | Platform HTTP API, loopback port `8080` |
| `worker` | Pending operation recovery, NetBox projection, reconciliation |
| `platform-db` | Platform ledger PostgreSQL; unique user/database/volume |
| `migrate` | One-shot platform migration service; exits successfully before API/worker start |
| `cloud-fake` | Deterministic VPC/subnet observations, missing tags, throttling, and incomplete scans for local scenarios |
| `netbox` | Real local NetBox API and browser UI; publishes no port itself |
| `ui-proxy` | The only published path to the NetBox UI, loopback port `18000` by default (`NETBOX_PORT` can override it, for both `ui-proxy` and the internal NetBox target it proxies to); `basic`, `entra` or `ldap` mode depending on which optional overlay is layered on (deploy/compose/README.md, [GUI authentication](GUI_AUTHENTICATION.md)) |
| `netbox-worker` | NetBox's own background work; distinct from the platform worker |
| `netbox-db` | Separate NetBox PostgreSQL and volume |
| `netbox-redis` / cache service as required | NetBox's required queue/cache configuration for the pinned image |
| `seed` | Explicit one-shot development bootstrap for tenant, VRF, pools, custom fields, and sample inventory |

### Worker reconciliation pass cost (measured 2026-09-21, package M4a)

The worker's `Tick` runs every `reconciliation.full_scan_interval_seconds` (30s in development;
`cmd/platform-ipam/main.go`). Its last step, `syncProjections`
(`internal/service/worker.go`), PATCHes **every** committed, non-released allocation's NetBox
prefix on **every** pass, unconditionally, to refresh `platform_last_observed_at` and the other
platform-owned custom fields -- known since package H4's end-to-end note. Gap M4 asked for measured
behaviour at representative counts before deciding whether that cost is worth reducing.

**Method.** A throw-away Compose project (`-p platform-ipam-m4a`, own volumes, own host port,
brought down with its volumes afterward) ran the same images against a synthetic ledger of
committed allocations, each paired with a matching NetBox prefix carrying the platform's own
custom-field markers (bulk-created through NetBox's REST API; the ledger rows inserted by SQL
piped to `psql`, under a distinct synthetic tenant so they never touched the real quota). NetBox's
own access log (`uwsgi`/gunicorn request line, one entry per HTTP request with the server-side
processing time in milliseconds) gave exact per-request counts and latencies with no extra
instrumentation; wall time is the span between the first and last logged request of a pass.

**Before (the every-pass, per-allocation `ledger.Update` this section first measured).**

| Committed allocations | NetBox requests/pass | Pass wall time | NetBox time (share of wall time) | NetBox latency (median / p95) | Pass as a share of the 30s interval |
| ---: | ---: | ---: | ---: | --- | ---: |
| 100 | 200 (2/allocation) | 54s | 40.6s (75%) | GET 147ms / 304ms, PATCH 218ms / 461ms | 180% |
| 1,000 | ~2,012 (2/allocation) | 879s (14.6 min) | 240.3s (27%) | GET 87ms / 160ms, PATCH 132ms / 249ms | 2,930% |
| 5,000 | 10,012 (2/allocation), extrapolated | ~13,358s (~3.7h), extrapolated from an 8.7-minute observed window at 0.75 req/s | 45.4s observed in that window (8.7% of the window) | GET 80ms / 189ms, PATCH 125ms / 267ms | ~44,500% |

The 5,000 "before" row was not run to completion -- the extrapolated pass length made that
impractical inside a single foreground command, so the number is a rate extrapolation from an
observed window, not a measured total.

An ordinary reservation issued concurrently with a pass was also measured against this "before"
code: at 100 committed allocations it took 2.0-2.9s against a 1.2-1.9s idle baseline (three
samples each); at 5,000 a single sample took 20.7s. `POST /v1/allocations` and every
`syncProjections` iteration shared one ledger transaction, serialized by a single Postgres
advisory lock (`internal/storage/postgres.go`'s `pg_advisory_xact_lock`), so a consumer's request
could queue behind however many of the worker's per-allocation writes were already in flight.

**What actually scaled, and why it was worse than the NetBox traffic alone.** NetBox's own
per-request latency did not grow with the allocation count (flat or slightly improving across the
three sizes above, consistent with primary-key reads/writes) -- NetBox was never the scaling
problem. The dominant cost was internal: `internal/storage/postgres.go`'s `persistState`, called
by every `Ledger.Update`, `DELETE`s and re-`INSERT`s **every row of every ledger table**
(allocations, operations, findings, and the rest) on every call, regardless of how much of the
state actually changed, and the old `syncProjections` called `s.ledger.Update` **once per
allocation** -- so one pass rewrote the whole ledger N times for N committed allocations, an O(N^2)
cost on top of the O(N) NetBox traffic. That is why NetBox's own time was 75% of the pass at N=100
but well under 10% of it by N~5,000: the gap was almost entirely repeated, unconditional Postgres
rewrites, not the PATCH calls the gap in the code names.

**The fix (Phase 3): batch the ledger write, not the PATCH.** `syncProjections` still calls
`inventory.Sync` (the GET+PATCH pair) once per allocation, in the same order, exactly as before --
that part of the cost is untouched. What changed is the bookkeeping write: every job's result
(revision, state, and whether `Sync` errored) is collected while the loop runs, and applied in
**one** `ledger.Update` after the loop, with the identical per-allocation guard the old per-job
write had (an allocation whose revision or state moved since its job was read is left alone) and
the identical PENDING+finding / CURRENT+resolve effects. A cheap pre-check (a read-only `View`,
never a second full-ledger rewrite) skips that single `Update` outright once every result is
already reflected in the ledger -- the steady-state case once nothing is drifting. Two things
are written regardless: a sync that **failed** is recorded on every pass, so the finding's
last-observed time keeps moving while the failure lasts; and a call that failed only because the
pass was cancelled under it is **not** recorded at all, because it says nothing about NetBox. See
`internal/service/worker.go`'s `syncProjections`, `applyProjectionResults` and
`projectionResultsAlreadyApplied`.

**After.**

| Committed allocations | NetBox requests/pass | Pass wall time | NetBox time (share of wall time) | NetBox latency (median / p95) | Pass as a share of the 30s interval | Speedup vs before |
| ---: | ---: | ---: | ---: | --- | ---: | ---: |
| 100 | 200 (2/allocation) | 16s | 15.9s (99%) | overall (GET+PATCH) median 76ms / p95 135ms | 53% | 3.4x |
| 1,000 | 2,000 (2/allocation) | 161s (2.7 min) | 160.3s (99.5%) | overall median 77ms / p95 112ms | 537% | 5.5x |
| 5,000 | 10,000 (2/allocation) | 813s (13.55 min), run to completion (not extrapolated) | 809.3s (99.6%) | overall median 78ms / p95 110ms | 2,710% | ~16.4x vs the "before" extrapolation |

Every "after" row is a directly observed, completed pass (the 5,000 row previously could not be
completed inside a practical number of foreground calls; after the fix it could). NetBox time is
now 99%+ of wall time at every size measured -- the ledger write has gone from the dominant cost to
effectively free, and what remains is (as expected) exactly the O(N) NetBox traffic the fix left
alone.

**What remains, stated plainly.** Two costs are unchanged by this package, on purpose (the narrowest
fix the numbers justified was batching the write, not skipping the PATCH):

1. **The PATCH itself still runs once per committed allocation per pass**, whether or not anything
   in the projection changed. At 5,000 allocations that is still a 13.55-minute pass, still far
   longer than the 30s development interval -- the "after" table's own numbers show this plainly.
   Skipping the PATCH when nothing changed (the option the gap's title also names) was considered
   and set aside for this package specifically because doing it correctly needs a durable
   per-allocation fingerprint of "what NetBox was last told" and a documented freshness bound for
   `platform_last_observed_at`, which touches `internal/domain` (outside this package's file grant)
   and was reserved for a follow-up.
2. **`persistState`'s whole-ledger rewrite on every `Update`** is untouched outside
   `syncProjections`'s own now-batched call: every other `ledger.Update` call in the codebase still
   pays it. It is a property of `internal/storage`, not of any one caller, and is an architectural
   question for the owner (gap M4), not a patch from this package -- see "Other O(N^2) shapes not
   changed here" below for exactly where else it shows up.

**Caveats.** A single-machine, laptop-class development NetBox and Postgres, both cold-cache
relative to a production deployment, are not a production measurement: absolute latencies will
differ (likely favourably) under provisioned infrastructure, but the O(N) NetBox / O(N^2) ledger
cost *shape* was a property of the code, not the machine, and reproduces at any scale; the same is
true of the fix's O(N) shape. Concurrent production traffic, network latency to a real NetBox, and
connection pooling behavior under load were not exercised, before or after.

**Other O(N^2) shapes not changed here.** `syncProjections` was not the only per-item
`ledger.Update` loop in `internal/service/worker.go`; the following were checked and deliberately
left alone in this package:

- `reconcileAllocations` already does its whole pass inside **one** `ledger.Update` call -- it does
  not have this shape and needed no change.
- `recoverReservations` and `recoverAdoptions` (through `commitRecovered`, `flagAgedReservation` and
  `flagStuckAdoption`/`flagStuckHold`) each call `ledger.Update` once per pending recovery job, the
  same shape `syncProjections` had. Their blast radius is bounded by the number of concurrently
  *pending* reservations/adoptions rather than the total committed count, which should be small in
  steady state, and their per-job effects are more varied (two different finding codes, adoption-
  specific audit events) than `syncProjections`'s uniform PENDING/CURRENT toggle -- batching them
  with the same guard and mutation-testing them as thoroughly is a larger, separate change.
- `reclaimTick` (`internal/service/service.go`, not `worker.go`, so outside this package's file
  grant regardless) has the same per-job `ledger.Update` shape after its own snapshot/delete calls,
  bounded by the number of quarantined, reclaim-eligible allocations rather than the total count.

All three are flagged here for a follow-up decision, as instructed, rather than changed in this
package.

### What M4a's measurement left out, measured (2026-09-22, package M4d)

[ADR 0017](decisions/0017-PERSISTING_ONLY_WHAT_A_LEDGER_TRANSACTION_CHANGED.md), accepted as an
intermediate step, asked M4d to measure the two things M4a's synthetic ledger could not: round trips
per `Ledger.Update` as a function of *each* state map's size (M4a's ledger held only allocations), and
reservation latency and pass behaviour with a **realistic** mix of operations, idempotency records,
findings and audit history. Nothing in `internal/storage` changed to take these measurements; both
parts run against the code exactly as M4a's fix left it.

**Part 1: round trips per `Update`, no database.** A new opt-in test,
`internal/storage/measure_update_test.go`'s `TestMeasureStatementsPerUpdate` (skipped unless
`IPAM_MEASURE=1`), instruments `persistState` directly with a counting fake of `pgx.Tx`: the fake
implements all eleven methods of `pgx.Tx` (pgx v5.10.0), panicking on every one `persistState` does
not call (`Query`, `QueryRow`, `CopyFrom`, `SendBatch`, `Prepare`, `Begin`, `Commit`, `Rollback`,
`LargeObjects`) and counting+tagging every `Exec` call by the table name parsed from its own SQL text.
This was chosen over a `pgx.QueryTracer` because a tracer needs a real `*pgx.Conn`/pool to attach to --
i.e. a database -- which this measurement must not require; the fake instead exercises the real
`persistState` function with no I/O at all, so a future change to its write pattern changes these
numbers automatically, and a call it does not expect panics loudly rather than under-counting silently.
The test builds synthetic `*domain.State` values and sweeps each of seven axes -- allocations,
operations (with roughly half marked `PENDING`, reproducing the record's `P` term), idempotency
records, findings, observation-carrying routing domains, coverage generations, and audit events -- at
sizes 0, 100 and 1,000, holding the other six at a constant 3.

Every measured total, and every per-table breakdown, matched
[ADR 0017](decisions/0017-PERSISTING_ONLY_WHAT_A_LEDGER_TRANSACTION_CHANGED.md)'s formula
`9 + 3A + O + P + R + D + F + C + E` exactly, at every size, on every axis (30 rows total; a
representative slice):

| Axis swept | Size | A | O | P | R | D | F | C | E | Measured `Exec` calls | Formula | Match |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | :---: |
| (baseline, all axes at 3) | 3 | 3 | 3 | 1 | 3 | 3 | 3 | 3 | 3 | 37 | 37 | yes |
| allocations | 1,000 | 1,000 | 3 | 1 | 3 | 3 | 3 | 3 | 3 | 3,028 | 3,028 | yes |
| operations | 1,000 | 3 | 1,000 | 500 | 3 | 3 | 3 | 3 | 3 | 1,533 | 1,533 | yes |
| idempotency | 1,000 | 3 | 3 | 1 | 1,000 | 3 | 3 | 3 | 3 | 1,034 | 1,034 | yes |
| findings | 1,000 | 3 | 3 | 1 | 3 | 3 | 1,000 | 3 | 3 | 1,034 | 1,034 | yes |
| observation domains | 1,000 | 3 | 3 | 1 | 3 | 1,000 | 3 | 3 | 3 | 1,034 | 1,034 | yes |
| coverage | 1,000 | 3 | 3 | 1 | 3 | 3 | 3 | 1,000 | 3 | 1,034 | 1,034 | yes |
| audit events | 1,000 | 3 | 3 | 1 | 3 | 3 | 3 | 3 | 1,000 | 1,034 | 1,034 | yes |

The record's formula is confirmed as an exact description of the code, not an approximation: **every**
term contributes its own round trip, one for one, with no batching or amortization anywhere. At the
sizes a realistic ledger of 1,000 committed allocations would plausibly carry (see Part 2's seeding
below: `A`=1,000, `O`≈1,000, `P`≈0 once reservations have committed, `R`=1,000, `E`=10,000 from ten
audit events per allocation), the formula predicts **9 + 3,000 + 1,000 + 0 + 1,000 + D + F + C +
10,000 ≈ 16,000** round trips for the ledger's single largest `Update` (the worker's batched
bookkeeping write after a pass) -- roughly *five times* the `3A`-only cost (≈3,009) M4a's thin,
allocations-only ledger would have produced at the same count, and dominated (about two-thirds) by the
audit-event term alone, exactly as the record's own reading of the code anticipated.

**Part 2: a realistic throw-away stack.** A second, disposable Compose project (`-p platform-ipam-m4d`,
`IPAM_API_PORT=18081`, its own volumes, `DOCKER_CONFIG` pointed at a scratch directory because the
sandbox's home directory is read-only) was brought up from the current code, bootstrapped and seeded
exactly as `deploy/compose/README.md` documents, then seeded with a synthetic ledger by SQL files piped
to `psql` (`_tmp/m4d/gen_ledger_sql.py`, git-ignored scratch), at two sizes in sequence, 100 then
+900 more (total 1,000 committed allocations under a synthetic tenant `m4d-synthetic`, never touching
the suite's own pool or quota). Unlike M4a's ledger, which carried only allocation rows, this one seeds,
per allocation: one committed allocation, one **completed** (`DONE`, not `PENDING`) reservation
operation, one idempotency record, and **ten audit events** (`planned`, `reserved`, `committed`, two
`synced` touches, a `release_requested`/`released`/`reserved`/`committed` cycle, and a final `synced`).
Ten was chosen because ADR 0017 itself names it: "a real ledger's history grows with every reservation,
release and pass ... ten per allocation is a defensible floor" -- this run seeds exactly that floor, so
the audit-term numbers below are a **lower bound** on a real deployment's, not an inflated one. A small,
fixed five findings (not one per allocation -- a healthy ledger has only a minority of allocations with
an open finding) were seeded once, alongside 1,000 matching NetBox prefixes created in two API batches
(100, then 900) through NetBox's REST API, tagged with the platform's own custom-field markers exactly
as M4a's `seed_prefixes.py` did.

*Reservation latency, idle versus during a worker pass.* Because the worker's own batched write and
NetBox traffic run continuously at these sizes (below), "idle" samples were taken with the m4d
project's own worker container stopped (never the API), and "during-pass" samples with it running; both
used `docker exec` into the api container (host loopback is unreachable from the sandbox), five or more
`POST /v1/allocations` samples each, ordinary vpc reservations against `pool_dev_euc1`:

| Committed allocations | Idle median / worst | During-pass median / worst | Samples |
| ---: | ---: | ---: | ---: |
| 100 | 1.03s / 1.63s | 1.06s / 1.46s | 5 idle, 6 during-pass (plus 10 unclassified mixed-window samples, 1.12-2.27s) |
| ~1,000 (1,000 synthetic + a few dozen of the measurement's own reservations, growing during the run) | 4.85s / 5.02s | 5.72s / 6.23s | 5 idle, 5 during-pass |

Two things stand out. First, **idle latency itself grew roughly 4.7x** between 100 and ~1,000
allocations (1.03s to 4.85s median) with the worker not running at all -- this is `reserve`'s own
`View` pre-check and planning `Update`, each paying `loadState`'s full decode of every table including
all ~10,000 audit rows (ADR 0017: "a read is unaffected [by decision one]... an `Update` remains
`O(state)` in rows fetched and decoded"). The `E` term costs reads as well as writes, and nothing in
Part 1 or in ADR 0017's decision one addresses that. Second, during-pass latency was only modestly
higher than idle at both sizes (about equal at 100, about 18% higher at ~1,000) rather than dramatically
worse, consistent with the record's observation that no ledger closure performs I/O: the worker's own
NetBox `GET`/`PATCH` calls happen outside the transaction, so a reservation racing for the single
advisory lock mostly queues behind short state loads/writes, not behind the pass's NetBox traffic.

*Pass wall time.* NetBox's own access log (the same instrument M4a used) shows, at 100 committed
allocations with this realistic ledger, three consecutive passes spanning **29.0s, 31.0s and 29.0s**,
each starting the instant the previous one's last request landed -- back to back, with **no idle gap**
between passes. At ~1,000 committed allocations, one full pass's NetBox-request span was **187s**. Set
beside M4a's post-fix numbers at the *same* allocation counts with its allocations-only ledger -- **16s**
(53% of the 30s interval) at 100, and **161s** at 1,000 -- this realistic ledger's pass at 100 now
consumes essentially the **entire** interval (97-103%, versus M4a's 53%) even though the fix from M4a's
own package (batching the bookkeeping write) is unchanged and in effect throughout; at 1,000 it is about
16% longer than M4a's thin-ledger equivalent. The 100-allocation case is the more consequential finding:
M4a's thin ledger made this size look like it had comfortable headroom (14s idle out of every 30s); a
ledger carrying the operations, idempotency and audit rows a real deployment would actually have leaves
**no** headroom at all at the same count.

*A first two-writer run.* `docker compose up -d --scale api=2` was attempted and refused --
`compose.yaml` publishes a fixed host port for the api service
(`127.0.0.1:${IPAM_API_PORT}:8080`), which a second replica cannot also bind, and changing that file is
outside this package's grant -- so the instructions' other named option was used: two concurrent client
loops (8 reservations each, 16 total, distinct allocation keys) against the one api replica, with the
worker running throughout. Eight of sixteen committed (`201`); the other eight received `503
domain_busy` ("an unresolved operation is fencing this overlap domain", `retryable: true`) -- the
documented overlap-domain fence declining a concurrent request, not a failure. Two further diagnostic
calls also committed and a third was fenced, for ten total new `developer`-tenant allocations.
**Lost-update check:** all ten have distinct `allocation_key`s, distinct CIDRs and distinct NetBox
`inventory_id`s (prefixes 1034-1043, sequential); `allocations` and `allocation_keys` row counts matched
exactly (10 = 10); a ledger-wide query for any `(tenant_id, allocation_key)` with more than one row
returned zero; and every one of the ten NetBox prefixes, read back directly by id, carried a
`platform_allocation_id` matching exactly one ledger row, one to one. No lost update, no duplicate key,
no orphaned NetBox object was found under this concurrency, at this scale, on this one run.

**What the numbers justify.** Item 2 of ADR 0017's evidence list is answered: the formula is exact, not
approximate, and a realistic ledger's `Update` cost is dominated by the audit-event term specifically
(about two-thirds of the round trips at the sizes measured) -- worse than M4a's allocations-only
measurement implied, and, per the record's own point, this term grows with **age**, not just size, so
it will keep growing on a fixed-size ledger that keeps running. That justifies proceeding with **M4e**
(the audit table alone): it is the single largest term, the smallest correct change, and removing it
does not touch the seven-table diff that carries the real correctness risk. It also justifies **M4f**
(the remaining diff) on the same reasoning at smaller scale (`3A + O + R` together are still a large
share of the total once `E` is gone), though M4f is the package that carries the correctness risk and
should wait for M4c's harness, per the record's own ordering. The pass-time and idle-latency findings
above argue **against** treating M4g (the projection `PATCH` skip) as urgent on its own: pass time is
still NetBox-request-bound (matching M4a exactly), M4g addresses at most half of that, and this
package's own numbers show the ledger-write side of the cost (not the NetBox side) is what changed
between M4a's thin ledger and a realistic one. **Item 3** is answered for the current code only, at one
machine, one run: reservation latency during a pass was measured (five-plus samples at two sizes, idle
and during-pass), and a two-writer run happened at least once with no lost update found -- but a single
run at modest concurrency (two clients, sixteen requests) is evidence the lock is not *obviously* broken
under contention, not a measurement of how it behaves under the production topology's five processes
(`api.replicas: 3`, `worker.replicas: 2`), which this package did not reproduce (scaling to two API
replicas was blocked, as above, and no second worker replica was run). **What would change this
recommendation, and how it depends on Q9:** if the owner's answer to Q9 says the representative ledger
size is a few hundred allocations with only occasional passes (not the continuous back-to-back pattern
measured here), the audit term's age-driven growth is still the dominant future cost and M4e remains
justified regardless; if Q9 says thousands of allocations with a hard 30-second interval requirement,
today's data already shows that requirement is unreachable through M4e/M4f/M4g alone (the pass is
NetBox-bound, not ledger-bound, at that scale) and the deferred per-aggregate-transaction option in ADR
0017 needs to move forward on a real two-writer measurement at production topology, which this run does
not provide.

**Caveats, as M4a's own section states them.** One machine, laptop-class, both Postgres and NetBox
cold-cache relative to a production deployment; absolute latencies will differ (likely favourably) under
provisioned infrastructure, though the cost *shape* -- write amplification proportional to state size
plus an unbounded audit term -- is a property of the code and reproduces at any scale. The two-writer
run used two client loops against one api container, not two real api replicas (blocked by the fixed
host port) and not a second worker replica; it demonstrates the lock does not obviously lose updates
under modest concurrency, and no more than that. The "idle" figures required stopping the m4d project's
own worker container (never `platform-ipam-dev`'s); a deployment's worker is never actually stopped, so
those numbers describe the ledger's cost under a reservation alone, not a claim about achievable
production idle latency. The findings seeded (five, fixed) and the observation-domain and coverage rows
(none seeded in Part 2, only exercised synthetically in Part 1) were not varied at realistic ratios in
Part 2's throw-away stack; Part 1's per-axis sweep is exact regardless, because it measures the code
directly rather than a scenario.

Base Compose should be able to use an external development NetBox by configuration; the NetBox override adds the complete local stack. Verify upstream image-specific startup, migration, worker, Redis, and healthcheck settings rather than copying unpinned commands. Upstream provides a Docker-based NetBox deployment project. [NetBox Docker](https://github.com/netbox-community/netbox-docker).

All dependencies need healthchecks; migration completion and actual readiness should gate startup. `depends_on` ordering by itself does not establish application readiness. Store generated local credentials in ignored local files or local secret mounts; commit variable names and placeholders only. Do not expose database or Redis ports by default.

Start the local stack with:

```bash
docker compose --env-file deploy/compose/.env \
  -f deploy/compose/compose.yaml \
  -f deploy/compose/compose.netbox.yaml up --build -d

docker compose --env-file deploy/compose/.env \
  -f deploy/compose/compose.yaml \
  -f deploy/compose/compose.netbox.yaml run --rm seed
```

Seed must be repeatable and refuse non-development endpoints. Default startup cannot obtain production cloud credentials. An optional real-AWS test mode explicitly selects an isolated sandbox account, verifies its account ID, and still limits the platform worker to observation roles. Terraform acceptance tests use separate sandbox provisioning credentials.

Development workflow: start stack, seed fixtures, run REST/Python reservation, inspect NetBox, add a matching fake AWS VPC, observe ACTIVE, request release, simulate absence, and advance a test-controlled clock to verify reclamation. The production clock/quarantine values must not be configurable by ordinary API callers. Test-time clock injection must be disabled in production builds/configuration.

Ordinary `down` preserves named volumes. Document destructive fixture resets separately and require the operator to select the development project explicitly. The key recovery test is stop/restart with volumes preserved after a simulated lost NetBox response: the same key must resolve to one allocation.

## 4. Kubernetes and Helm design

Use a `platform-ipam` chart with separate `api` and `worker` Deployments, one image digest, independent resource requests and replica settings, a ClusterIP API Service, internal Ingress, ServiceAccounts, policy ConfigMaps, secret references, NetworkPolicies, disruption budgets, and an explicit migration Job. Include optional monitoring objects only when the cluster provides their CRDs.

Do not require NetBox to be a platform chart subchart. Reuse an existing NetBox or deploy the [NetBox Helm chart](https://github.com/netbox-community/netbox-chart) as a separate release. This keeps application and inventory upgrades independently testable. Stage and prod can share a Kubernetes cluster only if namespaces, databases, credentials, network access, and account roles remain isolated; separate clusters are preferable where the platform already provides them.

The chart values shape is validated by `values.schema.json`:

```yaml
deploymentEnvironment: stage
image:
  repository: registry.example.com/platform/platform-ipam
  digest: sha256:REPLACE_WITH_PROMOTED_DIGEST
api:
  replicas: 2
worker:
  replicas: 1
database:
  existingSecret: platform-ipam-database
netbox:
  url: https://netbox.stage.example.com
  existingSecret: platform-ipam-netbox
auth:
  mode: oidc
  issuer: https://identity.example.com
  audience: platform-ipam-stage
aws:
  mode: live
  coverageConfigMap: platform-ipam-cloud-coverage
policy:
  configMap: platform-ipam-pools
ingress:
  host: ipam.stage.example.com
  tlsSecretName: platform-ipam-tls
ui:
  inventoryLinksEnabled: true
```

An optional `uiProxy` block (not shown above) adds a Caddy reverse proxy in front of an external
NetBox, reproducing the Compose `ui-proxy`'s `basic`-mode logic in the cluster. It is disabled by
default in both `deploy/environments/stage/values.yaml` and `deploy/environments/prod/values.yaml`
and, disabled, changes nothing in the rendered manifests; only `basic` mode is implemented for Helm
today. See [GUI authentication](GUI_AUTHENTICATION.md) section 8 and the
[chart README](../deploy/helm/platform-ipam/README.md) for the values reference and what enabling it
requires of the external NetBox.

### Running `adopt` and `onboard` in the cluster: `operatorJob`

An optional `operatorJob` block (work-plan package H3) adds an opt-in Job that runs
`platform-ipam adopt` or `platform-ipam onboard` in the cluster, instead of a hand-built one-off Job
or Pod holding database, NetBox and cloud credentials by hand. It is disabled by default and,
disabled, changes nothing in the rendered manifests. Each run is named with a required, caller-
supplied `runId`, so a `helm upgrade` that leaves the block enabled with the same `runId` cannot
silently re-run an `apply`; it is not a Helm hook, so it never gates a release the way the migration
Job does. `mode: onboard` gets NetBox credentials and the pools configuration only (no database URL,
no cloud role); `mode: adopt` gets exactly what the `worker` Deployment gets, including its
ServiceAccount. See the [chart README](../deploy/helm/platform-ipam/README.md#optional-operatorjob)
for the full values reference and least-privilege table, and the
[adoption runbook](../deploy/runbooks/ADOPTION.md) section 3 for the operating procedure. Validated
by `helm lint`/`helm template` only; never run against a real cluster.

`adopt` and `worker` authenticate no HTTP caller (`cmd/platform-ipam/main.go` builds
`transport.NewAuth` only for `mode: api`) and answer no UI request, so their own settings
validation (`internal/config.Settings.Validate`, work-plan package H5) does not require
`IPAM_OIDC_ISSUER`, `IPAM_OIDC_AUDIENCE`, a particular `IPAM_AUTH_MODE`, `IPAM_LISTEN_ADDR` or the UI
settings, in any environment; both still require the database and NetBox settings and, in stage/prod,
live AWS observations. As of work-plan package H7, the `operatorJob`'s `mode: adopt` environment (for
all three adopt subcommands — `plan`, `apply` and `abandon` alike) omits `IPAM_OIDC_ISSUER`,
`IPAM_OIDC_AUDIENCE`, `IPAM_AUTH_MODE` and `IPAM_LISTEN_ADDR` — one definition of each variable, shared
with (never copied from) the `worker` Deployment's own environment, via
`platform-ipam.workloadEnv`'s `full: false` parameter (`deploy/helm/platform-ipam/templates/_helpers.tpl`)
— so the Job's environment now matches exactly what `adopt` reads, rather than carrying settings it
silently ignores. Package H7 also made `operatorJob.command` an explicit allow-list per mode (`plan`,
`apply` and, for `mode: adopt` only, `abandon`) and made the mounted input table optional per command:
`abandon` ([ADR 0012](decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)) takes no table,
only `--allocation-id`/`--operator`/`--reason` through `operatorJob.args`, closing the gap H3 left where
the one `adopt` subcommand an operator needs when a domain is fenced could not run in the cluster. See
the [chart README](../deploy/helm/platform-ipam/README.md#optional-operatorjob) and the
[adoption runbook](../deploy/runbooks/ADOPTION.md) section 10 for the full procedure. Validated by
`helm lint`/`helm template` only; never run against a real cluster.

### The identity file and the operator role

`IPAM_IDENTITY_FILE` points at a YAML document with one top-level key,
`identities:`, a list of principals. `internal/config.Load` strict-decodes it
(`KnownFields(true)`), so an unrecognized key fails the whole load rather
than being silently ignored — the mechanism behind the roll-out rule below. A
tenant entry carries `subject`, `tenant_id`, `accounts`, `environments` and
`regions`. An operator entry
([ADR 0011](decisions/0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md))
carries only `subject` and `role: operator` — config validation fails the
load if an operator entry also carries `tenant_id`, `accounts`,
`environments` or `regions`, because an operator is a principal with **no**
tenant, not a tenant with extra rights:

```yaml
identities:
  - subject: some-operator-subject
    role: operator
```

An operator reads across every tenant on `GET /v1/pools`, `GET
/v1/pools/{id}/capacity`, `GET /v1/allocations`, `GET /v1/allocations/{id}`
and `GET /v1/findings` (the last de-duplicated to one row per resource, never
one row per tenant eligible to see it); it can perform no write — `POST`,
`PATCH`, `PUT .../binding` and `DELETE` are all refused for it exactly as for
any principal with no tenant — and `GET /v1/operations/{id}` is refused for
it by design in v1, not by omission: an operation id is not discoverable
except from a tenant's own response, so granting the read would widen the
surface without giving an operator anything it could otherwise reach. See
[`deploy/compose/README.md`](../deploy/compose/README.md#the-operator-identity)
for the full grant as shipped and ADR 0011 for why each boundary sits where
it does.

**Roll-out order**: deploy the binary that knows `Principal.Role` before an
identity file that sets `role`, and roll the identity file back to one
without `role` before rolling the binary back. `role` is a
forward-incompatible field precisely because of the strict decode above — an
older binary sees it as an unrecognized key and refuses to start loudly,
rather than silently ignoring it and treating the entry as an ordinary,
tenant-less-by-omission identity.

**Helm**: `identity.existingConfigMap` (`deploy/helm/platform-ipam/values.yaml`,
`templates/deployment.yaml`) already mounts an `identities.yaml` from a
ConfigMap the deployer owns and points `IPAM_IDENTITY_FILE` at its mount
path; the chart never inspects that file's contents. Adding an operator
identity to a Helm deployment today therefore needs **no chart change** —
only a `role: operator` entry in the same ConfigMap's `identities.yaml` that
already holds the deployment's tenant identities:

```yaml
identity:
  existingConfigMap: platform-ipam-identities
```

(the `platform-ipam-identities` ConfigMap itself is created and owned
outside this chart, as `identity.existingConfigMap`'s own name implies; the
chart mounts it, never generates it.)

**Two operator populations, not reconciled.** The NetBox UI's own operators
([ADR 0006](decisions/0006-OPERATOR_UI_AUTHENTICATION.md): a browser session
authenticated through `ui-proxy`'s `basic`, `entra` or `ldap` mode, landing
in NetBox's read-only `platform-operators` group) are a separate list from
the `role: operator` entries in this identity file — a bearer-token caller
against the platform API, never a browser session against NetBox. Nothing
maps one list to the other, and the gap is deliberate rather than an
oversight (see [GUI authentication](GUI_AUTHENTICATION.md) section 1's audit
table and ADR 0011's Consequences). The same person needs a separate entry
in each list to have both kinds of access, granted by different
administrators through different mechanisms, and removing them from one list
does not remove them from the other.

Prod overrides endpoint/secret names, identity audience, roles, pool configuration, replicas, resource sizing, and alert destinations. It preserves the tested application/chart versions. Configure image pull credentials and external secret synchronization through the cluster's existing platform services.

Both stage and prod reject development auth mode, fake AWS mode, shortened test-only quarantine overrides, and localhost NetBox URLs. Admission should verify configuration consistency at startup and expose a non-secret config version/hash for operations. Development fixture CIDRs must not leak into production defaults.

## 5. Build, validation, and promotion

1. On a change, run policy/API/adapter checks and Compose integration tests with real PostgreSQL/NetBox and the fake cloud adapter.
2. Build a release image once, record its digest and dependency/SBOM data, and package the versioned chart. Build/sign the Terraform provider independently against the compatible API contract.
3. Validate chart schema, `helm lint`, and rendered Kubernetes objects against the target cluster API versions. Render both stage and prod values; verify secret references without printing secret contents.
4. Deploy stage using its pinned values. Execute backward-compatible migrations as a controlled Job, then roll out API and worker. Make the release pipeline wait for the migration Job; a plain manifest Job does not enforce this ordering by itself.
5. Run staging smoke tests and sandbox Terraform acceptance: reserve, apply, observe ACTIVE, import/read, destroy, and verify quarantine. Exercise accelerated time only in isolated tests; a real stage soak can validate the configured production-length quarantine.
6. Promote the exact chart version/image digest through the production deployment process. Verify policy/role/secret differences and database backup readiness, then run safe read/reservation canaries in the designated canary pool.
7. Observe request error rates, frozen domains, reconciliation lag, coverage, and projection lag before expanding tenant/pool admission.

Choose a single owner for migration execution: CI/CD or the existing GitOps controller with explicit ordering. Do not rely on every pod to apply migrations concurrently. Migration Jobs use separately scoped credentials where supported. Helm rollback restores manifests, not database state; migrations must keep the prior binary compatible until the rollback window closes.

## 6. Deployment acceptance criteria

| Environment | Required evidence |
| --- | --- |
| Development | Fresh Compose startup and idempotent seed; API/UI accessible on loopback; persisted ledger/inventory survive restart; replica/race and lost-response tests; no production credentials required |
| Stage | Helm render/schema checks; actual Kubernetes startup; workload identity, secret mounts, TLS, network policies, migration ordering, graceful worker restart, backup restore, and sandbox Terraform flow |
| Prod | Same tested artifacts; reviewed real pools and coverage; complete initial inventory; canary lifecycle; alarms/runbooks; restore and rollback capability; no development auth/fake modes |

The optional NetBox UI can be disabled or made inaccessible to users without changing allocation or Terraform behavior. NetBox API availability remains a core runtime dependency for inventory mutations.
