package service

// Abandoning an adoption is the only operation in this project that removes a
// ledger row, and the whole argument that it is safe rests on the row being
// uncommitted (ADR 0012). An adoption persists its intent before it touches the
// inventory, so a hold that can never commit may already have converted a
// prefix; the order of effects is therefore forced and is three steps rather
// than two. The fence makes the operation terminal under the ledger lock, so
// no commit can follow and the overlap domain stops being fenced. The clear,
// outside any transaction, gives the inventory object back to the import that
// wrote it. Only then does the delete remove the allocation row. Every
// interruption of that sequence leaves a state a re-run converges from, and no
// interruption leaves an inventory object claiming an allocation the ledger
// does not hold -- which is the property this file exists to keep.
//
// Nothing here takes a domain.Principal. There is no tenant to act as, the
// operator role of ADR 0011 may write nothing at all, and a signature that
// cannot be filled from a request context is one more reason a handler will not
// grow around it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

const (
	// adoptionAbandonedAction is the audit action the fence appends. It is the
	// only event an abandon writes: the delete that follows is the mechanical
	// completion of the decision this event records, and a second event would
	// present one withdrawal as two -- and would have to be suppressed on every
	// converging re-run, which is exactly the bookkeeping the single event
	// avoids.
	adoptionAbandonedAction = "ADOPT_ABANDONED"
	// adoptionAbandonedCode is how a re-run recognises its own fence. It is
	// carried in the operation's durable Error value, beside the terminal
	// status, so the recognition is an exact comparison of a stored code rather
	// than a reading of the free-text reason an operator typed. Release is the
	// only other writer of Operation.Error and uses a different code on a
	// different operation type, so the two can never be confused.
	adoptionAbandonedCode = "adoption_abandoned"
	// abandonedOperationMessage is deliberately constant. The operator and the
	// reason live in the audit event; what the operation row has to carry is a
	// marker that never varies, because a converging re-run compares it.
	abandonedOperationMessage = "Adoption abandoned by an operator; the uncommitted hold was withdrawn."
	// adoptionStuckCode is the finding an unfinishable adoption raises
	// (internal/service/worker.go). reconcileAllocations does not reset it, so
	// the delete is the only thing that ever closes it.
	adoptionStuckCode = "adoption_stuck"
	// reservationStuckCode is its sibling for a pending RESERVE (package H4).
	reservationStuckCode = "reservation_stuck"
)

// What the inventory side of an abandon looks like, as a dry run reports it and
// as a real run records it before the clear.
const (
	abandonClaimNone      = "unclaimed"
	abandonClaimOurs      = "this_operation"
	abandonClaimOther     = "another_operation"
	abandonClaimAmbiguous = "ambiguous"
	abandonClaimUnknown   = "unknown"
)

// AbandonedAllocation is what the ledger held. It is a projection rather than
// domain.Allocation itself: a report is read by a person deciding whether to
// run the real thing, and the fields that decide it are these.
type AbandonedAllocation struct {
	ID            string `json:"id"`
	AllocationKey string `json:"allocation_key"`
	TenantID      string `json:"tenant_id"`
	DomainID      string `json:"domain_id"`
	CIDR          string `json:"cidr"`
	State         string `json:"state"`
	Committed     bool   `json:"committed"`
}

