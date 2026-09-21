package service

// Package E3: docs/API_V1.md section 5 promises that GET /v1/allocations/{id}
// "for an uncommitted known operation returns 409 allocation_pending with its
// operation ID, not a misleading 404" -- a promise api/openapi.yaml's schema
// already listed on this read's 409 but that Service.Get never kept (found end
// to end by H8d's review of ADR 0013, docs/WORK_PLAN.md's E3 block). These
// tests are the service-level table the E3 block asks for: owner+PENDING,
// owner+terminal, other tenant, operator, unknown id, and committed in every
// state -- each pinned to the exact status/code/details Get now produces.

import (
	"context"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

func pendingReadNow() time.Time { return time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC) }

// pendingReadFixture seeds one uncommitted hold for tenant "t" and returns the
// service plus the ledger, so each test can shape the hold's operation
// (PENDING, terminal, or absent) before reading it.
func pendingReadFixture(t *testing.T) (*Service, *storage.MemoryLedger, domain.Allocation) {
	t.Helper()
	now := pendingReadNow()
	ledger := storage.NewMemoryLedger()
	s := safetyService(now, ledger, &safetyInventory{}, &safetyObserver{observation: safetyObservation(now)})
	hold := safetyAllocation(now, "alloc-pending-read", "vpc", "10.5.0.0/24", "", "")
	hold.Committed, hold.InventoryID, hold.InventorySync = false, "", "PENDING"
	putSafetyAllocation(t, ledger, hold)
	return s, ledger, hold
}

