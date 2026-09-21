package service

// Abandon is the one operation in this project that removes a ledger row, so
// these tests are written around the two properties ADR 0012 says must never
// regress. No committed allocation can ever be abandoned, whatever state it is
// in and however it came to be owned. And no abandon can leave an inventory
// object claiming an allocation the ledger does not hold, which is a statement
// about the ORDER of three steps and not only about their outcomes -- so the
// inventory double here records when each clear happened and what the ledger
// held at that moment, and every refusal asserts that nothing was written and
// that the inventory was never asked to clear anything.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/storage"
)

const (
	abandoningOperator = "ops:bob"
	abandonReason      = "the reviewed record named the wrong VPC"
)

// abandonedNetwork is adoptedNetwork with the three values an adoption
// overwrites, so that every test here also carries the prior inventory the
// clear has to hand back (ADR 0012, PriorInventory).
func abandonedNetwork() domain.Network {
	n := adoptedNetwork()
	n.Status, n.AWSAccountID, n.AWSRegion = "deprecated", "123456789012", "eu"
	return n
}

func abandonedRecord() domain.AdoptionRecord {
	r := reviewedRecord()
	r.PriorStatus, r.PriorAccountID, r.PriorRegion = "deprecated", "123456789012", "eu"
	return r
}

// abandonCall is one call to Inventory.AbandonAdoption, with the two facts a
// test cannot reconstruct afterwards: whether the ledger still held the
// allocation at that moment, and whether anything in the inventory still
// claimed it.
type abandonCall struct {
	allocation  domain.Allocation
	operationID string
	prior       domain.PriorInventory
	rowHeld     bool
	marked      bool
}

// abandonRecorder is the inventory side of every test in this file. onCall runs
// after the call has been recorded and before it answers, which is the only
// place a test can change the world between the clear and the delete.
type abandonRecorder struct {
	calls  []abandonCall
	fail   map[int]error
	onCall func(n int)
}

func (r *abandonRecorder) hook(ledger *storage.MemoryLedger, inventory *safetyInventory) func(domain.Allocation, string, domain.PriorInventory) error {
	return func(a domain.Allocation, operationID string, prior domain.PriorInventory) error {
		call := abandonCall{allocation: a, operationID: operationID, prior: prior}
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
		if r.onCall != nil {
			r.onCall(n)
		}
		return r.fail[n]
	}
}

// abandonLedger loses a chosen number of Updates from the moment a test arms
// it. The memory ledger cannot be made to lose a write on its own, and losing
// the delete is exactly the interruption ADR 0012 says a re-run must converge
// from, so the failure is injected here rather than simulated by calling the
// steps' internals.
type abandonLedger struct {
	domain.Ledger
	failUpdates int
}

func (l *abandonLedger) Update(ctx context.Context, fn func(*domain.State) error) error {
	if l.failUpdates > 0 {
		l.failUpdates--
		return errors.New("the ledger write was lost")
	}
	return l.Ledger.Update(ctx, fn)
}

// abandonService is adoptService with the ledger widened to the port, so that a
// test can put abandonLedger in front of the memory ledger. Nothing else
// differs: the same pool, the same clock.
func abandonService(now time.Time, ledger domain.Ledger, inventory *safetyInventory, observer *safetyObserver) *Service {
	cfg := safetyConfig()
	cfg.Pools[0].AllowedPrefixLengths = []int{24, 25}
	cfg.SubnetPolicy.AllowedPrefixLengths = []int{26}
	s := New(cfg, ledger, inventory, observer)
	s.SetClock(func() time.Time { return now })
	return s
}

// abandonable is a real adoption that can never commit: planned through the
// service's own path, so the hold, the pending ADOPT operation, the reviewed
// record and the ADOPT_PLANNED event are exactly what a stuck adoption leaves
// rather than rows a test wrote by hand.
type abandonable struct {
	service    *Service
	ledger     *storage.MemoryLedger
	wrapped    *abandonLedger
	inventory  *safetyInventory
	observer   *safetyObserver
	recorder   *abandonRecorder
	allocation domain.Allocation
	operation  domain.Operation
}

func stuckAdoption(t *testing.T, now time.Time) abandonable {
	t.Helper()
	memory := storage.NewMemoryLedger()
	wrapped := &abandonLedger{Ledger: memory}
	inventory := &safetyInventory{
		networks: []domain.Network{abandonedNetwork()}, adoptID: adoptedNetworkID,
		adoptErr: errors.New("the adapter never answered"),
	}
	observer := &safetyObserver{observation: safetyObservation(now, adoptedResource())}
	s := abandonService(now, wrapped, inventory, observer)

	a, op, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 202 || a == nil || a.Committed || op == nil || op.Status != operationPending {
		t.Fatalf("planning an adoption that then cannot commit: allocation=%#v operation=%#v status=%d err=%v", a, op, status, err)
	}
	if op.Adoption == nil || *op.Adoption != abandonedRecord() {
		t.Fatalf("the pending operation does not carry the reviewed record: %#v", op.Adoption)
	}
	recorder := &abandonRecorder{fail: map[int]error{}}
	inventory.abandon = recorder.hook(memory, inventory)
	// The planning call was not the abandon's. A test that counts adapter calls
	// counts the ones the abandon made.
	inventory.adopts = nil
	return abandonable{s, memory, wrapped, inventory, observer, recorder, *a, *op}
}

// abandonIt runs the real thing with this file's operator and reason.
func (c abandonable) abandonIt(t *testing.T) (*AbandonReport, error) {
	t.Helper()
	return c.service.AbandonAdoption(context.Background(), c.allocation.ID, "", abandoningOperator, abandonReason)
}

