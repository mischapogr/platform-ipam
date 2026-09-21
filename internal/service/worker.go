package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// recoveryJob is one durable intent a worker pass may finish: an uncommitted
// allocation and the pending operation that fences its whole overlap domain
// until somebody resolves it.
type recoveryJob struct {
	allocation domain.Allocation
	operation  domain.Operation
}

// pendingRecoveryJobs collects the uncommitted holds of one operation type. The
// type is the filter the two recovery paths differ by, and at this point the
// only thing they may differ by: a hold that nothing recovers answers every
// reservation in its domain with 503 domain_busy until a person intervenes.
func (s *Service) pendingRecoveryJobs(ctx context.Context, operationType string) ([]recoveryJob, error) {
	var jobs []recoveryJob
	err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, o := range st.Operations {
			a := st.Allocations[o.AllocationID]
			if o.Type == operationType && o.Status == operationPending && a.ID != "" && !a.Committed && a.State == domain.Reserved {
				jobs = append(jobs, recoveryJob{a, *copyOperation(o)})
			}
		}
		return nil
	})
	return jobs, err
}

// recoveryEvidence is the cloud half of both recovery guards. A restart may
// happen long after the request was admitted, so no durable hold becomes a
// usable allocation unless current, complete, trustworthy evidence still
// permits it; absent or stale evidence is uncertainty, and uncertainty leaves
// the hold pending rather than refusing it. It also reports the parent's
// resource, which both overlap rules need for a subnet.
func (s *Service) recoveryEvidence(ctx context.Context, a domain.Allocation) (*domain.Observation, string) {
	if s.observer == nil {
		return nil, ""
	}
	d := domainFor(s.cfg, a.DomainID)
	observation, err := s.observe(ctx, d)
	if err != nil || observation == nil || !trustedObservation(*observation, d, s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) {
		return nil, ""
	}
	parentAWSID := ""
	if a.ParentAllocationID != "" {
		_ = s.ledger.View(ctx, func(st *domain.State) error {
			if parent := st.Allocations[a.ParentAllocationID]; parent.Binding != nil {
				parentAWSID = parent.Binding.ResourceID
			}
			return nil
		})
	}
	return observation, parentAWSID
}

// commitRecovered is the second of the two places a committed allocation comes
// into being (ADR 0010). Both recovery paths go through it, because an adoption
// differs from a reservation in the audit event it writes and in nothing it
// makes true of the allocation; a third assignment would leave every guarantee
// the ledger, the API and the safety tests rest on unproven for whichever path
// owned it. Every gate is rechecked under the ledger's lock, so a release or a
// competing commit that landed in between wins.
func (s *Service) commitRecovered(ctx context.Context, j recoveryJob, inventoryID string, event func(*domain.State, domain.Allocation)) error {
	return s.ledger.Update(ctx, func(st *domain.State) error {
		a, ok := st.Allocations[j.allocation.ID]
		o := st.Operations[j.operation.ID]
		if !ok || a.Committed || a.State != domain.Reserved || o.Status != operationPending || o.Type != j.operation.Type {
			return nil
		}
		a.Committed = true
		a.InventoryID = inventoryID
		a.InventorySync = "CURRENT"
		a.UpdatedAt = s.now().UTC()
		o.Status = operationSucceeded
		o.Result = map[string]string{"allocation_id": a.ID}
		o.UpdatedAt = a.UpdatedAt
		st.Allocations[a.ID] = a
		st.Operations[o.ID] = o
		event(st, a)
		return nil
	})
}

