# ADR 0017: A ledger write persists what it changed, not the whole ledger

Status: accepted, 2026-09-22, as a design, and as an intermediate step: the
owner decided, on the analysis this record and the M4a measurements give, that
the diff behind the unchanged port is the route and that per-aggregate
transactions with row locks — the model ADR 0001 specified — remain the target,
to be designed in their own record once M4d's two-writer numbers exist. Packages
M4c and M4d start first and change no production code; whether M4e to M4g are
built is decided from M4d's numbers, not from this record. Nothing here is
built. Proposed by package M4b of the [work plan](../WORK_PLAN.md) against gap
**M4** of the owner's [overlap-migration analysis](../IP_OVERLAP_MIGRATION.md)
-- "ledger operations use a global lock and rewrite state tables" -- and against
what package M4a measured and recorded in [deployment
environments](../DEPLOYMENT.md), section "Worker reconciliation pass cost". It
concerns `internal/storage` and the `domain.Ledger` port alone. It changes no
consumer contract, no API document, no finding code and no CLI verb, and it is
deliberately written so that [ADR
0010](0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md)'s adoption, [ADR
0012](0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)'s abandon and [ADR
0013](0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md)'s cancel keep every
guarantee they rest on, unchanged and unrestated.

The "what happens today" paragraphs are read from the checkout on the date
above, with file and line references, and are the only claims here about
demonstrated behaviour. Everything under **Decision** is a proposed contract:
none of it exists, and the numbers it would produce have not been taken.

## Context

### What one `Update` costs, read from the code

`PostgresLedger.Update` (`internal/storage/postgres.go:135-161`) opens a
transaction at READ COMMITTED, takes one advisory lock, calls `loadState`,
runs the caller's closure over the resulting `*domain.State`, and calls
`persistState`.

`persistState` (`postgres.go:322-416`) begins by deleting every row of nine
tables -- `allocations`, `allocation_keys`, `operations`,
`idempotency_requests`, `observations`, `findings`, `coverage`, `holds`,
`operation_barriers` (`postgres.go:323`) -- and then re-inserts the whole of
`*domain.State`, one `tx.Exec` per row:

- two inserts for every allocation, into `allocations` and `allocation_keys`
  (`postgres.go:328-339`);
- one insert for every operation (`postgres.go:340-348`);
- a **second** pass over the same allocation map, one insert each into `holds`
  (`postgres.go:349-357`);
- one insert for every operation whose status is `PENDING`, into
  `operation_barriers` (`postgres.go:358-369`);
- one insert for every idempotency record (`postgres.go:370-378`);
- one insert per routing domain that has observations, whose payload is that
  domain's whole retained array -- up to sixty-four observations, each
  carrying every `domain.Resource` the observer returned
  (`postgres.go:379-387`, retention at `internal/service/service.go:1824-1826`);
- one insert for every finding (`postgres.go:388-396`) and one per coverage
  generation (`postgres.go:397-401`);
- and one insert for **every audit event in the ledger's entire history**
  (`postgres.go:402-414`).

So the round trips one `Update` issues are

```
9 + 3A + O + P + R + D + F + C + E
```

where `A` is the number of allocations, `O` operations, `P` pending operations,
`R` idempotency records, `D` routing domains carrying observations, `F`
findings, `C` coverage generations and `E` **audit events since the ledger was
created**. Nothing in this repository batches or pipelines them: there is no
`SendBatch` and no `CopyFrom` anywhere in the checkout, so each `tx.Exec` is one
round trip to PostgreSQL, taken while the global lock is held.

Two of those terms deserve to be named separately from the rest.

`3A` is three writes per allocation because `allocation_keys` and `holds` are
derived from the allocation map and regenerated from it; `holds` costs a second
JSON marshal of the same value (`postgres.go:350`, after `postgres.go:329`).

`E` is the term that never shrinks. `audit_events` is the one table
`persistState` does not clear -- it is absent from the delete list at
`postgres.go:323` and its insert carries `ON CONFLICT (id) DO NOTHING`
(`postgres.go:411`) -- and that is exactly how the append-only property is
enforced, and what `TestPostgresAuditEventsRemainAppendOnly`
(`internal/storage/postgres_test.go:619-653`) pins: a caller that hands back a
state containing only its own event cannot erase history. But the enforcement
is by re-offering every historical row on every write. A reservation writes two
events (`service.go:366`, `service.go:435`), a binding one
(`service.go:752`), the worker's own commits and reclaims more
(`worker.go:669-671`), so `E` grows at several rows per allocation lifetime and
is bounded only by how long the deployment has been running. A ledger of a
fixed size therefore gets **slower every month** at a rate nothing in the code
or the configuration bounds.

### What a `View` costs, and why it is not free either

`View` (`postgres.go:110-133`) takes the same lock, runs the same `loadState`,
and commits without persisting -- so a closure's mutations inside a `View` are
discarded, in both stores. `loadState` (`postgres.go:169-320`) issues seven
queries and decodes every row of `allocations`, `operations`,
`idempotency_requests`, `observations`, `findings`, `coverage` and
`audit_events`. The audit table is read in full, ordered by id
(`postgres.go:297`), on **every read and every write**. So a read is
`O(A + O + R + D + F + C + E)` in rows decoded and in resident memory, and the
same `E` term applies.

