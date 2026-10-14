# SCALE-1 exploratory API reservation measurement, 2026-10-14

The [recorded run](RESERVATION_SCALE_2026_10_14.json) measures real local API
reservations committed to NetBox. Functional checks passed; no latency or
customer-capacity objective has been accepted. SCALE-1 remains open. AWS
observations were fake and no live AWS service was called.

## Method and scope

```sh
python3 tests/e2e/run-reservation-scale.py --out /tmp/reservation-scale.json
```

The opt-in runner builds the real release image, bootstraps a unique disposable
development Compose project, and starts one API, one platform worker, real
NetBox/its worker and PostgreSQL. The source fixture is copied into a private
directory; its tenant quota becomes 64 and policy version `scale-local-1` so
53 held /22 allocations fit the /16 pool. Repository and production policies
are untouched. One tenant, account, region, pool and overlap domain are used.

One successful warm-up is excluded from the timed phases. Each phase launches
1, 4 or 8 caller threads together; each caller makes four serial reservations.
Latency runs from initial POST to a committed `RESERVED` response, including
transient retries or pending-operation polling. The client honors the API's
numeric `Retry-After` header on 503 responses (two seconds in this run), uses
a two-second fallback/poll interval, and records retry codes and sleep time.
All operations completed inline here; pending-operation polling counts were
zero. Phase throughput includes caller scheduling and all backoff.

Phases append without deleting allocations or resetting permanent keys.
Resident allocation counts therefore increase between phases; concurrency
and state size are not independently controlled. The host has eight CPUs and
x86_64 architecture; services and measurement clients share it. The source
hashes, private policy hash, runtime Go source hashes and actual image IDs are
bound in the JSON report. The stack uses the maintained pinned image/config
definitions; it is not a production topology or a multi-replica measurement.

## Observations

| Clients | Samples | Resident allocations | Elapsed | Median | p95 | Maximum | Reservations/s | 503 retries |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 4 | 1 → 5 | 1.740 s | 445 ms | 463 ms | 463 ms | 2.298 | 0 |
| 4 | 16 | 5 → 21 | 8.812 s | 446 ms | 7,724 ms | 7,724 ms | 1.816 | 6 |
| 8 | 32 | 21 → 53 | 18.966 s | 631 ms | 10,566 ms | 12,470 ms | 1.687 | 28 |

p95 is the nearest-rank sample quantile: it equals the maximum for the 4- and
16-sample phases. These are small, single-run samples, not service objectives
or steady-state capacity estimates. The initial exploratory client used a
shorter retry interval; the published run follows the server's retry hint.

Every measured 503 was `domain_busy`, matching the existing unresolved-operation
fence in [reservation admission](../../internal/service/service.go). The
4- and 8-client phases accumulated 12 and 56 seconds of backoff across caller
threads; these overlapping sleeps are not wall-clock elapsed times. The data
demonstrates admission waiting under this one-domain workload. It does not
isolate ledger lock time, NetBox/database latency, host scheduling or periodic
worker contention, or establish that persistence changes alone would improve
the tail. Preserve the safety fence while designing any future concurrency
change.

## Persisted-state verification and remaining acceptance

All 53 reservations (warm-up plus 52 measured requests) had distinct IDs and
disjoint CIDRs. Each matched its exact NetBox prefix and tenant/key/pool
markers, committed hold and permanent key. The final databases contained 53
allocations/holds/keys/operations, 106 audit events, 53 retry records and 54
prefixes including the pool. Every original request was replayed outside the
timed phases and retained its ID/CIDR without adding audit or retry rows.
Unauthenticated and foreign-tenant reads stayed denied. All writers were
quiesced before the final table/marker comparison. The disposable project's
containers, volumes, network, image tag and private files were removed.

This does not replace the [1,000-allocation ledger baseline](SCALE_2026_10_14.md)
or prior reconciliation measurement: they measure different operations and
state sizes. Before accepting capacity or changing locking/admission design,
agree concurrent clients and arrival/read-write mix, representative resident
allocation/audit counts, account/domain distribution, acceptable p95, and the
target PostgreSQL/NetBox/API topology. Then measure that workload, discovery
and reconciliation in the customer-authorized environment. Real identity and
live AWS qualification remain external.