// recoverReservations retries the exact durable intent after a lost response or
// process restart. Inventory implementations must recover by operation marker;
// this path never asks an external system to choose another CIDR.
//
// Until package H4 this loop raised nothing when a pending RESERVE could be
// neither finished nor safely retried: a `continue` on every obstacle left a
// consumer's 202 fencing its overlap domain (every reservation there answers
// 503 domain_busy) with no visible reason. It now classifies exactly as
// recoverAdoption does -- uncertainty is retried in silence, a decision the
// adapter or the observation reached on evidence it read raises
// reservation_stuck -- and shares flagStuckHold with the adoption path.
func (s *Service) recoverReservations(ctx context.Context) error {
	jobs, err := s.pendingRecoveryJobs(ctx, reserveOperation)
	if err != nil {
		return err
	}
	if s.inventory == nil {
		return nil
	}
	for _, j := range jobs {
		observation, parentAWSID := s.recoveryEvidence(ctx, j.allocation)
		if observation == nil {
			// No observer, or no trustworthy current observation: uncertainty,
			// not a decision. Nothing is known, so nothing is said.
			continue
		}
		if !pendingReservationObservationSafe(j.allocation, *observation, parentAWSID) {
			// A trusted observation that shows something other than this
			// allocation's own correctly tagged resource on the held CIDR is a
			// decision reached on evidence read, exactly as an adoption's
			// reviewedOccupancy failing is one.
			if err := s.flagAgedReservation(ctx, j); err != nil {
				return err
			}
			continue
		}
		inventoryID, ensureErr := s.inventory.Ensure(ctx, j.allocation, j.operation.ID)
		if ensureErr != nil {
			if uncertainInventoryError(ensureErr) {
				continue
			}
			if err := s.flagAgedReservation(ctx, j); err != nil {
				return err
			}
			continue
		}
		if inventoryID == "" {
			// Defensive: an adapter answering with neither an id nor an error
			// says nothing usable either, but it is not the silence
			// uncertainInventoryError names, so it is treated as a decision.
			if err := s.flagAgedReservation(ctx, j); err != nil {
				return err
			}
			continue
		}
		if err = s.commitRecovered(ctx, j, inventoryID, func(st *domain.State, a domain.Allocation) {
			workerEvent(st, a, "reservation_committed", "Recovered the persisted inventory operation.", a.UpdatedAt)
			resolveFinding(st, a, "reservation_stuck")
		}); err != nil {
			return err
		}
	}
	return nil
}

// flagAgedReservation raises reservation_stuck once a decision has been
// reached about a pending hold, but only once the hold has outlived one
// observation age (s.cfg.Lifecycle.MaxObservationAge, the same interval that
// already bounds how stale evidence may be before it stops being trusted).
//
// A consumer's own synchronous POST calls Ensure itself, right after
// persisting the same pending operation this loop just read (the first
// caller's synchronous commit path in service.go's reserve) -- and api and
// worker are independent processes sharing one ledger (cmd/platform-ipam
// dispatches them separately), so the very next worker Tick can see that
// operation before the request that created it has gotten an answer. Calling
// Ensure again immediately can then answer with a decision that only
// reflects that race, not a genuinely stuck hold: raising a CRITICAL finding
// on a hold that is merely young would flap on every reservation racing its
// own recovery. Waiting out one observation age gives that request room to
// finish first.
func (s *Service) flagAgedReservation(ctx context.Context, j recoveryJob) error {
	threshold := time.Duration(s.cfg.Lifecycle.MaxObservationAge) * time.Second
	if threshold > 0 && s.now().UTC().Sub(j.operation.UpdatedAt) < threshold {
		return nil
	}
	return s.flagStuckHold(ctx, j.allocation, "reservation_stuck")
}