// stuck raises the adoption_stuck finding the way the worker raises it in
// production: a recovery pass that meets an adapter refusal.
func (c abandonable) stuck(t *testing.T) {
	t.Helper()
	c.inventory.adoptErr = errors.New("the reviewed prefix is no longer there")
	if err := c.service.recoverAdoptions(context.Background()); err != nil {
		t.Fatalf("recovery pass: %v", err)
	}
	if found := findingsWithCode(t, c.ledger, adoptionStuckCode); len(found) != 1 || found[0].Status != "OPEN" {
		t.Fatalf("want one open adoption_stuck finding, got %#v", found)
	}
	c.inventory.adopts = nil
}

func abandonOperation(t *testing.T, ledger *storage.MemoryLedger, allocationID string) domain.Operation {
	t.Helper()
	var out domain.Operation
	found := 0
	for _, o := range ledgerState(t, ledger).Operations {
		if o.AllocationID == allocationID {
			out, found = o, found+1
		}
	}
	if found != 1 {
		t.Fatalf("want exactly one operation for allocation %s, got %d", allocationID, found)
	}
	return out
}

func abandonEventsOfAction(t *testing.T, ledger *storage.MemoryLedger, action string) []domain.Event {
	t.Helper()
	var out []domain.Event
	for _, e := range ledgerState(t, ledger).Events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// --- property one: no committed allocation can ever be abandoned ------------

// A committed allocation is ownership, and ADR 0010 forbids removing it. The
// check that refuses it is a.Committed in abandonCheck, asked before anything
// else so that no later condition can become the reason a committed allocation
// is examined at all. Every state a committed allocation can be in is tried
// here, adopted and ordinarily reserved, through the real run and the dry run.
func TestNoCommittedAllocationCanEverBeAbandoned(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for _, state := range []string{domain.Reserved, domain.Active, domain.Quarantined, domain.Released} {
		for _, kind := range []string{adoptOperation, reserveOperation} {
			t.Run(state+"_"+kind, func(t *testing.T) {
				ledger := storage.NewMemoryLedger()
				inventory := &safetyInventory{networks: []domain.Network{abandonedNetwork()}}
				recorder := &abandonRecorder{fail: map[int]error{}}
				inventory.abandon = recorder.hook(ledger, inventory)
				s := abandonService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now, adoptedResource())})

				a := safetyAllocation(now, "alloc-committed", "vpc", adoptedCIDR, "", "")
				a.State = state
				putSafetyAllocation(t, ledger, a)
				record := abandonedRecord()
				var carried *domain.AdoptionRecord
				if kind == adoptOperation {
					carried = &record
				}
				if err := ledger.Update(context.Background(), func(st *domain.State) error {
					st.Operations["op-committed"] = domain.Operation{
						ID: "op-committed", Type: kind, Status: operationSucceeded, AllocationID: a.ID,
						TenantID: a.TenantID, DomainID: a.DomainID, Adoption: carried,
						CreatedAt: now, UpdatedAt: now,
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				before := ledgerState(t, ledger)

				for name, run := range map[string]func() (*AbandonReport, error){
					"abandon": func() (*AbandonReport, error) {
						return s.AbandonAdoption(context.Background(), a.ID, "", abandoningOperator, abandonReason)
					},
					"dry_run": func() (*AbandonReport, error) {
						return s.PlanAbandonAdoption(context.Background(), a.ID, "", abandoningOperator, abandonReason)
					},
				} {
					report, err := run()
					safetyAPIError(t, err, 409, "abandon_committed")
					if report != nil {
						t.Fatalf("%s: a refused abandon returned a report: %#v", name, report)
					}
					// The runbook's answer for a committed allocation is
					// release, and the message has to say so.
					if !strings.Contains(err.Error(), "DELETE /v1/allocations/") {
						t.Fatalf("%s: the refusal does not point at release: %v", name, err)
					}
				}
				if len(recorder.calls) != 0 {
					t.Fatalf("a refused abandon asked the inventory to clear: %#v", recorder.calls)
				}
				if after := ledgerState(t, ledger); !reflect.DeepEqual(before, after) {
					t.Fatalf("a refused abandon wrote to the ledger: before=%#v after=%#v", before, after)
				}
			})
		}
	}
}

// The same from the other side: an adoption that really committed, through the
// real path, is refused by the same check -- so the guarantee does not rest on
// how the committed row was built.
func TestARealAdoptionCannotBeAbandonedOnceItHasCommitted(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	s, ledger, inventory := adoptFixture(now, []domain.Network{abandonedNetwork()}, []domain.Resource{adoptedResource()})
	recorder := &abandonRecorder{fail: map[int]error{}}
	inventory.abandon = recorder.hook(ledger, inventory)

	a, _, status, err := s.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 || !a.Committed {
		t.Fatalf("adopt: allocation=%#v status=%d err=%v", a, status, err)
	}
	before := ledgerState(t, ledger)
	_, err = s.AbandonAdoption(context.Background(), a.ID, "", abandoningOperator, abandonReason)
	safetyAPIError(t, err, 409, "abandon_committed")
	if len(recorder.calls) != 0 {
		t.Fatalf("a refused abandon asked the inventory to clear: %#v", recorder.calls)
	}
	if after := ledgerState(t, ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused abandon wrote to the ledger: before=%#v after=%#v", before, after)
	}
}

// Every other refusal of the fence, each naming the check that reaches it and
// each leaving the ledger and the inventory untouched.
func TestTheFenceRefusesEverythingItCannotAbandon(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		allocationID string
		operationID  string
		operator     string
		reason       string
		seed         func(t *testing.T, c abandonable)
		status       int
		code         string
	}{
		"no_operator": {
			operator: "", reason: abandonReason, status: 422, code: "abandon_operator_required",
		},
		"blank_reason": {
			operator: abandoningOperator, reason: "   ", status: 422, code: "abandon_reason_required",
		},
		"no_allocation_id": {
			allocationID: " ", operator: abandoningOperator, reason: abandonReason,
			status: 422, code: "abandon_allocation_required",
		},
		"unknown_allocation": {
			allocationID: "alloc-nobody-has-heard-of", operator: abandoningOperator, reason: abandonReason,
			status: 404, code: "abandon_unknown_allocation",
		},
		"operation_id_does_not_match": {
			operationID: "op-somebody-elses", operator: abandoningOperator, reason: abandonReason,
			status: 409, code: "abandon_operation_mismatch",
		},
		"no_operation": {
			operator: abandoningOperator, reason: abandonReason, status: 409, code: "abandon_no_operation",
			seed: func(t *testing.T, c abandonable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					delete(st.Operations, c.operation.ID)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"a_reservation_is_out_of_scope": {
			operator: abandoningOperator, reason: abandonReason, status: 409, code: "abandon_not_an_adoption",
			seed: func(t *testing.T, c abandonable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					o := st.Operations[c.operation.ID]
					o.Type = reserveOperation
					st.Operations[o.ID] = o
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"the_commit_won_the_race": {
			operator: abandoningOperator, reason: abandonReason, status: 409, code: "abandon_operation_succeeded",
			seed: func(t *testing.T, c abandonable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					o := st.Operations[c.operation.ID]
					o.Status = operationSucceeded
					st.Operations[o.ID] = o
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"a_terminal_operation_this_abandon_did_not_write": {
			operator: abandoningOperator, reason: abandonReason, status: 409, code: "abandon_operation_terminal",
			seed: func(t *testing.T, c abandonable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					o := st.Operations[c.operation.ID]
					o.Status = operationFailed
					o.Error = domain.Err(409, "something_else", "not this abandon's doing")
					st.Operations[o.ID] = o
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		"several_operations_name_the_allocation": {
			operator: abandoningOperator, reason: abandonReason, status: 409, code: "abandon_ambiguous_operation",
			seed: func(t *testing.T, c abandonable) {
				if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
					second := st.Operations[c.operation.ID]
					second.ID = "op-second"
					st.Operations[second.ID] = second
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := stuckAdoption(t, now)
			if tc.seed != nil {
				tc.seed(t, c)
			}
			allocationID := tc.allocationID
			if allocationID == "" {
				allocationID = c.allocation.ID
			}
			before := ledgerState(t, c.ledger)
			report, err := c.service.AbandonAdoption(context.Background(), allocationID, tc.operationID, tc.operator, tc.reason)
			safetyAPIError(t, err, tc.status, tc.code)
			if report != nil {
				t.Fatalf("a refused abandon returned a report: %#v", report)
			}
			// The dry run reaches every one of these verdicts too: it runs the
			// fence's checks rather than a copy of them.
			planReport, planErr := c.service.PlanAbandonAdoption(context.Background(), allocationID, tc.operationID, tc.operator, tc.reason)
			if planErr == nil {
				t.Fatalf("the dry run accepted what the fence refused with %s: %#v", tc.code, planReport)
			}
			safetyAPIError(t, planErr, tc.status, tc.code)
			if len(c.recorder.calls) != 0 {
				t.Fatalf("a refused abandon asked the inventory to clear: %#v", c.recorder.calls)
			}
			if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused abandon wrote to the ledger: before=%#v after=%#v", before, after)
			}
		})
	}
}

// --- property two: no object may claim an allocation the ledger does not hold

// The order is the property. The clear is asked while the ledger still holds
// the allocation, twice, and the row disappears only after the second answer.
func TestTheClearHappensBeforeTheDelete(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)

	report, err := c.abandonIt(t)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if len(c.recorder.calls) != 2 {
		t.Fatalf("want the clear and the pre-delete repeat, got %d calls: %#v", len(c.recorder.calls), c.recorder.calls)
	}
	for i, call := range c.recorder.calls {
		if !call.rowHeld {
			t.Fatalf("call %d reached the inventory after the allocation row was gone", i+1)
		}
		if call.allocation.ID != c.allocation.ID || call.operationID != c.operation.ID {
			t.Fatalf("call %d named %s/%s, want %s/%s", i+1, call.allocation.ID, call.operationID, c.allocation.ID, c.operation.ID)
		}
	}
	if !report.Cleared || !report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; held {
		t.Fatal("the allocation row outlived the abandon")
	}
}

// A clear that refuses, and one whose outcome is unknown, both stop the run
// before the delete: the allocation row stays, so nothing can be left claiming
// an allocation the ledger does not hold. What differs is only the advice.
func TestAClearThatDoesNotSucceedLeavesTheAllocationInPlace(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"refusal":                {errors.New("more than one prefix claims this allocation"), 409, "abandon_inventory_refused"},
		"timeout":                {context.DeadlineExceeded, 503, "abandon_uncertain"},
		"wrapped_by_the_adapter": {wrappedUncertainAbandon(), 503, "abandon_uncertain"},
		// No timeout anywhere in the chain: a 5xx from the inventory, which the
		// adapter marks with the domain's own sentinel. Without it this was
		// reported as a refusal, and the operator was told to act on a blip.
		"unanswered_5xx": {fmt.Errorf("%w: clearing prefix 42: netbox PATCH: HTTP 503", domain.ErrInventoryUncertain), 503, "abandon_uncertain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := stuckAdoption(t, now)
			c.recorder.fail[1] = tc.err

			report, err := c.abandonIt(t)
			safetyAPIError(t, err, tc.status, tc.code)
			if !strings.Contains(err.Error(), "re-running this abandon is safe") {
				t.Fatalf("the refusal does not say a re-run is safe: %v", err)
			}
			if report == nil || report.Cleared || report.Deleted {
				t.Fatalf("report after a failed clear: %#v", report)
			}
			if len(c.recorder.calls) != 1 {
				t.Fatalf("want the run stopped at the first clear, got %d calls", len(c.recorder.calls))
			}
			held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
			if !ok || held.Committed {
				t.Fatalf("a failed clear did not leave the uncommitted hold in place: %#v", held)
			}
			// The fence is in place, which is what makes the re-run converge.
			if o := abandonOperation(t, c.ledger, c.allocation.ID); !abandonFenced(o) {
				t.Fatalf("the fence did not survive a failed clear: %#v", o)
			}
		})
	}
}

// wrappedUncertainAbandon is the shape internal/netbox/abandon.go returns for a
// lost call: its own sentinel wrapping the cause, with the cause still in the
// chain. The service classifies it without importing that package, exactly as
// the worker classifies Adopt's.
func wrappedUncertainAbandon() error {
	return errors.Join(errors.New("uncertain NetBox abandon: clearing prefix 4242"), context.DeadlineExceeded)
}

// The one window the ledger lock cannot close: a worker pass that began before
// the fence can PATCH after the clear. The second, idempotent clear immediately
// before the delete is what answers it -- where the markers have come back it
// removes them again and the abandon converges, and where the inventory refuses
// the second time the delete does not happen at all.
func TestTheMarkerReappearingBetweenClearAndDeleteIsAnsweredBeforeTheRowGoes(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)

	t.Run("the second clear removes it again and the abandon converges", func(t *testing.T) {
		c := stuckAdoption(t, now)
		remarked := abandonedNetwork()
		remarked.AllocationID, remarked.OperationID, remarked.Owned = c.allocation.ID, c.operation.ID, true
		c.recorder.onCall = func(n int) {
			switch n {
			case 1:
				// The in-flight worker pass lands its PATCH here.
				c.inventory.networks = []domain.Network{remarked}
			case 2:
				c.inventory.networks = []domain.Network{abandonedNetwork()}
			}
		}

		report, err := c.abandonIt(t)
		if err != nil {
			t.Fatalf("abandon: %v", err)
		}
		if len(c.recorder.calls) != 2 || !c.recorder.calls[1].marked {
			t.Fatalf("the second clear did not meet the re-marked network: %#v", c.recorder.calls)
		}
		if !report.Deleted {
			t.Fatalf("the abandon did not converge: %#v", report)
		}
		for _, n := range c.inventory.networks {
			if n.AllocationID == c.allocation.ID {
				t.Fatalf("a network still claims the deleted allocation: %#v", n)
			}
		}
	})

	t.Run("a second clear that refuses keeps the allocation", func(t *testing.T) {
		c := stuckAdoption(t, now)
		c.recorder.fail[2] = errors.New("refusing to clear a prefix another operation wrote")

		report, err := c.abandonIt(t)
		safetyAPIError(t, err, 409, "abandon_inventory_refused")
		if report == nil || !report.Cleared || report.Deleted {
			t.Fatalf("report: %#v", report)
		}
		if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
			t.Fatal("the allocation was deleted although the inventory refused the pre-delete clear")
		}
	})
}

// --- the happy path ---------------------------------------------------------

func TestAbandonWithdrawsAStuckAdoption(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.stuck(t)

	report, err := c.abandonIt(t)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if !report.Fenced || report.AlreadyFenced || !report.Cleared || !report.Deleted || !report.FindingResolved {
		t.Fatalf("report: %#v", report)
	}
	if report.DryRun || report.Operator != abandoningOperator || report.Reason != abandonReason {
		t.Fatalf("the report does not name what it did: %#v", report)
	}
	if report.Allocation.ID != c.allocation.ID || report.Allocation.CIDR != adoptedCIDR || report.Allocation.Committed {
		t.Fatalf("reported allocation: %#v", report.Allocation)
	}
	if report.Reviewed == nil || *report.Reviewed != abandonedRecord() {
		t.Fatalf("reported reviewed record: %#v", report.Reviewed)
	}

	st := ledgerState(t, c.ledger)
	if _, held := st.Allocations[c.allocation.ID]; held {
		t.Fatal("the allocation row survived the abandon")
	}
	// The adoption's idempotency record is forced to go with it: a record
	// pointing at a deleted allocation drives every later adoption under this
	// key into reserve's final 503 ledger_error for ever (ADR 0012).
	for id, request := range st.Requests {
		if request.AllocationID == c.allocation.ID {
			t.Fatalf("idempotency record %s still points at the deleted allocation: %#v", id, request)
		}
	}
	// The operation row stays, terminal, still carrying what was reviewed.
	o := abandonOperation(t, c.ledger, c.allocation.ID)
	if !abandonFenced(o) || o.Type != adoptOperation {
		t.Fatalf("the operation is not the terminal one this abandon wrote: %#v", o)
	}
	if o.Adoption == nil || *o.Adoption != abandonedRecord() {
		t.Fatalf("the operation lost the reviewed record: %#v", o.Adoption)
	}
	// Audit events outlive the row they describe: audit_events is the one table
	// persistState never clears.
	actions := map[string]int{}
	for _, e := range allocationEvents(st, c.allocation.ID) {
		actions[e.Action]++
		if e.Action != adoptionAbandonedAction {
			continue
		}
		if e.Actor != abandoningOperator {
			t.Fatalf("the abandon event names actor %q", e.Actor)
		}
		for _, want := range []string{abandonReason, c.allocation.ID, c.operation.ID, adoptedNetworkID, adoptedResourceID} {
			if !strings.Contains(e.Reason, want) {
				t.Fatalf("the abandon event's reason %q omits %q", e.Reason, want)
			}
		}
	}
	if actions[adoptKind.planned] != 1 || actions[adoptionAbandonedAction] != 1 || len(actions) != 2 {
		t.Fatalf("audit trail of the withdrawn adoption: %#v", actions)
	}
	// The finding is resolved, because nothing else ever would resolve it.
	found := findingsWithCode(t, c.ledger, adoptionStuckCode)
	if len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("want the stuck finding resolved, got %#v", found)
	}
	findings, err := c.service.Findings(context.Background(), safetyPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Code == adoptionStuckCode && f.Status == "OPEN" {
			t.Fatalf("an abandoned adoption still shows an open finding: %#v", f)
		}
	}
}

// The prior inventory is ledger evidence and the adapter must not learn to read
// the ledger for it, so it travels on the durable record and reaches the port
// verbatim.
func TestTheReviewedPriorInventoryReachesTheInventoryVerbatim(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)

	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	want := abandonedRecord().Prior()
	if want == (domain.PriorInventory{}) {
		t.Fatal("the fixture records no prior inventory, so this test proves nothing")
	}
	if len(c.recorder.calls) == 0 {
		t.Fatal("the inventory was never asked to clear")
	}
	for i, call := range c.recorder.calls {
		if call.prior != want {
			t.Fatalf("call %d handed the inventory %#v, want %#v", i+1, call.prior, want)
		}
	}
}

