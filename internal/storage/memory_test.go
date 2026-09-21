package storage

import (
	"context"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// TestMemoryLedgerRoundTripsTheFindingResourceIdentity is the runnable half of
// package G3b3's persistence claim: the resource identity an operator's domain
// view groups on (ADR 0011) needs no migration, because a finding rides in a
// JSON payload rather than in columns of its own -- `findings (id, tenant_id,
// domain_id, payload jsonb)` in PostgresLedger.Migrate, and a whole-state JSON
// clone here. Both encodings are encoding/json over domain.Finding, so a
// missing or misspelled struct tag fails here without a database, and
// postgres_test.go's sibling proves the same across a real pool reload when
// IPAM_TEST_DATABASE_URL is set.
func TestMemoryLedgerRoundTripsTheFindingResourceIdentity(t *testing.T) {
	ledger := NewMemoryLedger()
	ctx := context.Background()
	occupancy := domain.Finding{
		ID: "finding_occupancy", TenantID: "tenant-a", DomainID: "domain-a",
		Code: "unmanaged_occupancy", Severity: "WARNING", Status: "OPEN",
		AccountID: "123456789012", Region: "eu-central-1",
		ResourceType: "vpc", ResourceID: "vpc-0unmanaged000000",
		FirstObservedAt: time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC),
		LastObservedAt:  time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC),
	}
	// An allocation-scoped finding carries no resource identity. The fields are
	// omitempty, so this is also the check that an absent value comes back
	// absent rather than as something else.
	aged := domain.Finding{
		ID: "finding_aged", TenantID: "tenant-a", DomainID: "domain-a",
		AllocationID: "alloc_aged", Code: "reservation_aged", Severity: "WARNING",
		Status: "OPEN", FirstObservedAt: occupancy.FirstObservedAt,
		LastObservedAt: occupancy.LastObservedAt,
	}
	if err := ledger.Update(ctx, func(state *domain.State) error {
		state.Findings[occupancy.ID] = occupancy
		state.Findings[aged.ID] = aged
		return nil
	}); err != nil {
		t.Fatalf("store findings: %v", err)
	}

	state, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got, ok := state.Findings[occupancy.ID]
	if !ok {
		t.Fatal("the occupancy finding did not survive the clone")
	}
	if got != occupancy {
		t.Fatalf("the occupancy finding changed across the clone: %#v", got)
	}
	got, ok = state.Findings[aged.ID]
	if !ok {
		t.Fatal("the allocation-scoped finding did not survive the clone")
	}
	if got.ResourceType != "" || got.ResourceID != "" {
		t.Fatalf("an allocation-scoped finding gained a resource identity: %#v", got)
	}
}

// Abandoning an adoption removes an allocation and its idempotency record from
// the state maps and nothing else (ADR 0012, package H2b), so the whole
// operation rests on a removal from a map really being a removal from the
// ledger. Here that is a JSON clone; postgres_test.go's sibling proves the same
// for the nine tables persistState rewrites, and both have to agree about the
// one table it never clears: audit events outlive the allocation they describe.
func TestMemoryLedgerReallyDropsWhatAnAbandonRemoves(t *testing.T) {
	ledger := NewMemoryLedger()
	ctx := context.Background()
	const allocationID, requestID = "alloc_abandoned", "sha256-adopt-record"
	hold := domain.Allocation{
		Request: domain.Request{AllocationKey: "orders"},
		ID:      allocationID, TenantID: "tenant-a", DomainID: "domain-a", CIDR: "10.64.0.0/20",
		PoolID: "pool-a", State: domain.Reserved,
		CreatedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}
	if err := ledger.Update(ctx, func(state *domain.State) error {
		state.Allocations[hold.ID] = hold
		state.Requests[requestID] = domain.Idempotency{
			TenantID: hold.TenantID, Method: "ADOPT", Path: "ADOPT /v1/allocations",
			Key: hold.AllocationKey, Hash: "reviewed-record", AllocationID: hold.ID,
		}
		state.Operations["op_abandoned"] = domain.Operation{
			ID: "op_abandoned", Type: "ADOPT", Status: "PENDING", AllocationID: hold.ID,
			TenantID: hold.TenantID, DomainID: hold.DomainID,
			Adoption: &domain.AdoptionRecord{Operator: "ops:bob", NetworkID: "4242", ResourceID: "vpc-adopted"},
		}
		state.Events = append(state.Events, domain.Event{
			ID: "evt_planned", AllocationID: hold.ID, TenantID: hold.TenantID,
			Action: "ADOPT_PLANNED", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("store the hold: %v", err)
	}

	if err := ledger.Update(ctx, func(state *domain.State) error {
		delete(state.Allocations, allocationID)
		delete(state.Requests, requestID)
		state.Events = append(state.Events, domain.Event{
			ID: "evt_abandoned", AllocationID: allocationID, TenantID: "tenant-a",
			Action: "ADOPT_ABANDONED", Actor: "ops:bob", At: hold.CreatedAt,
		})
		return nil
	}); err != nil {
		t.Fatalf("withdraw the hold: %v", err)
	}

	state, err := ledger.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, held := state.Allocations[allocationID]; held {
		t.Fatal("the allocation came back after being removed from the state")
	}
	if _, kept := state.Requests[requestID]; kept {
		t.Fatal("the idempotency record came back after being removed from the state")
	}
	// The operation deliberately stays, carrying what was reviewed.
	operation, ok := state.Operations["op_abandoned"]
	if !ok || operation.Adoption == nil || operation.Adoption.NetworkID != "4242" {
		t.Fatalf("the operation or its reviewed record did not survive: %#v", operation)
	}
	seen := map[string]bool{}
	for _, event := range state.Events {
		seen[event.ID] = true
	}
	if !seen["evt_planned"] || !seen["evt_abandoned"] {
		t.Fatalf("audit events did not outlive the allocation they describe: %#v", state.Events)
	}
}
