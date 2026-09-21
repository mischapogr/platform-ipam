package service

// Cancelling a reservation is the project's first tenant-facing operation that
// removes a ledger row, and the whole argument that it is safe rests on two
// conditions the caller cannot manufacture (ADR 0013): the allocation is
// uncommitted, and the platform itself has already declared the hold stuck
// through an open reservation_stuck finding. A reservation persists its intent
// before it touches the inventory, so a hold that can never commit may already
// have created its prefix; the order of effects is therefore ADR 0012's, three
// steps and never two. The fence makes the operation terminal under the ledger
// lock, so no commit can follow and the overlap domain stops being fenced. The
// removal, outside any transaction, deletes the prefix that carries this
// allocation's marker. Only then does the delete remove the allocation row.
// Every interruption of that sequence leaves a state a re-run converges from,
// and no interruption leaves an inventory object claiming an allocation the
// ledger does not hold -- which is the property this file exists to keep.
//
// The two exits differ where the facts differ. An abandon clears and keeps,
// because an adoption's prefix belongs to an import and outlives the adoption;
// a cancel deletes, because a reservation's prefix belongs to nothing else and
// a cleared one would sit unowned at that CIDR while Ensure's exact-CIDR guard
// refused every later reservation there. What is shared is the service-level
// shape, and it is shared as code at the end of this file and in abandon.go:
// the committed refusal both make first and absolutely, the recognition of a
// fence by a stored code beside a terminal status, and the window refusal that
// keeps a replay from being answered 202 while a hold is being withdrawn.
//
// This takes the authenticated principal, because the actor is the consumer
// that made the request. The tenant comparison is Service.Operation's, so an
// operator -- a principal with no tenant (ADR 0011) -- is refused by a
// comparison that does not know the role exists, and no code here reads it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

const (
	// reservationCancelledAction is the audit action the fence appends. It is
	// the only event a cancel writes: the removal and the delete that follow
	// are the mechanical completion of the decision this event records, and a
	// second event would present one withdrawal as two -- and would have to be
	// suppressed on every converging re-run, which is exactly the bookkeeping
	// the single event avoids.
	reservationCancelledAction = "RESERVE_CANCELLED"
	// reservationCancelledCode is how a re-run recognises its own fence, and
	// how a replaying POST learns that the hold it found is being withdrawn. It
	// is carried in the operation's durable Error value beside the terminal
	// status, so the recognition is an exact comparison of a stored code rather
	// than a reading of any free text. It is also what GET /v1/operations/{id}
	// answers for ever afterwards, as a FailedOperation's error code.
	reservationCancelledCode = "reservation_cancelled"
	// cancelledOperationMessage is deliberately constant. What the operation
	// row has to carry is a marker that never varies, because a converging
	// re-run compares it; the tenant, the CIDR and the finding that justified
	// the cancel live in the audit event.
	cancelledOperationMessage = "Reservation cancelled by the tenant that requested it; the uncommitted hold was withdrawn."
)

// CancelledAllocation is what the ledger held. It is a projection rather than
// domain.Allocation itself: what a consumer and the transport above this need
// is what was withdrawn, not every column of a row that no longer exists.
type CancelledAllocation struct {
	ID            string `json:"id"`
	AllocationKey string `json:"allocation_key"`
	TenantID      string `json:"tenant_id"`
	DomainID      string `json:"domain_id"`
	CIDR          string `json:"cidr"`
	State         string `json:"state"`
	Committed     bool   `json:"committed"`
}

// CancelledOperation is the operation the fence makes terminal. Status is the
// status as the report was written: FAILED once the fence has run.
type CancelledOperation struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// CancelReport is the whole of what a cancel did. It is returned beside an
// error whenever the run got far enough to have one: a refusal from the fence
// returns no report, because nothing was established, while a removal that
// failed returns the report of everything up to it. Callers check the error
// first and use the report to say what had already happened.
type CancelReport struct {
	At         time.Time           `json:"at"`
	Actor      string              `json:"actor"`
	Allocation CancelledAllocation `json:"allocation"`
	Operation  CancelledOperation  `json:"operation"`
	// Fenced reports that this run wrote the fence; AlreadyFenced that it found
	// one an earlier, interrupted run had written and resumed from it.
	Fenced        bool `json:"fenced"`
	AlreadyFenced bool `json:"already_fenced"`
	// Removed reports that THIS run got an answer from the inventory that
	// nothing carries the allocation's marker. A converging re-run that finds
	// the hold already gone does not ask the inventory at all, and reports
	// false with Deleted true.
	Removed bool `json:"removed"`
	// Deleted reports that the uncommitted hold is gone from the ledger,
	// whether this run removed it or found it already removed.
	Deleted         bool `json:"deleted"`
	FindingResolved bool `json:"finding_resolved"`
}

