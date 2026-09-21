package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

const (
	operationPending   = "PENDING"
	operationSucceeded = "SUCCEEDED"
	operationFailed    = "FAILED"
	reserveOperation   = "RESERVE"
	adoptOperation     = "ADOPT"
	bindOperation      = "VERIFY_BINDING"
	reclaimOperation   = "RECLAIM"
)

// Adoption is the reviewed record that pins one existing network to an
// allocation (ADR 0010). It reaches the reservation body as a pointer on an
// unexported function with no transport binding, so a consumer request cannot
// pin: neither domain.Request nor the HTTP body can produce this value, which
// is a compile-time fact rather than a test.
type Adoption struct {
	// Operator is the subject of the person running the adoption. It is the
	// actor on both audit events, in place of the tenant principal the process
	// acts as; the service cannot verify it (ADR 0010, consequences).
	Operator string
	// CIDR is the exact network the operator reviewed. It replaces the choice
	// chooseCIDR would make, and nothing else: every rule that rejects a chosen
	// candidate still rejects this one.
	CIDR string
	// NetworkID is the inventory id of the imported occupancy at that CIDR --
	// the one object the snapshot overlap exemption names.
	NetworkID string
	// ResourceID is the cloud resource id at that CIDR -- the one object the
	// observation overlap exemption names.
	ResourceID string
}

// reservationKind carries the names that separate a reservation from an
// adoption. Adoption records its idempotency under a method and path of its
// own so it can never occupy the record the owning team's Idempotency-Key
// will need for their first POST.
type reservationKind struct{ operation, planned, committed, method, path, stuck string }

var (
	reserveKind = reservationKind{reserveOperation, "RESERVE_PLANNED", "RESERVE_COMMITTED", "POST", "POST /v1/allocations", reservationStuckCode}
	adoptKind   = reservationKind{adoptOperation, "ADOPT_PLANNED", "ADOPT_COMMITTED", "ADOPT", "ADOPT /v1/allocations", adoptionStuckCode}
)

type Service struct {
	cfg       domain.Config
	ledger    domain.Ledger
	inventory domain.Inventory
	observer  domain.Observer
	now       func() time.Time
}

func New(cfg domain.Config, ledger domain.Ledger, inventory domain.Inventory, observer domain.Observer) *Service {
	return &Service{cfg: cfg, ledger: ledger, inventory: inventory, observer: observer, now: time.Now}
}

// SetClock is intended for deterministic unit tests and does not affect the
// ledger's timestamps in another process.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Reserve(ctx context.Context, p domain.Principal, req domain.Request, key string) (*domain.Allocation, *domain.Operation, int, error) {
	return s.reserve(ctx, p, req, key, nil)
}