`loadState` reads seven tables; `persistState` writes nine. `allocation_keys`,
`holds` and `operation_barriers` are never loaded. They are write-only
projections, and nothing anywhere in this repository reads them: the service
asks `pendingDomain` (`service.go:1870-1878`) over `st.Operations` rather than
over the barrier table, and a search of the checkout finds no other consumer.
They exist because [ADR 0001](0001-IMPLEMENTATION_FOUNDATION.md) asked for
"allocation-key tombstones ... CIDR holds ... a durable domain operation
barrier", and they carry two things the state maps do not: the unique index that
enforces allocation-key identity, and a typed `cidr` column
(`postgres.go:93-96`) that no code queries. Whether an operator's own SQL or a
dashboard reads them is not knowable from here, and this record does not propose
removing them.

### The lock, stated precisely

There is exactly one lock, `pg_advisory_xact_lock(0x706c6174666f726d)`
(`postgres.go:108`, taken at `postgres.go:122` and `postgres.go:144`). It is
**exclusive** and transaction-scoped, and it is taken by `View` as well as by
`Update`. It is one constant for the whole deployment, so every transaction of
every process contends for it: a read on one API replica blocks a read on
another, and a reservation blocks behind a worker pass.

The transactions run at READ COMMITTED (`postgres.go:117`, `postgres.go:139`).
The serialization is the lock's, not the isolation level's, and the comment at
`postgres.go:114-116` says why that is sound: READ COMMITTED takes its snapshot
after a waiting transaction acquires the lock, so a contender always loads its
predecessor's committed state. Removing or narrowing the lock without replacing
what it does would leave READ COMMITTED alone against exactly the phantom reads
the overlap and quota checks must not have.

The store says all this about itself. `PostgresLedger`'s own doc comment
(`postgres.go:15-21`) calls the single advisory lock "a correctness-first
coordination boundary" that "can later be replaced by per-domain fencing after
contention is measured", and declines to claim an outbox. This record is that
later, for the write amplification; the fencing is treated below as a separate,
deferred question. ADR 0001, for its part, specified a **lock order** of
"overlap domain, parent allocation, allocation" and said "database uniqueness
protects allocation identity and idempotency keys; domain locking and CIDR
intersection checks protect peer allocation". The single global lock is
therefore already a simplification of what ADR 0001 decided, not a departure
this record would be inventing.

### What the store guarantees today

Ten properties, each with where it comes from. They are listed because the
decision below has to keep every one of them, and because two of them come from
the whole-ledger rewrite itself and are the ones a narrower writer could lose
quietly.

1. **One transaction at a time, deployment-wide.** The single exclusive
   advisory lock (`postgres.go:122`, `postgres.go:144`). Reads included.
2. **Serial in effect.** From the lock, not from READ COMMITTED
   (`postgres.go:114-117`).
3. **Whole-state atomicity.** A closure sees one consistent image of every
   table at once, and its writes land together or not at all
   (`postgres.go:143`, `postgres.go:157`).
4. **A closure that returns an error changes nothing.** `persistState` is not
   reached (`postgres.go:151-153`); pinned by
   `TestPostgresUpdateRollbackDoesNotPersistState` (`postgres_test.go:281-302`).
5. **Read-your-own-writes inside a closure**, trivially, because the state is
   one in-memory value.
6. **A `View` closure's mutations are discarded** -- no `persistState` in
   `View`, and a JSON clone in the memory store
   (`internal/storage/memory.go:27`).
7. **The audit trail is append-only**, by omission from the delete list plus
   `ON CONFLICT DO NOTHING` (`postgres.go:323`, `postgres.go:411`).
8. **Allocation-key identity is enforced by the database**, through
   `allocations UNIQUE (tenant_id, allocation_key)` (`postgres.go:80`) and
   `allocation_keys PRIMARY KEY (tenant_id, allocation_key)`
   (`postgres.go:84`). The two-ledger concurrency test
   (`postgres_test.go:304-370`) drives two independent ledgers at once and
   asserts exactly one winner for a shared key, with no lost update.
9. **Idempotency identity is enforced by the database**, through the primary key
   and the `idempotency_identity` unique index (`postgres.go:87-88`), on top of
   the service's own hashed request id (`idempotencyID`,
   `service.go:2070-2073`), so the database is a second, independent guard.
10. **Derived rows die with their aggregate, and free the key.** Because
    `allocation_keys` and `holds` are regenerated from the allocation map, a
    `delete(st.Allocations, id)` inside a closure removes them, and the unique
    index releases the key.
    `TestPostgresAbandoningAnAllocationDropsItsRowsAndFreesItsKey`
    (`postgres_test.go:380-474`) and
    `TestPostgresCancellingAReservationDropsItsRowsAndFreesItsKey`
    (`postgres_test.go:497-617`) both say so in as many words, and the second
    also asserts that the fence's removal of the last pending operation of a
    domain empties `operation_barriers`, because that table is regenerated from
    the pending operations alone (`postgres_test.go:553-557`).

Properties 7, 8 and 10 are the ones that rest on the rewrite. A narrower writer
has to reproduce 10 by deriving the same rows, and it has to reproduce 7 without
re-offering history.

### What each caller actually relies on

Thirty-eight `Ledger.View`/`Ledger.Update` call sites exist outside tests, all
in `internal/service`. They divide cleanly into two groups, and the division is
what makes the decision below possible.

**Callers that need the whole state, and for what.**

`reserve`'s pre-check `View` (`service.go:179-222`) and its planning `Update`
(`service.go:271-376`) are the heaviest. Both scan the entire allocation map
for a tenant-and-key match (`service.go:197`, `service.go:287`) -- there is no
index on `(tenant_id, allocation_key)` in the state, only in the database. The
`Update` additionally calls `pendingDomain` over every operation
(`service.go:314`, `service.go:1870-1878`), `countTenant` over every allocation
(`service.go:317`, `service.go:1899-1907`), `countChildren` over every
allocation for a subnet (`service.go:325`, `service.go:1908-1916`), and
`chooseCIDR` (`service.go:342`, `service.go:1374-1452`), which walks **all**
allocations once per candidate prefix. Its uncommitted-hold branch
(`service.go:1428-1435`) has no routing-domain filter at all: an uncommitted
hold anywhere in the ledger blocks a candidate, deliberately and conservatively.
`pinnedCIDR` repeats the same ledger scan for an adoption, unexempted
(`service.go:1506-1520`).

