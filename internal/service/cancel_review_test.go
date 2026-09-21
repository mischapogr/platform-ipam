package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// A cancel removes this hold's idempotency records and nobody else's, and the
// event it leaves belongs to the tenant whose hold it was, so that tenant can
// read it after the row is gone.
func TestCancelTouchesOnlyItsOwnRecordsAndAttributesItsEvent(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
		st.Requests["somebody-elses-record"] = domain.Idempotency{AllocationID: "alloc-somebody-else"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	st := ledgerState(t, c.ledger)
	if _, kept := st.Requests["somebody-elses-record"]; !kept {
		t.Fatal("the cancel removed an idempotency record that names another allocation")
	}
	events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction)
	if len(events) != 1 || events[0].TenantID != c.allocation.TenantID || events[0].AllocationID != c.allocation.ID {
		t.Fatalf("cancel events: %#v", events)
	}
}

// Without an inventory nobody can say whether a prefix carries the marker, so
// the cancel refuses before it fences anything.
func TestCancelWithoutAnInventoryWritesNothing(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	before := cancelOperation(t, c.ledger, c.operation.ID)
	bare := New(c.service.cfg, c.ledger, nil, c.observer)
	bare.SetClock(func() time.Time { return now })
	report, err := bare.CancelReservation(context.Background(), cancellingPrincipal(), c.operation.ID)
	var api *domain.APIError
	if report != nil || !errors.As(err, &api) || api.Status != 503 || api.Code != "dependency_unavailable" {
		t.Fatalf("report %#v, err %v", report, err)
	}
	if after := cancelOperation(t, c.ledger, c.operation.ID); after.Status != before.Status || after.Error != nil {
		t.Fatalf("the operation was fenced without an inventory: %#v", after)
	}
	if len(cancelEventsOfAction(t, c.ledger, reservationCancelledAction)) != 0 {
		t.Fatal("an event was written without an inventory")
	}
}
