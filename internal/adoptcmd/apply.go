package adoptcmd

// apply re-runs plan for every record before writing anything, then adopts
// one record at a time in parents-before-subnets order, stopping at the
// first refusal, pending result or error (ADR 0010: no undo, so there is no
// "continue on error").

import (
	"context"
	"fmt"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

// OutcomeStatus is one record's fate within an apply run.
type OutcomeStatus string

const (
	OutcomeAdopted      OutcomeStatus = "adopted"
	OutcomeFailed       OutcomeStatus = "failed"
	OutcomeNotAttempted OutcomeStatus = "not_attempted"
	// OutcomeWaitingForParent is a subnet that was not attempted because its
	// parent has no binding yet -- including a parent this same run adopted a
	// moment ago. It is not a failure and does not stop the run; it does keep
	// the run from exiting 0, because the file is not fully adopted.
	OutcomeWaitingForParent OutcomeStatus = "waiting_for_parent"
)

// Outcome is one record's result from the write phase of apply.
type Outcome struct {
	SourceRow     int           `json:"source_row"`
	TenantID      string        `json:"tenant_id"`
	AllocationKey string        `json:"allocation_key"`
	Scope         string        `json:"scope"`
	Status        OutcomeStatus `json:"status"`
	AllocationID  string        `json:"allocation_id,omitempty"`
	OperationID   string        `json:"operation_id,omitempty"`
	CIDR          string        `json:"cidr,omitempty"`
	HTTPStatus    int           `json:"http_status,omitempty"`
	RefusalCode   string        `json:"refusal_code,omitempty"`
	Message       string        `json:"message,omitempty"`
	// Adapter is true when Status is OutcomeFailed for a reason outside the
	// reviewed record itself (a transport or infrastructure error rather
	// than a business refusal). It is not part of the JSON report -- an
	// operator reading the report cares about the message, not which exit
	// code produced it -- but runApply uses it to choose between
	// ExitValidation and ExitAdapter.
	Adapter bool `json:"-"`
}

// ApplyReport is the one JSON document apply prints to stdout: the pre-flight
// plan (always present) and, only once it reported no refusals, the write
// phase's outcomes in the order they were attempted.
type ApplyReport struct {
	Plan     Report    `json:"plan"`
	Outcomes []Outcome `json:"outcomes,omitempty"`
}

// performApply is apply's core logic, independent of flags and I/O, so it
// can be tested directly against a fake Service. It never mutates records
// beyond what requestFor and the parent lookup compute, and every ledger
// write happens through svc.Adopt -- this function holds no allocation
// policy of its own.
func performApply(ctx context.Context, svc Service, cfg domain.Config, records []Record, operator string) (ApplyReport, error) {
	ordered := orderParentsFirst(records)

	pl := newParentLookup()
	pl.noteFileRows(ordered)
	if err := pl.seedFromLedger(ctx, svc, distinctTenants(ordered)); err != nil {
		return ApplyReport{}, err
	}

	plan := evaluateAll(ctx, svc, cfg, ordered, pl, operator)
	report := ApplyReport{Plan: plan}
	if plan.HasRefusals() {
		// Nothing attempted: apply refuses to start when any record's plan
		// verdict is a refusal, and writes nothing (docs/WORK_PLAN.md
		// package F4).
		return report, nil
	}

	stopped := false
	for _, r := range ordered {
		if stopped {
			report.Outcomes = append(report.Outcomes, Outcome{
				SourceRow: r.SourceRow, TenantID: r.TenantID, AllocationKey: r.AllocationKey, Scope: r.Scope,
				Status: OutcomeNotAttempted,
			})
			continue
		}

		outcome, halt := attemptOne(ctx, svc, cfg, pl, r, operator)
		report.Outcomes = append(report.Outcomes, outcome)
		if halt {
			stopped = true
		}
	}
	return report, nil
}

// attemptOne adopts exactly one record, returning whether the run must stop
// after it (a refusal, an unresolved parent, an error, or a 202 pending
// result -- every one of these is a reason apply must not attempt the next
// record, per ADR 0010's no-undo rule).
func attemptOne(ctx context.Context, svc Service, cfg domain.Config, pl *parentLookup, r Record, operator string) (Outcome, bool) {
	out := Outcome{SourceRow: r.SourceRow, TenantID: r.TenantID, AllocationKey: r.AllocationKey, Scope: r.Scope}

	req, err := requestFor(r)
	if err != nil {
		// Unreachable in practice: performApply's pre-flight plan pass
		// already validated every record with the same function. Handled
		// defensively rather than assumed away.
		out.Status, out.RefusalCode, out.Message = OutcomeFailed, "invalid_request", err.Error()
		return out, true
	}

	if r.Scope == "subnet" {
		parent, deferred, found := pl.resolve(r.TenantID, r.ParentAllocationKey)
		if found && !deferred && !parent.bound {
			out.Status, out.Message = OutcomeWaitingForParent, fmt.Sprintf(waitingForParentMessage, r.ParentAllocationKey)
			return out, false
		}
		if !found || deferred {
			// Unreachable given orderParentsFirst and a plan pass that
			// already reported every subnet's parent as resolved or
			// deferred: a deferred parent is a vpc row in this same file,
			// attempted earlier in this same loop, and its result -- success
			// or failure -- was already recorded or already stopped the run.
			out.Status, out.RefusalCode = OutcomeFailed, "invalid_parent"
			out.Message = fmt.Sprintf("parent allocation key %q did not resolve before this record was attempted", r.ParentAllocationKey)
			return out, true
		}
		req.ParentAllocationID = parent.id
	}

	principal, err := resolvePrincipal(cfg.Identities, r)
	if err != nil {
		out.Status, out.RefusalCode, out.Message = OutcomeFailed, "invalid_request", err.Error()
		return out, true
	}

	pin := service.Adoption{Operator: operator, CIDR: r.CIDR, NetworkID: r.NetBoxPrefixID, ResourceID: r.ResourceID}
	a, op, status, err := svc.Adopt(ctx, principal, req, pin)
	if err != nil {
		out.Status = OutcomeFailed
		out.RefusalCode, out.Message = codeAndMessage(err)
		out.Adapter = out.RefusalCode == "adapter_error"
		return out, true
	}
	if status == 202 {
		out.Status = OutcomeFailed
		out.HTTPStatus = status
		// The ids are what an operator needs to follow a pending adoption: the
		// allocation is invisible to GET until it commits, and an
		// adoption_stuck finding names the allocation.
		if a != nil {
			out.AllocationID, out.CIDR = a.ID, a.CIDR
		}
		if op != nil {
			out.OperationID = op.ID
		}
		out.Message = "adoption is pending: the write has not yet been confirmed. " +
			"The worker's recovery path finishes a pending adoption on its next pass, or raises an adoption_stuck finding if it cannot; " +
			"re-running apply is always safe and will report this record as adopted once it commits."
		return out, true
	}
	if a == nil {
		out.Status, out.Message = OutcomeFailed, "adopt returned no allocation and no error"
		out.Adapter = true
		return out, true
	}

	pl.record(r.TenantID, r.AllocationKey, *a)
	out.Status, out.AllocationID, out.CIDR, out.HTTPStatus = OutcomeAdopted, a.ID, a.CIDR, status
	return out, false
}
