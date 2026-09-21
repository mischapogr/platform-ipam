package service

import (
	"context"
	"log/slog"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// AdoptionVerdict is PlanAdoption's read-only answer for one reviewed record:
// what Adopt would do, without anything having been written. Allocation
// carries the fields Adopt would record -- CIDR, PoolID, DomainID and the
// Request (so PrefixLength, AccountID and the rest) are always the verified
// values, even when nothing durable exists yet.
type AdoptionVerdict struct {
	// Allocation is the existing allocation when Existing is true (a replay,
	// whichever way Adopt would report it), or a freshly assembled one --
	// ID, RequestHash, timestamps and every other minted field stay at their
	// zero value, since nothing has been persisted -- when apply would create
	// a new adoption.
	Allocation domain.Allocation
	// Existing is true when a matching allocation already exists under this
	// tenant and key: apply would replay it rather than create a new one.
	Existing bool
	// Pending is meaningful only when Existing is true. It reports that the
	// existing allocation has not committed, so apply would return 202 rather
	// than finish synchronously -- the operator should expect to re-run apply
	// (or, once package F3 lands, let the worker's recovery path finish it)
	// rather than treat the record as done.
	Pending bool
}

// PlanAdoption runs every check Adopt applies before its ledger write --
// Adopt's own tenant-and-key pre-check, validateRequest, the replay scan
// reserve makes before it ever opens a write transaction, the complete-
// snapshot and untrustworthy-observation guards, and pinnedCIDR -- and
// reports the verdict without calling Ledger.Update or Inventory.Adopt. It
// exists so `platform-ipam adopt plan` (ADR 0010, work plan package F4) has
// one place to ask "what would apply do with this record", sharing every
// rule with Adopt instead of re-deriving them: a rule that decides whether a
// candidate is adoptable lives exactly once, in the functions this function
// and Adopt both call, so `plan` cannot drift from `apply`.
//
// This function is additive: it is defined in its own file and calls only
// unexported helpers Adopt and reserve already use, so nothing above it in
// this package changes to support it.
func (s *Service) PlanAdoption(ctx context.Context, p domain.Principal, req domain.Request, pin Adoption) (*AdoptionVerdict, error) {
	if pin.Operator == "" {
		return nil, apiErr(422, "invalid_request", "an operator subject is required for the adoption audit trail")
	}
	if pin.CIDR == "" || pin.NetworkID == "" || pin.ResourceID == "" {
		return nil, apiErr(422, "invalid_request", "adoption requires a reviewed CIDR, inventory id and resource id")
	}
	if req.AllocationKey == "" {
		return nil, apiErr(422, "invalid_request", "allocation_key is invalid")
	}
	req.Description, req.Labels = "", nil

	// Mirrors Adopt's own pre-check, in the same order and before anything
	// else -- even before validateRequest: a key that already names an
	// allocation must be this adoption's replay (adoptedAs), or the request
	// is refused right here exactly as Adopt refuses it.
	if err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, old := range st.Allocations {
			if old.TenantID == p.TenantID && old.AllocationKey == req.AllocationKey {
				return adoptedAs(old, pin)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if err := s.ledgerReady(ctx); err != nil {
		return nil, err
	}
	r, pool, dom, err := s.validateRequest(ctx, p, req)
	if err != nil {
		return nil, err
	}

	// reserve's own pre-Update replay check: the idempotency record under
	// ADOPT's own method and path, or -- for a key with no such record yet --
	// a tenant-and-key match compared against the immutable hash. Read-only,
	// and identical to what reserve checks before it ever opens a write
	// transaction, so a record already fully or partially adopted is reported
	// as a replay rather than re-verified as if it were new.
	immutableHash := requestHash(r)
	httpHash := requestBodyHash(r)
	requestID := idempotencyID(p.TenantID, adoptKind.method, adoptKind.path, r.AllocationKey)
	var verdict *AdoptionVerdict
	if err := s.ledger.View(ctx, func(st *domain.State) error {
		if old, ok := st.Requests[requestID]; ok {
			if old.Hash != httpHash {
				return apiErr(409, "idempotency_mismatch", "the idempotency key was used with a different request")
			}
			a, _ := stateResults(st, old)
			if a != nil {
				if a.State == domain.Quarantined || a.State == domain.Released {
					return apiErr(409, "allocation_key_retired", "allocation key is permanently retired")
				}
				// reserve's replay paths refuse a hold an abandon has fenced
				// but not yet deleted, so this copy of them has to as well, or
				// plan would promise the 202 apply no longer gives (ADR 0012).
				if err := abandoningReplay(st, a); err != nil {
					return err
				}
				verdict = &AdoptionVerdict{Allocation: *a, Existing: true, Pending: !a.Committed}
			}
			return nil
		}
		for _, old := range st.Allocations {
			if old.TenantID != p.TenantID || old.AllocationKey != r.AllocationKey {
				continue
			}
			if old.RequestHash != immutableHash {
				return apiErr(409, "allocation_key_conflict", "allocation key belongs to a different immutable request")
			}
			if old.State == domain.Quarantined || old.State == domain.Released {
				return apiErr(409, "allocation_key_retired", "allocation key is permanently retired")
			}
			a := copyAllocation(old)
			if err := abandoningReplay(st, a); err != nil {
				return err
			}
			verdict = &AdoptionVerdict{Allocation: *a, Existing: true, Pending: !a.Committed}
			return nil
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if verdict != nil {
		return verdict, nil
	}

	// Fresh candidate from here: the same snapshot and observation guards
	// reserve applies before it selects (or, here, verifies) a CIDR.
	if s.inventory == nil {
		return nil, domain.Err(503, "dependency_unavailable", "inventory adapter is not configured")
	}
	snap, err := s.inventory.Snapshot(ctx, dom)
	if err != nil || !snap.Complete {
		slog.Warn("inventory snapshot unusable", "domain", dom.ID, "complete", snap.Complete, "reason", errString(err))
		return nil, domain.Err(503, "dependency_unavailable", "complete inventory snapshot is required")
	}
	obs, obsErr := s.observe(ctx, dom)
	if obsErr != nil {
		return nil, domain.Err(503, "dependency_unavailable", "complete cloud observation is required")
	}
	now := s.now().UTC()
	planningObs := domain.Observation{}
	if obs != nil {
		planningObs = *obs
	}
	if obs != nil && (!obs.Complete || obs.Generation != dom.CoverageGeneration || !nowFresh(now, obs.FinishedAt, s.cfg.Lifecycle.MaxObservationAge)) {
		return nil, domain.Err(503, "dependency_unavailable", "complete current cloud observation is required")
	}
	if obs == nil {
		var trusted bool
		if err := s.ledger.View(ctx, func(st *domain.State) error {
			rows := st.Observations[dom.ID]
			if len(rows) > 0 {
				planningObs = rows[len(rows)-1]
				trusted = trustedObservation(planningObs, dom, s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge)
			}
			return nil
		}); err != nil || !trusted {
			return nil, domain.Err(503, "dependency_unavailable", "complete cloud observation is required")
		}
	}

	// The remaining guards -- domain_busy, quota, parent, and pinnedCIDR's
	// verification -- run inside reserve's write transaction because they
	// must be atomic with persisting the hold. A plan is advisory, not a
	// reservation of the outcome: it reads the same state through a View
	// instead, which can never race with the ledger's own writers to produce
	// a wrong answer, only a stale one that a concurrent write since this
	// call has overtaken -- exactly the same staleness apply's own re-run of
	// plan before it writes is there to catch.
	err = s.ledger.View(ctx, func(st *domain.State) error {
		if busy := pendingDomain(st, dom.ID); busy != nil {
			return apiErr(503, "domain_busy", "an unresolved operation is fencing this overlap domain")
		}
		if pool.MaxAllocations > 0 && countTenant(st, p.TenantID, pool.ID) >= pool.MaxAllocations {
			return apiErr(409, "quota_exceeded", "allocation quota has been reached")
		}
		if r.Scope == "subnet" {
			parent := st.Allocations[r.ParentAllocationID]
			if parent.ID == "" || parent.TenantID != p.TenantID || parent.Scope != "vpc" || (parent.State != domain.Reserved && parent.State != domain.Active) || parent.AccountID != r.AccountID || parent.Region != r.Region {
				return apiErr(409, "invalid_parent", "parent allocation is unavailable or does not match the target")
			}
			if s.cfg.SubnetPolicy.MaxChildren > 0 && countChildren(st, parent.ID) >= s.cfg.SubnetPolicy.MaxChildren {
				return apiErr(409, "quota_exceeded", "parent child quota has been reached")
			}
		}
		latest := planningObs
		if rows := st.Observations[dom.ID]; len(rows) > 0 {
			candidate := rows[len(rows)-1]
			if !candidate.FinishedAt.Before(latest.FinishedAt) {
				latest = candidate
			}
		}
		if !trustedObservation(latest, dom, s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) {
			return apiErr(503, "dependency_unavailable", "complete current cloud observation is required")
		}
		verified, _, pinErr := pinnedCIDR(*pool, r, snap, latest, st, pin)
		if pinErr != nil {
			return pinErr
		}
		// Built field by field rather than as a domain.Allocation{...} literal
		// on purpose: this value is never persisted and never Committed, but
		// TestCommittedAllocationsComeIntoBeingInThreePlaces (adopt_test.go)
		// counts every composite literal of that type as a construction site,
		// and a plan verdict is not one of the three the ADR enumerates.
		var a domain.Allocation
		a.Request = r
		a.TenantID = p.TenantID
		a.DomainID = dom.ID
		a.RequestHash = immutableHash
		a.CIDR = verified
		a.PoolID = pool.ID
		a.State = domain.Reserved
		a.PolicyVersion = s.cfg.PolicyVersion
		verdict = &AdoptionVerdict{Allocation: a, Existing: false, Pending: false}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return verdict, nil
}