// cancelTarget is what the fence's checks agreed on: the uncommitted hold, the
// one RESERVE operation that fences its overlap domain, and the finding that
// justifies withdrawing it.
type cancelTarget struct {
	allocation    domain.Allocation
	operation     domain.Operation
	finding       domain.Finding
	alreadyFenced bool
	// done reports an operation this cancel already fenced whose hold has
	// already been deleted: an earlier run finished, and a DELETE that is
	// idempotent by definition says so rather than refusing.
	done bool
}

// CancelReservation withdraws a pending reservation the platform has declared
// stuck, as the tenant that made it: fence, remove, delete, in that order and
// never another. p is the authenticated principal and operationID the operation
// that tenant holds from their own 202. A re-run after an interruption at any
// point converges, and leaves exactly one audit event.
func (s *Service) CancelReservation(ctx context.Context, p domain.Principal, operationID string) (*CancelReport, error) {
	if err := s.ledgerReady(ctx); err != nil {
		return nil, err
	}
	// The removal is not optional. Without an inventory there is no way to find
	// out whether a prefix carries this allocation's marker, and deleting the
	// row on that ignorance is precisely the state ADR 0012 and ADR 0013 forbid.
	if s.inventory == nil {
		return nil, domain.Err(503, "dependency_unavailable", "inventory adapter is not configured")
	}

	// FENCE. One ledger transaction: every refusal is decided here, and where
	// none applies the operation becomes terminal and the audit event is
	// appended. From this moment no commit can happen -- reserve's commit
	// closure and commitRecovered both re-check the status under the lock --
	// pendingRecoveryJobs stops selecting the operation, pendingDomain stops
	// seeing it, and the overlap domain is unfenced.
	now := s.now().UTC()
	var target cancelTarget
	if err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		found, err := cancelCheck(st, p, operationID)
		if err != nil {
			return err
		}
		target = found
		if found.alreadyFenced {
			// An earlier run already wrote this fence and was interrupted. It
			// wrote the event too, so this run resumes in silence.
			return nil
		}
		o := found.operation
		o.Status = operationFailed
		o.Error = domain.Err(409, reservationCancelledCode, cancelledOperationMessage)
		o.UpdatedAt = now
		st.Operations[o.ID] = o
		st.Events = append(st.Events, domain.Event{
			ID: domain.NewID("evt"), AllocationID: found.allocation.ID, TenantID: found.allocation.TenantID,
			Actor: p.Subject, Action: reservationCancelledAction,
			Reason: cancelEvidence(p, found.allocation, found.operation, found.finding),
			At:     now, Revision: found.allocation.Revision,
		})
		target.operation = o
		return nil
	}); err != nil {
		return nil, cancelLedgerError(err, "the cancel could not be fenced")
	}

	report := cancelReport(s.now().UTC(), p.Subject, target)
	if target.done {
		// An earlier run of this cancel finished the whole sequence. Nothing is
		// left to remove and nothing is left to delete; saying so is the answer
		// a DELETE owes a repeat, and it is why the tenant's retry of a lost
		// response is not a refusal.
		return report, nil
	}

	// REMOVE. Outside any transaction, because it is a call to another system.
	// The evidence is this allocation's own marker and nothing the caller
	// supplied: the adapter refuses anything but the single prefix carrying
	// this allocation's id and this operation's id, and refuses that one if it
	// carries the import tag.
	if err := s.inventory.CancelReservation(ctx, target.allocation, target.operation.ID); err != nil {
		return report, cancelRemoveError(err)
	}
	report.Removed = true

	// The one window the ledger lock cannot close (ADR 0012, restated for a
	// reservation by ADR 0013): a worker pass that began before the fence
	// already holds its job list, calls Ensure, and can create the prefix after
	// the first removal. Its commitRecovered will write nothing, because the
	// operation is no longer pending, but the markers would be back. The port
	// offers no read by marker, and a second idempotent removal is stronger
	// than a read: where nothing is marked it writes nothing and answers at
	// once, and where something re-marked the prefix it removes it again and
	// the cancel converges instead of asking for a re-run that would do exactly
	// this. Either way an answer of nil means no prefix carried the markers at
	// the moment it read them, which is the question the delete turns on;
	// anything else stops the run with the allocation row still in place.
	if err := s.inventory.CancelReservation(ctx, target.allocation, target.operation.ID); err != nil {
		return report, cancelRemoveError(err)
	}

	// DELETE. One ledger transaction that re-checks everything under the lock.
	resolved := false
	if err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		a, held := st.Allocations[target.allocation.ID]
		if !held {
			// A previous run of this cancel already removed it. That is
			// convergence, not a conflict.
			return nil
		}
		if a.Committed {
			return apiErr(409, "cancel_state_changed", "allocation "+a.ID+" committed while this cancel was running; nothing was deleted")
		}
		o, exists := st.Operations[target.operation.ID]
		if !exists || o.AllocationID != a.ID || !cancelFenced(o) {
			return apiErr(409, "cancel_state_changed", "the operation this cancel fenced is no longer the terminal one it wrote; nothing was deleted, and a re-run will report the current state")
		}
		delete(st.Allocations, a.ID)
		// The consumer's own idempotency record is forced rather than chosen:
		// reserve consults it before anything else, so a record pointing at a
		// deleted allocation would make stateResults return nothing and drive
		// every later POST under that Idempotency-Key into reserve's final
		// apiErr(503, "ledger_error") for ever (ADR 0013). The key is the
		// consumer's own and cannot be recomputed from the row, so every record
		// naming this allocation goes -- which is also what removes a record
		// some other path raced into existence against the same hold.
		for id, request := range st.Requests {
			if request.AllocationID == a.ID {
				delete(st.Requests, id)
			}
		}
		if f, open := st.Findings[findingID(reserveKind.stuck, a.ID)]; open && f.Status != "RESOLVED" {
			resolved = true
		}
		// Nothing else ever would: the finding is keyed on the allocation id,
		// reconcileAllocations does not reset its code, and an orphaned open
		// CRITICAL would fail `client findings --fail-if-open` with exit 7 for
		// that tenant and for the operator for ever.
		resolveFinding(st, a, reserveKind.stuck)
		return nil
	}); err != nil {
		return report, cancelLedgerError(err, "the inventory object was removed but the allocation could not be deleted")
	}
	report.Deleted, report.FindingResolved = true, resolved
	return report, nil
}

