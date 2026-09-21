package service

// Cancelling a reservation is the first tenant-facing operation in this project
// that removes a ledger row, so these tests are written around the three
// properties ADR 0013 says must never regress. No committed allocation can be
// cancelled by this path, whatever state it is in and however it came to be
// owned. No cancel can leave an inventory object claiming an allocation the
// ledger does not hold, which is a statement about the ORDER of three steps and
// not only about their outcomes -- so the inventory double here records when
// each removal happened and what the ledger held at that moment. And no tenant,
// and no operator, can affect another tenant's hold: the cross-tenant half of
// that property also has a test in safety_contract_test.go, beside the other
// guarantees of the service boundary.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

const (
	// cancelKey is the consumer's own Idempotency-Key, which is NOT the
	// allocation key: a reservation's idempotency record is keyed on a value
	// the client chose, so the delete cannot recompute its id and has to find
	// every record naming the allocation instead.
	cancelKey            = "orders-idempotency-key"
	cancelAllocationKey  = "orders"
	cancelStuckEnsureErr = "CIDR already occupied in managed VRF"
)

// cancellingPrincipal is safetyPrincipal with a subject, because the actor on
// the RESERVE_CANCELLED event is Principal.Subject -- a verified identity,
// which is what makes a cancel's audit trail stronger than an abandon's
// unverified --operator string (ADR 0013).
func cancellingPrincipal() domain.Principal {
	p := safetyPrincipal()
	p.Subject = "t:carla"
	return p
}

// cancelCall is one call to Inventory.CancelReservation, with the two facts a
// test cannot reconstruct afterwards: whether the ledger still held the
// allocation at that moment, and whether anything in the inventory still
// carried its marker.
type cancelCall struct {
	allocation  domain.Allocation
	operationID string
	rowHeld     bool
	marked      bool
}

// cancelRecorder is the inventory side of every test in this file. A call that
// is not made to fail deletes the networks carrying the allocation's marker, as
// the adapter does; onCall then runs, after that effect and before the call
// answers, which is the only place a test can change the world between the
// removal and the delete.
type cancelRecorder struct {
	calls  []cancelCall
	fail   map[int]error
	onCall func(n int)
}

func (r *cancelRecorder) hook(ledger *storage.MemoryLedger, inventory *safetyInventory) func(domain.Allocation, string) error {
	return func(a domain.Allocation, operationID string) error {
		call := cancelCall{allocation: a, operationID: operationID}
		if st, err := ledger.Snapshot(context.Background()); err == nil {
			_, call.rowHeld = st.Allocations[a.ID]
		}
		for _, n := range inventory.networks {
			if n.AllocationID == a.ID {
				call.marked = true
			}
		}
		r.calls = append(r.calls, call)
		n := len(r.calls)
		err := r.fail[n]
		if err == nil {
			var kept []domain.Network
			for _, x := range inventory.networks {
				if x.AllocationID != a.ID {
					kept = append(kept, x)
				}
			}
			inventory.networks = kept
		}
		if r.onCall != nil {
			r.onCall(n)
		}
		return err
	}
}

// cancelService is safetyService with the ledger widened to the port, so that a
// test can put abandonLedger (abandon_test.go) in front of the memory ledger
// and lose a chosen write. Nothing else differs: the same pool, the same
// lifecycle, the same clock.
func cancelService(now time.Time, ledger domain.Ledger, inventory *safetyInventory, observer *safetyObserver) *Service {
	cfg := safetyConfig()
	cfg.Lifecycle = domain.Lifecycle{
		QuarantineHours: 1, RequiredAbsenceScans: 2, MinScanSpacing: 300, MaxObservationAge: 3600,
	}
	s := New(cfg, ledger, inventory, observer)
	s.SetClock(func() time.Time { return now })
	return s
}

// cancellable is a real reservation the platform has declared stuck: planned
// through the service's own Reserve, left pending by a lost adapter call, and
// flagged by the worker's own recoverReservations on a definite refusal, so the
// hold, the pending RESERVE operation, the consumer's idempotency record, the
// RESERVE_PLANNED event and the open reservation_stuck finding are exactly what
// a stuck reservation leaves rather than rows a test wrote by hand.
type cancellable struct {
	service    *Service
	ledger     *storage.MemoryLedger
	wrapped    *abandonLedger
	inventory  *safetyInventory
	observer   *safetyObserver
	recorder   *cancelRecorder
	allocation domain.Allocation
	operation  domain.Operation
}