// A hold planned before the reviewed record existed carries none, and is still
// abandonable: the clear falls back to what an import writes, and the audit
// trail says the record was missing rather than leaving a reader to guess.
func TestAHoldWithNoReviewedRecordCanStillBeAbandoned(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	ledger := storage.NewMemoryLedger()
	inventory := &safetyInventory{networks: []domain.Network{abandonedNetwork()}}
	recorder := &abandonRecorder{fail: map[int]error{}}
	inventory.abandon = recorder.hook(ledger, inventory)
	s := abandonService(now, ledger, inventory, &safetyObserver{observation: safetyObservation(now, adoptedResource())})
	a := safetyAllocation(now, "alloc-recordless", "vpc", adoptedCIDR, "", "")
	seedPendingHold(t, ledger, a, adoptOperation, nil)

	report, err := s.AbandonAdoption(context.Background(), a.ID, "", abandoningOperator, abandonReason)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if report.Reviewed != nil || !report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	for i, call := range recorder.calls {
		if call.prior != (domain.PriorInventory{}) {
			t.Fatalf("call %d invented a prior inventory: %#v", i+1, call.prior)
		}
	}
	events := abandonEventsOfAction(t, ledger, adoptionAbandonedAction)
	if len(events) != 1 || !strings.Contains(events[0].Reason, "no reviewed record was persisted") {
		t.Fatalf("the audit trail does not say the record was missing: %#v", events)
	}
}