// cancelCheck is the fence's whole judgement, and it is a pure function of the
// state so that it can be read, and mutated, as one thing. Each refusal carries
// a code of its own, because the transport above this has to turn them into
// statuses and a consumer has to know which of them means "never" and which
// means "not yet".
func cancelCheck(st *domain.State, p domain.Principal, operationID string) (cancelTarget, error) {
	var out cancelTarget
	// The lookup is Service.Operation's, deliberately: an unknown id, an id
	// belonging to another tenant and a principal with no tenant are one
	// answer, so a cancel leaks nothing an existing read did not. The operator
	// of ADR 0011 is exactly a principal with no tenant and is refused here by
	// the same comparison that refuses a stranger -- this code does not know
	// the role exists, which is the form ADR 0011 says the refusal must take.
	o, ok := st.Operations[operationID]
	if p.TenantID == "" || !ok || o.TenantID != p.TenantID {
		return out, apiErr(404, "not_found", "operation not found")
	}
	a, held := st.Allocations[o.AllocationID]
	// The one refusal that is absolute, asked of whatever the ledger holds
	// before anything here looks at the operation at all. A committed
	// allocation is ownership, and removing it is the undo ADR 0010 forbids; it
	// is tested first so that no later condition can be the reason a committed
	// allocation is examined. An absent row is not committed, so this reads as
	// nil and the branch below decides.
	if err := committedHoldRefusal(a, "cancel_committed", "cancelled"); err != nil {
		return out, err
	}
	if !held {
		// Only a cancel removes an uncommitted RESERVE row, so this is either
		// this cancel's own completed run -- which converges rather than
		// refusing, because a DELETE is idempotent by definition -- or a ledger
		// state nothing in this project can produce.
		if o.Type == reserveOperation && cancelFenced(o) {
			out.operation, out.alreadyFenced, out.done = o, true, true
			out.allocation.ID = o.AllocationID
			out.allocation.TenantID = o.TenantID
			out.allocation.DomainID = o.DomainID
			return out, nil
		}
		return out, apiErr(409, "cancel_no_allocation", "operation "+o.ID+" names allocation "+o.AllocationID+", which the ledger does not hold")
	}
	if o.Type != reserveOperation {
		message := "operation " + o.ID + " is a " + o.Type + " rather than a " + reserveOperation
		if o.Type == adoptOperation {
			message += "; withdrawing an uncommitted adoption is an operator's `platform-ipam adopt abandon` (ADR 0012), never a consumer's cancel"
		}
		return out, apiErr(409, "cancel_not_a_reservation", message)
	}
	switch {
	case o.Status == operationPending:
	case cancelFenced(o):
		// An earlier cancel fenced this operation and did not finish. Resuming
		// is how a re-run converges, so this is not a refusal.
		out.alreadyFenced = true
	case o.Status == operationSucceeded:
		return out, apiErr(409, "cancel_operation_succeeded", "operation "+o.ID+" has already succeeded: the commit won the race and this allocation is now owned. Release it with DELETE /v1/allocations/{id}")
	default:
		return out, apiErr(409, "cancel_operation_terminal", "operation "+o.ID+" is already in terminal status "+o.Status+", which this cancel did not write")
	}
	// The narrowing that keeps this off the hot path of a healthy reservation,
	// and it is not a policy knob (ADR 0013). The finding is the platform's own
	// verdict, reached by recoverReservations on evidence it read, age-gated
	// past one observation interval and already published to this tenant. So a
	// consumer cannot cancel a reservation that is merely slow, one that is
	// inside its own Ensure, or one whose refusal was uncertain. It is asked
	// only of a hold this cancel has not already fenced: refusing a resumed run
	// because something closed the finding in between would strand the hold
	// inside the window for ever, which is the one state worse than the one
	// this operation exists to end.
	if !out.alreadyFenced {
		f, open := st.Findings[findingID(reserveKind.stuck, a.ID)]
		if !open || f.Status != "OPEN" {
			return out, apiErr(409, "reservation_not_stuck", "no open "+reserveKind.stuck+" finding names allocation "+a.ID+": only a hold the platform itself has declared stuck can be cancelled, and a reservation that is merely slow, still being created, or refused for a reason the platform could not verify is not one")
		}
		out.finding = f
	}
	out.allocation, out.operation = a, o
	return out, nil
}