func stuckReservation(t *testing.T, now time.Time) cancellable {
	t.Helper()
	ctx := context.Background()
	memory := storage.NewMemoryLedger()
	wrapped := &abandonLedger{Ledger: memory}
	inventory := &safetyInventory{ensureErr: errors.New("the adapter never answered")}
	observer := &safetyObserver{observation: safetyObservation(now)}
	s := cancelService(now, wrapped, inventory, observer)

	a, op, status, err := s.Reserve(ctx, cancellingPrincipal(), safetyRequest(cancelAllocationKey), cancelKey)
	if err != nil || status != 202 || a == nil || a.Committed || op == nil || op.Status != operationPending || op.Type != reserveOperation {
		t.Fatalf("planning a reservation whose adapter call is then lost: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	// The prefix this reservation created before its commit was lost. Five
	// routes leave one behind a definite refusal (ADR 0013), and it is the
	// thing the cancel has to remove before the row can go.
	inventory.networks = append(inventory.networks, domain.Network{
		ID: "netbox-" + a.ID, CIDR: a.CIDR, AllocationID: a.ID, OperationID: op.ID, Owned: true,
	})

	// The worker's own verdict, past the flapping guard: a decision Ensure
	// reached on evidence it read.
	later := now.Add(2 * time.Hour)
	s.SetClock(func() time.Time { return later })
	observer.observation = safetyObservation(later)
	inventory.ensureErr = errors.New(cancelStuckEnsureErr)
	if err := s.recoverReservations(ctx); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if found := findingsWithCode(t, memory, reservationStuckCode); len(found) != 1 || found[0].Status != "OPEN" {
		t.Fatalf("want one open reservation_stuck finding, got %#v", found)
	}
	inventory.ensures = 0

	recorder := &cancelRecorder{fail: map[int]error{}}
	inventory.cancel = recorder.hook(memory, inventory)
	return cancellable{s, memory, wrapped, inventory, observer, recorder, *a, *op}
}

// cancelIt runs the real thing as the tenant that made the reservation.
func (c cancellable) cancelIt(t *testing.T) (*CancelReport, error) {
	t.Helper()
	return c.service.CancelReservation(context.Background(), cancellingPrincipal(), c.operation.ID)
}

// healthy lets a later reservation succeed: the fixture leaves the adapter
// refusing, which is what made the hold stuck in the first place.
func (c cancellable) healthy() { c.inventory.ensureErr = nil }

// fenceOnly stops the run at the first removal, which is what leaves the hold
// inside the window between the fence and the delete.
func (c cancellable) fenceOnly(t *testing.T) {
	t.Helper()
	c.recorder.fail[1] = errors.New("the removal was interrupted")
	if _, err := c.cancelIt(t); err == nil {
		t.Fatal("want the first run to stop at the removal")
	}
	delete(c.recorder.fail, 1)
}

func cancelOperation(t *testing.T, ledger *storage.MemoryLedger, id string) domain.Operation {
	t.Helper()
	o, ok := ledgerState(t, ledger).Operations[id]
	if !ok {
		t.Fatalf("operation %s is gone from the ledger", id)
	}
	return o
}

func cancelEventsOfAction(t *testing.T, ledger *storage.MemoryLedger, action string) []domain.Event {
	t.Helper()
	var out []domain.Event
	for _, e := range ledgerState(t, ledger).Events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func mutateOperation(t *testing.T, ledger *storage.MemoryLedger, id string, fn func(*domain.Operation)) {
	t.Helper()
	if err := ledger.Update(context.Background(), func(st *domain.State) error {
		o := st.Operations[id]
		fn(&o)
		st.Operations[id] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// --- property one: no committed allocation can be cancelled by this path -----

// A committed allocation is ownership, and ADR 0010 forbids removing it. The
// check that refuses it is committedHoldRefusal in cancelCheck, asked of
// whatever the ledger holds before anything looks at the operation at all, so
// that no later condition can become the reason a committed allocation is
// examined. Every state a committed allocation can be in is tried here, under
// every operation type it can carry and both statuses -- including a SUCCEEDED
// RESERVE, which would otherwise be refused by a different check and prove less.
func TestNoCommittedAllocationCanBeCancelled(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for _, state := range []string{domain.Reserved, domain.Active, domain.Quarantined, domain.Released} {
		for _, kind := range []string{reserveOperation, adoptOperation, bindOperation} {
			for _, status := range []string{operationPending, operationSucceeded} {
				t.Run(state+"_"+kind+"_"+status, func(t *testing.T) {
					ledger := storage.NewMemoryLedger()
					inventory := &safetyInventory{}
					recorder := &cancelRecorder{fail: map[int]error{}}
					inventory.cancel = recorder.hook(ledger, inventory)
					s := cancelService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now)})

					a := safetyAllocation(now, "alloc-committed", "vpc", "10.0.0.0/24", "", "")
					a.State = state
					putSafetyAllocation(t, ledger, a)
					if err := ledger.Update(context.Background(), func(st *domain.State) error {
						st.Operations["op-committed"] = domain.Operation{
							ID: "op-committed", Type: kind, Status: status, AllocationID: a.ID,
							TenantID: a.TenantID, DomainID: a.DomainID, CreatedAt: now, UpdatedAt: now,
						}
						// Even an open finding -- the one condition that
						// normally unlocks a cancel -- changes nothing.
						workerFinding(st, a, reservationStuckCode, "CRITICAL", now)
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					before := ledgerState(t, ledger)

					report, err := s.CancelReservation(context.Background(), cancellingPrincipal(), "op-committed")
					safetyAPIError(t, err, 409, "cancel_committed")
					if report != nil {
						t.Fatalf("a refused cancel returned a report: %#v", report)
					}
					// The runbook's answer for a committed allocation is
					// release, and the message has to say so.
					if !strings.Contains(err.Error(), "DELETE /v1/allocations/") {
						t.Fatalf("the refusal does not point at release: %v", err)
					}
					if len(recorder.calls) != 0 {
						t.Fatalf("a refused cancel asked the inventory to remove anything: %#v", recorder.calls)
					}
					if after := ledgerState(t, ledger); !reflect.DeepEqual(before, after) {
						t.Fatalf("a refused cancel wrote to the ledger: before=%#v after=%#v", before, after)
					}
				})
			}
		}
	}
}

// The same from the other side: a reservation that really committed, through
// the real path, is refused by the same check -- so the guarantee does not rest
// on how the committed row was built.
func TestARealReservationCannotBeCancelledOnceItHasCommitted(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{}
	recorder := &cancelRecorder{fail: map[int]error{}}
	inventory.cancel = recorder.hook(ledger, inventory)
	s := cancelService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now)})

	a, op, status, err := s.Reserve(context.Background(), cancellingPrincipal(), safetyRequest(cancelAllocationKey), cancelKey)
	if err != nil || status != 201 || a == nil || !a.Committed {
		t.Fatalf("reserve: allocation=%#v status=%d err=%v", a, status, err)
	}
	before := ledgerState(t, ledger)
	report, err := s.CancelReservation(context.Background(), cancellingPrincipal(), op.ID)
	safetyAPIError(t, err, 409, "cancel_committed")
	if report != nil {
		t.Fatalf("a refused cancel returned a report: %#v", report)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("a refused cancel asked the inventory to remove anything: %#v", recorder.calls)
	}
	if after := ledgerState(t, ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused cancel wrote to the ledger: before=%#v after=%#v", before, after)
	}
}

// Every other refusal of the fence, each naming the check that reaches it and
// each leaving the ledger and the inventory untouched.
func TestTheFenceRefusesEverythingItCannotCancel(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		operationID string
		principal   func() domain.Principal
		seed        func(t *testing.T, c cancellable)
		status      int
		code        string
		says        string
	}{
		"unknown_operation": {
			operationID: "op-nobody-has-heard-of", status: 404, code: "not_found",
		},
		"blank_operation_id": {
			operationID: " ", status: 404, code: "not_found",
		},
		"another_tenants_operation": {
			principal: tenantUPrincipal, status: 404, code: "not_found",
		},
		"an_operator_has_no_tenant": {
			principal: operatorPrincipal, status: 404, code: "not_found",
		},
		"an_adoption_is_adopt_abandons": {
			status: 409, code: "cancel_not_a_reservation", says: "adopt abandon",
			seed: func(t *testing.T, c cancellable) {
				mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) { o.Type = adoptOperation })
			},
		},
		"a_binding_verification_is_a_different_lifecycle": {
			status: 409, code: "cancel_not_a_reservation",
			seed: func(t *testing.T, c cancellable) {
				mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) { o.Type = bindOperation })
			},
		},
		"the_commit_won_the_race": {
			status: 409, code: "cancel_operation_succeeded", says: "DELETE /v1/allocations/",
			seed: func(t *testing.T, c cancellable) {
				mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) { o.Status = operationSucceeded })
			},
		},
		"a_terminal_operation_this_cancel_did_not_write": {
			status: 409, code: "cancel_operation_terminal",
			seed: func(t *testing.T, c cancellable) {
				mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) {
					o.Status = operationFailed
					o.Error = domain.Err(409, "something_else", "not this cancel's doing")
				})
			},
		},
		"an_abandons_fence_is_not_a_cancels": {
			// The abandon's own code on a RESERVE operation: a fence, but not
			// one this cancel wrote, so it is refused rather than resumed.
			status: 409, code: "cancel_operation_terminal",
			seed: func(t *testing.T, c cancellable) {
				mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) {
					o.Status = operationFailed
					o.Error = domain.Err(409, adoptionAbandonedCode, abandonedOperationMessage)
				})
			},
		},
		"no_finding_at_all": {
			status: 409, code: "reservation_not_stuck", says: reservationStuckCode,
			seed: func(t *testing.T, c cancellable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					delete(st.Findings, findingID(reservationStuckCode, c.allocation.ID))
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"a_resolved_finding_is_not_an_open_one": {
			status: 409, code: "reservation_not_stuck",
			seed: func(t *testing.T, c cancellable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					resolveFinding(st, c.allocation, reservationStuckCode)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"a_finding_about_something_else_does_not_count": {
			status: 409, code: "reservation_not_stuck",
			seed: func(t *testing.T, c cancellable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					delete(st.Findings, findingID(reservationStuckCode, c.allocation.ID))
					workerFinding(st, c.allocation, "reservation_aged", "WARNING", now)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"an_operation_whose_allocation_the_ledger_does_not_hold": {
			status: 409, code: "cancel_no_allocation",
			seed: func(t *testing.T, c cancellable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					delete(st.Allocations, c.allocation.ID)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := stuckReservation(t, now)
			if tc.seed != nil {
				tc.seed(t, c)
			}
			p := cancellingPrincipal()
			if tc.principal != nil {
				p = tc.principal()
			}
			operationID := tc.operationID
			if operationID == "" {
				operationID = c.operation.ID
			}
			before := ledgerState(t, c.ledger)

			report, err := c.service.CancelReservation(context.Background(), p, operationID)
			safetyAPIError(t, err, tc.status, tc.code)
			if report != nil {
				t.Fatalf("a refused cancel returned a report: %#v", report)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal does not say %q: %v", tc.says, err)
			}
			if len(c.recorder.calls) != 0 {
				t.Fatalf("a refused cancel asked the inventory to remove anything: %#v", c.recorder.calls)
			}
			if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused cancel wrote to the ledger: before=%#v after=%#v", before, after)
			}
		})
	}
}

// --- property three: nobody else can touch this tenant's hold ----------------

// The 404 is the whole refusal, and it has to be the refusal an existing read
// already gives: another tenant and an operator get exactly what an unknown id
// gets, so a cancel leaks nothing GET /v1/operations/{id} did not. Neither may
// read past the lookup either -- no inventory call, no ledger write, and the
// finding they were not allowed to act on is still open afterwards.
func TestNoOtherPrincipalCanCancelThisTenantsHold(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, principal := range map[string]func() domain.Principal{
		"another_tenant": tenantUPrincipal,
		"an_operator":    operatorPrincipal,
	} {
		t.Run(name, func(t *testing.T) {
			c := stuckReservation(t, now)
			p := principal()
			before := ledgerState(t, c.ledger)

			report, err := c.service.CancelReservation(context.Background(), p, c.operation.ID)
			safetyAPIError(t, err, 404, "not_found")
			if report != nil {
				t.Fatalf("a refused cancel returned a report: %#v", report)
			}
			// Indistinguishable from an unknown id: same status, same code,
			// same message.
			_, unknown := c.service.CancelReservation(context.Background(), p, "op-nobody-has-heard-of")
			if err.Error() != unknown.Error() {
				t.Fatalf("the refusal tells an existing id from an unknown one: %v versus %v", err, unknown)
			}
			if len(c.recorder.calls) != 0 {
				t.Fatalf("a refused cancel asked the inventory to remove anything: %#v", c.recorder.calls)
			}
			if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused cancel wrote to the ledger: before=%#v after=%#v", before, after)
			}
			if found := findingsWithCode(t, c.ledger, reservationStuckCode); len(found) != 1 || found[0].Status != "OPEN" {
				t.Fatalf("the finding changed under a refused cancel: %#v", found)
			}
			// And the owning tenant can still cancel it, so the refusal was
			// about who asked and nothing else.
			if _, err := c.cancelIt(t); err != nil {
				t.Fatalf("the owning tenant's cancel after the refusal: %v", err)
			}
		})
	}
}

// The tenant comparison alone would let two tenantless things match: an
// operator, whose Principal carries no tenant by construction (ADR 0011), and
// an operation whose TenantID is empty -- which nothing writes today but which
// a corrupted row, a hand-edited ledger or a future path that forgot to set it
// could produce. A principal with no tenant owns no operation, so the refusal
// does not rest on operations always having one.
func TestAPrincipalWithNoTenantOwnsNoOperation(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	mutateOperation(t, c.ledger, c.operation.ID, func(o *domain.Operation) { o.TenantID = "" })
	before := ledgerState(t, c.ledger)

	report, err := c.service.CancelReservation(context.Background(), operatorPrincipal(), c.operation.ID)
	safetyAPIError(t, err, 404, "not_found")
	if report != nil {
		t.Fatalf("an operator cancelled a tenantless operation: %#v", report)
	}
	// And so is a bare principal with neither tenant nor role.
	if _, err := c.service.CancelReservation(context.Background(), domain.Principal{Subject: "nobody"}, c.operation.ID); err == nil {
		t.Fatal("a principal with no tenant cancelled a tenantless operation")
	}
	if len(c.recorder.calls) != 0 {
		t.Fatalf("a refused cancel asked the inventory to remove anything: %#v", c.recorder.calls)
	}
	if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused cancel wrote to the ledger: before=%#v after=%#v", before, after)
	}
}

// --- property two: no object may claim an allocation the ledger does not hold

// The order is the property. The removal is asked while the ledger still holds
// the allocation, twice, and the row disappears only after the second answer.
func TestTheRemovalHappensBeforeTheDelete(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(c.recorder.calls) != 2 {
		t.Fatalf("want the removal and the pre-delete repeat, got %d calls: %#v", len(c.recorder.calls), c.recorder.calls)
	}
	for i, call := range c.recorder.calls {
		if !call.rowHeld {
			t.Fatalf("call %d reached the inventory after the allocation row was gone", i+1)
		}
		if call.allocation.ID != c.allocation.ID || call.operationID != c.operation.ID {
			t.Fatalf("call %d named %s/%s, want %s/%s", i+1, call.allocation.ID, call.operationID, c.allocation.ID, c.operation.ID)
		}
	}
	if !c.recorder.calls[0].marked || c.recorder.calls[1].marked {
		t.Fatalf("the first removal should have met the marked network and the second should not: %#v", c.recorder.calls)
	}
	if !report.Removed || !report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; held {
		t.Fatal("the allocation row outlived the cancel")
	}
	for _, n := range c.inventory.networks {
		if n.AllocationID == c.allocation.ID {
			t.Fatalf("a network still claims the deleted allocation: %#v", n)
		}
	}
}

// A removal that refuses, and one whose outcome is unknown, both stop the run
// before the delete: the allocation row stays, so nothing can be left claiming
// an allocation the ledger does not hold. What differs is only the advice --
// and the adapter's answered 404 between its two reads is on the refusal side,
// because a 404 is an answer (the H8a review note).
func TestARemovalThatDoesNotSucceedLeavesTheAllocationInPlace(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"ambiguous":        {errors.New("more than one prefix claims this allocation: 2 prefixes claim allocation alloc-1"), 409, "cancel_inventory_refused"},
		"another_ops":      {errors.New(`refusing to delete a prefix another operation wrote: prefix 42 carries platform_operation_id "op-other"`), 409, "cancel_inventory_refused"},
		"imported":         {errors.New("refusing to delete a prefix that carries the import tag: prefix 42 is tagged platform-ipam-imported"), 409, "cancel_inventory_refused"},
		"conflict":         {errors.New("conflicting NetBox object: prefix 42 changed between the read and the delete"), 409, "cancel_inventory_refused"},
		"read_back":        {errors.New("prefix is still present after the delete was sent: prefix 42 still reads back"), 409, "cancel_inventory_refused"},
		"answered_404":     {errors.New("uncertain NetBox reservation cancel: reading prefix 42: netbox GET: HTTP 404"), 409, "cancel_inventory_refused"},
		"timeout":          {context.DeadlineExceeded, 503, "cancel_uncertain"},
		"cancelled":        {context.Canceled, 503, "cancel_uncertain"},
		"network_timeout":  {&net.DNSError{Err: "i/o timeout", Name: "netbox", IsTimeout: true}, 503, "cancel_uncertain"},
		"unanswered_5xx":   {fmt.Errorf("%w: uncertain NetBox reservation cancel: deleting prefix 42: netbox DELETE: HTTP 503", domain.ErrInventoryUncertain), 503, "cancel_uncertain"},
		"unanswered_chain": {fmt.Errorf("%w: %w: deleting prefix 42", domain.ErrInventoryUncertain, context.DeadlineExceeded), 503, "cancel_uncertain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := stuckReservation(t, now)
			c.recorder.fail[1] = tc.err

			report, err := c.cancelIt(t)
			safetyAPIError(t, err, tc.status, tc.code)
			if !strings.Contains(err.Error(), "re-running this cancel is safe") {
				t.Fatalf("the refusal does not say a re-run is safe: %v", err)
			}
			if report == nil || report.Removed || report.Deleted {
				t.Fatalf("report after a failed removal: %#v", report)
			}
			if len(c.recorder.calls) != 1 {
				t.Fatalf("want the run stopped at the first removal, got %d calls", len(c.recorder.calls))
			}
			held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
			if !ok || held.Committed {
				t.Fatalf("a failed removal did not leave the uncommitted hold in place: %#v", held)
			}
			// The fence is in place, which is what makes the re-run converge.
			if o := cancelOperation(t, c.ledger, c.operation.ID); !cancelFenced(o) {
				t.Fatalf("the fence did not survive a failed removal: %#v", o)
			}
			// And the finding is still open, so nothing has been half-resolved.
			if found := findingsWithCode(t, c.ledger, reservationStuckCode); len(found) != 1 || found[0].Status != "OPEN" {
				t.Fatalf("a failed removal touched the finding: %#v", found)
			}
		})
	}
}