// AbandonedOperation is the operation the fence makes terminal. Status is the
// status as the report was written: PENDING for a dry run over a live hold,
// FAILED once the fence has run.
type AbandonedOperation struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// AbandonClaim is what the inventory says about this allocation, read through
// Inventory.Snapshot -- the one read the port offers. Two limits are worth
// stating where they are read rather than only in the report. Snapshot is
// scoped to the allocation's routing domain and therefore to that domain's VRF,
// while the clear's own search is by marker and VRF-agnostic, so a prefix an
// adoption wrote outside the VRF reads as unclaimed here and is still found and
// cleared by a real run. And the answer is a moment in time: a worker pass that
// began before the fence can re-mark a prefix after this read, which is why the
// real run asks the inventory to clear twice rather than trusting any read.
type AbandonClaim struct {
	Claim       string `json:"claim"`
	Count       int    `json:"count"`
	NetworkID   string `json:"network_id,omitempty"`
	NetworkCIDR string `json:"network_cidr,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// AbandonReport is the whole of what an abandon did, or -- for a dry run --
// what it found and what a real run would do. Package H2c prints it as one JSON
// document, so every field carries a tag and every value is a copy.
//
// It is returned beside an error whenever the run got far enough to have one: a
// refusal from the fence returns no report, because nothing was established,
// while a clear that failed returns the report of everything up to it. Callers
// check the error first and use the report to say what had already happened.
type AbandonReport struct {
	DryRun     bool                   `json:"dry_run"`
	At         time.Time              `json:"at"`
	Operator   string                 `json:"operator"`
	Reason     string                 `json:"reason"`
	Allocation AbandonedAllocation    `json:"allocation"`
	Operation  AbandonedOperation     `json:"operation"`
	Reviewed   *domain.AdoptionRecord `json:"reviewed"`
	Inventory  AbandonClaim           `json:"inventory"`
	// Fenced reports that this run wrote the fence; AlreadyFenced that it found
	// one an earlier, interrupted run had written and resumed from it.
	Fenced          bool     `json:"fenced"`
	AlreadyFenced   bool     `json:"already_fenced"`
	Cleared         bool     `json:"cleared"`
	Deleted         bool     `json:"deleted"`
	FindingResolved bool     `json:"finding_resolved"`
	WouldDo         []string `json:"would_do,omitempty"`
}

// abandonTarget is what the fence's checks agreed on: the uncommitted hold and
// the one ADOPT operation that fences its overlap domain.
type abandonTarget struct {
	allocation    domain.Allocation
	operation     domain.Operation
	alreadyFenced bool
}

// AbandonAdoption withdraws an uncommitted adoption: fence, clear, delete, in
// that order and never another. allocationID names the hold, operationID is
// optional and must name that hold's own operation when it is given, operator
// is the subject the audit trail names and reason is the operator's own words,
// both mandatory. A re-run after an interruption at any point converges.
func (s *Service) AbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*AbandonReport, error) {
	if err := abandonInputs(allocationID, operator, reason); err != nil {
		return nil, err
	}
	if err := s.ledgerReady(ctx); err != nil {
		return nil, err
	}
	// The clear is not optional. Without an inventory there is no way to find
	// out whether a prefix claims this allocation, and deleting the row on that
	// ignorance is precisely the state ADR 0012 forbids.
	if s.inventory == nil {
		return nil, domain.Err(503, "dependency_unavailable", "inventory adapter is not configured")
	}

	// FENCE. One ledger transaction: every refusal is decided here, and where
	// none applies the operation becomes terminal and the audit event is
	// appended. From this moment both commit paths -- reserve's commit closure
	// and commitRecovered -- re-check the status under the lock and become
	// no-ops, pendingRecoveryJobs stops selecting the operation, and the
	// overlap domain is unfenced.
	now := s.now().UTC()
	var target abandonTarget
	if err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		found, err := abandonCheck(st, allocationID, operationID)
		if err != nil {
			return err
		}
		target = found
		if found.alreadyFenced {
			// An earlier run already wrote this fence and was interrupted. It
			// wrote the event too, so this run resumes at the clear in silence.
			return nil
		}
		o := found.operation
		o.Status = operationFailed
		o.Error = domain.Err(409, adoptionAbandonedCode, abandonedOperationMessage)
		o.UpdatedAt = now
		st.Operations[o.ID] = o
		st.Events = append(st.Events, domain.Event{
			ID: domain.NewID("evt"), AllocationID: found.allocation.ID, TenantID: found.allocation.TenantID,
			Actor: operator, Action: adoptionAbandonedAction,
			Reason: abandonEvidence(operator, reason, found.allocation, found.operation),
			At:     now, Revision: found.allocation.Revision,
		})
		target.operation = o
		return nil
	}); err != nil {
		return nil, abandonLedgerError(err, "the abandon could not be fenced")
	}

	report := abandonReport(false, s.now().UTC(), operator, reason, target)
	report.Inventory = s.abandonClaim(ctx, target.allocation, target.operation.ID)

	// CLEAR. Outside any transaction, because it is a call to another system:
	// the evidence is this allocation's own marker and nothing an operator
	// typed, and what it restores comes from the durable record rather than
	// from anything read now.
	prior := domain.PriorInventory{}
	if target.operation.Adoption != nil {
		prior = target.operation.Adoption.Prior()
	}
	if err := s.inventory.AbandonAdoption(ctx, target.allocation, target.operation.ID, prior); err != nil {
		return report, abandonClearError(err)
	}
	report.Cleared = true

	// The one window the ledger lock cannot close (ADR 0012): a worker pass
	// that began before the fence already holds its job list, and its PATCH can
	// land after the clear, re-marking the prefix. Its commitRecovered will
	// write nothing, because the operation is no longer pending, but the
	// markers would be back. The port offers no read by marker, and a second
	// idempotent clear is stronger than one: where nothing is marked it writes
	// nothing, and where the markers have come back it removes them again
	// instead of refusing and asking for a re-run that would do exactly this.
	// Either way an answer of nil means no prefix carried the markers at the
	// moment it read them, which is the question the delete turns on; anything
	// else stops the run with the allocation row still in place.
	if err := s.inventory.AbandonAdoption(ctx, target.allocation, target.operation.ID, prior); err != nil {
		return report, abandonClearError(err)
	}

	// DELETE. One ledger transaction that re-checks everything under the lock.
	resolved := false
	if err := s.ledger.Update(ctx, func(st *domain.State) error {
		ensureState(st)
		a, held := st.Allocations[target.allocation.ID]
		if !held {
			// A previous run of this abandon already removed it. That is
			// convergence, not a conflict.
			return nil
		}
		if a.Committed {
			return apiErr(409, "abandon_state_changed", "allocation "+a.ID+" committed while this abandon was running; nothing was deleted")
		}
		o, exists := st.Operations[target.operation.ID]
		if !exists || o.AllocationID != a.ID || !abandonFenced(o) {
			return apiErr(409, "abandon_state_changed", "the operation this abandon fenced is no longer the terminal one it wrote; nothing was deleted, and a re-run will report the current state")
		}
		delete(st.Allocations, a.ID)
		// The adoption's own idempotency record is forced rather than chosen:
		// reserve consults it before anything else, so a record pointing at a
		// deleted allocation would make stateResults return nothing and drive
		// every later adoption under this key into reserve's final
		// apiErr(503, "ledger_error") for ever (ADR 0012). Any other record
		// naming this allocation would do the same to whichever path wrote it.
		delete(st.Requests, idempotencyID(a.TenantID, adoptKind.method, adoptKind.path, a.AllocationKey))
		for id, request := range st.Requests {
			if request.AllocationID == a.ID {
				delete(st.Requests, id)
			}
		}
		if f, open := st.Findings[findingID(adoptionStuckCode, a.ID)]; open && f.Status != "RESOLVED" {
			resolved = true
		}
		// Nothing else ever would: the finding is keyed on the allocation id,
		// reconcileAllocations does not reset its code, and an orphaned open
		// CRITICAL would fail `client findings --fail-if-open` for ever.
		resolveFinding(st, a, adoptionStuckCode)
		return nil
	}); err != nil {
		return report, abandonLedgerError(err, "the inventory was cleared but the allocation could not be deleted")
	}
	report.Deleted, report.FindingResolved = true, resolved
	return report, nil
}

// PlanAbandonAdoption is the dry run. It applies every check the fence applies
// and performs the same marker search, writes nothing at all to the ledger or
// to the inventory, and reports what it found together with what a real run
// would do. The precedent is `adopt plan` (ADR 0010): abandon is the one
// command in this project that deletes a ledger row, and it should be possible
// to look first.
func (s *Service) PlanAbandonAdoption(ctx context.Context, allocationID, operationID, operator, reason string) (*AbandonReport, error) {
	if err := abandonInputs(allocationID, operator, reason); err != nil {
		return nil, err
	}
	if err := s.ledgerReady(ctx); err != nil {
		return nil, err
	}
	var target abandonTarget
	if err := s.ledger.View(ctx, func(st *domain.State) error {
		found, err := abandonCheck(st, allocationID, operationID)
		if err != nil {
			return err
		}
		target = found
		return nil
	}); err != nil {
		return nil, abandonLedgerError(err, "the abandon could not be planned")
	}
	report := abandonReport(true, s.now().UTC(), operator, reason, target)
	report.Inventory = s.abandonClaim(ctx, target.allocation, target.operation.ID)
	report.WouldDo = abandonPlan(target, report.Inventory)
	return report, nil
}

// abandonCheck is the fence's whole judgement, and it is a pure function of the
// state so that the dry run reaches exactly the same verdict through a View.
// Each refusal carries a code of its own, because the command that runs this
// has to turn them into exit codes and an operator has to know which of them
// means "never" and which means "not yet".
func abandonCheck(st *domain.State, allocationID, operationID string) (abandonTarget, error) {
	var out abandonTarget
	a, ok := st.Allocations[allocationID]
	if !ok {
		return out, apiErr(404, "abandon_unknown_allocation", "no allocation with id "+allocationID+" exists in the ledger")
	}
	// The one refusal that is absolute. A committed allocation is ownership,
	// and removing it is exactly the undo ADR 0010 forbids; it is tested before
	// anything else so that no later condition can be the reason a committed
	// allocation is examined at all. The comparison is committedHoldRefusal
	// (cancel.go), one function both withdrawals call first, so that the check
	// cannot be relaxed on one path while a test of the other still passes.
	if err := committedHoldRefusal(a, "abandon_committed", "abandoned"); err != nil {
		return out, err
	}
	var found []domain.Operation
	for _, o := range st.Operations {
		if o.AllocationID != a.ID {
			continue
		}
		if operationID != "" && o.ID != operationID {
			continue
		}
		found = append(found, o)
	}
	switch {
	case len(found) == 0 && operationID != "":
		return out, apiErr(409, "abandon_operation_mismatch", "operation "+operationID+" does not name allocation "+a.ID)
	case len(found) == 0:
		// This code cannot produce that state, and abandoning it would mean
		// guessing whether an inventory write ever happened.
		return out, apiErr(409, "abandon_no_operation", "uncommitted allocation "+a.ID+" has no operation, so nothing records what was reviewed or whether the inventory was written")
	case len(found) > 1:
		// Map iteration would otherwise pick one of them at random.
		return out, apiErr(409, "abandon_ambiguous_operation", "several operations name allocation "+a.ID+"; name the one to abandon")
	}
	o := found[0]
	if o.Type != adoptOperation {
		return out, apiErr(409, "abandon_not_an_adoption", "operation "+o.ID+" is a "+o.Type+" rather than an "+adoptOperation+"; abandoning a pending "+reserveOperation+" is deliberately out of scope (ADR 0012)")
	}
	switch {
	case o.Status == operationPending:
	case abandonFenced(o):
		// An earlier abandon fenced this operation and did not finish. Resuming
		// is how a re-run converges, so this is not a refusal.
		out.alreadyFenced = true
	case o.Status == operationSucceeded:
		return out, apiErr(409, "abandon_operation_succeeded", "operation "+o.ID+" has already succeeded: the commit won the race and this adoption is no longer uncommitted")
	default:
		return out, apiErr(409, "abandon_operation_terminal", "operation "+o.ID+" is already in terminal status "+o.Status+", which this abandon did not write")
	}
	out.allocation, out.operation = a, o
	return out, nil
}

// abandonFenced reports the terminal operation an abandon wrote. It is the
// durable, exact recognition a converging re-run needs: a stored code beside a
// terminal status, never a reading of the reason an operator typed. The
// comparison itself is fencedWith (cancel.go), which a consumer's cancel of a
// stuck reservation makes with its own code (ADR 0013).
func abandonFenced(o domain.Operation) bool { return fencedWith(o, adoptionAbandonedCode) }

// abandonInProgress reports an allocation whose hold has been fenced by a
// withdrawal that has not reached its delete -- an operator's abandon of an
// ADOPT (ADR 0012) or a consumer's cancel of a RESERVE (ADR 0013). Between
// those two writes the hold is uncommitted with no pending operation, which is
// a state reserve's replay paths would otherwise read as "still being worked
// on" and answer 202. It returns the refusal rather than a bare yes, because
// the two withdrawals answer for themselves by name and must never be mistaken
// for one another (ADR 0013: the shared code takes the operation type from the
// fence it found, and neither exit may claim the other's).
func abandonInProgress(st *domain.State, allocationID string) *domain.APIError {
	for _, o := range st.Operations {
		if o.AllocationID != allocationID {
			continue
		}
		if err := withdrawalRefusal(o); err != nil {
			return err
		}
	}
	return nil
}

// abandoningReplay is the window refusal, and it exists because 202 with no
// operation is a lie. A concurrent `adopt apply` over the same reviewed record,
// or the owning team's own POST under the agreed key, finds the hold under the
// tenant and key and replays it; while a withdrawal is between its fence and
// its delete that hold is about to stop existing, so the honest answer is that
// something is in progress and the request should be repeated once it is over.
// It is retryable for the same reason: nothing about the request is wrong.
func abandoningReplay(st *domain.State, a *domain.Allocation) error {
	if a == nil || a.Committed {
		return nil
	}
	if err := abandonInProgress(st, a.ID); err != nil {
		return err
	}
	return nil
}

// abandonInputs refuses what the service can judge without reading anything.
// The operator subject and the reason are the whole of the audit trail an
// abandon leaves behind, and the service cannot verify either, so the one thing
// it can insist on is that both are there (ADR 0010, consequences).
func abandonInputs(allocationID, operator, reason string) error {
	if strings.TrimSpace(operator) == "" {
		return apiErr(422, "abandon_operator_required", "an operator subject is required for the abandon audit trail")
	}
	if strings.TrimSpace(reason) == "" {
		return apiErr(422, "abandon_reason_required", "a reason is required: an abandon is recorded in the audit trail and has to say why")
	}
	if strings.TrimSpace(allocationID) == "" {
		return apiErr(422, "abandon_allocation_required", "an allocation id is required")
	}
	return nil
}

// abandonEvidence is the audit reason. domain.Event carries no structured
// details, so the operator's own words and everything that identifies what was
// withdrawn go into the string (ADR 0012). A hold planned before the reviewed
// record was durable says so rather than leaving the reader to wonder.
//
// It cannot say whether a prefix was cleared: the event is written by the
// fence, which runs before the clear precisely so that no interruption can
// leave an inventory object claiming a deleted allocation.
func abandonEvidence(operator, reason string, a domain.Allocation, o domain.Operation) string {
	if o.Adoption == nil {
		return fmt.Sprintf("abandon requested by %s: %s; allocation %s (%s, key %q), operation %s, no reviewed record was persisted with this adoption",
			operator, reason, a.ID, a.CIDR, a.AllocationKey, o.ID)
	}
	rec := *o.Adoption
	return fmt.Sprintf("abandon requested by %s: %s; allocation %s (%s, key %q), operation %s, reviewed inventory network %s, cloud resource %s (import batch %q)",
		operator, reason, a.ID, a.CIDR, a.AllocationKey, o.ID, rec.NetworkID, rec.ResourceID, rec.ImportBatch)
}

// abandonClaim asks the inventory which network claims this allocation, through
// Snapshot -- the only read the port has. See AbandonClaim for what that costs
// in precision; it is a report, and nothing decides anything on it.
func (s *Service) abandonClaim(ctx context.Context, a domain.Allocation, operationID string) AbandonClaim {
	if s.inventory == nil {
		return AbandonClaim{Claim: abandonClaimUnknown, Reason: "inventory adapter is not configured"}
	}
	snap, err := s.inventory.Snapshot(ctx, domainFor(s.cfg, a.DomainID))
	if err != nil {
		return AbandonClaim{Claim: abandonClaimUnknown, Reason: errString(err)}
	}
	var claimed []domain.Network
	for _, n := range snap.Networks {
		if n.AllocationID == a.ID {
			claimed = append(claimed, n)
		}
	}
	out := AbandonClaim{Count: len(claimed)}
	if len(claimed) == 1 {
		out.NetworkID, out.NetworkCIDR, out.OperationID = claimed[0].ID, claimed[0].CIDR, claimed[0].OperationID
	}
	switch {
	case !snap.Complete:
		out.Claim, out.Reason = abandonClaimUnknown, "the inventory snapshot is incomplete, so the absence of a claim proves nothing"
	case len(claimed) == 0:
		out.Claim = abandonClaimNone
	case len(claimed) > 1:
		out.Claim = abandonClaimAmbiguous
	case claimed[0].OperationID == operationID:
		out.Claim = abandonClaimOurs
	default:
		out.Claim = abandonClaimOther
	}
	return out
}

// abandonReport is the shape both entry points fill before they diverge.
func abandonReport(dryRun bool, at time.Time, operator, reason string, t abandonTarget) *AbandonReport {
	out := &AbandonReport{
		DryRun: dryRun, At: at, Operator: operator, Reason: reason,
		Allocation: AbandonedAllocation{
			ID: t.allocation.ID, AllocationKey: t.allocation.AllocationKey, TenantID: t.allocation.TenantID,
			DomainID: t.allocation.DomainID, CIDR: t.allocation.CIDR, State: t.allocation.State,
			Committed: t.allocation.Committed,
		},
		Operation:     AbandonedOperation{ID: t.operation.ID, Type: t.operation.Type, Status: t.operation.Status},
		Fenced:        !dryRun && !t.alreadyFenced,
		AlreadyFenced: t.alreadyFenced,
	}
	if t.operation.Adoption != nil {
		reviewed := *t.operation.Adoption
		out.Reviewed = &reviewed
	}
	return out
}

// abandonPlan is what a dry run says a real run would do, in the order it would
// do it.
func abandonPlan(t abandonTarget, claim AbandonClaim) []string {
	fence := "fence: set operation " + t.operation.ID + " to " + operationFailed + " (" + adoptionAbandonedCode + ") and append one " + adoptionAbandonedAction + " audit event"
	if t.alreadyFenced {
		fence = "fence: already written by an earlier run of this abandon; nothing more is written and no second event is appended"
	}
	clear := "clear: ask the inventory to return the network claiming allocation " + t.allocation.ID + " to imported occupancy"
	switch claim.Claim {
	case abandonClaimNone:
		clear += "; nothing claims it now, so this would write nothing"
	case abandonClaimOurs:
		clear += "; network " + claim.NetworkID + " (" + claim.NetworkCIDR + ") carries this operation's markers and would be cleared"
	case abandonClaimOther:
		clear += "; network " + claim.NetworkID + " (" + claim.NetworkCIDR + ") carries operation " + claim.OperationID + " instead, which the inventory would refuse to clear"
	case abandonClaimAmbiguous:
		clear += fmt.Sprintf("; %d networks claim it, which the inventory would refuse to clear", claim.Count)
	default:
		clear += "; what the inventory holds could not be read (" + claim.Reason + ")"
	}
	return []string{
		fence,
		clear,
		"clear again: repeat the same call immediately before the delete, so a worker pass that re-marked the network after the first clear cannot outlive it",
		"delete: remove allocation " + t.allocation.ID + " and its " + adoptKind.method + " idempotency record, and resolve any open " + adoptionStuckCode + " finding; operation " + t.operation.ID + " stays, " + operationFailed + ", carrying what was reviewed",
	}
}

// abandonClearError turns the inventory's answer into one an operator can act
// on. Both outcomes stop the run before the delete, so neither can leave an
// object claiming an allocation the ledger does not hold; what differs is the
// advice. Uncertainty is classified exactly as the worker classifies Adopt's
// errors (uncertainInventoryError), which is what keeps this package free of
// any dependency on the adapter's own sentinels.
func abandonClearError(err error) error {
	if uncertainInventoryError(err) {
		return domain.Err(503, "abandon_uncertain", "the inventory did not say whether it cleared the network ("+err.Error()+"); nothing has been deleted and re-running this abandon is safe")
	}
	return domain.Err(409, "abandon_inventory_refused", "the inventory refused to clear the network this adoption converted ("+err.Error()+"); nothing has been deleted and re-running this abandon is safe once the cause is resolved")
}

// abandonLedgerError keeps a refusal this file decided intact and turns
// anything the ledger itself raised into a retryable failure, because a lost
// ledger write leaves a state a re-run converges from.
func abandonLedgerError(err error, what string) error {
	var api *domain.APIError
	if errors.As(err, &api) {
		return err
	}
	return domain.Err(503, "abandon_incomplete", what+" ("+err.Error()+"); re-running this abandon is safe")
}