// Adopt gives an existing network an owner by running the reservation body
// with the operator's reviewed candidate pinned (ADR 0010). It is deliberately
// not an endpoint and not a client verb: it acts as p's tenant without that
// tenant's authentication, so it exists only for the operator process mode,
// and the audit trail names operator rather than the tenant principal.
//
// The reviewed record is the whole request. Description and labels are dropped
// here rather than hashed: requestHash excludes them, and dropping them makes
// a re-run of the same adoption a replay whatever the operator's notes say.
func (s *Service) Adopt(ctx context.Context, p domain.Principal, req domain.Request, pin Adoption) (*domain.Allocation, *domain.Operation, int, error) {
	if pin.Operator == "" {
		return nil, nil, 0, apiErr(422, "invalid_request", "an operator subject is required for the adoption audit trail")
	}
	if pin.CIDR == "" || pin.NetworkID == "" || pin.ResourceID == "" {
		return nil, nil, 0, apiErr(422, "invalid_request", "adoption requires a reviewed CIDR, inventory id and resource id")
	}
	if req.AllocationKey == "" {
		return nil, nil, 0, apiErr(422, "invalid_request", "allocation_key is invalid")
	}
	req.Description, req.Labels = "", nil
	// A key that already names an allocation is replayed by the body, and the
	// request hash cannot tell a replay of this adoption from a replay of
	// something else: a consumer request carries no CIDR. The comparison with
	// the reviewed record therefore happens here, on both sides of the body --
	// before, so that a refusal writes nothing, and after, because a consumer's
	// reservation can land between this read and the body's transaction.
	if err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, old := range st.Allocations {
			if old.TenantID == p.TenantID && old.AllocationKey == req.AllocationKey {
				return adoptedAs(old, pin)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, 0, err
	}
	// The allocation key is the adoption's idempotency key: a second apply over
	// the same reviewed record must converge on the same allocation, and under
	// adoptKind's method and path it cannot collide with the consumer's.
	a, op, status, err := s.reserve(ctx, p, req, req.AllocationKey, &pin)
	if err == nil && a != nil {
		if err := adoptedAs(*a, pin); err != nil {
			return nil, nil, 0, err
		}
	}
	return a, op, status, err
}

// adoptedAs refuses an allocation that is not the adoption of the reviewed
// record: another CIDR, or a commit on another inventory object. Retired and
// conflicting keys are left to the body, which already answers them.
func adoptedAs(a domain.Allocation, pin Adoption) error {
	if a.State == domain.Released || a.State == domain.Quarantined {
		return nil
	}
	reviewed, err := netip.ParsePrefix(pin.CIDR)
	if err != nil || a.CIDR != reviewed.Masked().String() {
		return apiErr(409, "adoption_conflict", "the allocation key already holds a different CIDR than the one reviewed")
	}
	if a.Committed && a.InventoryID != pin.NetworkID {
		return apiErr(409, "adoption_conflict", "the allocation key is already committed on a different inventory object than the one reviewed")
	}
	return nil
}

func (s *Service) reserve(ctx context.Context, p domain.Principal, req domain.Request, key string, pin *Adoption) (*domain.Allocation, *domain.Operation, int, error) {
	// The pin substitutes three things and nothing else: the candidate's
	// selection becomes its verification, Ensure becomes Adopt, and the
	// operation and audit names say ADOPT with the operator as actor. Every
	// other gate below is the same code for both paths, which is what keeps
	// safety_contract_test.go meaningful for adoption (ADR 0010).
	kind, actor, commitActor := reserveKind, p.Subject, ""
	if pin != nil {
		kind, actor, commitActor = adoptKind, pin.Operator, pin.Operator
	}
	if key == "" {
		return nil, nil, 0, apiErr(400, "idempotency_key_required", "idempotency key is required")
	}
	if err := s.ledgerReady(ctx); err != nil {
		return nil, nil, 0, err
	}
	r, pool, dom, err := s.validateRequest(ctx, p, req)
	if err != nil {
		return nil, nil, 0, err
	}
	immutableHash := requestHash(r)
	httpHash := requestBodyHash(r)
	path := kind.path
	requestID := idempotencyID(p.TenantID, kind.method, path, key)
	// Idempotent recovery is ledger-only. A replay of a committed response must
	// remain available during a NetBox or observation outage.
	var priorAllocation *domain.Allocation
	var priorOperation *domain.Operation
	var priorStatus int
	if readErr := s.ledger.View(ctx, func(st *domain.State) error {
		if old, ok := st.Requests[requestID]; ok {
			if old.Hash != httpHash {
				return apiErr(409, "idempotency_mismatch", "the idempotency key was used with a different request")
			}
			priorAllocation, priorOperation = stateResults(st, old)
			if priorAllocation != nil && (priorAllocation.State == domain.Quarantined || priorAllocation.State == domain.Released) {
				return apiErr(409, "allocation_key_retired", "allocation key is permanently retired")
			}
			// An abandon between its fence and its delete leaves an uncommitted
			// hold with no pending operation, which every replay path here
			// would otherwise report as 202 (ADR 0012, package H2b).
			if err := abandoningReplay(st, priorAllocation); err != nil {
				return err
			}
			priorStatus = resultStatus(priorAllocation, priorOperation)
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
			priorAllocation = copyAllocation(old)
			if err := abandoningReplay(st, priorAllocation); err != nil {
				return err
			}
			if !old.Committed {
				if op := pendingForAllocation(st, old.ID); op != nil {
					priorOperation = copyOperation(*op)
				}
			}
			priorStatus = resultStatus(priorAllocation, priorOperation)
			return nil
		}
		return nil
	}); readErr != nil {
		return nil, nil, 0, readErr
	}
	if priorAllocation != nil {
		return priorAllocation, priorOperation, priorStatus, nil
	}
	// Reads are deliberately outside the ledger transaction. The transaction
	// below rechecks all durable holds and persists the exact candidate before
	// Ensure is called.
	var snap domain.InventorySnapshot
	if s.inventory == nil {
		return nil, nil, 0, domain.Err(503, "dependency_unavailable", "inventory adapter is not configured")
	}
	snap, err = s.inventory.Snapshot(ctx, dom)
	if err != nil || !snap.Complete {
		// The response stays generic so adapter detail never reaches a
		// consumer, but an operator cannot act on a bare 503: record why the
		// snapshot was unusable.
		slog.Warn("inventory snapshot unusable", "domain", dom.ID, "complete", snap.Complete, "reason", errString(err))
		return nil, nil, 0, domain.Err(503, "dependency_unavailable", "complete inventory snapshot is required")
	}
	obs, obsErr := s.observe(ctx, dom)
	if obsErr != nil {
		return nil, nil, 0, domain.Err(503, "dependency_unavailable", "complete cloud observation is required")
	}
	now := s.now().UTC()
	planningObs := domain.Observation{}
	if obs != nil {
		planningObs = *obs
	}
	if obs != nil && (!obs.Complete || obs.Generation != dom.CoverageGeneration || !nowFresh(now, obs.FinishedAt, s.cfg.Lifecycle.MaxObservationAge)) {
		return nil, nil, 0, domain.Err(503, "dependency_unavailable", "complete current cloud observation is required")
	}
	if obs == nil {
		var trusted bool
		if readErr := s.ledger.View(ctx, func(st *domain.State) error {
			rows := st.Observations[dom.ID]
			if len(rows) > 0 {
				planningObs = rows[len(rows)-1]
				trusted = trustedObservation(planningObs, dom, s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge)
			}
			return nil
		}); readErr != nil || !trusted {
			return nil, nil, 0, domain.Err(503, "dependency_unavailable", "complete cloud observation is required")
		}
	}
	var allocation *domain.Allocation
	var operation *domain.Operation
	var evidence string
	status := 201
	attemptEnsure := false
	err = s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		if old, ok := st.Requests[requestID]; ok {
			if old.Hash != httpHash {
				return apiErr(409, "idempotency_mismatch", "the idempotency key was used with a different request")
			}
			allocation, operation = stateResults(st, old)
			if allocation != nil && (allocation.State == domain.Quarantined || allocation.State == domain.Released) {
				return apiErr(409, "allocation_key_retired", "allocation key is permanently retired")
			}
			if err := abandoningReplay(st, allocation); err != nil {
				return err
			}
			status = resultStatus(allocation, operation)
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
			// Asked before the idempotency record is written, so a request
			// refused by the abandon window leaves the ledger untouched.
			if err := abandoningReplay(st, copyAllocation(old)); err != nil {
				return err
			}
			st.Requests[requestID] = domain.Idempotency{TenantID: p.TenantID, Method: kind.method, Path: path, Key: key, Hash: httpHash, AllocationID: old.ID}
			allocation = copyAllocation(old)
			if !old.Committed {
				if op := pendingForAllocation(st, old.ID); op != nil {
					operation = copyOperation(*op)
				}
				status = 202
			} else {
				status = 200
			}
			return nil
		}
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
		var candidate, reason string
		var record *domain.AdoptionRecord
		if pin == nil {
			chosen, ok := chooseCIDR(*pool, r, snap, latest, st)
			if !ok {
				return apiErr(409, "pool_exhausted", "no eligible CIDR is available")
			}
			candidate, reason = chosen, "exact candidate held before inventory mutation"
		} else {
			verified, reviewed, pinErr := pinnedCIDR(*pool, r, snap, latest, st, *pin)
			if pinErr != nil {
				return pinErr
			}
			// The reviewed record becomes durable here, in the transaction that
			// persists the hold it justifies. The pin itself lives in the
			// operator's process, which a crash takes with it, and the worker
			// that finishes the adoption has to repeat the comparison below
			// against the object an operator reviewed rather than against
			// whatever the adapter answers (ADR 0010).
			record = &domain.AdoptionRecord{Operator: pin.Operator, NetworkID: pin.NetworkID, ResourceID: pin.ResourceID, ImportBatch: reviewed.ImportBatch, PriorStatus: reviewed.Status, PriorAccountID: reviewed.AWSAccountID, PriorRegion: reviewed.AWSRegion}
			candidate, reason = verified, adoptionEvidence(*record, latest)
			evidence = reason
		}
		a := domain.Allocation{Request: r, ID: domain.NewID("alloc"), TenantID: p.TenantID, DomainID: dom.ID, RequestHash: immutableHash, CIDR: candidate, PoolID: pool.ID, State: domain.Reserved, Revision: 1, PolicyVersion: s.cfg.PolicyVersion, InventorySync: "PENDING", CreatedAt: now, UpdatedAt: now, Committed: false, ReleaseBlockers: []string{}}
		op := domain.Operation{ID: domain.NewID("op"), Type: kind.operation, Status: operationPending, AllocationID: a.ID, TenantID: p.TenantID, DomainID: dom.ID, Adoption: record, CreatedAt: now, UpdatedAt: now}
		st.Allocations[a.ID] = a
		st.Operations[op.ID] = op
		st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: a.ID, TenantID: p.TenantID, Actor: actor, Action: kind.planned, Reason: reason, At: now, Revision: a.Revision})
		st.Requests[requestID] = domain.Idempotency{TenantID: p.TenantID, Method: kind.method, Path: path, Key: key, Hash: httpHash, AllocationID: a.ID, OperationID: op.ID}
		if obs != nil {
			recordObservation(st, *obs)
		}
		allocation = copyAllocation(a)
		operation = copyOperation(op)
		status = 202
		attemptEnsure = true
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if allocation == nil {
		return nil, nil, 0, apiErr(503, "ledger_error", "ledger returned no result")
	}
	if allocation.Committed {
		return allocation, operation, status, nil
	}
	// Existing pending operations are recovered by Tick. The first caller that
	// owns a newly persisted reservation may attempt the exact external create.
	if attemptEnsure && operation != nil && operation.Type == kind.operation {
		var invID string
		var ensureErr error
		if pin == nil {
			invID, ensureErr = s.inventory.Ensure(ctx, *allocation, operation.ID)
		} else {
			invID, ensureErr = s.inventory.Adopt(ctx, *allocation, operation.ID)
			// Where it is not the reviewed object, the write has already
			// happened: refuse to commit and leave the operation pending for a
			// person (ADR 0010, no undo). The question is asked of the durable
			// record rather than of the pin, so the worker recovering this
			// operation after a crash asks exactly the same one.
			if ensureErr == nil && invID != "" && !adoptedTheReviewedObject(*operation, invID) {
				return nil, nil, 0, apiErr(409, "adoption_conflict", "the adopted network is not the inventory object that was reviewed; the adoption remains pending")
			}
		}
		if ensureErr == nil && invID != "" {
			commitErr := s.ledger.Update(ctx, func(st *domain.State) error {
				a := st.Allocations[allocation.ID]
				if a.ID == "" {
					// A withdrawal can remove the hold between this request's
					// adapter call and this transaction: a consumer's cancel of
					// a stuck reservation (ADR 0013) is the only thing that
					// deletes an uncommitted RESERVE row. The operation it
					// fenced is still here and says so, so carry it out for the
					// answer below rather than reporting a pending operation
					// that no longer exists.
					if fenced, ok := st.Operations[operation.ID]; ok {
						operation = copyOperation(fenced)
					}
					return apiErr(404, "not_found", "allocation not found")
				}
				o := st.Operations[operation.ID]
				if a.Committed || a.State == domain.Quarantined || a.State == domain.Released || o.Status != operationPending {
					allocation = copyAllocation(a)
					operation = copyOperation(o)
					return nil
				}
				a.Committed = true
				a.InventoryID = invID
				a.InventorySync = "CURRENT"
				a.UpdatedAt = s.now().UTC()
				st.Allocations[a.ID] = a
				commitReason := "inventory exact create confirmed"
				if pin != nil {
					commitReason = "inventory adoption confirmed; " + evidence
				}
				st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: a.ID, TenantID: a.TenantID, Actor: commitActor, Action: kind.committed, Reason: commitReason, At: a.UpdatedAt, Revision: a.Revision})
				if o.ID != "" {
					o.Status = operationSucceeded
					o.Result = map[string]string{"allocation_id": a.ID}
					o.UpdatedAt = a.UpdatedAt
					st.Operations[o.ID] = o
				}
				// The worker's own commit resolves a stuck-hold finding through
				// commitRecovered (worker.go); this is the other place a pending
				// hold can finish. api, worker and adopt are independent
				// processes sharing one ledger, so a worker pass can flag this
				// exact pending operation between this request's own persist
				// and its own adapter call -- the worker's attempt meets a 412
				// because this request's write landed first, say -- and left
				// unresolved here that CRITICAL would never close:
				// reconcileAllocations does not reset either code, and the
				// worker resolves them only from its own recovery path. The
				// code is one more of the names a reservation and an adoption
				// differ by (package H4 found it for RESERVE; the review made
				// it hold for ADOPT too).
				resolveFinding(st, a, kind.stuck)
				*allocation = a
				operation = copyOperation(o)
				return nil
			})
			if commitErr == nil && allocation.Committed && allocation.State != domain.Quarantined && allocation.State != domain.Released {
				return allocation, operation, 201, nil
			}
			// The commit closure re-checks the operation's status under the
			// ledger lock and becomes a no-op where a withdrawal fenced it
			// while this request was inside its adapter call. Falling through
			// to the constant 202 would hand the caller a PendingOperation
			// schema over an operation that is already FAILED, and would tell
			// them their hold is being worked on when it is being withdrawn
			// (ADR 0013). Answer from the operation's real status instead; it
			// is the same window abandoningReplay answers for a replay, and it
			// is retryable, because the key is free once the withdrawal is over.
			if operation != nil {
				if refusal := withdrawalRefusal(*operation); refusal != nil {
					return nil, nil, 0, refusal
				}
			}
		}
		return allocation, operation, 202, nil
	}
	return allocation, operation, 202, nil
}

func (s *Service) Get(ctx context.Context, p domain.Principal, id string) (domain.Allocation, error) {
	var out domain.Allocation
	err := s.ledger.View(ctx, func(st *domain.State) error {
		a, ok := st.Allocations[id]
		// An unknown id and another tenant's allocation share one answer,
		// unchanged, decided before committed/uncommitted is even considered:
		// docs/API_V1.md section 1 requires an unauthorized id to be
		// indistinguishable from an unknown one, and ADR 0011 stage two drops
		// only the tenant comparison for an operator.
		if !ok || (!p.IsOperator() && a.TenantID != p.TenantID) {
			return apiErr(404, "not_found", "allocation not found")
		}
		if a.Committed {
			out = a
			return nil
		}
		// The hold is uncommitted. ADR 0011: an operator never sees it, exactly
		// as before -- do not leak it through a different status than the
		// tenant that holds it would get.
		if p.IsOperator() {
			return apiErr(404, "not_found", "allocation not found")
		}
		// E3 (docs/API_V1.md section 5; the mismatch H8d's review found):
		// docs/API_V1.md promises that "for an uncommitted known operation"
		// this read "returns 409 allocation_pending with its operation ID, not
		// a misleading 404". pendingForAllocation names the one operation that
		// promise is about: a still-PENDING RESERVE or ADOPT for this
		// allocation. Anything else uncommitted -- no operation at all, or one
		// left FAILED by a cancel's or an abandon's fence window (ADR 0012,
		// ADR 0013) -- carries no promise this document makes and stays 404,
		// exactly as it always has.
		if op := pendingForAllocation(st, id); op != nil {
			return pendingAllocationError(op.ID)
		}
		return apiErr(404, "not_found", "allocation not found")
	})
	return out, err
}