// The one window the ledger lock cannot close: a worker pass that began before
// the fence can create the prefix after the first removal. The second,
// idempotent removal immediately before the delete is what answers it -- where
// the markers have come back it removes them again and the cancel converges,
// and where the inventory refuses the second time the delete does not happen.
func TestTheMarkerReappearingBetweenRemovalAndDeleteIsAnsweredBeforeTheRowGoes(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	t.Run("the second removal deletes it again and the cancel converges", func(t *testing.T) {
		c := stuckReservation(t, now)
		remarked := domain.Network{
			ID: "netbox-again", CIDR: c.allocation.CIDR, AllocationID: c.allocation.ID,
			OperationID: c.operation.ID, Owned: true,
		}
		c.recorder.onCall = func(n int) {
			if n == 1 {
				// The in-flight worker pass lands its create here, after the
				// first removal has read the world and before the delete.
				c.inventory.networks = append(c.inventory.networks, remarked)
			}
		}

		report, err := c.cancelIt(t)
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if len(c.recorder.calls) != 2 || !c.recorder.calls[1].marked {
			t.Fatalf("the second removal did not meet the re-marked network: %#v", c.recorder.calls)
		}
		if !report.Deleted {
			t.Fatalf("the cancel did not converge: %#v", report)
		}
		for _, n := range c.inventory.networks {
			if n.AllocationID == c.allocation.ID {
				t.Fatalf("a network still claims the deleted allocation: %#v", n)
			}
		}
	})

	t.Run("a second removal that refuses keeps the allocation", func(t *testing.T) {
		c := stuckReservation(t, now)
		c.recorder.fail[2] = errors.New("refusing to delete a prefix another operation wrote")

		report, err := c.cancelIt(t)
		safetyAPIError(t, err, 409, "cancel_inventory_refused")
		if report == nil || !report.Removed || report.Deleted {
			t.Fatalf("report: %#v", report)
		}
		if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
			t.Fatal("the allocation was deleted although the inventory refused the pre-delete removal")
		}
	})
}