`Release` (`service.go:761-805`) scans allocations for unreleased children
(`service.go:775`) and operations for a pending binding to fail
(`service.go:786`). `List` (`service.go:537`) and `Findings`
(`service.go:853`), including `operatorFindings` (`service.go:914-930`), read
their whole map by definition. `capacity` (`internal/service/capacity.go:111`)
sums occupancy over every allocation of a domain and asks `pendingDomain`.
`validateRequest` opens a `View` of its own for a subnet's parent
(`service.go:1309`) -- a keyed read, but a separate transaction and therefore a
separate acquisition of the global lock.

In the worker: `pendingRecoveryJobs` (`worker.go:28-36`) scans every operation;
`reconcileAllocations` (`worker.go:344-484`) does its whole pass inside **one**
`Update`, resetting the codes it owns over every finding
(`worker.go:345-351`), walking every allocation, and fanning occupancy findings
out over every pool's eligible tenants (`worker.go:455-480`);
`syncProjections`'s job-collection `View` (`worker.go:545-555`) scans every
allocation and asks `pendingForAllocation` for each, which is itself a scan of
the operations (`service.go:1861-1869`) -- `O(A x O)` inside one closure;
`reclaimTick`'s planning `Update` (`service.go:1086-1116`) scans operations and
allocations; `Tick`'s per-domain observation `Update` (`service.go:998-1028`)
walks every allocation of that domain; and `Tick`'s binding-job `View`
(`service.go:1045-1064`) walks every operation.

**Callers that only touch named rows.** `reserve`'s commit closure
(`service.go:405-459`) reads two entities by key, writes those two, appends one
event and resolves one finding by its computed id. `commitRecovered`
(`worker.go:75-92`), `finishBinding` (`service.go:723-755`), `flagStuckHold`
(`worker.go:284-291`), `applyProjectionResults` (`worker.go:614-630`), `Patch`
(`service.go:559-605`), `Get` (`service.go:485`), `Operation`
(`service.go:817`) and the fence and delete transactions of both `cancel`
(`internal/service/cancel.go:148`, `cancel.go:214`) and `abandon`
(`internal/service/abandon.go:170`, `abandon.go:232`) are all of this shape.
The two delete closures additionally sweep `st.Requests` for records naming the
deleted allocation (`cancel.go:249-253`, `abandon.go:255-259`), which is a scan
of a map that is small by construction.

**Two things every caller relies on that are easy to overlook.** First, no
closure performs I/O. Every NetBox call and every observation happens strictly
outside the transaction -- `reserve` reads the snapshot and the observation
before opening its `Update` (`service.go:233-265`), `Tick` observes before
its `Update` (`service.go:997-998`), and `cancel` and `abandon` say so in
comments at their remove and clear steps. That is what makes a single global
lock survivable at all, and it is a property the decision below must not weaken.
Second, `Update` is used as a compare-and-set: every commit path re-reads the
allocation's revision and state and the operation's status under the lock and
becomes a no-op if they moved (`service.go:421`, `worker.go:78`,
`worker.go:617`). Those guards are what make an interleaving of an API replica
and a worker safe today, and they are per-row questions asked of a whole-state
snapshot.

The deployment that has to survive all of this is configured for concurrency:
`deploy/environments/prod/values.yaml` sets `api.replicas: 3` and
`worker.replicas: 2`, and stage sets 2 and 1. Five processes, one advisory lock.
No test and no measurement in this repository has ever run two workers.

### What M4a measured, and what it did not

Package M4a's dated section in [deployment environments](../DEPLOYMENT.md)
records, from NetBox's own access log on a throw-away Compose project, that one
worker pass took **54 s at 100 committed allocations, 879 s at 1,000 and an
extrapolated 3.7 h at 5,000** before its fix, and **16 s, 161 s and 813 s**
after it; that NetBox's per-request latency was flat across all three sizes and
was never the scaling problem; and that the cost which did scale was
`persistState`, called once per allocation by the old `syncProjections`. The
fix batched the bookkeeping write into one `Update` after the loop
(`worker.go:540-631`), which removed the quadratic term and left the linear
NetBox traffic -- two requests per allocation per pass, 13.55 minutes at 5,000
-- exactly as it was. That section is the measurement this record builds on and
it is not repeated here.

Four things it did not establish, each of which changes what the numbers mean.

**It did not measure a realistic mix of tables.** The section says the ledger
rows were "inserted by SQL piped to `psql`". The scratch files that produced
them were still in the checkout on the date of this record, outside version
control, and they insert into `allocations` and `allocation_keys` and nothing
else -- no operations, no idempotency records, no findings and, decisively, no
audit events. So the `3A` term was measured and the `O`, `R`, `F` and `E` terms
were whatever the development stack itself had accumulated, which nothing
recorded. In a deployment of the same allocation count, every committed
allocation carries at least one operation and one idempotency record, and the
audit table holds several rows per allocation lifetime. A real ledger's `Update`
is therefore plausibly several times the round trips M4a's synthetic one issued
at the same allocation count, and its audit term grows with age rather than
with size. This is an inference from the code and the seeding method, not a
measurement, and it is the single most important number this record asks for
before anything is built.