// --- interruption and convergence -------------------------------------------

// A re-run after the clear was interrupted resumes at the clear and finishes,
// and the audit trail says one thing happened rather than two: the fence
// recognises its own terminal operation by the code it stored there.
func TestARerunAfterAnInterruptedClearConvergesWithOneEvent(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.stuck(t)
	c.recorder.fail[1] = errors.New("the clear was interrupted")

	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}
	delete(c.recorder.fail, 1)

	report, err := c.abandonIt(t)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if report.Fenced || !report.AlreadyFenced || !report.Deleted {
		t.Fatalf("the re-run did not resume from the fence it found: %#v", report)
	}
	if events := abandonEventsOfAction(t, c.ledger, adoptionAbandonedAction); len(events) != 1 {
		t.Fatalf("want exactly one abandon event in total, got %d: %#v", len(events), events)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; held {
		t.Fatal("the re-run did not delete the allocation")
	}
}

// And after the delete itself was lost. The clear has already happened, so the
// re-run finds nothing marked, which the adapter reports as success with
// nothing to do -- which is what lets the second run reach the delete.
func TestARerunAfterAnInterruptedDeleteConverges(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.stuck(t)
	// Arm the loss during the last call before the delete transaction.
	c.recorder.onCall = func(n int) {
		if n == 2 {
			c.wrapped.failUpdates = 1
		}
	}

	report, err := c.abandonIt(t)
	safetyAPIError(t, err, 503, "abandon_incomplete")
	if report == nil || !report.Cleared || report.Deleted {
		t.Fatalf("report after a lost delete: %#v", report)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
		t.Fatal("a lost delete removed the allocation anyway")
	}

	again, err := c.abandonIt(t)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.Fenced || !again.AlreadyFenced || !again.Deleted {
		t.Fatalf("the re-run did not converge: %#v", again)
	}
	if events := abandonEventsOfAction(t, c.ledger, adoptionAbandonedAction); len(events) != 1 {
		t.Fatalf("want exactly one abandon event in total, got %d: %#v", len(events), events)
	}
	if found := findingsWithCode(t, c.ledger, adoptionStuckCode); len(found) != 1 || found[0].Status != "RESOLVED" {
		t.Fatalf("the re-run did not resolve the finding: %#v", found)
	}
}