// The delete re-checks, under the ledger lock, that the operation is still the
// terminal one this cancel fenced. The removal runs outside any transaction, so
// something else can rewrite the operation in between; a row must not disappear
// on the strength of a fence that is no longer there.
func TestTheCancelsDeleteRefusesWhenTheFenceIsNoLongerTheOneItWrote(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, rewrite := range map[string]func(o *domain.Operation){
		"another_writers_error": func(o *domain.Operation) {
			o.Error = domain.Err(500, "something_else", "rewritten by another writer")
		},
		"the_operation_succeeded_after_all": func(o *domain.Operation) {
			o.Status, o.Error = operationSucceeded, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := stuckReservation(t, now)
			c.recorder.onCall = func(n int) {
				if n == 2 {
					mutateOperation(t, c.ledger, c.operation.ID, rewrite)
				}
			}

			report, err := c.cancelIt(t)
			safetyAPIError(t, err, 409, "cancel_state_changed")
			if report == nil || report.Deleted {
				t.Fatalf("report: %#v", report)
			}
			if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
				t.Fatal("the allocation was deleted although the fence it rested on had changed")
			}
		})
	}
}

// And the other re-check: the commit won the race between the removal and the
// delete, so the hold is now owned and nothing may remove it.
func TestTheDeleteRefusesAnAllocationThatCommittedWhileTheCancelRan(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.recorder.onCall = func(n int) {
		if n != 2 {
			return
		}
		if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
			a := st.Allocations[c.allocation.ID]
			a.Committed = true
			st.Allocations[a.ID] = a
			return nil
		}); err != nil {
			t.Fatalf("committing the allocation: %v", err)
		}
	}

	report, err := c.cancelIt(t)
	safetyAPIError(t, err, 409, "cancel_state_changed")
	if report == nil || report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
	if !ok || !held.Committed {
		t.Fatalf("the committed allocation did not survive the cancel: %#v", held)
	}
}

