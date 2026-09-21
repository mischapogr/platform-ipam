# Finding codes

Status: reference, 2026-09-20. `GET /v1/findings` ([docs/API_V1.md](API_V1.md) section 9) returns findings whose `code` this document indexes. It is generated from, and kept honest against, the source: `internal/service/reservation_recovery_test.go`'s `TestDocumentedFindingCodesMatchTheSource` (package H4) parses every non-test `.go` file under `cmd/` and `internal/`, collects the code named at every `workerFinding(` call, every `(*Service).flagStuckHold(` call and every `Code:` field of a `domain.Finding{}` literal, and fails if this document is missing one or names one the source never raises.

Every finding shares the same envelope (`internal/transport/http.go`'s `publicFinding`): a stable `id`, `severity` (`CRITICAL` or `WARNING`), `code`, a nullable `allocation_id`, a nullable `account_id`/`region`, `first_observed_at`/`last_observed_at`, and `status` (`OPEN` or `RESOLVED`). An operator additionally always sees `domain_id`; sees `resource_type`/`resource_id` when the finding names a cloud resource directly (the domain-level occupancy codes); and sees `tenant_id` only where `allocation_id` is set (an allocation-scoped finding has exactly one tenant; a domain-level, de-duplicated row does not). A tenant's own view never carries any of those four fields. `docs/API_V1.md` section 9 covers the full contract, including the operator's de-duplication of domain-level rows.

Two shapes of finding exist:

- **Allocation-scoped**: names one allocation (`allocation_id` set), stored and read one row per allocation. A tenant sees its own; an operator sees it too, unmerged (ADR 0011 stage two, package G3b3), because it is a fact about one allocation with exactly one owning tenant -- including an allocation that is still an uncommitted hold, invisible to `GET /v1/allocations/{id}` but visible through its finding.
- **Domain-level occupancy**: names no allocation (`allocation_id` empty, `resource_type`/`resource_id` set instead), fanned out by `reconcileAllocations`'s occupancy loop (`internal/service/worker.go`) as one stored row per eligible tenant of every pool in the domain -- the same underlying fact about one cloud resource. A tenant sees its own copy; an operator sees the whole group collapsed into a single row (`operatorFindings`), grouped on domain, code, account, region, resource type and resource id.

"Reset every pass" below means `reconcileAllocations`'s own reset switch (`internal/service/worker.go`): at the start of each pass it marks every finding of that code `RESOLVED`, then re-raises it fresh wherever the condition it checks still holds -- so an allocation-scoped or domain-level code in that list self-heals within one worker interval once its cause is gone, with no separate "resolve" step anywhere else. A code **not** in that list is resolved only by the specific code path named below.

## coverage_incomplete

- **Severity**: WARNING.
- **Scope**: allocation-scoped, one row per committed, non-released allocation in the affected domain.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: either of two places finds the domain's cloud coverage unusable for a committed allocation -- `Tick` (`internal/service/service.go`), when the observation attempt this pass just made is missing, incomplete, or carries a coverage generation other than the domain's configured one; or `reconcileAllocations` (`internal/service/worker.go`), when the latest *stored* observation for the domain is absent or fails `trustedObservation` (incomplete, wrong generation, missing timestamps, or stale past `lifecycle.maximum_observation_age_seconds`).
- **Resolved**: reset every pass -- closes itself once a fresh, complete, correctly generationed observation lands within the freshness window.
- **What to do**: check the cloud observer's connectivity and credentials, and that the domain's `coverage_generation` matches what the observer reports; otherwise wait for the next scan (`reconciliation.full_scan_interval_seconds`).

## multiple_resource_claims

- **Severity**: CRITICAL.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `reconcileAllocations` finds more than one observed AWS resource carrying this allocation's `platform-ipam:allocation-id` tag in the same pass.
- **Resolved**: reset every pass -- closes once at most one resource carries the tag.
- **What to do**: find which resource wrongly carries the tag (a copy, a template, or a mistaken re-tag) and remove it; the platform will not guess which of the two is real.

## resource_missing

- **Severity**: CRITICAL.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `reconcileAllocations` finds an `ACTIVE` allocation whose recorded binding is no longer confirmed by the latest observation, and no observed resource carries the allocation's tag at all.
- **Resolved**: reset every pass -- closes once the resource reappears (and its binding is observed again) or once the allocation moves out of `ACTIVE` (for example, via `Release`).
- **What to do**: confirm whether the AWS resource was deleted out of band; if so, coordinate a release, otherwise wait for it to reappear or be re-tagged.

## binding_mismatch

- **Severity**: CRITICAL.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `reconcileAllocations`, in either of two places: (1) an `ACTIVE` allocation whose recorded binding is no longer confirmed by observation, but at least one resource *does* carry its tag -- the tagged resource's shape (target, CIDR, scope, parent, AZ) disagrees with the recorded binding; (2) a still-`RESERVED` allocation with exactly one claimed resource whose shape does not match what `bindingObserved` requires for promotion -- raised instead of promoting the allocation to `ACTIVE`.
- **Resolved**: reset every pass -- closes once the tagged resource's shape agrees with the allocation again (or, for case 2, once promotion succeeds).
- **What to do**: check the tagged resource's CIDR, VPC/subnet nesting, and availability zone against the allocation's own record; fix whichever is wrong.

## reservation_aged

- **Severity**: WARNING.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `reconcileAllocations` finds a still-`RESERVED` allocation with no claimed resource whose age exceeds `lifecycle.reservation_age_alert_hours` (when that setting is greater than zero).
- **Resolved**: reset every pass -- closes once a resource claims the allocation (promoting it) or once it is released.
- **What to do**: tag the intended AWS resource with the allocation's key, or release the reservation if it is no longer needed.

## cloud_occupancy

- **Severity**: WARNING.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `reconcileAllocations` finds a `QUARANTINED` allocation whose address space is still occupied in the cloud (`observationConflict`).
- **Resolved**: reset every pass -- closes once nothing conflicts with the quarantined space.
- **What to do**: informational. The quarantined CIDR is not free yet; no action is required unless the occupant itself needs attention.

## unmanaged_occupancy

- **Severity**: WARNING.
- **Scope**: domain-level occupancy, fanned out to every eligible tenant of every pool in the domain; an operator sees one de-duplicated row per resource.
- **Seen by**: every eligible tenant of the domain's pools, one row each; an operator, collapsed to one row.
- **Raised when**: `reconcileAllocations`'s occupancy loop (`internal/service/worker.go`, below the main per-allocation loop) finds an observed cloud resource with no `platform-ipam:allocation-id` claim (or a claim naming an allocation that is not committed, or not in this domain, or released), **and** the resource is not the one a committed, non-released adoption reviewed at exactly this identity (`adoptedResources`) -- an adopted network is owned but cannot be tagged (ADR 0010), so it is exempted by name rather than by the absence of a tag.
- **Resolved**: reset every pass -- closes once the resource is tagged, imported, adopted, or removed.
- **What to do**: tag the resource with the platform's ownership tags if it should become a managed allocation's binding, `onboard import` it if it should become tracked occupancy without being owned, or `adopt` it (see [ADR 0010](decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md)) if it should become an owned allocation.

## inventory_sync_pending

- **Severity**: WARNING.
- **Scope**: allocation-scoped.
- **Seen by**: the allocation's tenant; an operator, unmerged.
- **Raised when**: `syncProjections` (`internal/service/worker.go`) fails to push a committed, non-released allocation's current state into its NetBox metadata projection (`Inventory.Sync`).
- **Resolved**: **not** reset by `reconcileAllocations`. Resolved only by `syncProjections` itself, the next pass its own `Sync` call for that allocation succeeds.
- **What to do**: usually transient -- the allocation's own state (the ledger) is unaffected, only the NetBox metadata mirror is stale. If it stays open across several passes, check NetBox connectivity and the adapter's write permissions.

## adoption_stuck

- **Severity**: CRITICAL -- the domain is fenced while this is open (every reservation there answers `503 domain_busy`).
- **Scope**: allocation-scoped, on the uncommitted allocation the stuck `ADOPT` operation belongs to.
- **Seen by**: the allocation's (would-be) owning tenant; an operator, unmerged.
- **Raised when**: `recoverAdoption` (`internal/service/worker.go`) cannot finish a pending `ADOPT` operation on a worker pass, for any of the causes [deploy/runbooks/ADOPTION.md section 10](../deploy/runbooks/ADOPTION.md) enumerates in full -- never for a timeout, a cancellation, or `domain.ErrInventoryUncertain`, which are retried in silence.
- **Resolved**: **not** reset by `reconcileAllocations` (deliberately excluded -- worker.go's own comment says so). Resolved either by `recoverAdoption`'s own commit (`commitRecovered`'s event callback), once a later pass converges, or by `platform-ipam adopt abandon` (package H2c) withdrawing the uncommitted adoption.
- **What to do**: see the runbook's diagnosis and resolution procedure (section 10) linked above; do not duplicate it here.

## reservation_stuck

- **Severity**: CRITICAL -- the domain is fenced while this is open (every reservation there answers `503 domain_busy`), exactly as `adoption_stuck` fences it.
- **Scope**: allocation-scoped, on the uncommitted allocation the stuck `RESERVE` operation belongs to.
- **Seen by**: the allocation's (would-be) owning tenant; an operator, unmerged.
- **Raised when**: `recoverReservations` (`internal/service/worker.go`, package H4) cannot finish a pending `RESERVE` operation on a worker pass, for a decision reached on evidence read -- a definite `Inventory.Ensure` refusal (for example, the held CIDR is already occupied by a prefix created out of band), an empty inventory id with no error, or a trusted, current observation showing something other than this allocation's own correctly tagged resource on the held CIDR. Never for uncertainty -- no observer, an untrusted or incomplete observation, or `uncertainInventoryError` (a timeout, a cancellation, or `domain.ErrInventoryUncertain`, which `internal/netbox/client.go`'s `Ensure` now marks the same way `Adopt` always has) -- those are retried in silence. Withheld, even once a decision is reached, until the pending operation is older than one observation age (`lifecycle.maximum_observation_age_seconds`): a consumer's own synchronous `POST` calls `Ensure` itself right after persisting the same operation a worker pass can already see (api and worker are independent processes sharing one ledger), so a decision reached on a hold that young could just be racing its own creator rather than being genuinely stuck.
- **Resolved**: **not** reset by `reconcileAllocations` (same reason as `adoption_stuck`: reopening it every pass would fight the flapping guard above). Two ways, both terminal for this finding: `recoverReservations`'s own commit (`commitRecovered`'s callback), or -- if the request that originally created the hold finishes it synchronously instead of the worker -- the matching resolution in `reserve`'s own synchronous commit path (`internal/service/service.go`); or the owning consumer's own `DELETE /v1/operations/{operation_id}` / `client cancel` (`internal/service/cancel.go`, [ADR 0013](decisions/0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md), package H8), resolved in the same ledger transaction that deletes the withdrawn hold.
- **What to do**: make the ground truth match what the reservation expected -- most often, remove or coordinate ownership of whatever now occupies the held CIDR out of band -- after which the next worker pass converges on its own. Where that cannot be done, the owning tenant may cancel the hold: `DELETE /v1/operations/{operation_id}` or `platform-ipam client cancel --operation-id <id>`, refused with `409 reservation_not_stuck` unless this finding is `OPEN` for that allocation, and never available to an operator (ADR 0011 is untouched by this: the cancel is the consumer's own write). Cancelling fences the operation to `FAILED` with the stored code `reservation_cancelled`, deletes any NetBox prefix carrying the allocation's marker, deletes the uncommitted allocation row and the consumer's idempotency record, and resolves this finding; the allocation key is left free, and a retry under the **same** key is a fresh reservation, not a replay of the cancelled one. A cancel attempted while no worker is running to have raised this finding in the first place answers `409 reservation_not_stuck` about a hold that may genuinely be dead -- that is the honest answer, not a bug, and the fix is to start the worker, never to reach for the database.