**It did not measure concurrency.** One machine, one API replica, one worker.
The one concurrent figure it has -- an ordinary reservation taking 2.0-2.9 s at
100 allocations and a single 20.7 s sample at 5,000 while a pass ran -- was
taken against the **pre-fix** code only. There is no post-fix concurrent number
at all, and there is no two-writer number of any kind. Since the whole argument
for the global advisory lock is that contention has not been measured
(`postgres.go:15-21`), this is the measurement the lock's future turns on.

**It did not separate the load from the write.** Both `View` and `Update` pay
`loadState`. M4a's fix removed `N` rewrites but left `N` loads in the same pass
(one per `View`, plus the batched `Update`). Whether the remaining cost is the
load or the write is not known, and the two options below differ precisely on
which one they attack.

**It was a laptop.** The section says so. Absolute latencies will differ under
provisioned infrastructure. The cost *shape* is a property of the code and does
reproduce; the thresholds are not.

### The second cost, which is a different problem

`syncProjections` still calls `Inventory.Sync` once per committed allocation per
pass, and `Sync` (`internal/netbox/client.go:789-826`) is one `GET` followed by
one unconditional `PATCH`. The `GET` is not optional: it verifies that the
prefix still carries this allocation's identity (`client.go:811`) and it reads
back the custom fields the platform does not own so the `PATCH` does not erase
them (`client.go:815-819`). So the most a "skip the unchanged write" change can
remove is the `PATCH` -- half the requests, and at 5,000 allocations still
roughly six and a half minutes against a thirty-second interval.

What makes every pass look changed is one field. `ownedFields`
(`client.go:667-679`) writes `platform_last_observed_at` from
`Allocation.LastObservedAt` (`client.go:672-673`), and
`reconcileAllocations` sets `LastObservedAt` to the current observation's
`FinishedAt` on every pass (`worker.go:363`). Every other owned field is stable
between lifecycle changes. So a naive "compare what we are about to write with
what the `GET` returned" would never skip anything.

The question M4a deferred was what freshness bound that field's readers need.
Read from the code, the answer is that **it has no readers in this repository**.
`Snapshot` maps a NetBox prefix onto `domain.Network` at `client.go:453` and
does not decode it. The API's `last_observed_at`
(`internal/transport/http.go:623`) comes from the ledger's own
`Allocation.LastObservedAt`. No finding, no reuse rule and no adoption or
abandon check consults it; the abandon path only lists it among the fields it
clears. Its only reader is a person looking at NetBox, which is exactly the
audience [ADR 0011](0011-WHAT_AN_OPERATOR_WHO_IS_NOT_A_TENANT_MAY_SEE.md)
grants inventory reads to, and no code can say what staleness that person will
accept.

The arithmetic against `Lifecycle.MaxObservationAge` confirms that it is not a
correctness bound. That setting -- 600 seconds in the development fixture
(`deploy/compose/fixtures/pools.yaml:39`), against a 30-second scan interval at
line 42 -- bounds how stale an *observation* may be before `trustedObservation`
(`service.go:1204-1206`) stops trusting it. It governs the ledger's own
decisions and never reaches NetBox. And the two numbers already cross: at 5,000
allocations a pass takes 813 s, so the allocations written at the end of a pass
already carry a `platform_last_observed_at` older than `MaxObservationAge` by
the time the next pass reaches them, and nothing misbehaves -- because nothing
reads it. The every-pass `PATCH` is therefore not defending any invariant this
project has written down.

## Decision

Two decisions, separable, in this order.

**One.** The `domain.Ledger` port keeps its whole-state closure contract
exactly as it is, and `PostgresLedger.Update` stops rewriting the ledger. It
persists the difference between the state `loadState` produced and the state the
closure returned: the rows that were added, the rows whose encoding changed, and
the rows whose keys are gone. The advisory lock, the isolation level, the
transaction boundary, the schema and the migration mechanism are unchanged, and
no file in `internal/service` changes.

**Two.** The projection `PATCH` is skipped when the fields the adapter would
write already agree with the fields the `GET` just returned, with
`platform_last_observed_at` excluded from that comparison and refreshed on a
cadence of its own. No durable "what NetBox was last told" fingerprint is added
to `domain.Allocation`, because the `GET` already carries one and the read
cannot be avoided anyway. The cadence is a configured value, and what it should
be is the owner's to say.

The rest of this section is what each of those means in detail, what was
rejected, and what has to be true before either is switched on.

### One, in detail: persist the difference

The mechanism follows from something `persistState` already does. It marshals
every entity to JSON in order to write it (`postgres.go:329`, `postgres.go:342`,
`postgres.go:350`, and so on). If `loadState` keeps the raw bytes it read
alongside each decoded value, then the comparison "did this row change" is a
byte comparison against work the writer was going to do regardless. The
per-transaction cost becomes the marshalling that already happened, plus the
round trips for the rows that actually differ, and nothing else.

That gives, per table:

- **Added**: a key present in the closure's map and absent from the loaded one
  -- `INSERT`.
- **Changed**: a key in both whose re-marshalled bytes differ -- `UPDATE`, or
  `INSERT ... ON CONFLICT (pk) DO UPDATE`, which keeps the statement count at
  one and needs no read.
- **Removed**: a key in the loaded map and absent from the closure's --
  `DELETE`, by primary key.

Deletion is computed from the **key sets**, never from the byte comparison.
That is the one rule whose violation loses data, and it is stated separately
because everything else in this scheme fails safe: if byte-stability does not
hold for some entity, the diff writes a row that did not need writing, which is
a performance regression and never a correctness one. Byte-stability is
therefore a property to be proved by test, not assumed -- Go's `encoding/json`
sorts map keys, so `Labels` and `Operation.Result` are stable, and a
`time.Time` that came from an unmarshal re-marshals to the same string, but
"therefore" is not evidence and the harness below is where the evidence goes.