// --- the happy path ---------------------------------------------------------

func TestCancelWithdrawsAStuckReservation(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !report.Fenced || report.AlreadyFenced || !report.Removed || !report.Deleted || !report.FindingResolved {
		t.Fatalf("report: %#v", report)
	}
	if report.Actor != cancellingPrincipal().Subject {
		t.Fatalf("the report does not name who cancelled: %#v", report)
	}
	if report.Allocation.ID != c.allocation.ID || report.Allocation.CIDR != c.allocation.CIDR || report.Allocation.Committed {
		t.Fatalf("reported allocation: %#v", report.Allocation)
	}
	if report.Allocation.AllocationKey != cancelAllocationKey || report.Allocation.TenantID != "t" {
		t.Fatalf("reported allocation: %#v", report.Allocation)
	}
	if report.Operation.ID != c.operation.ID || report.Operation.Type != reserveOperation || report.Operation.Status != operationFailed {
		t.Fatalf("reported operation: %#v", report.Operation)
	}

	st := ledgerState(t, c.ledger)
	if _, held := st.Allocations[c.allocation.ID]; held {
		t.Fatal("the allocation row survived the cancel")
	}
	// The consumer's own idempotency record is forced to go with it: a record
	// pointing at a deleted allocation drives every later POST under that
	// Idempotency-Key into reserve's final 503 ledger_error for ever.
	for id, request := range st.Requests {
		if request.AllocationID == c.allocation.ID {
			t.Fatalf("idempotency record %s still points at the deleted allocation: %#v", id, request)
		}
	}
	// The operation row stays, terminal, and keeps answering.
	o := cancelOperation(t, c.ledger, c.operation.ID)
	if !cancelFenced(o) || o.Type != reserveOperation || o.Error.Message != cancelledOperationMessage {
		t.Fatalf("the operation is not the terminal one this cancel wrote: %#v", o)
	}
	got, err := c.service.Operation(context.Background(), cancellingPrincipal(), c.operation.ID)
	if err != nil || got.Status != operationFailed || got.Error == nil || got.Error.Code != reservationCancelledCode {
		t.Fatalf("GET /v1/operations/{id} after the cancel: %#v %v", got, err)
	}
	// The allocation answers 404 after, as it did before: an uncommitted hold
	// was never readable.
	if _, err := c.service.Get(context.Background(), cancellingPrincipal(), c.allocation.ID); err == nil {
		t.Fatal("the cancelled hold became readable")
	}
	// Audit events outlive the row they describe.
	actions := map[string]int{}
	for _, e := range allocationEvents(st, c.allocation.ID) {
		actions[e.Action]++
		if e.Action != reservationCancelledAction {
			continue
		}
		if e.Actor != cancellingPrincipal().Subject {
			t.Fatalf("the cancel event names actor %q", e.Actor)
		}
		for _, want := range []string{c.allocation.ID, c.allocation.CIDR, cancelAllocationKey, c.operation.ID, reservationStuckCode} {
			if !strings.Contains(e.Reason, want) {
				t.Fatalf("the cancel event's reason %q omits %q", e.Reason, want)
			}
		}
	}
	if actions[reserveKind.planned] != 1 || actions[reservationCancelledAction] != 1 || len(actions) != 2 {
		t.Fatalf("audit trail of the withdrawn reservation: %#v", actions)
	}
	// The finding is resolved, because nothing else ever would.
	found := findingsWithCode(t, c.ledger, reservationStuckCode)
	if len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("want the stuck finding resolved, got %#v", found)
	}
	findings, err := c.service.Findings(context.Background(), cancellingPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Code == reservationStuckCode && f.Status == "OPEN" {
			t.Fatalf("a cancelled reservation still shows an open finding: %#v", f)
		}
	}
	// And the operator, who could not cancel it, sees it resolved too.
	operatorSees, err := c.service.Findings(context.Background(), operatorPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range operatorSees {
		if f.Code == reservationStuckCode && f.Status == "OPEN" {
			t.Fatalf("the operator still sees an open finding for a cancelled reservation: %#v", f)
		}
	}
}

// --- interruption and convergence -------------------------------------------

// A re-run after the removal was interrupted resumes at the removal and
// finishes, and the audit trail says one thing happened rather than two: the
// fence recognises its own terminal operation by the code it stored there.
func TestARerunAfterAnInterruptedRemovalConvergesWithOneEvent(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if report.Fenced || !report.AlreadyFenced || !report.Removed || !report.Deleted {
		t.Fatalf("the re-run did not resume from the fence it found: %#v", report)
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("want exactly one cancel event in total, got %d: %#v", len(events), events)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; held {
		t.Fatal("the re-run did not delete the allocation")
	}
}

// Interrupted after the first removal but before the second: the re-run finds
// nothing marked, which the adapter reports as success with nothing to do, and
// converges.
func TestARerunAfterAnInterruptedSecondRemovalConverges(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.recorder.fail[2] = errors.New("the second removal was interrupted")

	if _, err := c.cancelIt(t); err == nil {
		t.Fatal("want the first run to stop at the second removal")
	}
	if !c.recorder.calls[0].marked {
		t.Fatalf("the first removal never met the marked network: %#v", c.recorder.calls)
	}
	delete(c.recorder.fail, 2)

	again, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.Fenced || !again.AlreadyFenced || !again.Deleted {
		t.Fatalf("the re-run did not converge: %#v", again)
	}
	if c.recorder.calls[2].marked {
		t.Fatalf("the re-run's first removal still met a marked network: %#v", c.recorder.calls)
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("want exactly one cancel event in total, got %d: %#v", len(events), events)
	}
}

// And after the delete itself was lost.
func TestARerunAfterAnInterruptedCancelDeleteConverges(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	// Arm the loss during the last call before the delete transaction.
	c.recorder.onCall = func(n int) {
		if n == 2 {
			c.wrapped.failUpdates = 1
		}
	}

	report, err := c.cancelIt(t)
	safetyAPIError(t, err, 503, "cancel_incomplete")
	if report == nil || !report.Removed || report.Deleted {
		t.Fatalf("report after a lost delete: %#v", report)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
		t.Fatal("a lost delete removed the allocation anyway")
	}

	c.recorder.onCall = nil
	again, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.Fenced || !again.AlreadyFenced || !again.Deleted {
		t.Fatalf("the re-run did not converge: %#v", again)
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("want exactly one cancel event in total, got %d: %#v", len(events), events)
	}
	if found := findingsWithCode(t, c.ledger, reservationStuckCode); len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("the re-run did not resolve the finding: %#v", found)
	}
}