// --- what the withdrawal gives back -----------------------------------------

// The allocation key is free again, and that is the decision ADR 0012 takes:
// the owning team agreed that key for that network before the run, and the
// network is still unowned afterwards. A correct adoption under the same key is
// a fresh allocation -- not a replay of the abandoned one, and not the
// 503 ledger_error a surviving idempotency record would cause for ever.
func TestTheAllocationKeyIsReusableAfterAnAbandon(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("abandon: %v", err)
	}

	c.inventory.adoptErr = nil
	a, op, status, err := c.service.Adopt(context.Background(), safetyPrincipal(), safetyRequest("orders"), adoptPin())
	if err != nil || status != 201 || a == nil {
		t.Fatalf("re-adoption under the same key: allocation=%#v status=%d err=%v", a, status, err)
	}
	if a.ID == c.allocation.ID {
		t.Fatal("the re-adoption replayed the abandoned allocation")
	}
	if !a.Committed || a.CIDR != adoptedCIDR || a.InventoryID != adoptedNetworkID || op == nil || op.Status != operationSucceeded {
		t.Fatalf("re-adopted allocation: %#v operation=%#v", *a, op)
	}
}

// The owning team's own POST under the agreed key is an ordinary reservation
// again: the key is not retired, because retirement is derived from a row and
// there is no row.
func TestTheOwningTeamsPostAfterAnAbandonIsAnOrdinaryReservation(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("abandon: %v", err)
	}

	got, _, status, err := c.service.Reserve(context.Background(), safetyPrincipal(), safetyRequest("orders"), "team-http-key")
	if err != nil || status != 201 || got == nil {
		t.Fatalf("the owning team's POST: allocation=%#v status=%d err=%v", got, status, err)
	}
	if got.ID == c.allocation.ID || got.CIDR == adoptedCIDR {
		t.Fatalf("the team's POST recovered the abandoned hold rather than reserving: %#v", *got)
	}
	if c.inventory.ensures != 1 {
		t.Fatalf("want one ordinary inventory create, got %d", c.inventory.ensures)
	}
}