// cancelFenced reports the terminal operation a cancel wrote. It is the
// durable, exact recognition a converging re-run and every replay path need: a
// stored code beside a terminal status, never a reading of free text.
func cancelFenced(o domain.Operation) bool { return fencedWith(o, reservationCancelledCode) }

// cancelEvidence is the audit reason. domain.Event carries no structured
// details, so everything that identifies what was withdrawn goes into the
// string: the allocation, its CIDR and key, the operation, and the finding that
// justified the cancel with the window over which the platform observed it
// (ADR 0013). No reason from the consumer is required or recorded -- section 5
// says a DELETE needs no body, the platform supplies the objective
// justification in the finding, and a tenant ending their own dead request is
// not exercising authority over anybody else.
func cancelEvidence(p domain.Principal, a domain.Allocation, o domain.Operation, f domain.Finding) string {
	return fmt.Sprintf("cancel requested by %s: allocation %s (%s, key %q), operation %s, %s open since %s (last observed %s)",
		p.Subject, a.ID, a.CIDR, a.AllocationKey, o.ID, reserveKind.stuck,
		f.FirstObservedAt.UTC().Format(time.RFC3339), f.LastObservedAt.UTC().Format(time.RFC3339))
}

// cancelReport is the shape the method fills before it does anything.
func cancelReport(at time.Time, actor string, t cancelTarget) *CancelReport {
	return &CancelReport{
		At: at, Actor: actor,
		Allocation: CancelledAllocation{
			ID: t.allocation.ID, AllocationKey: t.allocation.AllocationKey, TenantID: t.allocation.TenantID,
			DomainID: t.allocation.DomainID, CIDR: t.allocation.CIDR, State: t.allocation.State,
			Committed: t.allocation.Committed,
		},
		Operation:     CancelledOperation{ID: t.operation.ID, Type: t.operation.Type, Status: t.operation.Status},
		Fenced:        !t.alreadyFenced,
		AlreadyFenced: t.alreadyFenced,
		Deleted:       t.done,
	}
}