func putOperation(t *testing.T, ledger *storage.MemoryLedger, o domain.Operation) {
	t.Helper()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		st.Operations[o.ID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGetAnUncommittedHoldWithAPendingOperationAnswersAllocationPending is the
// central grant: the owning tenant, reading its own uncommitted hold while its
// RESERVE is still PENDING, gets 409 allocation_pending, retryable, carrying
// that operation's id under details.operation_id -- the shape
// pendingAllocationError documents because docs/API_V1.md names none.
func TestGetAnUncommittedHoldWithAPendingOperationAnswersAllocationPending(t *testing.T) {
	s, ledger, hold := pendingReadFixture(t)
	op := domain.Operation{ID: "op-" + hold.ID, Type: reserveOperation, Status: operationPending, AllocationID: hold.ID, TenantID: hold.TenantID, DomainID: hold.DomainID, CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt}
	putOperation(t, ledger, op)

	_, err := s.Get(context.Background(), safetyPrincipal(), hold.ID)
	safetyAPIError(t, err, 409, "allocation_pending")
	var apiErr *domain.APIError
	if got := err; got == nil {
		t.Fatal("expected an error")
	} else if x, ok := got.(*domain.APIError); ok {
		apiErr = x
	} else {
		t.Fatalf("error=%T, want *domain.APIError", got)
	}
	if !apiErr.Retryable {
		t.Fatalf("allocation_pending must be retryable: %#v", apiErr)
	}
	if apiErr.Details == nil || apiErr.Details["operation_id"] != op.ID {
		t.Fatalf("details=%#v, want operation_id=%q", apiErr.Details, op.ID)
	}
}

// An ADOPT hold makes the same promise as a RESERVE hold: E3's block names
// both, and reserve()'s kind.operation is the only thing that differs between
// them.
func TestGetAnUncommittedAdoptionWithAPendingOperationAnswersAllocationPending(t *testing.T) {
	s, ledger, hold := pendingReadFixture(t)
	op := domain.Operation{ID: "op-" + hold.ID, Type: adoptOperation, Status: operationPending, AllocationID: hold.ID, TenantID: hold.TenantID, DomainID: hold.DomainID, CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt}
	putOperation(t, ledger, op)

	_, err := s.Get(context.Background(), safetyPrincipal(), hold.ID)
	safetyAPIError(t, err, 409, "allocation_pending")
}

// TestGetAnUncommittedHoldWithATerminalOperationStays404 covers the window a
// cancel's or an abandon's fence leaves behind (ADR 0012, ADR 0013): the
// operation is FAILED, not PENDING, so pendingForAllocation finds nothing and
// the read carries no promise docs/API_V1.md makes -- it stays 404, exactly as
// before E3.
func TestGetAnUncommittedHoldWithATerminalOperationStays404(t *testing.T) {
	s, ledger, hold := pendingReadFixture(t)
	op := domain.Operation{
		ID: "op-" + hold.ID, Type: reserveOperation, Status: operationFailed, AllocationID: hold.ID,
		TenantID: hold.TenantID, DomainID: hold.DomainID, CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt,
		Error: domain.Err(409, reservationCancelledCode, cancelledOperationMessage),
	}
	putOperation(t, ledger, op)

	_, err := s.Get(context.Background(), safetyPrincipal(), hold.ID)
	safetyAPIError(t, err, 404, "not_found")
}

// TestGetAnUncommittedHoldWithNoOperationStays404 is the defensive case E3's
// block names ("a failed reservation"): nothing pending at all for the
// allocation -- pendingForAllocation returns nil exactly as it does for the
// terminal case above, and the answer is the same 404.
func TestGetAnUncommittedHoldWithNoOperationStays404(t *testing.T) {
	s, _, hold := pendingReadFixture(t)
	_, err := s.Get(context.Background(), safetyPrincipal(), hold.ID)
	safetyAPIError(t, err, 404, "not_found")
}

// TestGetAnotherTenantsPendingHoldAndAnUnknownIDShareOneBody is the privacy
// property docs/API_V1.md section 1 requires: an uncommitted hold that exists
// but belongs to a different tenant must be indistinguishable from an id that
// does not exist at all, PENDING operation notwithstanding -- the tenant
// comparison in Get runs before pendingForAllocation is ever consulted.
func TestGetAnotherTenantsPendingHoldAndAnUnknownIDShareOneBody(t *testing.T) {
	s, ledger, hold := pendingReadFixture(t)
	putOperation(t, ledger, domain.Operation{ID: "op-" + hold.ID, Type: reserveOperation, Status: operationPending, AllocationID: hold.ID, TenantID: hold.TenantID, DomainID: hold.DomainID, CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt})

	foreign := tenantUPrincipal()
	_, otherErr := s.Get(context.Background(), foreign, hold.ID)
	safetyAPIError(t, otherErr, 404, "not_found")

	_, unknownErr := s.Get(context.Background(), safetyPrincipal(), "alloc-does-not-exist-at-all")
	safetyAPIError(t, unknownErr, 404, "not_found")

	otherAPIErr := otherErr.(*domain.APIError)
	unknownAPIErr := unknownErr.(*domain.APIError)
	if otherAPIErr.Code != unknownAPIErr.Code || otherAPIErr.Message != unknownAPIErr.Message || otherAPIErr.Status != unknownAPIErr.Status {
		t.Fatalf("another tenant's pending hold must read identically to an unknown id: other=%#v unknown=%#v", otherAPIErr, unknownAPIErr)
	}
}

// TestGetOperatorNeverSeesAllocationPending is ADR 0011's half of the E3
// contract: an operator with a PENDING operation on the hold still gets the
// tenant's own 404, never the new 409 -- a hold must not become MORE visible
// to an operator than it was before E3.
func TestGetOperatorNeverSeesAllocationPending(t *testing.T) {
	s, ledger, hold := pendingReadFixture(t)
	putOperation(t, ledger, domain.Operation{ID: "op-" + hold.ID, Type: reserveOperation, Status: operationPending, AllocationID: hold.ID, TenantID: hold.TenantID, DomainID: hold.DomainID, CreatedAt: hold.CreatedAt, UpdatedAt: hold.CreatedAt})

	_, err := s.Get(context.Background(), operatorPrincipal(), hold.ID)
	safetyAPIError(t, err, 404, "not_found")
}

// TestGetCommittedAllocationIsUnaffectedInEveryState pins that a committed
// allocation's answer never depends on pendingForAllocation at all: E3 must
// not change behaviour for the one case (a.Committed) that already worked,
// whatever lifecycle state it is in and whatever stray operation rows exist.
func TestGetCommittedAllocationIsUnaffectedInEveryState(t *testing.T) {
	now := pendingReadNow()
	for _, state := range []string{domain.Reserved, domain.Active, domain.Quarantined, domain.Released} {
		t.Run(state, func(t *testing.T) {
			ledger := storage.NewMemoryLedger()
			s := safetyService(now, ledger, &safetyInventory{}, &safetyObserver{observation: safetyObservation(now)})
			a := safetyAllocation(now, "alloc-committed-"+state, "vpc", "10.6.0.0/24", "", "")
			a.State = state
			putSafetyAllocation(t, ledger, a)
			got, err := s.Get(context.Background(), safetyPrincipal(), a.ID)
			if err != nil || got.ID != a.ID || got.State != state {
				t.Fatalf("committed allocation in state %s: got=%#v err=%v", state, got, err)
			}
		})
	}
}