// The quota slot comes back with the row, because countTenant counts every
// allocation that is not RELEASED and never consults Committed.
func TestTheQuotaSlotComesBackWithTheRow(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.service.cfg.Pools[0].MaxAllocations = 1
	if got := countTenant(ledgerState(t, c.ledger), "t", "p"); got != 1 {
		t.Fatalf("the hold does not occupy a quota slot: %d", got)
	}

	// Fence only: the hold is still there, so the slot is still spent.
	c.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}
	_, _, _, err := c.service.Reserve(context.Background(), safetyPrincipal(), safetyRequest("billing"), "billing-key")
	safetyAPIError(t, err, 409, "quota_exceeded")

	delete(c.recorder.fail, 1)
	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got := countTenant(ledgerState(t, c.ledger), "t", "p"); got != 0 {
		t.Fatalf("the quota slot did not come back: %d", got)
	}
	if _, _, status, err := c.service.Reserve(context.Background(), safetyPrincipal(), safetyRequest("billing"), "billing-key"); err != nil || status != 201 {
		t.Fatalf("reservation after the abandon: status=%d err=%v", status, err)
	}
}

// The availability half of the fix: the pending operation fenced the whole
// overlap domain, and the fence alone -- before the clear and the delete --
// releases it, because the operation is no longer pending.
func TestTheOverlapDomainAnswersReservationsAgainAfterTheFence(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	_, _, _, err := c.service.Reserve(context.Background(), safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	safetyAPIError(t, err, 503, "domain_busy")

	c.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}

	a, _, status, err := c.service.Reserve(context.Background(), safetyPrincipal(), safetyRequest("neighbour"), "neighbour-key")
	if err != nil || status != 201 || a == nil {
		t.Fatalf("the domain is still fenced after the fence: allocation=%#v status=%d err=%v", a, status, err)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
		t.Fatal("the fence deleted the hold")
	}
}

// What the fence does not release is the hold's own CIDR: chooseCIDR and
// pinnedCIDR both block on any allocation with Committed false before they look
// at its state, which is why ADR 0012 says the row has to go rather than move
// to some terminal state. Adoption is the path that shows it, because an
// imported prefix keeps blocking an ordinary reservation either way.
func TestTheHoldBlocksItsCIDRUntilTheRowIsGone(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}
	c.inventory.adoptErr = nil

	_, _, _, err := c.service.Adopt(context.Background(), safetyPrincipal(), safetyRequest("second-key"), adoptPin())
	safetyAPIError(t, err, 409, "adoption_refused")
	if !strings.Contains(err.Error(), "uncommitted allocation") {
		t.Fatalf("the refusal does not name the hold: %v", err)
	}

	delete(c.recorder.fail, 1)
	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	a, _, status, err := c.service.Adopt(context.Background(), safetyPrincipal(), safetyRequest("second-key"), adoptPin())
	if err != nil || status != 201 || a == nil || a.CIDR != adoptedCIDR {
		t.Fatalf("the CIDR is still blocked after the row is gone: allocation=%#v status=%d err=%v", a, status, err)
	}
}

// --- the window between the fence and the delete ----------------------------

// Between the two writes the hold is uncommitted with no pending operation, and
// every replay path would read that as "pending" and answer 202 with no
// operation, which is a lie. All three say the same thing instead, and none of
// them writes anything (the H1 review's finding).
func TestTheAbandonWindowRefusesEveryReplayPath(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}
	before := ledgerState(t, c.ledger)
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"adopt": func() error {
			_, _, _, err := c.service.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin())
			return err
		},
		"consumer_reserve": func() error {
			_, _, _, err := c.service.Reserve(ctx, safetyPrincipal(), safetyRequest("orders"), "team-http-key")
			return err
		},
		"plan_adoption": func() error {
			_, err := c.service.PlanAdoption(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin())
			return err
		},
	} {
		err := run()
		safetyAPIError(t, err, 409, "adoption_abandoning")
		var api *domain.APIError
		if errors.As(err, &api) && !api.Retryable {
			t.Fatalf("%s: the window refusal is not retryable: %#v", name, api)
		}
	}
	if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused replay wrote to the ledger: before=%#v after=%#v", before, after)
	}

	// Once the abandon has finished the window is over, and the key answers
	// ordinarily again.
	delete(c.recorder.fail, 1)
	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	c.inventory.adoptErr = nil
	if _, _, status, err := c.service.Adopt(ctx, safetyPrincipal(), safetyRequest("orders"), adoptPin()); err != nil || status != 201 {
		t.Fatalf("adoption after the window closed: status=%d err=%v", status, err)
	}
}

// --- the worker -------------------------------------------------------------

// After the fence the operation is no longer pending, so pendingRecoveryJobs
// stops selecting it: a worker pass in the window converts nothing, commits
// nothing and raises no second finding.
func TestTheWorkerLeavesAFencedAdoptionAlone(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.stuck(t)
	c.recorder.fail[1] = errors.New("the clear was interrupted")
	if _, err := c.abandonIt(t); err == nil {
		t.Fatal("want the first run to stop at the clear")
	}
	c.inventory.adoptErr, c.inventory.adopts = nil, nil

	if err := c.service.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(c.inventory.adopts) != 0 {
		t.Fatalf("a worker pass tried to finish a fenced adoption: %#v", c.inventory.adopts)
	}
	held, ok := ledgerState(t, c.ledger).Allocations[c.allocation.ID]
	if !ok || held.Committed {
		t.Fatalf("a worker pass committed a fenced adoption: %#v", held)
	}
	if o := abandonOperation(t, c.ledger, c.allocation.ID); !abandonFenced(o) {
		t.Fatalf("a worker pass changed the fenced operation: %#v", o)
	}
	if found := findingsWithCode(t, c.ledger, adoptionStuckCode); len(found) != 1 {
		t.Fatalf("want the one finding the recovery raised, got %#v", found)
	}
}