// Two cancels of the same hold can overlap, because the consumer's client may
// simply retry. The second one's delete transaction finds the row already gone
// and treats that as convergence rather than as a conflict: refusing there
// would answer a failure for a withdrawal that did happen.
func TestADeleteThatFindsTheRowAlreadyGoneConverges(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.recorder.onCall = func(n int) {
		if n != 2 {
			return
		}
		// A concurrent run of the same cancel wins the race to the delete.
		if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
			delete(st.Allocations, c.allocation.ID)
			return nil
		}); err != nil {
			t.Fatalf("the concurrent delete: %v", err)
		}
	}

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("the cancel that lost the race to the delete: %v", err)
	}
	if !report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("want exactly one cancel event in total, got %d: %#v", len(events), events)
	}
}

// A DELETE is idempotent by definition, which is the reason ADR 0013 chose it.
// A repeat of a cancel that already finished is therefore success with nothing
// to do, and not a refusal: the tenant whose response was lost retries and
// learns that the withdrawal happened.
func TestASecondCancelAfterTheFirstFinishedIsSuccessWithNothingToDo(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	before := ledgerState(t, c.ledger)
	calls := len(c.recorder.calls)

	again, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if again.Fenced || !again.AlreadyFenced || !again.Deleted || again.Removed {
		t.Fatalf("a repeat of a finished cancel: %#v", again)
	}
	if again.Operation.ID != c.operation.ID || again.Allocation.ID != c.allocation.ID {
		t.Fatalf("the repeat does not name what was withdrawn: %#v", again)
	}
	if len(c.recorder.calls) != calls {
		t.Fatalf("a repeat of a finished cancel asked the inventory again: %#v", c.recorder.calls[calls:])
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("a repeat wrote a second event: %#v", events)
	}
	if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a repeat of a finished cancel wrote to the ledger: before=%#v after=%#v", before, after)
	}
}

// A second cancel inside the window resumes the first rather than refusing it,
// which is the same convergence a retry after a lost response needs.
func TestASecondCancelInsideTheWindowResumesRatherThanRefusing(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("the second cancel inside the window: %v", err)
	}
	if report.Fenced || !report.AlreadyFenced || !report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	if events := cancelEventsOfAction(t, c.ledger, reservationCancelledAction); len(events) != 1 {
		t.Fatalf("want exactly one cancel event in total, got %d: %#v", len(events), events)
	}
}

// The finding precondition is asked of a hold this cancel has not already
// fenced, and of no other. Refusing a resumed run because something closed the
// finding in between would strand the hold inside the window for ever, with an
// uncommitted row, no pending operation and every replay answering 409.
func TestAResumedCancelDoesNotAskForTheFindingAgain(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)
	if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
		delete(st.Findings, findingID(reservationStuckCode, c.allocation.ID))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	report, err := c.cancelIt(t)
	if err != nil {
		t.Fatalf("the resumed cancel was refused after its finding disappeared: %v", err)
	}
	if !report.Deleted || report.FindingResolved {
		t.Fatalf("report: %#v", report)
	}
}

// --- what the withdrawal gives back -----------------------------------------

// The allocation key is free again, and the consumer's own Idempotency-Key
// works: a retry under the same key and the same body is a fresh reservation --
// a new allocation id, not a replay of the failure and not the
// 503 ledger_error a surviving idempotency record would cause for ever.
func TestTheSameKeyReservesAgainAfterACancel(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.healthy()

	a, op, status, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest(cancelAllocationKey), cancelKey)
	if err != nil || status != 201 || a == nil {
		t.Fatalf("the retry under the same key: allocation=%#v status=%d err=%v", a, status, err)
	}
	if a.ID == c.allocation.ID {
		t.Fatal("the retry replayed the cancelled allocation")
	}
	if !a.Committed || op == nil || op.Status != operationSucceeded || op.ID == c.operation.ID {
		t.Fatalf("retried allocation: %#v operation=%#v", *a, op)
	}
	// chooseCIDR is a deterministic first fit, so the fresh reservation lands
	// on the same candidate -- a cancel cannot be used to re-roll a CIDR.
	if a.CIDR != c.allocation.CIDR {
		t.Fatalf("the fresh reservation landed on %s, want the same first fit %s", a.CIDR, c.allocation.CIDR)
	}
	// And the cancelled operation still answers, beside the new one.
	if got, err := c.service.Operation(context.Background(), cancellingPrincipal(), c.operation.ID); err != nil || got.Error == nil || got.Error.Code != reservationCancelledCode {
		t.Fatalf("the cancelled operation stopped answering: %#v %v", got, err)
	}
}

// The quota slot comes back with the row, because countTenant counts every
// allocation that is not RELEASED and never consults Committed.
func TestTheQuotaSlotComesBackWithTheRowAfterACancel(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.service.cfg.Pools[0].MaxAllocations = 1
	if got := countTenant(ledgerState(t, c.ledger), "t", "p"); got != 1 {
		t.Fatalf("the hold does not occupy a quota slot: %d", got)
	}

	// Fence only: the hold is still there, so the slot is still spent.
	c.fenceOnly(t)
	c.healthy()
	_, _, _, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("billing"), "billing-key")
	safetyAPIError(t, err, 409, "quota_exceeded")

	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := countTenant(ledgerState(t, c.ledger), "t", "p"); got != 0 {
		t.Fatalf("the quota slot did not come back: %d", got)
	}
	if _, _, status, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("billing"), "billing-key"); err != nil || status != 201 {
		t.Fatalf("reservation after the cancel: status=%d err=%v", status, err)
	}
}

// The availability half of the fix: the pending operation fenced the whole
// overlap domain, and the fence alone -- before the removal and the delete --
// releases it, because the operation is no longer pending.
func TestTheOverlapDomainAnswersReservationsAgainAfterTheCancelsFence(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.healthy()
	_, _, _, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	safetyAPIError(t, err, 503, "domain_busy")

	c.fenceOnly(t)

	a, _, status, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	if err != nil || status != 201 || a == nil {
		t.Fatalf("the domain is still fenced after the fence: allocation=%#v status=%d err=%v", a, status, err)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
		t.Fatal("the fence deleted the hold")
	}
}

