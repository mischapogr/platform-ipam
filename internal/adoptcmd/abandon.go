package adoptcmd

// `adopt abandon` (docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md,
// docs/WORK_PLAN.md package H2c): the third adopt subcommand, and the only one
// that takes no table. It withdraws an uncommitted adoption that can never
// commit -- the fence/clear/delete sequence lives entirely in
// Service.AbandonAdoption (internal/service/abandon.go); this file only
// parses flags, calls it (or, for --dry-run, Service.PlanAbandonAdoption),
// and turns its answer into a JSON document and an exit code. It holds no
// allocation policy of its own, exactly as plan.go and apply.go do not.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/service"
)

// abandonFlagNames are the flags checkArgs's "abandon" case and runAbandon
// both pass to splitPositional, so the table-path-before-flags convention
// (splitPositional's doc comment) parses "--allocation-id x --operator y"
// forms the same way whichever of the two runs first. --dry-run is
// deliberately absent: it takes no value, so splitPositional must not try to
// consume the argument after it.
var abandonFlagNames = []string{"allocation-id", "operator", "reason", "operation-id"}

// abandonFlags is the flag.FlagSet both checkArgs and runAbandon build, so
// there is exactly one definition of what `adopt abandon`'s flags are.
type abandonFlags struct {
	allocationID, operator, reason, operationID *string
	dryRun                                      *bool
}

func newAbandonFlags() (*flag.FlagSet, *abandonFlags) {
	set := newFlagSet("abandon")
	f := &abandonFlags{
		allocationID: set.String("allocation-id", "", "id of the uncommitted adoption to abandon (from the apply report's pending outcome, or the adoption_stuck finding) (required)"),
		operator:     set.String("operator", "", "audit subject recorded on the ADOPT_ABANDONED event; nothing else (required)"),
		reason:       set.String("reason", "", "free-text reason recorded in the audit trail (required, not blank)"),
		operationID:  set.String("operation-id", "", "the hold's own ADOPT operation id; must name this allocation's operation when given"),
		dryRun:       set.Bool("dry-run", false, "report what a real run would find and would do; writes nothing to the ledger or to NetBox"),
	}
	return set, f
}

// checkAbandonArgs is checkArgs's "abandon" case: every usage mistake this
// subcommand can make is caught here, before a ledger connection, NetBox
// client or cloud observer is ever built (CheckArgs's whole point). abandon
// takes no positional table path at all -- unlike plan and apply -- so any
// bare argument is itself a usage error.
func checkAbandonArgs(args []string) error {
	positional, flagArgs := splitPositional(args, abandonFlagNames...)
	set, f := newAbandonFlags()
	if err := parseFlags(set, flagArgs); err != nil {
		return err
	}
	if len(positional) != 0 {
		return usagef("abandon: takes no table path or other positional argument (got %q); abandon acts on one allocation id, not a reviewed table", positional[0])
	}
	return abandonRequiredFlags(f)
}

// abandonRequiredFlags is the one definition of "well-formed enough to call
// the service" that checkAbandonArgs and runAbandon share. --allocation-id,
// --operator and a non-blank --reason are required; the service's own
// abandon_*_required refusals (internal/service/abandon.go, abandonInputs)
// exist as a backstop for a caller that reaches Main directly, bypassing
// CheckArgs -- exactly what this package's own tests do -- and abandonExit
// maps them back to ExitUsage for that reason.
func abandonRequiredFlags(f *abandonFlags) error {
	if strings.TrimSpace(*f.allocationID) == "" {
		return usagef("abandon: --allocation-id is required")
	}
	if strings.TrimSpace(*f.operator) == "" {
		return usagef("abandon: --operator is required")
	}
	if strings.TrimSpace(*f.reason) == "" {
		return usagef("abandon: --reason is required and must not be blank")
	}
	return nil
}