// --- the dry run ------------------------------------------------------------

func TestTheDryRunWritesNothingAndSaysWhatARealRunWouldDo(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.stuck(t)
	before := ledgerState(t, c.ledger)

	report, err := c.service.PlanAbandonAdoption(context.Background(), c.allocation.ID, c.operation.ID, abandoningOperator, abandonReason)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !report.DryRun || report.Fenced || report.AlreadyFenced || report.Cleared || report.Deleted || report.FindingResolved {
		t.Fatalf("a dry run reported doing something: %#v", report)
	}
	if report.Operation.ID != c.operation.ID || report.Operation.Type != adoptOperation || report.Operation.Status != operationPending {
		t.Fatalf("reported operation: %#v", report.Operation)
	}
	if report.Allocation.ID != c.allocation.ID || report.Allocation.Committed || report.Allocation.State != domain.Reserved {
		t.Fatalf("reported allocation: %#v", report.Allocation)
	}
	if report.Reviewed == nil || *report.Reviewed != abandonedRecord() {
		t.Fatalf("reported reviewed record: %#v", report.Reviewed)
	}
	for _, want := range []string{"fence:", "clear:", "clear again:", "delete:"} {
		found := false
		for _, line := range report.WouldDo {
			if strings.HasPrefix(line, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("the dry run does not describe %q: %#v", want, report.WouldDo)
		}
	}
	if len(c.recorder.calls) != 0 {
		t.Fatalf("a dry run asked the inventory to clear: %#v", c.recorder.calls)
	}
	if after := ledgerState(t, c.ledger); !reflect.DeepEqual(before, after) {
		t.Fatalf("a dry run wrote to the ledger: before=%#v after=%#v", before, after)
	}
}

// The inventory has no read of its own for "who claims this allocation", so the
// dry run answers from the snapshot every other service path already takes. It
// has to tell apart the situations that decide whether a real run would clear
// anything, be refused, or have nothing to do.
func TestTheDryRunReportsEachInventorySituation(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		networks func(c abandonable) []domain.Network
		complete bool
		claim    string
		count    int
	}{
		"nothing claims it": {
			networks: func(abandonable) []domain.Network { return []domain.Network{abandonedNetwork()} },
			complete: true, claim: abandonClaimNone, count: 0,
		},
		"one network carries this operation's markers": {
			networks: func(c abandonable) []domain.Network {
				n := abandonedNetwork()
				n.AllocationID, n.OperationID, n.Owned = c.allocation.ID, c.operation.ID, true
				return []domain.Network{n}
			},
			complete: true, claim: abandonClaimOurs, count: 1,
		},
		"one network carries another operation's markers": {
			networks: func(c abandonable) []domain.Network {
				n := abandonedNetwork()
				n.AllocationID, n.OperationID, n.Owned = c.allocation.ID, "op-somebody-elses", true
				return []domain.Network{n}
			},
			complete: true, claim: abandonClaimOther, count: 1,
		},
		"two networks claim it": {
			networks: func(c abandonable) []domain.Network {
				first := abandonedNetwork()
				first.AllocationID, first.OperationID, first.Owned = c.allocation.ID, c.operation.ID, true
				second := domain.Network{ID: "4243", CIDR: "10.9.0.0/24", AllocationID: c.allocation.ID, OperationID: c.operation.ID, Owned: true}
				return []domain.Network{first, second}
			},
			complete: true, claim: abandonClaimAmbiguous, count: 2,
		},
		"an incomplete snapshot proves nothing": {
			networks: func(abandonable) []domain.Network { return []domain.Network{abandonedNetwork()} },
			complete: false, claim: abandonClaimUnknown, count: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := stuckAdoption(t, now)
			c.inventory.networks = tc.networks(c)
			c.inventory.incomplete = !tc.complete

			report, err := c.service.PlanAbandonAdoption(context.Background(), c.allocation.ID, "", abandoningOperator, abandonReason)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if report.Inventory.Claim != tc.claim || report.Inventory.Count != tc.count {
				t.Fatalf("reported inventory: %#v, want claim %q over %d networks", report.Inventory, tc.claim, tc.count)
			}
			if len(c.recorder.calls) != 0 {
				t.Fatalf("a dry run asked the inventory to clear: %#v", c.recorder.calls)
			}
		})
	}
}

// --- structural guarantees --------------------------------------------------

// Inventory.AbandonAdoption is the only call in this project that takes
// ownership away from an inventory object without deleting it, so where it may
// be reached from is part of the guarantee rather than an implementation
// detail. Two calls, both in abandon.go: the clear, and the repeat immediately
// before the delete that answers a worker pass which re-marked the object. A
// third caller would be a route to removing markers that neither fenced an
// operation nor deleted a hold, so this count is raised only together with the
// argument for raising it, and never loosened into a range.
func TestInventoryAbandonAdoptionIsCalledOnlyByTheAbandonFile(t *testing.T) {
	calls := 0
	for name, f := range packageFiles(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AbandonAdoption" {
				return true
			}
			receiver, ok := sel.X.(*ast.SelectorExpr)
			if !ok || receiver.Sel.Name != "inventory" {
				t.Fatalf("%s calls AbandonAdoption on something other than the inventory port", name)
			}
			if name != "abandon.go" {
				t.Fatalf("%s clears an inventory object; only abandon.go may", name)
			}
			calls++
			return true
		})
	}
	if calls != 2 {
		t.Fatalf("want exactly two Inventory.AbandonAdoption calls (the clear and the pre-delete repeat), got %d", calls)
	}
}