// What the fence does not release is the hold's own CIDR: chooseCIDR blocks on
// any allocation with Committed false before it looks at the state, which is
// why ADR 0013 says the row has to go rather than move to some terminal state.
func TestTheFencedReservationBlocksItsCIDRUntilTheRowIsGone(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)
	c.healthy()

	during, _, status, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("second-key"), "second-http-key")
	if err != nil || status != 201 || during == nil {
		t.Fatalf("a reservation during the window: allocation=%#v status=%d err=%v", during, status, err)
	}
	if during.CIDR == c.allocation.CIDR {
		t.Fatalf("the fenced hold stopped blocking its own CIDR before its row was gone: %s", during.CIDR)
	}

	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	after, _, status, err := c.service.Reserve(context.Background(), cancellingPrincipal(), safetyRequest("third-key"), "third-http-key")
	if err != nil || status != 201 || after == nil {
		t.Fatalf("a reservation after the row is gone: allocation=%#v status=%d err=%v", after, status, err)
	}
	if after.CIDR != c.allocation.CIDR {
		t.Fatalf("the cancelled hold's CIDR is still blocked: got %s, want %s", after.CIDR, c.allocation.CIDR)
	}
}

// --- the window between the fence and the delete ----------------------------

// Between the two writes the hold is uncommitted with no pending operation, and
// every replay path would read that as "pending" and answer 202 with no
// operation -- which the transport then turns into 503 dependency_unavailable.
// All of them say the same honest thing instead, and none of them writes
// anything.
func TestTheCancelWindowRefusesEveryReplayPath(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)
	c.healthy()
	before := ledgerState(t, c.ledger)
	ctx := context.Background()
	p := cancellingPrincipal()

	for name, run := range map[string]func() error{
		// The consumer's retry under their own Idempotency-Key: reserve's
		// record scan, in the read and again in the write transaction.
		"same_idempotency_key": func() error {
			_, _, _, err := c.service.Reserve(ctx, p, safetyRequest(cancelAllocationKey), cancelKey)
			return err
		},
		// A new Idempotency-Key for the same allocation key: reserve's
		// tenant-and-key scan, in the read and again in the write transaction.
		"new_idempotency_key": func() error {
			_, _, _, err := c.service.Reserve(ctx, p, safetyRequest(cancelAllocationKey), "a-different-http-key")
			return err
		},
	} {
		err := run()
		safetyAPIError(t, err, 409, "reservation_cancelling")
		var api *domain.APIError
		if errors.As(err, &api) && !api.Retryable {
			t.Fatalf("%s: the window refusal is not retryable: %#v", name, api)
		}
	}
	if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused replay wrote to the ledger: before=%#v after=%#v", before, after)
	}

	// Once the cancel has finished the window is over, and the key answers
	// ordinarily again.
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if _, _, status, err := c.service.Reserve(ctx, p, safetyRequest(cancelAllocationKey), cancelKey); err != nil || status != 201 {
		t.Fatalf("reservation after the window closed: status=%d err=%v", status, err)
	}
}

// The two windows answer for themselves and are never confused: a fenced ADOPT
// still says adoption_abandoning, byte for byte as package H2b left it, and a
// fenced RESERVE says reservation_cancelling.
func TestTheTwoWithdrawalWindowsNeverAnswerForEachOther(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	reserve := stuckReservation(t, now)
	reserve.fenceOnly(t)
	reserve.healthy()
	_, _, _, err := reserve.service.Reserve(ctx, cancellingPrincipal(), safetyRequest(cancelAllocationKey), cancelKey)
	safetyAPIError(t, err, 409, "reservation_cancelling")
	if !strings.Contains(err.Error(), "a cancel of this reservation is in progress; retry when it has finished") {
		t.Fatalf("the cancel window's message: %v", err)
	}

	adopt := stuckAdoption(t, time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	adopt.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := adopt.abandonIt(t); err == nil {
		t.Fatal("want the abandon to stop at the clear")
	}
	_, _, _, err = adopt.service.Reserve(ctx, safetyPrincipal(), safetyRequest("orders"), "team-http-key")
	safetyAPIError(t, err, 409, "adoption_abandoning")
	if !strings.Contains(err.Error(), "an abandon of this adoption is in progress; retry when it has finished") {
		t.Fatalf("the abandon window's message changed: %v", err)
	}
}

// A commit closure that finds its operation fenced must answer the operation's
// real status, never the constant 202: the 202 response is a PendingOperation
// and that schema pins status to PENDING, so a 202 over an operation that is
// already FAILED is both a lie and a schema violation (ADR 0013).
func TestACommitClosureThatFindsItsOperationFencedDoesNotAnswer202(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	for name, stopAfterFence := range map[string]bool{
		"the_fence_landed_and_the_row_is_still_there": true,
		"the_whole_cancel_finished":                   false,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			memory := storage.NewMemoryLedger()
			inventory := &safetyInventory{}
			observer := &safetyObserver{observation: safetyObservation(now)}
			s := cancelService(now, memory, inventory, observer)
			recorder := &cancelRecorder{fail: map[int]error{}}
			inventory.cancel = recorder.hook(memory, inventory)
			if stopAfterFence {
				recorder.fail[1] = errors.New("the removal was interrupted")
			}

			// The request's own synchronous Ensure is where a cancel's fence
			// can land: reserve calls the adapter outside any transaction and
			// opens its commit transaction afterwards.
			inventory.onEnsure = func() {
				inventory.onEnsure = nil
				var hold domain.Allocation
				var operationID string
				if err := memory.Update(ctx, func(st *domain.State) error {
					for _, a := range st.Allocations {
						hold = a
					}
					for _, o := range st.Operations {
						operationID = o.ID
					}
					// The platform's own verdict, which is what unlocks a
					// cancel. Seeded directly because this test is about the
					// commit closure and not about the worker's classification.
					workerFinding(st, hold, reservationStuckCode, "CRITICAL", now)
					return nil
				}); err != nil {
					t.Fatalf("seeding the finding: %v", err)
				}
				_, cancelErr := s.CancelReservation(ctx, cancellingPrincipal(), operationID)
				if cancelErr != nil && !stopAfterFence {
					t.Fatalf("the racing cancel: %v", cancelErr)
				}
			}

			a, op, status, err := s.Reserve(ctx, cancellingPrincipal(), safetyRequest(cancelAllocationKey), cancelKey)
			safetyAPIError(t, err, 409, "reservation_cancelling")
			if a != nil || op != nil || status != 0 {
				t.Fatalf("a fenced commit answered %d with allocation=%#v operation=%#v", status, a, op)
			}
			var api *domain.APIError
			if errors.As(err, &api) && !api.Retryable {
				t.Fatalf("the answer is not retryable: %#v", api)
			}
		})
	}
}

// --- the worker -------------------------------------------------------------