// pendingAllocationError is the 409 docs/API_V1.md section 5 promises for
// Get, above. The document names no exact JSON shape for it, so this is
// that shape -- api/openapi.yaml's Conflict response documents it to match,
// word for word: code allocation_pending, retryable (the caller should read
// the named operation and, once it resolves, retry the same read), and the
// operation id under details.operation_id.
func pendingAllocationError(operationID string) error {
	err := domain.Err(409, "allocation_pending", "The allocation is not yet committed; its reservation is still being worked on. Read the named operation.")
	err.Retryable = true
	err.Details = map[string]any{"operation_id": operationID}
	return err
}

func (s *Service) List(ctx context.Context, p domain.Principal) ([]domain.Allocation, error) {
	out := []domain.Allocation{}
	err := s.ledger.View(ctx, func(st *domain.State) error {
		for _, a := range st.Allocations {
			// ADR 0011 stage two: an operator lists committed allocations across
			// tenants. `a.Committed` is unchanged for the reason given in Get,
			// and the ordering below is on the id alone, so widening the filter
			// widens the page and changes nothing about how it is cut.
			if (p.IsOperator() || a.TenantID == p.TenantID) && a.Committed {
				out = append(out, a)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

func (s *Service) Patch(ctx context.Context, p domain.Principal, id string, description *string, labels *map[string]string, etag int64, key string) (domain.Allocation, error) {
	var out domain.Allocation
	if key == "" {
		return out, apiErr(400, "idempotency_key_required", "idempotency key is required")
	}
	hash := patchHash(id, description, labels, etag)
	err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		rid := idempotencyID(p.TenantID, "PATCH", "/v1/allocations/"+id, key)
		if old, ok := st.Requests[rid]; ok {
			if old.Hash != hash {
				return apiErr(409, "idempotency_mismatch", "the idempotency key was used with a different request")
			}
			a := st.Allocations[id]
			if a.ID == "" || a.TenantID != p.TenantID || !a.Committed {
				return apiErr(404, "not_found", "allocation not found")
			}
			out = a
			return nil
		}
		a, ok := st.Allocations[id]
		if !ok || a.TenantID != p.TenantID || !a.Committed {
			return apiErr(404, "not_found", "allocation not found")
		}
		if a.State == domain.Quarantined || a.State == domain.Released {
			return apiErr(409, "allocation_key_retired", "released allocations cannot be changed")
		}
		if a.Revision != etag {
			return apiErr(412, "revision_conflict", "allocation revision does not match If-Match")
		}
		if description == nil && labels == nil {
			return apiErr(422, "invalid_request", "patch must include description or labels")
		}
		if description != nil {
			if len(*description) > 512 {
				return apiErr(422, "invalid_request", "description is too long")
			}
			a.Description = *description
		}
		if labels != nil {
			if err := validateLabels(*labels); err != nil {
				return err
			}
			a.Labels = cloneLabels(*labels)
		}
		a.Revision++
		a.UpdatedAt = s.now().UTC()
		st.Allocations[id] = a
		st.Requests[rid] = domain.Idempotency{TenantID: p.TenantID, Method: "PATCH", Path: "/v1/allocations/" + id, Key: key, Hash: hash, AllocationID: id}
		st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: id, TenantID: p.TenantID, Actor: p.Subject, Action: "PATCH", Reason: "mutable metadata updated", At: a.UpdatedAt, Revision: a.Revision})
		out = a
		return nil
	})
	return out, err
}

func (s *Service) Bind(ctx context.Context, p domain.Principal, id string, b domain.Binding, key string) (*domain.Allocation, *domain.Operation, int, error) {
	var out *domain.Allocation
	var op *domain.Operation
	var allocationKey string
	var expectedCIDR, expectedAZ, expectedParent string
	if key == "" {
		return nil, nil, 0, apiErr(400, "idempotency_key_required", "idempotency key is required")
	}
	hash := bindingHash(id, b)
	rid := idempotencyID(p.TenantID, "PUT", "/v1/allocations/"+id+"/binding", key)
	obsByDomain := map[string]domain.Observation{}
	err := s.ledger.View(ctx, func(st *domain.State) error {
		a, ok := st.Allocations[id]
		if !ok || a.TenantID != p.TenantID || !a.Committed {
			return apiErr(404, "not_found", "allocation not found")
		}
		allocationKey = a.AllocationKey
		expectedCIDR, expectedAZ = a.CIDR, a.AvailabilityZoneID
		if a.ParentAllocationID != "" {
			if parent, ok := st.Allocations[a.ParentAllocationID]; ok && parent.Binding != nil {
				expectedParent = parent.Binding.ResourceID
			}
		}
		if b.Provider != "aws" || b.ResourceType != a.Scope || b.ResourceID == "" || b.AccountID != a.AccountID || b.Region != a.Region {
			return apiErr(422, "invalid_request", "binding candidate does not match the allocation target")
		}
		if a.Binding != nil && sameBinding(*a.Binding, b) {
			x := a
			out = &x
			return nil
		}
		if a.Binding != nil {
			return apiErr(409, "binding_conflict", "allocation already has a different binding")
		}
		if x := pendingForAllocation(st, id); x != nil {
			if x.Type == bindOperation && x.Candidate != nil && sameBinding(*x.Candidate, b) {
				y := *x
				op = &y
				return nil
			}
			if x.Type == bindOperation {
				return apiErr(409, "binding_conflict", "a different binding verification is already pending")
			}
		}
		if os := st.Observations[a.DomainID]; len(os) > 0 {
			obsByDomain[a.DomainID] = os[len(os)-1]
		}
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if out != nil {
		return out, nil, 200, nil
	}
	now := s.now().UTC()
	err = s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		a, ok := st.Allocations[id]
		if !ok || a.TenantID != p.TenantID || !a.Committed {
			return apiErr(404, "not_found", "allocation not found")
		}
		if a.State == domain.Quarantined || a.State == domain.Released {
			return apiErr(409, "allocation_key_retired", "allocation cannot be rebound")
		}
		if old, ok := st.Requests[rid]; ok {
			if old.Hash != hash {
				return apiErr(409, "idempotency_mismatch", "the idempotency key was used with a different request")
			}
			if x := pendingForAllocation(st, id); x != nil {
				y := *x
				op = &y
			}
			return nil
		}
		if x := pendingForAllocation(st, id); x != nil {
			y := *x
			op = &y
			return nil
		}
		o := domain.Operation{ID: domain.NewID("op"), Type: bindOperation, Status: operationPending, AllocationID: id, TenantID: p.TenantID, DomainID: a.DomainID, Candidate: &b, CreatedAt: now, UpdatedAt: now}
		st.Operations[o.ID] = o
		st.Requests[rid] = domain.Idempotency{TenantID: p.TenantID, Method: "PUT", Path: "/v1/allocations/" + id + "/binding", Key: key, Hash: hash, AllocationID: id, OperationID: o.ID}
		op = &o
		return nil
	})
	if err != nil {
		return nil, nil, 0, err
	}
	if op == nil {
		return nil, nil, 202, apiErr(503, "ledger_error", "binding operation was not persisted")
	}
	if obs, ok := obsByDomain[op.DomainID]; ok && trustedObservation(obs, domainFor(s.cfg, op.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) && bindingObserved(obs, b, id, allocationKey, expectedCIDR, expectedAZ, expectedParent) {
		if latest, e := s.observe(ctx, domainFor(s.cfg, op.DomainID)); e != nil || latest == nil || !trustedObservation(*latest, domainFor(s.cfg, op.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) || !bindingObserved(*latest, b, id, allocationKey, expectedCIDR, expectedAZ, expectedParent) {
			return nil, op, 202, nil
		}
		return s.finishBinding(ctx, id, *op, b, 200)
	}
	if s.observer != nil {
		if obs, e := s.observe(ctx, domainFor(s.cfg, op.DomainID)); e == nil && obs != nil && trustedObservation(*obs, domainFor(s.cfg, op.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) && bindingObserved(*obs, b, id, allocationKey, expectedCIDR, expectedAZ, expectedParent) {
			latest, e := s.observe(ctx, domainFor(s.cfg, op.DomainID))
			if e != nil || latest == nil || !trustedObservation(*latest, domainFor(s.cfg, op.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) || !bindingObserved(*latest, b, id, allocationKey, expectedCIDR, expectedAZ, expectedParent) {
				return nil, op, 202, nil
			}
			return s.finishBinding(ctx, id, *op, b, 200)
		}
	}
	return nil, op, 202, nil
}

func (s *Service) finishBinding(ctx context.Context, id string, op domain.Operation, b domain.Binding, status int) (*domain.Allocation, *domain.Operation, int, error) {
	now := s.now().UTC()
	b.VerifiedAt = &now
	var a domain.Allocation
	err := s.ledger.Update(ctx, func(st *domain.State) error {
		x, ok := st.Allocations[id]
		if !ok {
			return apiErr(404, "not_found", "allocation not found")
		}
		if x.State != domain.Reserved || x.Binding != nil {
			return apiErr(409, "binding_conflict", "allocation is no longer available for binding")
		}
		o := st.Operations[op.ID]
		if o.Status != operationPending || o.Candidate == nil || !sameBinding(*o.Candidate, b) {
			return apiErr(409, "binding_conflict", "binding operation is no longer current")
		}
		if x.Scope == "subnet" {
			parent, ok := st.Allocations[x.ParentAllocationID]
			if !ok || parent.State != domain.Active || parent.Binding == nil {
				return apiErr(409, "invalid_parent", "parent binding is not verified")
			}
		}
		x.Binding = &b
		x.State = domain.Active
		x.InventorySync = "PENDING"
		x.Revision++
		x.UpdatedAt = now
		st.Allocations[id] = x
		o.Status = operationSucceeded
		o.Result = map[string]string{"allocation_id": id}
		o.UpdatedAt = now
		st.Operations[o.ID] = o
		a = x
		st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: id, TenantID: x.TenantID, Action: "BIND", Reason: "cloud binding verified", At: now, Revision: x.Revision})
		op = o
		return nil
	})
	return &a, &op, status, err
}

func (s *Service) Release(ctx context.Context, p domain.Principal, id string) (domain.Allocation, int, error) {
	var out domain.Allocation
	err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		a, ok := st.Allocations[id]
		if !ok || a.TenantID != p.TenantID || !a.Committed {
			return apiErr(404, "not_found", "allocation not found")
		}
		if a.State == domain.Released {
			out = a
			return nil
		}
		if a.State == domain.Quarantined {
			out = a
			return nil
		}
		for _, child := range st.Allocations {
			if child.ParentAllocationID == id && ((!child.Committed && child.State != domain.Released) || child.State == domain.Reserved || child.State == domain.Active || pendingForAllocation(st, child.ID) != nil) {
				return apiErr(409, "children_present", "allocation has unreleased children")
			}
		}
		now := s.now().UTC()
		hours := s.cfg.Lifecycle.QuarantineHours
		if hours <= 0 {
			hours = 24 * 7
		}
		until := now.Add(time.Duration(hours) * time.Hour)
		for opID, o := range st.Operations {
			if o.AllocationID == id && o.Status == operationPending && o.Type == bindOperation {
				o.Status = operationFailed
				o.Error = domain.Err(409, "allocation_key_retired", "Allocation was released during binding verification.")
				o.UpdatedAt = now
				st.Operations[opID] = o
			}
		}
		a.State = domain.Quarantined
		a.InventorySync = "PENDING"
		a.ReleaseRequestedAt = &now
		a.QuarantineUntil = &until
		a.UpdatedAt = now
		a.Revision++
		a.ReleaseBlockers = []string{"quarantine_not_elapsed"}
		st.Allocations[id] = a
		st.Events = append(st.Events, domain.Event{ID: domain.NewID("evt"), AllocationID: id, TenantID: p.TenantID, Actor: p.Subject, Action: "RELEASE_REQUESTED", Reason: "consumer release intent", At: now, Revision: a.Revision})
		out = a
		return nil
	})
	if err != nil {
		return out, 0, err
	}
	if out.State == domain.Released {
		return out, 204, nil
	}
	return out, 202, nil
}

func (s *Service) Operation(ctx context.Context, p domain.Principal, id string) (domain.Operation, error) {
	var out domain.Operation
	err := s.ledger.View(ctx, func(st *domain.State) error {
		o, ok := st.Operations[id]
		// GET /v1/operations/{id} is deliberately NOT granted to an operator in
		// v1 (ADR 0011): an operation id is discoverable only from a tenant's own
		// 202 or error payload, so the grant would buy an operator almost nothing
		// while widening the surface that has to keep answering 404 correctly. An
		// operator has no tenant, so this comparison refuses it unchanged.
		if !ok || o.TenantID != p.TenantID {
			return apiErr(404, "not_found", "operation not found")
		}
		out = o
		return nil
	})
	return out, err
}
func (s *Service) Pools(ctx context.Context, p domain.Principal) ([]domain.Pool, error) {
	out := []domain.Pool{}
	for _, pool := range s.cfg.Pools {
		// ADR 0011 stage two: an operator receives every configured pool, not an
		// eligible subset -- eligible_tenants answers "what may this tenant
		// reserve from", which is not the question an operator asks. Because the
		// answer is no longer empty, the transport's stage-one refusal
		// (403 no_eligible_pool, decided there on this full list) stops applying
		// to an operator without that handler changing.
		if p.IsOperator() || eligibleString(pool.EligibleTenants, p.TenantID) {
			out = append(out, pool)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Service) Capacity(ctx context.Context, p domain.Principal, poolID string) (map[string]any, error) {
	return s.capacity(ctx, p, poolID)
}
func (s *Service) Findings(ctx context.Context, p domain.Principal) ([]domain.Finding, error) {
	out := []domain.Finding{}
	err := s.ledger.View(ctx, func(st *domain.State) error {
		// ADR 0011 stage two grants an operator a domain-wide findings view, and
		// it cannot be this filter widened: the occupancy fan-out writes one row
		// per eligible tenant, so one unmanaged resource in a domain with two
		// eligible tenants is two rows whose public projections differ only in an
		// opaque id, and a cross-tenant list would show one problem twice with no
		// way for the reader to tell. operatorFindings is that view; a tenant's
		// own is the comparison below, untouched.
		if p.IsOperator() {
			out = operatorFindings(st)
			return nil
		}
		for _, f := range st.Findings {
			if f.TenantID == p.TenantID {
				out = append(out, f)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return nil
	})
	return out, err
}

// occupancyGroup is the key ADR 0011 names for the operator's view of a finding
// that names no allocation: the routing domain, the code, the account, the
// region and the cloud resource. It is the fan-out's own key
// (internal/service/worker.go) minus the recipient tenant, which is the only
// thing the fan-out adds, and the two have to stay in step -- if they drift
// apart the operator view silently starts double-counting again, which is why
// the two-eligible-tenants test exists.
type occupancyGroup struct {
	domainID     string
	code         string
	accountID    string
	region       string
	resourceType string
	resourceID   string
}

// domainLevelFinding reports a finding the operator view may collapse, and both
// halves of the condition matter. A finding that names an allocation is a fact
// about that allocation, which exactly one tenant holds, so there is nothing to
// collapse and it is returned as it stands -- including one whose allocation is
// an uncommitted hold, such as adoption_stuck, which is precisely the finding an
// operator exists to see. A finding with no resource identity carries nothing to
// group on: the fan-out key is a hash and is not reversible, so a row written
// before those fields existed and not yet refreshed by a worker pass has to be
// left alone, and collapsing such rows on the fields that remain -- code,
// account, region -- would merge two genuinely different resources observed in
// the same pass, which is the one mistake this view may not make.
func domainLevelFinding(f domain.Finding) bool {
	return f.AllocationID == "" && f.ResourceType != "" && f.ResourceID != ""
}

// operatorFindings is the whole of the findings grant: every finding in the
// ledger, with the fan-out's per-tenant duplicates of one domain-level fact
// collapsed into one row each. Ordering is by id over a set of ids that is
// unique by construction -- each stored finding belongs to exactly one group and
// each group is represented by one of its own members -- so the answer is the
// same on every call however the ledger's map iterates, which is what the
// transport's `id > cursor` paging needs to be stable between calls.
func operatorFindings(st *domain.State) []domain.Finding {
	out := []domain.Finding{}
	groups := map[occupancyGroup][]domain.Finding{}
	for _, f := range st.Findings {
		if !domainLevelFinding(f) {
			out = append(out, f)
			continue
		}
		key := occupancyGroup{f.DomainID, f.Code, f.AccountID, f.Region, f.ResourceType, f.ResourceID}
		groups[key] = append(groups[key], f)
	}
	for _, members := range groups {
		out = append(out, collapseOccupancy(members))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// collapseOccupancy reduces one group's rows to the single row an operator sees.
// The representative is the member with the lexicographically smallest id, so
// the id a cursor was cut on is still there on the next call (ADR 0011); the
// window is the union of the members' windows; and the status is OPEN if any
// member is, which is the only direction that is safe for a gate -- a group
// whose members disagree in any other way cannot make `--fail-if-open` pass.
// Severity is the highest any member carries.
//
// The row carries no tenant. Each member names one recipient of a fact about the
// estate, and a domain-level fact has no recipient: naming one of them here
// would present an accident of tenancy configuration as a property of the thing
// observed. What the response shows is package G3b4's decision.
func collapseOccupancy(members []domain.Finding) domain.Finding {
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	out := members[0]
	out.TenantID = ""
	for _, f := range members[1:] {
		if f.FirstObservedAt.Before(out.FirstObservedAt) {
			out.FirstObservedAt = f.FirstObservedAt
		}
		if f.LastObservedAt.After(out.LastObservedAt) {
			out.LastObservedAt = f.LastObservedAt
		}
		if findingSeverityRank(f.Severity) > findingSeverityRank(out.Severity) {
			out.Severity = f.Severity
		}
		if f.Status == "OPEN" {
			out.Status = "OPEN"
		}
	}
	return out
}

// findingSeverityRank orders the severity vocabulary api/openapi.yaml declares
// for a Finding. A value outside it ranks below all three on purpose: a grouped
// row must never report a severity this project cannot explain, and one that
// outranked CRITICAL would be a way to manufacture one.
func findingSeverityRank(severity string) int {
	switch severity {
	case "CRITICAL":
		return 3
	case "WARNING":
		return 2
	case "INFO":
		return 1
	default:
		return 0
	}
}

func (s *Service) Tick(ctx context.Context) error {
	if err := s.ledgerReady(ctx); err != nil {
		return err
	}
	if err := s.recoverReservations(ctx); err != nil {
		return err
	}
	// A crashed adoption is recovered by its own path: the reserve recovery
	// filters it out on purpose, and the operation it leaves behind fences the
	// whole overlap domain until one of the two finishes it (ADR 0010).
	if err := s.recoverAdoptions(ctx); err != nil {
		return err
	}
	// Observation is read-only and never occurs inside Ledger.Update.
	for _, d := range s.cfg.Domains {
		obs, err := s.observe(ctx, d)
		if updateErr := s.ledger.Update(ctx, func(st *domain.State) error {
			ensureState(st)
			if obs == nil {
				obs = &domain.Observation{DomainID: d.ID, Generation: d.CoverageGeneration, Complete: false, FinishedAt: s.now().UTC(), Error: errString(err)}
			}
			if err != nil {
				obs.Complete = false
				obs.Error = err.Error()
			}
			recordObservation(st, *obs)
			for id, a := range st.Allocations {
				if a.DomainID != d.ID || !a.Committed || a.State == domain.Released {
					continue
				}
				code := ""
				if !obs.Complete || obs.Generation != d.CoverageGeneration {
					code = "coverage_incomplete"
				}
				if code == "" {
					continue
				}
				fid := findingID(code, id)
				f := st.Findings[fid]
				if f.ID == "" {
					f = domain.Finding{ID: fid, TenantID: a.TenantID, DomainID: d.ID, AllocationID: id, Code: code, Severity: "WARNING", Status: "OPEN", FirstObservedAt: obs.FinishedAt}
				}
				f.LastObservedAt = obs.FinishedAt
				f.Status = "OPEN"
				st.Findings[fid] = f
			}
			return nil
		}); updateErr != nil && err == nil {
			err = updateErr
		}
		if err != nil {
			continue
		}
	}
	if err := s.reconcileAllocations(ctx); err != nil {
		return err
	}
	// Complete pending binding operations from the latest trusted observation.
	var bindingJobs []struct {
		a      domain.Allocation
		o      domain.Operation
		parent string
	}
	_ = s.ledger.View(ctx, func(st *domain.State) error {
		for _, o := range st.Operations {
			if o.Status != operationPending || o.Type != bindOperation || o.Candidate == nil {
				continue
			}
			a, ok := st.Allocations[o.AllocationID]
			if ok {
				parent := ""
				if p, present := st.Allocations[a.ParentAllocationID]; present && p.Binding != nil {
					parent = p.Binding.ResourceID
				}
				bindingJobs = append(bindingJobs, struct {
					a      domain.Allocation
					o      domain.Operation
					parent string
				}{a, o, parent})
			}
		}
		return nil
	})
	for _, job := range bindingJobs {
		if s.observer == nil {
			continue
		}
		obs, err := s.observe(ctx, domainFor(s.cfg, job.o.DomainID))
		if err == nil && obs != nil && trustedObservation(*obs, domainFor(s.cfg, job.o.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) && bindingObserved(*obs, *job.o.Candidate, job.a.ID, job.a.AllocationKey, job.a.CIDR, job.a.AvailabilityZoneID, job.parent) {
			_, _, _, _ = s.finishBinding(ctx, job.a.ID, job.o, *job.o.Candidate, 200)
		}
	}
	if err := s.syncProjections(ctx); err != nil {
		return err
	}
	return s.reclaimTick(ctx)
}

func (s *Service) reclaimTick(ctx context.Context) error {
	var jobs []struct {
		a domain.Allocation
		o domain.Operation
	}
	now := s.now().UTC()
	err := s.ledger.Update(ctx, func(st *domain.State) error {
		// Recover a deletion whose external response was lost. The same exact
		// allocation identity is retried; no new candidate is selected.
		for _, o := range st.Operations {
			if o.Type != reclaimOperation || o.Status != operationPending {
				continue
			}
			if a, ok := st.Allocations[o.AllocationID]; ok && a.State == domain.Quarantined {
				jobs = append(jobs, struct {
					a domain.Allocation
					o domain.Operation
				}{a, o})
			}
		}
		for _, a := range st.Allocations {
			if a.State != domain.Quarantined || !a.Committed || !reclaimEligible(st, a, s.cfg, now) {
				continue
			}
			o := domain.Operation{ID: domain.NewID("op"), Type: reclaimOperation, Status: operationPending, AllocationID: a.ID, TenantID: a.TenantID, DomainID: a.DomainID, CreatedAt: now, UpdatedAt: now}
			st.Operations[o.ID] = o
			a.ReleaseBlockers = []string{"pending_operation"}
			a.UpdatedAt = now
			st.Allocations[a.ID] = a
			workerEvent(st, a, "reclaim_planned", "Persisted exact inventory deletion intent.", now)
			jobs = append(jobs, struct {
				a domain.Allocation
				o domain.Operation
			}{a, o})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if s.inventory == nil {
			continue
		}
		var fenced bool
		var parentAWSID string
		if fenceErr := s.ledger.View(ctx, func(st *domain.State) error {
			fenced = reclaimFence(st, st.Allocations[job.a.ID], job.o.ID, s.cfg, s.now().UTC())
			if parent := st.Allocations[job.a.ParentAllocationID]; parent.Binding != nil {
				parentAWSID = parent.Binding.ResourceID
			}
			return nil
		}); fenceErr != nil || !fenced {
			continue
		}
		pre, preErr := s.inventory.Snapshot(ctx, domainFor(s.cfg, job.a.DomainID))
		if preErr != nil || !pre.Complete || snapshotConflict(job.a, pre) {
			continue
		}
		delErr := s.inventory.Delete(ctx, job.a)
		post, postErr := s.inventory.Snapshot(ctx, domainFor(s.cfg, job.a.DomainID))
		postObs, _ := s.observe(ctx, domainFor(s.cfg, job.a.DomainID))
		if delErr == nil && (postErr != nil || !post.Complete || snapshotConflict(job.a, post) || postObs == nil || !trustedObservation(*postObs, domainFor(s.cfg, job.a.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) || observationConflict(job.a, *postObs, parentAWSID)) {
			delErr = fmt.Errorf("post-delete verification is incomplete or still observes occupancy")
		}
		if updateErr := s.ledger.Update(ctx, func(st *domain.State) error {
			o := st.Operations[job.o.ID]
			a := st.Allocations[job.a.ID]
			if delErr != nil {
				o.UpdatedAt = s.now().UTC()
				a.ReleaseBlockers = []string{"post_delete_verification_pending"}
				if postObs != nil {
					recordObservation(st, *postObs)
				}
				st.Allocations[a.ID] = a
				st.Operations[o.ID] = o
				return nil
			}
			if !reclaimFence(st, a, job.o.ID, s.cfg, s.now().UTC()) || postObs == nil || !trustedObservation(*postObs, domainFor(s.cfg, a.DomainID), s.now().UTC(), s.cfg.Lifecycle.MaxObservationAge) || observationConflict(a, *postObs, parentAWSID) || postErr != nil || !post.Complete || snapshotConflict(a, post) {
				o.UpdatedAt = s.now().UTC()
				st.Operations[o.ID] = o
				a.ReleaseBlockers = []string{"post_delete_verification_pending"}
				st.Allocations[a.ID] = a
				if postObs != nil {
					recordObservation(st, *postObs)
				}
				return nil
			}
			a.State = domain.Released
			a.InventorySync = "CURRENT"
			a.ReleaseBlockers = nil
			a.UpdatedAt = s.now().UTC()
			a.Revision++
			st.Allocations[a.ID] = a
			o.Status = operationSucceeded
			o.Result = map[string]string{"allocation_id": a.ID}
			o.UpdatedAt = a.UpdatedAt
			st.Operations[o.ID] = o
			workerEvent(st, a, "released", "Inventory deletion and fresh absence confirmed.", a.UpdatedAt)
			return nil
		}); updateErr != nil {
			return updateErr
		}
	}
	return nil
}

func (s *Service) observe(ctx context.Context, d domain.Domain) (*domain.Observation, error) {
	if s.observer == nil {
		return nil, nil
	}
	if d.ID == "" || d.CoverageGeneration == "" || len(d.CloudCoverage) == 0 {
		return nil, fmt.Errorf("cloud coverage for domain %q is not configured", d.ID)
	}
	o, err := s.observer.Observe(ctx, d)
	if err != nil {
		return nil, err
	}
	if o.DomainID != d.ID || o.Generation != d.CoverageGeneration || o.FinishedAt.IsZero() || o.StartedAt.IsZero() || o.FinishedAt.Before(o.StartedAt) {
		return nil, fmt.Errorf("observer returned untrusted domain, generation, or timestamps")
	}
	return &o, nil
}

func trustedObservation(o domain.Observation, d domain.Domain, now time.Time, maxAge int) bool {
	return len(d.CloudCoverage) > 0 && o.DomainID == d.ID && o.Generation == d.CoverageGeneration && o.Complete && !o.StartedAt.IsZero() && !o.FinishedAt.IsZero() && !o.FinishedAt.Before(o.StartedAt) && nowFresh(now, o.FinishedAt, maxAge)
}

func snapshotConflict(a domain.Allocation, snap domain.InventorySnapshot) bool {
	cidr, err := netip.ParsePrefix(a.CIDR)
	if err != nil {
		return true
	}
	for _, n := range snap.Networks {
		q, e := netip.ParsePrefix(n.CIDR)
		if e != nil || !cidr.Overlaps(q) {
			continue
		}
		if n.ParentPool || n.AllocationID == a.ID {
			continue
		}
		if a.Scope == "subnet" && n.AllocationID == a.ParentAllocationID && q.Contains(cidr.Addr()) {
			continue
		}
		return true
	}
	return false
}
func observationConflict(a domain.Allocation, o domain.Observation, parentAWSID ...string) bool {
	cidr, err := netip.ParsePrefix(a.CIDR)
	if err != nil {
		return true
	}
	for _, r := range o.Resources {
		for _, raw := range append([]string{r.CIDR}, r.CIDRs...) {
			q, e := netip.ParsePrefix(raw)
			if e != nil || !cidr.Overlaps(q) {
				continue
			}
			if a.Scope == "subnet" && r.Type == "vpc" && len(parentAWSID) > 0 && parentAWSID[0] != "" && r.ID == parentAWSID[0] && r.AccountID == a.AccountID && r.Region == a.Region {
				continue
			}
			return true
		}
	}
	return false
}
func parentResourceMatches(st *domain.State, parentID, resourceID string) bool {
	if parentID == "" {
		return false
	}
	parent, ok := st.Allocations[parentID]
	return ok && parent.Binding != nil && parent.Binding.ResourceID == resourceID
}
func (s *Service) ledgerReady(ctx context.Context) error {
	if s.ledger == nil {
		return apiErr(503, "dependency_unavailable", "ledger is not configured")
	}
	if err := s.ledger.Ready(ctx); err != nil {
		return apiErr(503, "dependency_unavailable", "allocation ledger is unavailable")
	}
	return nil
}

func (s *Service) validateRequest(ctx context.Context, p domain.Principal, in domain.Request) (domain.Request, *domain.Pool, domain.Domain, error) {
	if p.TenantID == "" {
		return in, nil, domain.Domain{}, apiErr(403, "forbidden", "authenticated principal has no tenant")
	}
	r := in
	if r.AddressFamily == "" {
		r.AddressFamily = "ipv4"
	}
	if r.Scope != "vpc" && r.Scope != "subnet" {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "scope must be vpc or subnet")
	}
	if r.AddressFamily != "ipv4" || r.PrefixLength < 0 || r.PrefixLength > 32 {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "only valid IPv4 prefix lengths are supported")
	}
	if !validKey(r.AllocationKey) {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "allocation_key is invalid")
	}
	if len(r.Description) > 512 {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "description is too long")
	}
	if err := validateLabels(r.Labels); err != nil {
		return r, nil, domain.Domain{}, err
	}
	if r.Scope == "vpc" && (r.ParentAllocationID != "" || r.AvailabilityZoneID != "") {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "VPC requests cannot specify a parent or availability zone")
	}
	if r.Scope == "subnet" && r.ParentAllocationID == "" {
		return r, nil, domain.Domain{}, apiErr(422, "invalid_request", "subnet requests require a parent allocation")
	}
	if !eligibleString(p.Environments, r.Environment) || !eligibleString(p.Regions, r.Region) {
		return r, nil, domain.Domain{}, apiErr(403, "forbidden", "principal is not authorized for the target")
	}
	if r.AccountID == "" {
		if len(p.Accounts) != 1 {
			return r, nil, domain.Domain{}, apiErr(422, "account_required", "account must be specified for this principal")
		}
		r.AccountID = p.Accounts[0]
	} else if !eligibleString(p.Accounts, r.AccountID) {
		return r, nil, domain.Domain{}, apiErr(403, "forbidden", "principal is not authorized for the account")
	}
	var parent domain.Allocation
	if r.Scope == "subnet" {
		if s.ledger == nil {
			return r, nil, domain.Domain{}, apiErr(503, "dependency_unavailable", "ledger is not configured")
		}
		if err := s.ledger.View(ctx, func(st *domain.State) error {
			var ok bool
			parent, ok = st.Allocations[r.ParentAllocationID]
			if !ok || !parent.Committed || parent.TenantID != p.TenantID {
				return apiErr(409, "invalid_parent", "parent allocation is unavailable")
			}
			return nil
		}); err != nil {
			return r, nil, domain.Domain{}, err
		}
		requiredScope := s.cfg.SubnetPolicy.RequireParentScope
		if requiredScope == "" {
			requiredScope = "vpc"
		}
		if parent.Scope != requiredScope || (parent.State != domain.Reserved && parent.State != domain.Active) || parent.AccountID != r.AccountID || parent.Region != r.Region || parent.Environment != r.Environment {
			return r, nil, domain.Domain{}, apiErr(409, "invalid_parent", "parent allocation does not match the target")
		}
		if !containsInt(s.cfg.SubnetPolicy.AllowedPrefixLengths, r.PrefixLength) {
			return r, nil, domain.Domain{}, apiErr(422, "policy_violation", "subnet prefix length is not allowed")
		}
	}
	var selected *domain.Pool
	for i := range s.cfg.Pools {
		pool := &s.cfg.Pools[i]
		if pool.Environment == r.Environment && pool.Region == r.Region && pool.AddressFamily == r.AddressFamily && eligibleString(pool.EligibleTenants, p.TenantID) && eligibleString(pool.EligibleAccounts, r.AccountID) {
			candidate := pool
			if r.Scope == "subnet" {
				if pool.Scope != "vpc" || parent.PoolID != pool.ID {
					continue
				}
				copyPool := *pool
				copyPool.Scope = "subnet"
				copyPool.CIDR = parent.CIDR
				candidate = &copyPool
			} else if pool.Scope != r.Scope {
				continue
			}
			if containsInt(func() []int {
				if r.Scope == "subnet" {
					return s.cfg.SubnetPolicy.AllowedPrefixLengths
				}
				return candidate.AllowedPrefixLengths
			}(), r.PrefixLength) {
				if selected != nil {
					return r, nil, domain.Domain{}, apiErr(422, "ambiguous_pool", "more than one pool matches the request")
				}
				selected = candidate
			}
		}
	}
	if selected == nil {
		return r, nil, domain.Domain{}, apiErr(422, "policy_violation", "no eligible pool matches the request")
	}
	var dom domain.Domain
	for _, d := range s.cfg.Domains {
		if d.ID == selected.DomainID {
			dom = d
		}
	}
	if dom.ID == "" {
		return r, nil, domain.Domain{}, apiErr(422, "policy_violation", "pool references an unknown overlap domain")
	}
	return r, selected, dom, nil
}

func chooseCIDR(pool domain.Pool, r domain.Request, snap domain.InventorySnapshot, obs domain.Observation, st *domain.State) (string, bool) {
	base, err := netip.ParsePrefix(pool.CIDR)
	if err != nil {
		return "", false
	}
	base = base.Masked()
	bits := r.PrefixLength
	if bits < base.Bits() || bits > 32 {
		return "", false
	}
	step := uint64(1) << uint(bits-base.Bits())
	if bits == 0 {
		step = 1
	}
	baseInt := addrInt(base.Addr())
	for i := uint64(0); i < step; i++ {
		start := baseInt + (i << (uint(32 - bits)))
		p, ok := prefixFromInt(start, bits)
		if !ok {
			continue
		}
		if overlapsExcluded(p, pool.ExcludedCIDRs) {
			continue
		}
		blocked := false
		for _, n := range snap.Networks {
			q, e := netip.ParsePrefix(n.CIDR)
			if e == nil && p.Overlaps(q) {
				if n.ParentPool || r.Scope == "subnet" && n.AllocationID == r.ParentAllocationID && q.Contains(p.Addr()) {
					continue
				}
				blocked = true
				break
			}
		}
		for _, resource := range obs.Resources {
			for _, raw := range append([]string{resource.CIDR}, resource.CIDRs...) {
				q, e := netip.ParsePrefix(raw)
				if e != nil || !p.Overlaps(q) {
					continue
				}
				if r.Scope == "subnet" && resource.Type == "vpc" && parentResourceMatches(st, r.ParentAllocationID, resource.ID) && q.Contains(p.Addr()) {
					continue
				}
				blocked = true
				break
			}
			if blocked {
				break
			}
		}
		if blocked {
			continue
		}
		for _, a := range st.Allocations {
			if !a.Committed {
				q, e := netip.ParsePrefix(a.CIDR)
				if e == nil && p.Overlaps(q) {
					blocked = true
					break
				}
			}
			if a.Committed && a.State != domain.Released && a.DomainID == pool.DomainID {
				q, e := netip.ParsePrefix(a.CIDR)
				if e == nil && p.Overlaps(q) {
					if r.Scope == "subnet" && a.ID == r.ParentAllocationID && q.Contains(p.Addr()) {
						continue
					}
					blocked = true
					break
				}
			}
		}
		if !blocked {
			return p.String(), true
		}
	}
	return "", false
}

// pinnedCIDR verifies the operator's reviewed candidate where chooseCIDR would
// select one, and refuses rather than choosing anything else. Every rule that
// would reject a chosen candidate is repeated here; adoption adds three overlap
// exemptions and no more (ADR 0010, amended 2026-09-20). Two of them name
// exactly one object the operator reviewed: the imported network at exactly
// this CIDR, and the observed resource at it. The third is the reviewed VPC's
// own observed children, which rest on no operator input beyond the two ids
// already reviewed -- see reviewedVPCChildren. A second unmanaged network or a
// second resource that is not such a child is a refusal.
func pinnedCIDR(pool domain.Pool, r domain.Request, snap domain.InventorySnapshot, obs domain.Observation, st *domain.State, pin Adoption) (string, domain.Network, error) {
	var none domain.Network
	base, err := netip.ParsePrefix(pool.CIDR)
	if err != nil {
		return "", none, apiErr(422, "policy_violation", "pool configuration is unavailable")
	}
	base = base.Masked()
	p, err := netip.ParsePrefix(pin.CIDR)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return "", none, apiErr(422, "invalid_request", "adopted CIDR is not a canonical IPv4 prefix")
	}
	// The owning team's first POST carries a prefix length, and the request
	// hash covers it. A /16 recorded under a request for a /20 would refuse
	// that team's own request forever (ADR 0010).
	if p.Bits() != r.PrefixLength {
		return "", none, apiErr(422, "invalid_request", "adopted CIDR does not have the requested prefix length")
	}
	// For a subnet, validateRequest has already narrowed pool.CIDR to the
	// parent's range, so this is containment in the parent.
	if !base.Contains(p.Addr()) || p.Bits() < base.Bits() {
		return "", none, apiErr(422, "policy_violation", "adopted CIDR is outside the pool")
	}
	if overlapsExcluded(p, pool.ExcludedCIDRs) {
		return "", none, apiErr(409, "adoption_refused", "adopted CIDR overlaps excluded address space")
	}
	// The children the reviewed VPC is allowed to contain, derived from the one
	// observation both halves below read, so the snapshot rule and the
	// observation rule cannot disagree about what a child is.
	children := reviewedVPCChildren(obs, p, r, pin.ResourceID)
	reviewed, err := reviewedNetwork(snap, p, r, pin.NetworkID, "", children)
	if err != nil {
		return "", none, err
	}
	parentResource := ""
	if parent, ok := st.Allocations[r.ParentAllocationID]; ok && parent.Binding != nil {
		parentResource = parent.Binding.ResourceID
	}
	if err := reviewedOccupancy(obs, p, r, pin.ResourceID, parentResource); err != nil {
		return "", none, err
	}
	// The ledger scan is chooseCIDR's, unchanged and unexempted: a hold or a
	// live allocation over this space is never something an operator can review
	// away.
	for _, a := range st.Allocations {
		q, e := netip.ParsePrefix(a.CIDR)
		if e != nil || !p.Overlaps(q) {
			continue
		}
		if !a.Committed {
			return "", none, apiErr(409, "adoption_refused", "an uncommitted allocation holds the adopted CIDR")
		}
		if a.State != domain.Released && a.DomainID == pool.DomainID {
			if r.Scope == "subnet" && a.ID == r.ParentAllocationID && q.Contains(p.Addr()) {
				continue
			}
			return "", none, apiErr(409, "adoption_refused", "an existing allocation holds the adopted CIDR")
		}
	}
	return p.String(), reviewed, nil
}

// reviewedNetwork is the snapshot half of adoption's first overlap exemption:
// exactly one inventory object may overlap the candidate, and it must be the one
// the operator named, at exactly this CIDR, carrying the import tag and owned by
// nobody (ADR 0010). own names the allocation whose markers the object may
// already carry: empty while the allocation is being planned, and the
// allocation's own id when the worker recovers an adoption whose adapter write
// outlived the commit that was meant to follow it, where converging on that
// object is a replay rather than a second adoption. children carries adoption's
// third exemption and is built by reviewedVPCChildren from the same observation
// reviewedOccupancy reads. Planning and recovery share all of it so that the
// evidence which admits an adoption is the evidence that finishes it.
func reviewedNetwork(snap domain.InventorySnapshot, p netip.Prefix, r domain.Request, networkID, own string, children reviewedChildren) (domain.Network, error) {
	var none domain.Network
	reviewed, found := none, 0
	for _, n := range snap.Networks {
		q, e := netip.ParsePrefix(n.CIDR)
		if e != nil || !p.Overlaps(q) {
			continue
		}
		if n.ParentPool || r.Scope == "subnet" && n.AllocationID == r.ParentAllocationID && q.Contains(p.Addr()) {
			continue
		}
		unowned := n.Imported && !n.Owned && n.AllocationID == "" && n.OperationID == ""
		if q.Masked() == p && n.ID == networkID && (unowned || own != "" && n.AllocationID == own) {
			reviewed, found = n, found+1
			continue
		}
		// The third exemption's snapshot half: an imported, unowned prefix
		// strictly inside the pin is one of the reviewed VPC's own subnets only
		// if an observed child of that VPC holds exactly that CIDR as its
		// primary one. An imported prefix inside the pin that no observed child
		// accounts for is still somebody's occupancy and still a refusal, which
		// is what keeps the exemption from widening into "anything nested".
		if unowned && q.Bits() > p.Bits() && p.Contains(q.Addr()) && children[q.Masked()] != "" {
			continue
		}
		return none, apiErr(409, "adoption_refused", "another network in the inventory occupies the adopted CIDR")
	}
	if found != 1 {
		return none, apiErr(409, "adoption_refused", "the reviewed imported network was not found at the adopted CIDR")
	}
	return reviewed, nil
}

// reviewedOccupancy is the observation half of adoption's second exemption:
// exactly one observed resource may overlap the candidate, and it must be the
// one the operator reviewed -- or, under the third exemption, one of that
// resource's own observed child subnets, which are not counted at all. The
// worker's recovery repeats it rather than the reservation's tag rule, because
// the platform cannot tag somebody else's VPC and the adopted resource
// therefore never carries the marker a recovered reservation is recognised by
// (ADR 0010).
func reviewedOccupancy(obs domain.Observation, p netip.Prefix, r domain.Request, resourceID, parentResource string) error {
	resources := 0
	for _, resource := range obs.Resources {
		overlaps := false
		for _, raw := range append([]string{resource.CIDR}, resource.CIDRs...) {
			q, e := netip.ParsePrefix(raw)
			if e != nil || !p.Overlaps(q) {
				continue
			}
			// A subnet's own parent VPC is exempt exactly as it is for a
			// reservation, and only on the same evidence: a verified parent
			// binding. Adoption adds no third exemption for it, so a subnet
			// cannot be adopted until its parent's binding is observed.
			if r.Scope == "subnet" && resource.Type == "vpc" && parentResource != "" && resource.ID == parentResource && q.Contains(p.Addr()) {
				continue
			}
			overlaps = true
			break
		}
		if !overlaps {
			continue
		}
		// The third exemption's observation half. It is asked after the overlap
		// test and before the reviewed-resource test, so a child whose primary
		// CIDR is outside the pin while a secondary association reaches into it
		// falls through to reviewedResource and is refused there.
		if childOfReviewedVPC(resource, r, p, resourceID) {
			continue
		}
		if !reviewedResource(resource, r, p, resourceID, parentResource) {
			return apiErr(409, "adoption_refused", "another cloud resource occupies the adopted CIDR")
		}
		resources++
	}
	if resources != 1 {
		return apiErr(409, "adoption_refused", "the reviewed cloud resource was not observed at the adopted CIDR")
	}
	return nil
}

// reviewedChildren indexes the reviewed VPC's own observed subnets by their
// primary CIDR. It is the only channel between adoption's two overlap rules,
// and it carries cloud evidence exclusively: nothing an operator typed reaches
// it beyond the two ids their record already names.
type reviewedChildren map[netip.Prefix]string

// childOfReviewedVPC reports whether x is a subnet the reviewed VPC itself
// owns, and is adoption's third overlap exemption (ADR 0010, amended
// 2026-09-20). The two exemptions the record shipped with refuse every real
// VPC: an organization inventory imports a VPC together with its subnets and
// the observer sees all of them, so a VPC's own children were "another network"
// and "another cloud resource" and no VPC that had any could ever be adopted.
//
// The exemption is as narrow as the evidence allows, and rests on nothing the
// operator supplied beyond pin.ResourceID, which they already reviewed. x must
// be a subnet; its parent must be the reviewed VPC itself, so a subnet of a
// neighbouring VPC is not covered by it; its account and region must be the
// request's, so a resource observed in a coverage cell this request does not
// speak for is not covered either; it must carry no platform claim, because a
// claimed subnet is already somebody's allocation and never a free child; and
// its PRIMARY CIDR must lie inside the pin, because a secondary association is
// occupancy evidence and never what a resource issues (the same rule
// reviewedResource applies to the reviewed object itself). Equality with the
// pin is allowed: AWS lets a subnet span its whole VPC, and such a subnet is
// still the reviewed VPC's own child.
//
// Only a vpc-scoped pin gets this. A subnet has no children the platform models,
// so a pinned subnet keeps exactly the two exemptions it had.
func childOfReviewedVPC(x domain.Resource, r domain.Request, p netip.Prefix, resourceID string) bool {
	if r.Scope != "vpc" || x.Type != "subnet" || resourceID == "" || x.ParentID != resourceID {
		return false
	}
	if x.AccountID != r.AccountID || x.Region != r.Region {
		return false
	}
	if x.Tags["platform-ipam:allocation-id"] != "" {
		return false
	}
	q, err := netip.ParsePrefix(x.CIDR)
	if err != nil {
		return false
	}
	q = q.Masked()
	return q.Bits() >= p.Bits() && p.Contains(q.Addr())
}

// reviewedVPCChildren collects, from the one observation the adoption is being
// judged against, the primary CIDRs of the reviewed VPC's own child subnets. It
// is what lets the snapshot rule ask a question only the cloud can answer:
// whether an imported prefix inside the pin is one of those children. The
// direction matters and is deliberately one-way. Every inventory object inside
// the pin must be accounted for by an observed child, because an imported
// prefix nothing in the cloud explains is occupancy somebody else reviewed and
// wrote, and adopting over it would claim address space on no evidence. The
// converse is not required: an observed child with no imported prefix is simply
// occupancy the inventory does not know about yet -- a partial import, which is
// an import problem rather than an ownership ambiguity -- and it stays visible,
// because the reconciler keeps reporting it as unmanaged_occupancy until it is
// itself imported and adopted.
func reviewedVPCChildren(obs domain.Observation, p netip.Prefix, r domain.Request, resourceID string) reviewedChildren {
	var out reviewedChildren
	for _, x := range obs.Resources {
		if !childOfReviewedVPC(x, r, p, resourceID) {
			continue
		}
		q, err := netip.ParsePrefix(x.CIDR)
		if err != nil {
			continue
		}
		if out == nil {
			out = reviewedChildren{}
		}
		out[q.Masked()] = x.ID
	}
	return out
}

// adoptedTheReviewedObject reports whether the inventory converted the object
// the operator reviewed. Inventory.Adopt locates the prefix by CIDR and VRF
// rather than by id, so the answer is the reviewed one only if the id agrees,
// and the question is asked of the operation's durable record so that the
// operator's own process and the worker recovering after its crash cannot
// disagree about what was reviewed (ADR 0010).
func adoptedTheReviewedObject(o domain.Operation, inventoryID string) bool {
	return o.Adoption != nil && inventoryID != "" && inventoryID == o.Adoption.NetworkID
}

// reviewedResource reports whether x is the single cloud resource the operator
// reviewed. The primary CIDR must equal the candidate: a secondary association
// is occupancy evidence and can never be adopted under (ADR 0010), and nothing
// already claiming an allocation is adoptable at all.
func reviewedResource(x domain.Resource, r domain.Request, p netip.Prefix, resourceID, parentResource string) bool {
	if x.ID != resourceID || x.Type != r.Scope || x.AccountID != r.AccountID || x.Region != r.Region {
		return false
	}
	if q, err := netip.ParsePrefix(x.CIDR); err != nil || q.Masked() != p {
		return false
	}
	if x.Tags["platform-ipam:allocation-id"] != "" {
		return false
	}
	if r.Scope == "subnet" {
		if parentResource == "" || x.ParentID != parentResource {
			return false
		}
		if r.AvailabilityZoneID == "" || x.ZoneID != r.AvailabilityZoneID {
			return false
		}
	}
	return true
}

// adoptionEvidence is the audit reason for both adoption events. domain.Event
// carries no structured details, so what the operator reviewed and what the
// service saw when it agreed go into the string (ADR 0010). It reads the
// durable record rather than the pin, so a commit written by the worker after a
// crash says exactly what the operator's own process would have said.
func adoptionEvidence(rec domain.AdoptionRecord, obs domain.Observation) string {
	return fmt.Sprintf("adoption reviewed by %s: inventory network %s (import batch %q), cloud resource %s, observation %s finished %s",
		rec.Operator, rec.NetworkID, rec.ImportBatch, rec.ResourceID, obs.Generation, obs.FinishedAt.UTC().Format(time.RFC3339))
}

func reclaimEligible(st *domain.State, a domain.Allocation, cfg domain.Config, now time.Time) bool {
	if a.State != domain.Quarantined || !a.Committed || a.ReleaseRequestedAt == nil || a.QuarantineUntil == nil || now.Before(*a.QuarantineUntil) {
		return false
	}
	for _, child := range st.Allocations {
		if child.ParentAllocationID == a.ID && child.State != domain.Released {
			return false
		}
	}
	for _, o := range st.Operations {
		if o.DomainID == a.DomainID && o.Status == operationPending {
			return false
		}
	}
	d := domainFor(cfg, a.DomainID)
	rows := st.Observations[a.DomainID]
	if len(rows) == 0 {
		return false
	}
	parentID := ""
	if parent := st.Allocations[a.ParentAllocationID]; parent.Binding != nil {
		parentID = parent.Binding.ResourceID
	}
	need := cfg.Lifecycle.RequiredAbsenceScans
	if need < 2 {
		need = 2
	}
	spacing := cfg.Lifecycle.MinScanSpacing
	if spacing < 1 {
		spacing = 300
	}
	count := 0
	var previous time.Time
	for i := len(rows) - 1; i >= 0; i-- {
		o := rows[i]
		if !trustedObservation(o, d, now, cfg.Lifecycle.MaxObservationAge) || o.StartedAt.Before(*a.ReleaseRequestedAt) || observationConflict(a, o, parentID) {
			break
		}
		if count == 0 || previous.Sub(o.FinishedAt) >= time.Duration(spacing)*time.Second {
			count++
			previous = o.FinishedAt
		}
		if count >= need {
			return true
		}
	}
	return false
}

func reclaimFence(st *domain.State, a domain.Allocation, operationID string, cfg domain.Config, now time.Time) bool {
	current := st.Operations[operationID]
	if current.Status != operationPending || current.Type != reclaimOperation || current.AllocationID != a.ID {
		return false
	}
	// Evaluate every release gate again while excluding only this deletion job.
	copyState := *st
	copyState.Operations = make(map[string]domain.Operation, len(st.Operations))
	for id, o := range st.Operations {
		if id != operationID {
			copyState.Operations[id] = o
		}
	}
	return reclaimEligible(&copyState, a, cfg, now)
}

func domainFor(cfg domain.Config, id string) domain.Domain {
	for _, d := range cfg.Domains {
		if d.ID == id {
			return d
		}
	}
	return domain.Domain{ID: id, CoverageGeneration: ""}
}
func recordObservation(st *domain.State, o domain.Observation) {
	xs := st.Observations[o.DomainID]
	if len(xs) > 0 && o.FinishedAt.Before(xs[len(xs)-1].FinishedAt) {
		return
	}
	if !o.Complete || st.Coverage[o.DomainID] != o.Generation {
		xs = nil
	}
	if len(xs) > 0 && o.FinishedAt.Equal(xs[len(xs)-1].FinishedAt) {
		xs[len(xs)-1] = o
	} else {
		xs = append(xs, o)
	}
	if len(xs) > 64 {
		xs = xs[len(xs)-64:]
	}
	st.Observations[o.DomainID] = xs
	st.Coverage[o.DomainID] = o.Generation
}
func bindingObserved(o domain.Observation, b domain.Binding, id, key, expectedCIDR, expectedAZ, expectedParent string) bool {
	if !o.Complete {
		return false
	}
	for _, r := range o.Resources {
		if r.ID != b.ResourceID || r.Type != b.ResourceType || r.AccountID != b.AccountID || r.Region != b.Region {
			continue
		}
		if r.Tags["platform-ipam:allocation-id"] != id {
			return false
		}
		if key != "" && r.Tags["platform-ipam:allocation-key"] != key {
			return false
		}
		if expectedAZ != "" && r.ZoneID != expectedAZ {
			return false
		}
		if expectedParent != "" && r.ParentID != expectedParent {
			return false
		}
		if expectedCIDR != "" {
			// AWS VPC secondary associations are occupancy evidence but cannot
			// activate a v1 primary-CIDR allocation.
			if r.CIDR != expectedCIDR {
				return false
			}
		}
		return true
	}
	return false
}
func pendingForAllocation(st *domain.State, id string) *domain.Operation {
	for _, o := range st.Operations {
		if o.AllocationID == id && o.Status == operationPending {
			x := o
			return &x
		}
	}
	return nil
}
func pendingDomain(st *domain.State, id string) *domain.Operation {
	for _, o := range st.Operations {
		if o.DomainID == id && o.Status == operationPending {
			x := o
			return &x
		}
	}
	return nil
}
func stateResults(st *domain.State, r domain.Idempotency) (*domain.Allocation, *domain.Operation) {
	var a *domain.Allocation
	var o *domain.Operation
	if x, ok := st.Allocations[r.AllocationID]; ok {
		a = copyAllocation(x)
	}
	if x, ok := st.Operations[r.OperationID]; ok {
		o = copyOperation(x)
	}
	return a, o
}
func resultStatus(a *domain.Allocation, o *domain.Operation) int {
	if o != nil && o.Status == operationPending {
		return 202
	}
	if a != nil && a.Committed {
		return 200
	}
	return 202
}
func countTenant(st *domain.State, tenant, pool string) int {
	n := 0
	for _, a := range st.Allocations {
		if a.TenantID == tenant && a.PoolID == pool && a.State != domain.Released {
			n++
		}
	}
	return n
}
func countChildren(st *domain.State, id string) int {
	n := 0
	for _, a := range st.Allocations {
		if a.ParentAllocationID == id && a.State != domain.Released {
			n++
		}
	}
	return n
}
func ensureState(st *domain.State) {
	if st.Allocations == nil {
		st.Allocations = map[string]domain.Allocation{}
	}
	if st.Operations == nil {
		st.Operations = map[string]domain.Operation{}
	}
	if st.Requests == nil {
		st.Requests = map[string]domain.Idempotency{}
	}
	if st.Observations == nil {
		st.Observations = map[string][]domain.Observation{}
	}
	if st.Findings == nil {
		st.Findings = map[string]domain.Finding{}
	}
	if st.Coverage == nil {
		st.Coverage = map[string]string{}
	}
}
func copyAllocation(a domain.Allocation) *domain.Allocation {
	x := a
	x.Labels = cloneLabels(a.Labels)
	x.ReleaseBlockers = append([]string(nil), a.ReleaseBlockers...)
	if a.Binding != nil {
		b := *a.Binding
		x.Binding = &b
	}
	return &x
}
func copyOperation(o domain.Operation) *domain.Operation {
	x := o
	if o.Result != nil {
		x.Result = map[string]string{}
		for k, v := range o.Result {
			x.Result[k] = v
		}
	}
	if o.Candidate != nil {
		b := *o.Candidate
		x.Candidate = &b
	}
	if o.Adoption != nil {
		a := *o.Adoption
		x.Adoption = &a
	}
	return &x
}
func cloneLabels(x map[string]string) map[string]string {
	if x == nil {
		return map[string]string{}
	}
	y := map[string]string{}
	for k, v := range x {
		y[k] = v
	}
	return y
}
func apiErr(status int, code, msg string) error { return domain.Err(status, code, msg) }
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func validKey(x string) bool {
	if len(x) < 1 || len(x) > 128 {
		return false
	}
	for i, r := range x {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/') || i == 0 && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}
func validateLabels(x map[string]string) error {
	if len(x) > 20 {
		return apiErr(422, "invalid_request", "too many labels")
	}
	for k, v := range x {
		if len(k) == 0 || len(k) > 64 || len(v) > 256 || strings.HasPrefix(k, "platform-ipam/") {
			return apiErr(422, "invalid_request", "invalid label")
		}
	}
	return nil
}
func eligibleString(xs []string, v string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func containsInt(xs []int, v int) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func requestHash(r domain.Request) string {
	// Description and labels are mutable metadata. They are deliberately
	// excluded so a lost-response retry with changed metadata still recovers
	// the same permanent allocation identity.
	r.Description = ""
	r.Labels = nil
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func requestBodyHash(r domain.Request) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func nowFresh(now, observed time.Time, maxSeconds int) bool {
	if observed.IsZero() || observed.After(now) {
		return false
	}
	if maxSeconds <= 0 {
		maxSeconds = 600
	}
	return now.Sub(observed) <= time.Duration(maxSeconds)*time.Second
}
func patchHash(id string, d *string, l *map[string]string, e int64) string {
	b, _ := json.Marshal(struct {
		ID string
		D  *string
		L  *map[string]string
		E  int64
	}{id, d, l, e})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func bindingHash(id string, b domain.Binding) string {
	x, _ := json.Marshal(struct {
		ID string
		B  domain.Binding
	}{id, b})
	h := sha256.Sum256(x)
	return hex.EncodeToString(h[:])
}
func idempotencyID(tenant, method, path, key string) string {
	h := sha256.Sum256([]byte(tenant + "\x00" + method + "\x00" + path + "\x00" + key))
	return hex.EncodeToString(h[:])
}
func findingID(code, allocationID string) string {
	h := sha256.Sum256([]byte(code + "\x00" + allocationID))
	return "finding_" + hex.EncodeToString(h[:])
}
func sameBinding(a, b domain.Binding) bool {
	return a.Provider == b.Provider && a.ResourceType == b.ResourceType && a.ResourceID == b.ResourceID && a.AccountID == b.AccountID && a.Region == b.Region
}
func overlapsExcluded(p netip.Prefix, xs []string) bool {
	for _, raw := range xs {
		q, e := netip.ParsePrefix(raw)
		if e == nil && p.Overlaps(q) {
			return true
		}
	}
	return false
}
func addrInt(a netip.Addr) uint64 {
	a = a.Unmap()
	b := a.As4()
	return uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
}
func prefixFromInt(x uint64, bits int) (netip.Prefix, bool) {
	if bits < 0 || bits > 32 {
		return netip.Prefix{}, false
	}
	v := uint32(x)
	a := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
	return netip.PrefixFrom(a, bits).Masked(), true
}