// The service's own abandon had no caller outside a test until package H2c
// added `platform-ipam adopt abandon` in internal/adoptcmd/abandon.go
// (runAbandon calls svc.AbandonAdoption for a real run and
// svc.PlanAbandonAdoption for --dry-run, each exactly once, on the Service
// interface svc -- not *service.Service directly, which is what lets that
// package's own tests use a fake in its place). This test now pins
// internal/adoptcmd as the ONLY non-test caller of Service.AbandonAdoption and
// Service.PlanAbandonAdoption on the service, so ADR 0012's rule that abandon
// is never an API endpoint and is never reached from client, onboard or main
// directly stays a property of the source rather than a promise: every other
// directory here must still call neither.
func TestServiceAbandonIsCalledOnlyByAdoptcmd(t *testing.T) {
	found := map[string]int{}
	for _, dir := range []string{".", "../adoptcmd", "../transport", "../cli", "../onboardcmd", "../../cmd/platform-ipam"} {
		for name, f := range packageFiles(t, dir) {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "AbandonAdoption":
					// The port's own method is allowed where the sibling test
					// above pins it (internal/service/abandon.go's clear,
					// reached through s.inventory); anything else naming
					// AbandonAdoption is a caller of the service's own method.
					if receiver, ok := sel.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "inventory" {
						return true
					}
				case "PlanAbandonAdoption":
				default:
					return true
				}
				if dir != "../adoptcmd" {
					t.Fatalf("%s/%s calls Service.%s; only internal/adoptcmd may call the service's own abandon (ADR 0012: never an API endpoint, never the operator role)", dir, name, sel.Sel.Name)
				}
				found[sel.Sel.Name]++
				return true
			})
		}
	}
	if found["AbandonAdoption"] != 1 {
		t.Fatalf("want exactly one call to Service.AbandonAdoption in internal/adoptcmd (abandon.go's runAbandon), got %d", found["AbandonAdoption"])
	}
	if found["PlanAbandonAdoption"] != 1 {
		t.Fatalf("want exactly one call to Service.PlanAbandonAdoption in internal/adoptcmd (abandon.go's runAbandon), got %d", found["PlanAbandonAdoption"])
	}
}

// --- added in the H2b review ------------------------------------------------

// The delete re-checks, under the ledger lock, that the operation is still the
// terminal one this abandon fenced. The clear runs outside any transaction, so
// something else can rewrite the operation in between; a row must not disappear
// on the strength of a fence that is no longer there.
func TestTheDeleteRefusesWhenTheFenceIsNoLongerTheOneItWrote(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	c.recorder.onCall = func(n int) {
		if n != 2 {
			return
		}
		// Between the pre-delete clear and the delete: the operation is still
		// FAILED, but no longer with the error this abandon wrote.
		if err := c.ledger.Update(context.Background(), func(st *domain.State) error {
			o := st.Operations[c.operation.ID]
			o.Error = domain.Err(500, "something_else", "rewritten by another writer")
			st.Operations[o.ID] = o
			return nil
		}); err != nil {
			t.Fatalf("rewriting the operation: %v", err)
		}
	}

	report, err := c.abandonIt(t)
	safetyAPIError(t, err, 409, "abandon_state_changed")
	if report == nil || report.Deleted {
		t.Fatalf("report: %#v", report)
	}
	if _, held := ledgerState(t, c.ledger).Allocations[c.allocation.ID]; !held {
		t.Fatal("the allocation was deleted although the fence it rested on had changed")
	}
}

// reserve can leave a consumer's own idempotency record pointing at a pending
// hold: when a POST under the agreed key races the adoption's first
// transaction, the key scan inside reserve's Update writes a record under
// POST's method and path and the consumer's Idempotency-Key. (The ordinary,
// unraced POST is answered from the read-only View and writes none.) If the
// abandon left such a record behind, that team's retry with the same
// Idempotency-Key would resolve to an allocation that no longer exists and
// answer 503 ledger_error for ever. Every record naming the allocation goes.
func TestAbandonRemovesAConsumersRecordOfThePendingHoldToo(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	c := stuckAdoption(t, now)
	ctx := context.Background()
	p := safetyPrincipal()

	validated, _, _, err := c.service.validateRequest(ctx, p, safetyRequest("orders"))
	if err != nil {
		t.Fatalf("validating the consumer's request: %v", err)
	}
	const consumerKey = "consumer-idempotency-key"
	recordID := idempotencyID(p.TenantID, reserveKind.method, reserveKind.path, consumerKey)
	if err := c.ledger.Update(ctx, func(st *domain.State) error {
		st.Requests[recordID] = domain.Idempotency{TenantID: p.TenantID, Method: reserveKind.method, Path: reserveKind.path,
			Key: consumerKey, Hash: requestBodyHash(validated), AllocationID: c.allocation.ID, OperationID: c.operation.ID}
		return nil
	}); err != nil {
		t.Fatalf("seeding the raced consumer record: %v", err)
	}

	if _, err := c.abandonIt(t); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	for id, request := range ledgerState(t, c.ledger).Requests {
		if request.AllocationID == c.allocation.ID {
			t.Fatalf("record %s still names the deleted allocation: %#v", id, request)
		}
	}
	// The team retries with the same Idempotency-Key: an ordinary reservation,
	// not a replay of a row that is gone and not a ledger error.
	c.inventory.networks = nil
	c.observer.observation = safetyObservation(now)
	again, _, status, err := c.service.Reserve(ctx, p, safetyRequest("orders"), consumerKey)
	if err != nil || status != 201 || again == nil || again.ID == c.allocation.ID {
		t.Fatalf("the retry after the abandon: allocation=%#v status=%d err=%v", again, status, err)
	}
}