The three derived tables keep being **derived**, not diffed. `allocation_keys`
and `holds` are recomputed from the allocation map and `operation_barriers`
from the pending operations, exactly as today, and only the rows whose derived
encoding changed are written. That preserves property 10 above without
special-casing anything: an allocation removed from the map removes its
`allocation_keys` and `holds` rows because the derivation no longer produces
them, and a fenced operation empties its domain's barrier because the
derivation no longer produces that either -- which is precisely what
`postgres_test.go:553-557` asserts.

The audit table needs no diff at all, only a stop. `persistState` already
refuses to overwrite an existing event (`postgres.go:411`); the change is to
stop offering the events `loadState` just read. Appended events are the ones
whose ids are not in the loaded set, and the `ON CONFLICT DO NOTHING` stays, as
the belt to that braces. This removes the `E` term from writes. It does not
remove it from reads, and that is taken up below.

**What the change costs.** A write becomes `O(changes)` round trips instead of
`O(state)`, plus an `O(state)` in-process marshal-and-compare. A read is
unchanged. The load is unchanged, so an `Update` remains `O(state)` in rows
fetched and decoded; if the load turns out to dominate after the writes are
gone, that is a further decision and this record does not pre-empt it.

**What it risks.** Not lost updates, not phantoms, not idempotency: those are
properties of the lock, the isolation and the unique indexes, none of which
this touches. Three risks are real and specific:

1. **A key-set bug loses a row silently.** A deletion missed by the diff leaves
   a row the reload will hand back to the next closure, and the ledger quietly
   keeps an allocation that an abandon or a cancel removed -- the exact state
   ADR 0012 and ADR 0013 forbid. The differential harness exists for this.
2. **A derived table drifts out of step.** Nothing reads `allocation_keys`,
   `holds` or `operation_barriers` back, so a reload comparison is blind to
   them, and only a direct row comparison catches a derivation that stopped
   producing a row it should have. `postgres_test.go`'s `countRows`
   (`postgres_test.go:479-486`) already exists because of this; the harness must
   compare contents, not counts.
3. **The unique index fires on a different statement.** Today a duplicate
   allocation key collides during the full re-insert and rolls the whole
   transaction back (`postgres_test.go:343-355`). Under a diff it collides on
   the one `INSERT` that introduced it, and rolls the whole transaction back for
   the same reason. The outcome should be identical; that it is identical is a
   test, not an assumption.