// runAbandon is Main's "abandon" case. It never touches the table-reading or
// principal-resolution machinery plan.go/apply.go use: abandon supplies no
// tenant (ADR 0012 -- Service.AbandonAdoption takes no domain.Principal) and
// no CIDR, only the four strings the fence and the clear need.
func runAbandon(ctx context.Context, args []string, svc Service, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, abandonFlagNames...)
	set, f := newAbandonFlags()
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 0 {
		return usageFail(stderr, usagef("abandon: takes no table path or other positional argument (got %q); abandon acts on one allocation id, not a reviewed table", positional[0]))
	}
	if err := abandonRequiredFlags(f); err != nil {
		return usageFail(stderr, err)
	}

	var (
		report *service.AbandonReport
		err    error
	)
	if *f.dryRun {
		report, err = svc.PlanAbandonAdoption(ctx, *f.allocationID, *f.operationID, *f.operator, *f.reason)
	} else {
		report, err = svc.AbandonAdoption(ctx, *f.allocationID, *f.operationID, *f.operator, *f.reason)
	}

	// The report is printed whenever the service returned one, whether or not
	// it is also returning an error: a refusal from the fence returns no
	// report (nothing was established), while a clear that failed returns the
	// report of everything up to it (internal/service/abandon.go's own doc
	// comment on AbandonReport). Printing before deciding the exit code means
	// an operator sees exactly as much as a real run wrote, in every outcome
	// that reached the service.
	// One JSON document on stdout in every outcome that reached the service:
	// the report's own fields when there is a report, and an "error" object
	// when there is an error -- both when a run failed after the fence. A
	// refusal at the fence has no report, and a pipeline reading stdout must
	// still learn why without parsing stderr.
	document := abandonDocument{AbandonReport: report, Error: abandonError(err)}
	if writeErr := printJSON(stdout, document); writeErr != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt abandon: writing report: %v\n", writeErr)
		return ExitAdapter
	}

	if err != nil {
		code := abandonExit(err)
		fmt.Fprintf(stderr, "platform-ipam adopt abandon: %v\n", err)
		if code == ExitAdapter {
			fmt.Fprintln(stderr, "platform-ipam adopt abandon: re-running this abandon is safe")
		}
		return code
	}

	if *f.dryRun {
		fmt.Fprintf(stderr, "platform-ipam adopt abandon: dry run for allocation %s; nothing was written; inventory claim: %s\n",
			*f.allocationID, report.Inventory.Claim)
	} else {
		fmt.Fprintf(stderr, "platform-ipam adopt abandon: allocation %s: deleted=%v finding_resolved=%v\n",
			*f.allocationID, report.Deleted, report.FindingResolved)
	}
	return ExitOK
}

// abandonExit maps the service's answer to this package's three non-zero
// exit codes (the same three plan/apply use, so a caller does not need a
// second table to read them):
//
//	2 (ExitUsage)       the service's own abandon_*_required backstop -- see
//	                     abandonRequiredFlags's comment; unreachable through
//	                     CheckArgs, reachable only by calling Main directly
//	3 (ExitValidation)  a refusal the operator must act on: every APIError
//	                     whose Status is 404, 409 or 422 (unknown allocation,
//	                     committed, wrong operation, already terminal, the
//	                     window refusal, and the inventory's own refusals)
//	4 (ExitAdapter)     infrastructure or an uncertain outcome: every APIError
//	                     with Status 503 (abandon_uncertain, abandon_incomplete,
//	                     dependency_unavailable) and anything that is not a
//	                     *domain.APIError at all (a context error, a transport
//	                     failure that reached this package unwrapped)
//
// Every one of these codes, and the message beside it, come from the service;
// this function only chooses which exit code carries them.
// abandonDocument is what `adopt abandon` prints. The report is embedded, so
// its fields sit at the top level exactly as the service defines them; a nil
// report contributes nothing.
type abandonDocument struct {
	*service.AbandonReport
	Error *abandonFailure `json:"error,omitempty"`
}

type abandonFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// abandonError renders an error for the document. Anything that is not the
// service's own APIError is infrastructure, and says so rather than borrowing
// a code it does not have.
func abandonError(err error) *abandonFailure {
	if err == nil {
		return nil
	}
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return &abandonFailure{Code: apiErr.Code, Message: apiErr.Message}
	}
	return &abandonFailure{Code: "adapter_error", Message: err.Error()}
}

func abandonExit(err error) int {
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "abandon_operator_required", "abandon_reason_required", "abandon_allocation_required":
			return ExitUsage
		}
		switch apiErr.Status {
		case 503:
			return ExitAdapter
		default:
			// 404, 409, 422 and anything else the service might one day add:
			// a decision the service reached on evidence it read, not an
			// infrastructure problem, so the safe default is "the operator
			// must act on this" rather than "safe to blindly re-run".
			return ExitValidation
		}
	}
	return ExitAdapter
}