// cancelRemoveError turns the inventory's answer into one a consumer can act
// on. Both outcomes stop the run before the delete, so neither can leave an
// object claiming an allocation the ledger does not hold; what differs is the
// advice. Uncertainty is classified exactly as the worker classifies Ensure's
// errors (uncertainInventoryError), which is what keeps this package free of
// any dependency on the adapter's own sentinels -- and which is why the
// adapter's answered 404 between its two reads, which says something rather
// than nothing, is reported here as a refusal whose re-run converges on
// "nothing to delete" rather than as an outcome nobody knows.
func cancelRemoveError(err error) error {
	if uncertainInventoryError(err) {
		return domain.Err(503, "cancel_uncertain", "the inventory did not say whether it removed the network this reservation created ("+err.Error()+"); nothing has been deleted and re-running this cancel is safe")
	}
	return domain.Err(409, "cancel_inventory_refused", "the inventory refused to remove the network this reservation created ("+err.Error()+"); nothing has been deleted and re-running this cancel is safe once the cause is resolved")
}

// cancelLedgerError keeps a refusal cancelCheck decided intact and turns
// anything the ledger itself raised into a retryable failure, because a lost
// ledger write leaves a state a re-run converges from.
func cancelLedgerError(err error, what string) error {
	var api *domain.APIError
	if errors.As(err, &api) {
		return err
	}
	return domain.Err(503, "cancel_incomplete", what+" ("+err.Error()+"); re-running this cancel is safe")
}

// --- shared with the abandon (ADR 0012, generalised by ADR 0013) -------------

// committedHoldRefusal is the one refusal both withdrawals make, and make
// first: a committed allocation is ownership and neither exit may remove it,
// in any state and however it came to be owned. It lives here, in one function
// both entry points call, so that the check cannot be relaxed on one path
// while a test of the other still passes; the code and the verb differ because
// each exit answers for itself.
func committedHoldRefusal(a domain.Allocation, code, verb string) error {
	if !a.Committed {
		return nil
	}
	return apiErr(409, code, "allocation "+a.ID+" is committed and is therefore owned: a committed allocation can never be "+verb+", in any state. Release it with DELETE /v1/allocations/{id}")
}

// fencedWith is how both withdrawals recognise their own fence: a terminal
// operation carrying a code they stored in its durable Error. Release is the
// only other writer of Operation.Error and uses a different code on a different
// operation type, so no two of the three can be confused.
func fencedWith(o domain.Operation, code string) bool {
	return o.Status == operationFailed && o.Error != nil && o.Error.Code == code
}

// withdrawalRefusal is the window answer for one operation, and it exists
// because 202 with no operation -- or 202 over an operation that is already
// FAILED -- is a lie. Between a withdrawal's fence and its delete the hold is
// uncommitted with no pending operation, which every replay path reads as
// "still being worked on". Each exit answers for itself so that neither can be
// mistaken for the other, and both are retryable for the same reason: nothing
// about the request is wrong, and once the withdrawal is over the key answers
// ordinarily again.
func withdrawalRefusal(o domain.Operation) *domain.APIError {
	var err *domain.APIError
	switch {
	case o.Type == adoptOperation && fencedWith(o, adoptionAbandonedCode):
		err = domain.Err(409, "adoption_abandoning", "an abandon of this adoption is in progress; retry when it has finished")
	case o.Type == reserveOperation && fencedWith(o, reservationCancelledCode):
		err = domain.Err(409, "reservation_cancelling", "a cancel of this reservation is in progress; retry when it has finished")
	default:
		return nil
	}
	err.Retryable = true
	return err
}