**What it needs from the schema and from the migrate mode: nothing.** Every
table `persistState` writes already has a primary key that a targeted write can
address (`postgres.go:77-99`). No column is added and no index is added, so the
migration mechanism -- one idempotent `CREATE TABLE IF NOT EXISTS` script
inserting schema version 1 (`postgres.go:71-106`), with `Ready` asserting that
version (`postgres.go:63-69`) and the Helm migration Job running it as a
`pre-install,pre-upgrade` hook (the chart's `templates/migration-job.yaml:8-10`)
-- is untouched. That matters more than it looks: the mechanism has no path to a
version 2 at all, so an option that needed one would have to build that path
first. This one does not.

### What decision one deliberately does not do

It does not narrow the lock. It does not change the isolation level. It does not
change `domain.State`, `domain.Ledger` or any type in `internal/domain`. It does
not touch `internal/service`, so the single-path invariant that
`TestCommittedAllocationsComeIntoBeingInThreePlaces`
(`internal/service/adopt_test.go:728-766`) enforces -- one `domain.Allocation`
composite literal and exactly two assignments of `Committed`, at
`service.go:426` and `worker.go:81` -- is not merely preserved but not
approached. It does not make a read cheaper. It does not shorten a worker pass,
because after M4a the pass is NetBox-bound. It removes write amplification, and
that is all it claims.

### Two, in detail: the projection PATCH

The `GET` in `Sync` already returns the prefix's current custom fields
(`client.go:807-819`). Comparing the map the adapter is about to send against
the map it just read is therefore free, and a `PATCH` whose result would be
identical is skipped. The comparison excludes `platform_last_observed_at`,
which changes every pass by construction and which this record has established
nothing in the repository reads back.

That field is then written on its own cadence: a `PATCH` is sent when some
other owned field differs, **or** when the value recorded in NetBox is older
than a configured refresh interval. Nothing durable is added to
`domain.Allocation` for this, because the value NetBox holds is the value the
`GET` returned, and the freshness question is answered from it directly. This is
narrower than what M4a's follow-up assumed it would need, and the narrowing
comes entirely from noticing that the read is unavoidable.

**What this decides and what it does not.** It decides the mechanism: a
comparison against the read, not a durable fingerprint, and one field on a
cadence. It does not decide the cadence. That is a question about what staleness
an operator reading NetBox will accept, no code can answer it, and it is
squarely inside the owner's Q9 ("scan freshness"). Until the owner names a
number, the honest default is the one that changes nothing observable: a refresh
interval equal to the scan interval, which reproduces today's behaviour exactly
and lets the mechanism ship without deciding the policy.

**What it is worth, stated plainly.** At most half the requests of a pass. At
5,000 allocations that is roughly 813 s down to something near 400 s -- still
more than thirteen times the thirty-second interval. **Skipping the `PATCH`
does not make a pass fit its interval, and this record does not claim it does.**
Making a pass fit its interval needs either fewer jobs per pass (a rotation, so
that each allocation is synced every *k*th pass) or concurrent `Sync` calls, and
both change what a pass means. Neither is decided here, and neither should be
decided before the first is measured.

### The alternatives, and why each is not the first step

**Rejected: cache the loaded state in process, invalidated by a version row.**
A process keeps the last `*domain.State` it loaded and a version number; each
transaction reads one version row under the lock and reloads only if it moved.
It attacks the load, which the diff leaves alone, and under a read-heavy,
write-light load it would help. It is rejected as a first step for four
reasons. With `api.replicas: 3` and `worker.replicas: 2`, every write
invalidates every other process's cache, so under steady write traffic nearly
every transaction reloads anyway -- the saving disappears exactly when it is
needed.
The cached value cannot be handed to a closure, which mutates it, so each use
costs a deep copy: the memory ledger already demonstrates what that costs, a
whole-state JSON round trip on every `View` and every `Update`
(`memory.go:27`, `memory.go:37`, `memory.go:57-70`). A wrong cache is a lost
update or a stale read, which is a correctness failure with no deterministic
test to catch it, whereas a wrong diff fails safe in the direction described
above. And it should not be judged before the split between load cost and write
cost is measured, which M4a did not do. It is worth revisiting if -- and only
if -- that measurement shows the load dominating after the writes are gone.

**Deferred, and named as the destination: per-aggregate transactions with row
locks and explicit queries.** The real relational model, and what ADR 0001
described: a domain-level lock rather than a global one, a `SELECT ... FOR
UPDATE` on the aggregate, an indexed lookup for the idempotency record, a
`COUNT` for the quota, and a range predicate for the overlap check. It would
make `View` cheap as well as `Update`, which neither other option does.

It is deferred, not rejected, and the reasons are specific. It changes the
`Ledger` port, so every one of those call sites changes, including
`reserve`'s planning transaction, which is the one place a committed allocation
is constructed. The overlap and quota checks become phantom-prone the moment the
global lock goes: under READ COMMITTED with per-row locks, two concurrent
reservations in one domain can each find no conflicting allocation and both
insert. Fixing that properly needs SERIALIZABLE with a retry loop, or a
domain-level lock -- which is today's lock, narrowed -- or a database exclusion
constraint. The constraint is tempting, because `holds` already carries the
typed `cidr` column (`postgres.go:95`) an `EXCLUDE USING gist` would need. But
it cannot express today's rule: `chooseCIDR`'s uncommitted branch
(`service.go:1428-1435`) blocks on a hold in **any** routing domain, so a
domain-scoped constraint would silently change behaviour and a domain-less one
would need its own justification. That is a decision of its own, and it should
be taken with a measurement of lock contention in front of it -- which is
exactly what `postgres.go:15-21` said, and what nothing has yet produced.

**Rejected as the whole answer: change nothing and bound the deployment.** A
documented supported ledger size is not wrong as a statement, and this record
makes one below. It is rejected as the answer because the audit term makes the
bound a function of *time* rather than of size: a small estate running for two
years issues more round trips per write than a large one that started last
week, and a size bound that did not say so would be misleading. The useful half
is kept: what is known about size today is stated, and nothing is claimed beyond
it.

### How equivalence would be proven before any switch

Nothing is switched over on an argument. Five things, in this order, and the
first of them is built before any store changes.

**A differential harness over generated operation sequences.** A generator
produces sequences drawn from the mutations the service actually performs --
insert an allocation, change its revision and state, delete it, append events,
raise and resolve findings by their computed ids, rotate observations past the
sixty-four-row retention, add and fence pending operations, write and delete
idempotency records. Each sequence is applied to both stores against two
throw-away schemas, and after **every** step three things are compared: the
reloaded `*domain.State`, the contents of all nine tables row by row, and the
error returned. Comparing the reloaded state alone is not enough, and the
reason is in the code: `loadState` reads seven tables and `persistState` writes
nine, so a reload is blind to `allocation_keys`, `holds` and
`operation_barriers` -- the three a derivation is most likely to get wrong.

The harness has to be shown capable of failing. Mutants of the current
`persistState` -- drop the barrier regeneration, drop the `allocation_keys`
row, keep a deleted allocation, write an event that was already present with
different content -- are each run through it, and each must be killed. A
harness that has never failed is not evidence.

Two divergences already in the checkout have to be handled rather than
discovered: closures mint ids with `domain.NewID` (`domain/types.go:341-347`),
so a sequence must inject its ids or the two runs are trivially unequal; and
`persistState` itself assigns an id to any event that arrives without one
(`postgres.go:403-405`) while the memory store does not, so an event appended
with an empty id already round-trips differently through the two
implementations. `View` differs too -- the memory store takes a read lock and
allows concurrent readers (`memory.go:25`), the Postgres store takes the
exclusive advisory lock. These are properties to record, not bugs to fix inside
this work.

**The opt-in PostgreSQL tests, unchanged.** `internal/storage/postgres_test.go`
is skipped unless `IPAM_TEST_DATABASE_URL` is set, insists on a loopback host,
and creates and drops a freshly generated schema per test
(`postgres_test.go:35-62`). Every existing test must pass without modification
-- in particular the append-only test, the two-ledger concurrency test and the
two delete tests, which are the four that pin the properties most at risk. A
test that has to be edited to pass is a behaviour change, and a behaviour change
here is a defect.

**The full end-to-end suite**, both halves, green -- `tests/e2e/run-e2e.sh`,
against the real ledger and a real NetBox, which is where the reservation,
adoption, abandon and cancel paths meet the store for real.

**M4a's measurement method, re-run with before and after.** The same throw-away
Compose project, the same synthetic seeding, the same NetBox access-log method,
at the same three counts, so the new numbers are comparable to the ones already
recorded. Additionally, and unlike M4a: a ledger seeded with a realistic mix --
operations, idempotency records, findings and, above all, audit events at
several rows per allocation -- because that mix is what the `E` term is about
and the existing numbers do not contain it.

**The measurements M4a did not take.** Reservation latency while a pass runs,
against the current code and against the changed code, at each count, with more
than one sample. And two writers: two API replicas and two worker replicas
against one ledger, which is what `deploy/environments/prod/values.yaml`
configures and what nothing has ever run. Without that second one, nothing can
be said about whether the global advisory lock needs narrowing, and the
deferred option above stays undecidable.

### The per-item `Update` loops that remain

M4a named three and left them alone: `recoverReservations` and
`recoverAdoptions`, which call `Update` once per pending recovery job through
`commitRecovered`, `flagAgedReservation` and `flagStuckHold`
(`worker.go:150`, `worker.go:176-182`, `worker.go:284`), and `reclaimTick`,
which does the same per reclaim job after its own snapshot and delete calls
(`service.go:1126`, `service.go:1145`). All three are bounded by pending or
quarantined counts rather than by the total, which should be small in steady
state.

This record's position is that **batching them is not worth a package once the
store is fixed**, and the reasoning is arithmetic rather than taste. Their cost
today is `jobs x O(state)`; after decision one it is `jobs x O(changes)`, and
each job changes a handful of rows. What would remain is `jobs` acquisitions of
the global lock and `jobs` loads, which is a lock-contention question and not a
write-amplification one -- and the lock is the deferred decision, not this one.
Batching them would also be a change inside `internal/service`, where M4a found
that the equivalent change needed two review corrections to get its guards
right, and their per-job effects are more varied than `syncProjections`'s
uniform toggle: two finding codes, adoption-specific audit events, and in
`reclaimTick`'s case an inventory delete between the read and the write that
cannot be hoisted out of the loop at all. The honest order is: fix the store,
re-measure, and only then ask whether the remaining per-job loads matter. If the
answer is that they do, it is a lock decision and belongs with the deferred
option.

### What depends on the owner's question Q9

Q9 of the owner's analysis reserves to the owner "what request latency, scan
freshness, outage behavior, account growth and support response are acceptable",
and supplies the measurable acceptance criteria for gaps M4 and M5. This record
decides none of it. Four things it leaves open, with the number that would
settle each.

**Whether 5,000 committed allocations is representative.** M4a chose 100, 1,000
and 5,000 as a range, not as a target. If the real figure is a few hundred, the
write amplification is a minor cost and only the audit term is worth removing,
because that one grows with age rather than with size -- decision one would
shrink to its audit sub-cut. If the figure is tens of thousands, the load term
matters too and the deferred option comes forward.

**Whether a thirty-second scan interval is required.** If a pass may take
minutes, the projection decision's value falls sharply and decision two becomes
optional. If it may not, then neither decision here is sufficient and a rotation
or concurrent syncing has to be decided as well.

**What staleness of `platform_last_observed_at` an operator will accept.** That
is decision two's cadence, and nothing in the code can answer it. Until it is
named, the default reproduces today's behaviour.

**What reservation latency is acceptable while a pass runs.** That is the number
that decides whether the global advisory lock ever has to be narrowed -- that
is, whether the deferred option is ever taken. The measurement exists to be
taken; the threshold does not exist to be inferred.

### What this record deliberately does not do

It does not change the `domain.Ledger` port or the closure contract. It does not
narrow or remove the advisory lock. It does not add a column, an index or a
schema version. It does not propose removing `allocation_keys`, `holds` or
`operation_barriers`, which this repository does not read but something outside
it might. It does not claim an outbox, which `postgres.go:15-21` has declined to
claim since the beginning and which is still not implemented. It does not touch
`internal/service` or `internal/domain`. It does not make a worker pass fit its
interval. It does not state a supported ledger size as a fact -- see the
consequences below for what it does state. And it decides nothing about what
counts, latencies or freshness are acceptable, which is Q9's.

### The cut of M4c to M4g

Five packages, each independently reviewable, in this order. The first two
change no production code at all.

**M4c -- the differential harness, before any store change.** A generated-
sequence harness in `internal/storage`, gated on `IPAM_TEST_DATABASE_URL` like
the rest of `postgres_test.go`, comparing the current Postgres store against
itself across a schema boundary and against the memory ledger, on reloaded
state, on all nine tables row by row, and on returned errors. It ships with the
mutants that prove it can fail, and with the recorded divergences (id minting,
`View` locking) documented rather than papered over. Nothing outside
`internal/storage` changes.

**M4d -- measure what M4a did not.** With the harness in place and still no
store change: a direct count of round trips per `Update` as a function of each
state map's size, taken with a counting wrapper around the transaction and
needing no database; and, on a throw-away stack, reservation latency while a
pass runs against the current post-M4a code, with more than one sample, plus a
first two-writer run at the counts the owner names. Recorded as a dated
paragraph beside M4a's own in [deployment environments](../DEPLOYMENT.md). If
these numbers say the current store is adequate at the sizes Q9 names, the
remaining packages are not taken and this record records that instead.

**M4e -- the audit table alone.** The smallest correct change and the one
unbounded term: `persistState` stops offering the events `loadState` read, and
inserts only the appended ones. `ON CONFLICT DO NOTHING` stays.
`TestPostgresAuditEventsRemainAppendOnly` must pass untouched, and M4c's harness
is what proves the rest. One table, one loop, a few lines.

**M4f -- the diff for the remaining eight tables.** With M4e's precedent and
M4c green: `loadState` retains the bytes it read, `persistState` becomes added
/ changed / removed per table with the three derived tables recomputed and then
diffed, deletion computed from key sets, and byte-stability proved as a property
over generated entities. The whole e2e suite green. This is the package that
carries the risk, and it is last among the store packages on purpose.

**M4g -- the projection `PATCH`.** Inside `internal/netbox`: skip the `PATCH`
when the owned fields agree with what the `GET` returned, excluding the field
the cadence governs, with the cadence as a configured value defaulting to the
scan interval so that shipping it changes nothing observable. Tests, mutation
tests, and the e2e suite -- several of whose modules were written around the
every-pass write.

The deferred option -- per-aggregate transactions and a narrower lock -- is not
in this cut. It gets its own record, written after M4d's two-writer numbers
exist.

### The evidence that moves this record to accepted

In order, and each item is a thing that can be shown rather than argued:

1. M4c's harness exists, is gated, and kills every mutant of the current
   `persistState` listed above -- including at least one that only a row-by-row
   comparison of `allocation_keys`, `holds` or `operation_barriers` can catch.
2. M4d's round-trip count confirms or refutes, as a direct measurement, the
   arithmetic `9 + 3A + O + P + R + D + F + C + E` stated in the context above,
   and reports what a ledger with a realistic audit history costs per `Update`
   at each of the owner's counts.
3. M4d's reservation-latency-during-a-pass numbers exist for the current code,
   with more than one sample, and a two-writer run has happened at least once.
4. After M4e and M4f, the harness reports the new store and the old store
   indistinguishable over every generated sequence, on all nine tables.
5. Every existing test in `internal/storage/postgres_test.go` passes unmodified,
   and the full end-to-end suite is green, both halves.
6. The re-run of M4a's own measurement shows the write cost per `Update` no
   longer growing with the ledger's size or age, and the pass time unchanged
   except where M4g removed a `PATCH`.
7. The owner has answered enough of Q9 for the supported-size statement in the
   consequences below to be replaced by a number.

Items 1 to 3 are reachable without changing any production code. If item 2
shows the current store adequate at the counts Q9 names, this record is
accepted as *not taken*, with the measurement recorded, rather than quietly
left open.

## Consequences

The ledger keeps one shape of transaction and one lock, so everything that
reasons about the store -- ADR 0010's adoption, ADR 0012's abandon, ADR 0013's
cancel, and the fence-then-act sequences all three depend on -- keeps reasoning
about the same thing. The cost of that decision is unchanged too: one global
exclusive lock means a read on one API replica waits behind a write on another,
and a consumer's reservation waits behind whatever the worker is doing. Nothing
here improves that, and the measurement that would say whether it must be
improved does not yet exist.

A write stops being proportional to the ledger and becomes proportional to the
change. The audit trail stops charging every past event to every future write,
which is the difference between a deployment that gets slower with size and one
that gets slower with age. A read is unaffected, and an `Update` still loads the
whole ledger before its closure runs, so the ledger's working set still has to
fit in a process's memory and the load still costs what it costs.

The risk moves from arithmetic to correctness. Today's writer is slow and
obviously right: it deletes everything and writes everything, so a reload cannot
disagree with the state that was handed to it. A diff is fast and conditionally
right, and the condition is a key-set computation and a byte comparison that
nobody can eyeball. That is why the harness is the first package and the diff
is the last, and why the harness has to be shown killing mutants before it is
trusted to clear anything.

Three tables that nothing in this repository reads keep being written. This
record leaves them alone deliberately; if the owner confirms nothing outside
reads them either, retiring them is a further, easy saving, and if something
does read them, the derivation has just become a supported interface and should
be documented as one.

On supported size, stated as an unknown rather than as a bound: nothing here
establishes a ledger size this deployment supports. What is known is that at
5,000 committed allocations on laptop-class development infrastructure, with an
artificially thin ledger carrying no audit history, one worker pass takes 13.55
minutes against a thirty-second interval, and that the per-write cost also
grows with the number of audit events, which no configuration bounds and which
the existing measurement did not contain. A number that could be put in an
operations document needs M4d, and a number that could be promised needs Q9.

And the projection decision leaves a bigger one open. Halving a pass's requests
does not make the pass fit its interval at the sizes M4a measured, so if those
sizes are representative, this project still owes a decision about what a
reconciliation pass is: every allocation every pass, or a rotation, or
concurrent syncing. That decision is not this record's, it needs numbers this
record asks for, and it should not be taken by accident inside a performance
package.

Measured on 2026-09-22 by packages M4c and M4d, the first two of the cut above,
with no production code changed. M4c's differential harness exists, is gated,
and kills six mutants of the current `persistState`, four of them only through a
row-by-row comparison of a write-only table, which is evidence item 1. M4d's
counting test confirms the arithmetic `9 + 3A + O + P + R + D + F + C + E` as an
exact description of the code on every axis at every size, and at a realistic
thousand-allocation ledger with ten audit events per allocation the pass's
bookkeeping write costs about sixteen thousand statements, two thirds of them
the audit term, which is evidence item 2. Its second stack measured what this
record's first decision does not cover: with the worker stopped, an ordinary
reservation's median latency grew from one second at a hundred allocations to
almost five at a thousand, because `loadState` decodes the whole audit history
on every read as well as every write, so the audit term is a read cost too and
the first store package, M4e, must stop the read as well as the re-offer. A
first two-writer run found no lost update, which is evidence item 3 in its
weakest form, two client loops against one replica rather than the configured
topology. On these numbers the owner and the lead took M4e first and M4f after
it on the harness, and set the projection package M4g aside as not urgent, a
pass being bound by NetBox requests at every size measured.