// After the fence the operation is no longer pending, so pendingRecoveryJobs
// stops selecting it: a worker pass in the window creates nothing, commits
// nothing and raises no second finding.
func TestTheWorkerLeavesAFencedReservationAlone(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	c.fenceOnly(t)
	c.healthy()
	c.inventory.ensures = 0

	if err := c.service.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if c.inventory.ensures != 0 {
		t.Fatalf("a worker pass tried to finish a fenced reservation: %d Ensure calls", c.inventory.ensures)
	}
	held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
	if !ok || held.Committed {
		t.Fatalf("a worker pass committed a fenced reservation: %#v", held)
	}
	if o := cancelOperation(t, c.ledger, c.operation.ID); !cancelFenced(o) {
		t.Fatalf("a worker pass changed the fenced operation: %#v", o)
	}
	if found := findingsWithCode(t, c.ledger, reservationStuckCode); len(found) != 1 {
		t.Fatalf("want the one finding the recovery raised, got %#v", found)
	}
	// And the cancel still converges after the worker has had its pass.
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("the cancel after a worker pass: %v", err)
	}
}

// And after the cancel: the worker must not resurrect what a tenant withdrew.
func TestTheWorkerDoesNotResurrectACancelledReservation(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	c := stuckReservation(t, now)
	if _, err := c.cancelIt(t); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c.healthy()
	c.inventory.ensures = 0

	for i := 0; i < 2; i++ {
		if err := c.service.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
	if c.inventory.ensures != 0 {
		t.Fatalf("a worker pass asked the inventory about a cancelled reservation: %d Ensure calls", c.inventory.ensures)
	}
	st := ledgerState(t, c.ledger)
	if _, held := st.Allocations[c.allocation.ID]; held {
		t.Fatal("a worker pass recreated the cancelled allocation")
	}
	if o := cancelOperation(t, c.ledger, c.operation.ID); !cancelFenced(o) {
		t.Fatalf("a worker pass changed the cancelled operation: %#v", o)
	}
	found := findingsWithCode(t, c.ledger, reservationStuckCode)
	if len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("a worker pass reopened the resolved finding: %#v", found)
	}
}

// --- structural guarantees --------------------------------------------------

// Inventory.CancelReservation is the only call in this project that destroys an
// inventory object found by a marker rather than by a stored inventory id, so
// where it may be reached from is part of the guarantee rather than an
// implementation detail. Two calls, both in cancel.go: the removal, and the
// repeat immediately before the delete that answers a worker pass which
// re-created the object. A third caller would be a route to deleting a prefix
// that neither fenced an operation nor deleted a hold, so this count is raised
// only together with the argument for raising it, and never loosened into a
// range.
func TestInventoryCancelReservationIsCalledOnlyByTheCancelFile(t *testing.T) {
	calls := 0
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "CancelReservation" {
				return true
			}
			receiver, ok := sel.X.(*ast.SelectorExpr)
			if !ok || receiver.Sel.Name != "inventory" {
				t.Fatalf("%s calls CancelReservation on something other than the inventory port", name)
			}
			if name != "cancel.go" {
				t.Fatalf("%s deletes an inventory object; only cancel.go may", name)
			}
			calls++
			return true
		})
	}
	if calls != 2 {
		t.Fatalf("want exactly two Inventory.CancelReservation calls (the removal and the pre-delete repeat), got %d", calls)
	}
}

// The service's own cancel had no caller outside a test until package H8c
// wired it to the one place ADR 0013 permits: internal/transport's handler
// for DELETE /v1/operations/{operation_id}. This test used to pin the count
// at zero (TestServiceCancelReservationHasNoNonTestCallerYet) so that the
// package which added the first caller would have to come here and say so;
// H8c does, by turning it into this test, deliberately, rather than
// retiring it -- the invariant that matters from here on is not "nobody
// calls this" but "only the transport does, and only once": never a
// process-mode command, because ADR 0013 makes this the consumer's own exit
// and not an operator's, and never a second call site that could race or
// duplicate the fence-remove-delete sequence cancel.go performs.
func TestServiceCancelReservationsOnlyNonTestCallerIsTheTransport(t *testing.T) {
	found := map[string]int{}
	for _, dir := range []string{".", "../transport", "../cli", "../adoptcmd", "../onboardcmd", "../../cmd/platform-ipam"} {
		for name, f := range packageFiles(t, dir) {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "CancelReservation" {
					return true
				}
				// The port's own method is allowed where the sibling test above
				// pins it (cancel.go's two calls through s.inventory); anything
				// else naming CancelReservation is a caller of the service's
				// own method.
				if receiver, ok := sel.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "inventory" {
					return true
				}
				found[dir+"/"+name]++
				return true
			})
		}
	}
	if len(found) != 1 || found["../transport/http.go"] != 1 {
		t.Fatalf("Service.CancelReservation must have exactly one non-test caller, internal/transport/http.go's DELETE handler (ADR 0013: the consumer's own exit, over HTTP, never a process-mode command); got %#v", found)
	}
}

// ADR 0013's first property has to be written against the source and not only
// against behaviour, "for the reason ADR 0012 gives: the failure it guards
// against is a later change that helpfully relaxes the check". The source-level
// test now covers two entry points instead of one: in both abandonCheck and
// cancelCheck the committed refusal is one shared call, and it is made before
// anything looks at the operation's type, its status or the findings -- so no
// later condition can become the reason a committed allocation is examined.
func TestBothWithdrawalsRefuseACommittedAllocationBeforeAnythingElse(t *testing.T) {
	want := map[string]bool{"abandonCheck": true, "cancelCheck": true}
	seen := map[string]int{}
	// The conditions that must not be able to precede the committed refusal:
	// the operation's shape, and the finding that unlocks a cancel.
	later := map[string]bool{"Type": true, "Status": true, "Findings": true, "findingID": true}
	total := 0
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "committedHoldRefusal" {
				total++
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !want[fn.Name.Name] || fn.Body == nil {
				return true
			}
			committed := 0
			var committedPos token.Pos
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "committedHoldRefusal" {
					committed++
					committedPos = call.Pos()
				}
				return true
			})
			if committed != 1 {
				t.Fatalf("%s in %s calls committedHoldRefusal %d times; the committed refusal must be one shared call", fn.Name.Name, name, committed)
			}
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				switch x := m.(type) {
				case *ast.SelectorExpr:
					if later[x.Sel.Name] && x.Pos() < committedPos {
						t.Errorf("%s in %s reads %s before it refuses a committed allocation", fn.Name.Name, name, x.Sel.Name)
					}
				case *ast.Ident:
					if later[x.Name] && x.Pos() < committedPos {
						t.Errorf("%s in %s reaches %s before it refuses a committed allocation", fn.Name.Name, name, x.Name)
					}
				}
				return true
			})
			seen[fn.Name.Name]++
			return false
		})
	}
	for name := range want {
		if seen[name] != 1 {
			t.Fatalf("%s was not found exactly once in this package's sources; the guard is checking nothing", name)
		}
	}
	// And nothing else in the package may make that refusal, because a third
	// caller would be a third place the check could be relaxed.
	if total != 2 {
		t.Fatalf("want exactly two callers of committedHoldRefusal, one per withdrawal, got %d", total)
	}
}