// recoverAdoptions finishes a crashed adoption. recoverReservations cannot: it
// filters on the reserve type and calls Ensure, which is permanently refused
// for a CIDR that already holds a prefix, and its observation guard demands a
// tag the platform is not allowed to write (ADR 0010). Without this sibling a
// pending ADOPT operation fences its overlap domain -- every consumer
// reservation there answered 503 domain_busy -- until somebody edits the
// database by hand. It calls the one adapter operation that can finish the
// intent, under the evidence that admitted it, and writes nothing else: an
// adoption has no undo, so wherever recovery is not certain the operation stays
// pending and a person is told.
func (s *Service) recoverAdoptions(ctx context.Context) error {
	jobs, err := s.pendingRecoveryJobs(ctx, adoptOperation)
	if err != nil {
		return err
	}
	if s.inventory == nil {
		return nil
	}
	for _, j := range jobs {
		if err := s.recoverAdoption(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverAdoption(ctx context.Context, j recoveryJob) error {
	// Without the durable record there is no reviewed id to hold the adapter's
	// answer against, and an adoption that cannot prove it converted the
	// reviewed object must never commit. That is a stuck adoption rather than a
	// recoverable one.
	if j.operation.Adoption == nil {
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	record := *j.operation.Adoption
	cidr, err := netip.ParsePrefix(j.allocation.CIDR)
	if err != nil {
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	observation, parentAWSID := s.recoveryEvidence(ctx, j.allocation)
	if observation == nil {
		return nil
	}
	// A trusted observation that no longer shows the reviewed resource alone at
	// the adopted CIDR is not uncertainty: the ground the operator reviewed has
	// changed, and only a person can say what that means.
	if reviewedOccupancy(*observation, cidr, j.allocation.Request, record.ResourceID, parentAWSID) != nil {
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	snapshot, snapshotErr := s.inventory.Snapshot(ctx, domainFor(s.cfg, j.allocation.DomainID))
	if snapshotErr != nil || !snapshot.Complete {
		return nil
	}
	// The adapter looks only at the adopted CIDR, so the snapshot is what still
	// sees a second unmanaged network overlapping it, and what tells a prefix
	// this operation already converted from one that has since become somebody
	// else's. Asking here also keeps the adapter out of a call it would refuse.
	// The children come from the observation reviewedOccupancy has just been
	// satisfied by, so recovery admits exactly what planning admitted.
	children := reviewedVPCChildren(*observation, cidr, j.allocation.Request, record.ResourceID)
	if _, err := reviewedNetwork(snapshot, cidr, j.allocation.Request, record.NetworkID, j.allocation.ID, children); err != nil {
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	inventoryID, adoptErr := s.inventory.Adopt(ctx, j.allocation, j.operation.ID)
	if adoptErr != nil {
		if uncertainInventoryError(adoptErr) {
			return nil
		}
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	if !adoptedTheReviewedObject(j.operation, inventoryID) {
		return s.flagStuckAdoption(ctx, j.allocation)
	}
	return s.commitRecovered(ctx, j, inventoryID, func(st *domain.State, a domain.Allocation) {
		// The operator is the actor here as on the planned event: the worker
		// performed the write, but the adoption is still theirs.
		st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: a.ID, TenantID: a.TenantID, Actor: record.Operator, Action: adoptKind.committed, Reason: "inventory adoption confirmed; " + adoptionEvidence(record, *observation), At: a.UpdatedAt, Revision: a.Revision})
		resolveFinding(st, a, "adoption_stuck")
	})
}

// flagStuckAdoption is what is left when a pending adoption can be neither
// finished nor abandoned: there is no undo, the operation keeps fencing its
// overlap domain, and nothing here touches the inventory. A person has to look
// at it, so the severity is the one that says so. workerFinding keys on the
// allocation, so a further pass updates that finding instead of adding another.
func (s *Service) flagStuckAdoption(ctx context.Context, a domain.Allocation) error {
	return s.flagStuckHold(ctx, a, "adoption_stuck")
}

// flagStuckHold is what flagStuckAdoption and flagAgedReservation share: a
// pending hold -- a reservation or an adoption -- that can be neither
// finished nor safely retried. Both are the same shape once a decision has
// been reached: the operation keeps fencing its overlap domain, nothing here
// touches the inventory, and a person has to look at it, so the severity is
// the one that says so. workerFinding keys on the allocation, so a further
// pass updates that finding instead of adding another. The two recovery
// paths still never cross: this only ever runs from code each of them owns,
// under the code each of them decided on.
func (s *Service) flagStuckHold(ctx context.Context, a domain.Allocation, code string) error {
	return s.ledger.Update(ctx, func(st *domain.State) error {
		held, ok := st.Allocations[a.ID]
		if !ok || held.Committed {
			return nil
		}
		workerFinding(st, held, code, "CRITICAL", s.now().UTC())
		return nil
	})
}

// uncertainInventoryError reports an adapter error that says nothing about the
// object it was asked about. A timeout or a cancellation may or may not have
// written, so the operation stays pending without a finding and the next pass
// repeats the call, which is safe because Adopt writes nothing until every
// check has passed and converges on a prefix it already converted. Any other
// error is a decision the adapter reached on evidence it read -- a different
// owner, a missing prefix, a lost import tag -- and repeating it will not
// change the answer, so somebody is told instead.
func uncertainInventoryError(err error) bool {
	if errors.Is(err, domain.ErrInventoryUncertain) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// An already-created, correctly tagged AWS resource can be evidence of a
// crashed apply rather than a conflicting claimant. Any different overlap still
// keeps the reservation pending for operator investigation.
func pendingReservationObservationSafe(a domain.Allocation, o domain.Observation, parentAWSID string) bool {
	want, err := netip.ParsePrefix(a.CIDR)
	if err != nil {
		return false
	}
	for _, resource := range o.Resources {
		for _, raw := range append([]string{resource.CIDR}, resource.CIDRs...) {
			got, parseErr := netip.ParsePrefix(raw)
			if parseErr != nil || !want.Overlaps(got) {
				continue
			}
			if a.Scope == "subnet" && resource.Type == "vpc" && parentAWSID != "" && resource.ID == parentAWSID && got.Contains(want.Addr()) {
				continue
			}
			if resource.Type == a.Scope && resource.CIDR == a.CIDR && resource.AccountID == a.AccountID && resource.Region == a.Region && resource.Tags["platform-ipam:allocation-id"] == a.ID && resource.Tags["platform-ipam:allocation-key"] == a.AllocationKey {
				continue
			}
			return false
		}
	}
	return true
}

// reconcileAllocations evaluates one ledger snapshot of the latest coverage.
// It never trusts client tags alone: target, primary CIDR, scope, parent and AZ
// must all agree, and duplicate claims inhibit automatic binding.
func (s *Service) reconcileAllocations(ctx context.Context) error {
	now := s.now().UTC()
	return s.ledger.Update(ctx, func(st *domain.State) error {
		for id, f := range st.Findings {
			switch f.Code {
			case "coverage_incomplete", "resource_missing", "binding_mismatch", "multiple_resource_claims", "unmanaged_occupancy", "reservation_aged", "cloud_occupancy":
				f.Status = "RESOLVED"
				st.Findings[id] = f
			}
		}
		for id, a := range st.Allocations {
			if !a.Committed || a.State == domain.Released {
				continue
			}
			rows := st.Observations[a.DomainID]
			d := domainFor(s.cfg, a.DomainID)
			if len(rows) == 0 || !trustedObservation(rows[len(rows)-1], d, now, s.cfg.Lifecycle.MaxObservationAge) {
				workerFinding(st, a, "coverage_incomplete", "WARNING", now)
				continue
			}
			o := rows[len(rows)-1]
			a.LastObservedAt = &o.FinishedAt
			st.Allocations[id] = a
			var claimed []domain.Resource
			for _, r := range o.Resources {
				if r.Tags["platform-ipam:allocation-id"] == a.ID {
					claimed = append(claimed, r)
				}
			}
			if len(claimed) > 1 {
				workerFinding(st, a, "multiple_resource_claims", "CRITICAL", o.FinishedAt)
				continue
			}
			parentID := ""
			if a.Scope == "subnet" {
				parent := st.Allocations[a.ParentAllocationID]
				if parent.State != domain.Active || parent.Binding == nil {
					continue
				}
				parentID = parent.Binding.ResourceID
			}
			if a.State == domain.Quarantined {
				if observationConflict(a, o, parentID) {
					workerFinding(st, a, "cloud_occupancy", "WARNING", o.FinishedAt)
				}
				continue
			}
			if a.State == domain.Active {
				if a.Binding == nil || !bindingObserved(o, *a.Binding, a.ID, a.AllocationKey, a.CIDR, a.AvailabilityZoneID, parentID) {
					code := "resource_missing"
					if len(claimed) > 0 {
						code = "binding_mismatch"
					}
					workerFinding(st, a, code, "CRITICAL", o.FinishedAt)
				}
				continue
			}
			if len(claimed) == 0 {
				if s.cfg.Lifecycle.ReservationAgeAlertHours > 0 && now.Sub(a.CreatedAt) >= time.Duration(s.cfg.Lifecycle.ReservationAgeAlertHours)*time.Hour {
					workerFinding(st, a, "reservation_aged", "WARNING", o.FinishedAt)
				}
				continue
			}
			r := claimed[0]
			b := domain.Binding{Provider: "aws", ResourceType: a.Scope, ResourceID: r.ID, AccountID: a.AccountID, Region: a.Region}
			if !bindingObserved(o, b, a.ID, a.AllocationKey, a.CIDR, a.AvailabilityZoneID, parentID) {
				workerFinding(st, a, "binding_mismatch", "CRITICAL", o.FinishedAt)
				continue
			}
			if pendingForAllocation(st, a.ID) != nil {
				continue
			}
			b.VerifiedAt = &o.FinishedAt
			a.Binding = &b
			a.State = domain.Active
			a.Revision++
			a.UpdatedAt = now
			a.InventorySync = "PENDING"
			st.Allocations[id] = a
			op := domain.Operation{ID: domain.NewID("op"), Type: bindOperation, Status: operationSucceeded, AllocationID: a.ID, TenantID: a.TenantID, DomainID: a.DomainID, Candidate: &b, Result: map[string]string{"allocation_id": a.ID}, CreatedAt: now, UpdatedAt: now}
			st.Operations[op.ID] = op
			workerEvent(st, a, "binding_verified", "Observed one matching AWS resource.", now)
		}
		// Occupancy is also visible when tags are missing or refer to unknown IDs.
		// Emit only to tenants whose configured pools belong to this routing domain.
		for _, d := range s.cfg.Domains {
			rows := st.Observations[d.ID]
			if len(rows) == 0 {
				continue
			}
			o := rows[len(rows)-1]
			if !trustedObservation(o, d, now, s.cfg.Lifecycle.MaxObservationAge) {
				continue
			}
			adopted := adoptedResources(st, d.ID)
			for _, r := range o.Resources {
				claim := r.Tags["platform-ipam:allocation-id"]
				a, ok := st.Allocations[claim]
				if ok && a.DomainID == d.ID && a.State != domain.Released {
					continue
				}
				// An adopted network is owned in the ledger and untagged in the
				// cloud, because the platform cannot tag somebody else's
				// resource (ADR 0010). The claim lookup above cannot see that,
				// and this finding names no allocation and is fanned out to
				// every eligible tenant of the domain, so left alone it would
				// report an owned network as unmanaged occupancy to people who
				// can neither act on it nor tell it from the real thing. A
				// resource carrying somebody else's claim is still reported:
				// only the absence of a claim is what an adoption explains.
				if claim == "" && adopted[r.ID] == resourceIdentity(r.Type, r.AccountID, r.Region, r.CIDR) {
					continue
				}
				for _, pool := range s.cfg.Pools {
					if pool.DomainID != d.ID {
						continue
					}
					for _, tenant := range pool.EligibleTenants {
						key := findingID("unmanaged_occupancy", d.ID+":"+tenant+":"+r.AccountID+":"+r.Region+":"+r.Type+":"+r.ID)
						f := st.Findings[key]
						if f.ID == "" {
							f = domain.Finding{ID: key, TenantID: tenant, DomainID: d.ID, Code: "unmanaged_occupancy", Severity: "WARNING", AccountID: r.AccountID, Region: r.Region, FirstObservedAt: o.FinishedAt}
						}
						// The identity the operator's domain view groups these
						// rows on (ADR 0011), and the one place in this project
						// that sets it. It is written on every pass rather than
						// only where the row is created, so a row stored before
						// the two fields existed carries them again after one
						// worker interval and needs no backfill: the key is a
						// hash of exactly these values, so a row found under it
						// is a row about this resource. FirstObservedAt is
						// deliberately left alone -- the finding is the same one,
						// and when it was first seen is not what changed.
						f.ResourceType, f.ResourceID = r.Type, r.ID
						f.Status = "OPEN"
						f.LastObservedAt = o.FinishedAt
						st.Findings[key] = f
					}
				}
			}
		}
		return nil
	})
}

// adoptedResources indexes, by cloud resource id, the networks one routing
// domain's adoptions own: the resource an operator reviewed, recorded on the
// ADOPT operation of an allocation that is committed and not released (a
// pending or refused adoption has no committed allocation, so it exempts
// nothing). The
// value is the identity bindingObserved and the adoption rules both use -- type,
// account, region and primary CIDR -- so a reviewed resource that has since
// changed shape, a secondary association, and a partial overlap all stay
// reportable. It is deliberately not "any committed allocation": an untagged
// resource sitting on an ordinary reservation was never reviewed by anybody,
// and reporting it is how its owner learns to tag it.
func adoptedResources(st *domain.State, domainID string) map[string]string {
	out := map[string]string{}
	for _, o := range st.Operations {
		if o.Type != adoptOperation || o.Adoption == nil || o.Adoption.ResourceID == "" {
			continue
		}
		a, ok := st.Allocations[o.AllocationID]
		if ok && a.Committed && a.DomainID == domainID && a.State != domain.Released {
			out[o.Adoption.ResourceID] = resourceIdentity(a.Scope, a.AccountID, a.Region, a.CIDR)
		}
	}
	return out
}

func resourceIdentity(kind, account, region, cidr string) string {
	return kind + "\x00" + account + "\x00" + region + "\x00" + cidr
}

// projectionSyncResult is one Sync call's outcome, collected while
// syncProjections's loop still holds the plain domain.Allocation it read the
// job from (before any write), so the batched write below can still apply
// the identical per-job revision/state guard a per-job write always could.
type projectionSyncResult struct {
	allocation domain.Allocation
	err        error
}

// Pending metadata is durable on each allocation. Rechecking all committed
// projections also repairs drift and eventual ordering after worker restarts.
// No lifecycle or reuse decision depends on a NetBox metadata projection.
//
// Until package M4a this called s.ledger.Update once PER allocation:
// internal/storage's persistState rewrites every row of every ledger table on
// every Update, so a pass syncing N allocations cost O(N^2) Postgres writes,
// not O(N) -- the dominant cost at scale, well above the NetBox traffic
// itself (docs/DEPLOYMENT.md's dated measurement section). The NetBox calls
// still happen exactly as before, one Sync per job, in the same order; only
// the bookkeeping write is batched into one Update after the loop, applying
// the same revision/state guard and the same PENDING/CURRENT and finding
// effects per allocation that the old per-job Update applied. A cheap
// pre-check (a View, not a second full rewrite) skips that Update outright
// when every result is already reflected in the ledger.
func (s *Service) syncProjections(ctx context.Context) error {
	if s.inventory == nil {
		return nil
	}
	var jobs []domain.Allocation
	if err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, a := range st.Allocations {
			if a.Committed && a.State != domain.Released && a.InventoryID != "" {
				if op := pendingForAllocation(st, a.ID); op != nil && op.Type == reclaimOperation {
					continue
				}
				jobs = append(jobs, a)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// interrupted is set only when ctx itself ends the pass early (Tick's own
	// context, checked before each remaining job -- never a single NetBox
	// call's own timeout, which Sync already reports as an ordinary error on
	// its own job). Results gathered before that point must still be
	// recorded: a per-job write kept every job that finished before a
	// cancellation landed, and the batched write below keeps that property by
	// flushing whatever was collected before returning the interruption.
	var results []projectionSyncResult
	var interrupted error
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			interrupted = err
			break
		}
		syncErr := s.inventory.Sync(ctx, job)
		if syncErr != nil && ctx.Err() != nil {
			// The pass was cancelled while this call was in flight, so its
			// error says nothing about the inventory. Recording it would
			// raise inventory_sync_pending for a healthy allocation on every
			// shutdown; the per-job write this replaced could not record it
			// either, because its own Update ran under the cancelled context.
			interrupted = ctx.Err()
			break
		}
		results = append(results, projectionSyncResult{allocation: job, err: syncErr})
	}
	if len(results) > 0 {
		if err := s.applyProjectionResults(ctx, results, interrupted != nil); err != nil {
			return fmt.Errorf("record inventory projection result: %w", err)
		}
	}
	return interrupted
}

// applyProjectionResults writes syncProjections's collected results in one
// ledger.Update. flushingCancellation is true only when the pass was cut
// short by ctx itself: the pre-check below is skipped (its own read could
// fail against an ending context, and durability matters more here than
// saving one write) and the write detaches from ctx's cancellation with
// context.WithoutCancel so the results already gathered are not lost with it
// -- the interruption is still returned to the caller by syncProjections,
// only the flush itself is allowed to finish.
func (s *Service) applyProjectionResults(ctx context.Context, results []projectionSyncResult, flushingCancellation bool) error {
	now := s.now().UTC()
	if !flushingCancellation {
		if unchanged, err := s.projectionResultsAlreadyApplied(ctx, results); err == nil && unchanged {
			return nil
		}
		// A failed pre-check is not fatal: fall through to the Update, which
		// re-validates every guard itself under the ledger lock regardless of
		// what the View above could or could not observe.
	}
	writeCtx := ctx
	if flushingCancellation {
		writeCtx = context.WithoutCancel(ctx)
	}
	return s.ledger.Update(writeCtx, func(st *domain.State) error {
		for _, r := range results {
			a, ok := st.Allocations[r.allocation.ID]
			if !ok || a.Revision != r.allocation.Revision || a.State != r.allocation.State {
				continue
			}
			if r.err != nil {
				a.InventorySync = "PENDING"
				workerFinding(st, a, "inventory_sync_pending", "WARNING", now)
			} else {
				a.InventorySync = "CURRENT"
				resolveFinding(st, a, "inventory_sync_pending")
			}
			st.Allocations[a.ID] = a
		}
		return nil
	})
}

// projectionResultsAlreadyApplied reports whether every result is already
// reflected in the ledger, so the batched Update above -- and the full-ledger
// rewrite persistState gives it -- can be skipped outright when a pass finds
// nothing to change (the common case once every allocation's projection is
// current). It applies the identical revision/state guard the write does: a
// result whose allocation moved on is not "already applied", it is simply not
// this result's to judge, and the write path is what actually decides that:
// skipping the write over it here changes nothing the write would have
// changed either. A read-only View, never a second rewrite.
func (s *Service) projectionResultsAlreadyApplied(ctx context.Context, results []projectionSyncResult) (bool, error) {
	unchanged := true
	err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, r := range results {
			a, ok := st.Allocations[r.allocation.ID]
			if !ok || a.Revision != r.allocation.Revision || a.State != r.allocation.State {
				continue
			}
			open := st.Findings[findingID("inventory_sync_pending", a.ID)].Status == "OPEN"
			if r.err != nil {
				// A failure is always written, even a repeated one: the
				// finding's last-observed time is how an operator tells a
				// failure that is still happening from one that stopped
				// being looked at. Failures are rare; the pass this skip
				// exists for is the one where every sync succeeded.
				unchanged = false
				return nil
			} else if a.InventorySync != "CURRENT" || open {
				unchanged = false
				return nil
			}
		}
		return nil
	})
	return unchanged, err
}

func workerEvent(st *domain.State, a domain.Allocation, action, reason string, at time.Time) {
	st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: a.ID, TenantID: a.TenantID, Actor: "reconciliation-worker", Action: action, Reason: reason, At: at, Revision: a.Revision})
}
func workerFinding(st *domain.State, a domain.Allocation, code, severity string, at time.Time) {
	key := findingID(code, a.ID)
	f := st.Findings[key]
	if f.ID == "" {
		f = domain.Finding{ID: key, TenantID: a.TenantID, DomainID: a.DomainID, AllocationID: a.ID, Code: code, Severity: severity, AccountID: a.AccountID, Region: a.Region, FirstObservedAt: at}
	}
	f.Status = "OPEN"
	f.LastObservedAt = at
	st.Findings[key] = f
}

// resolveFinding closes one this worker raised. reconcileAllocations reopens
// the codes it owns from scratch on every pass; a code raised outside it is
// resolved where the condition that raised it is answered instead.
func resolveFinding(st *domain.State, a domain.Allocation, code string) {
	key := findingID(code, a.ID)
	if f, ok := st.Findings[key]; ok {
		f.Status = "RESOLVED"
		st.Findings[key] = f
	}
}
