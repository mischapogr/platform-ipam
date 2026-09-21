package adoptcmd

// Main, flag parsing and I/O: the house style internal/onboardcmd uses
// (splitPositional's table-path-before-flags convention, usageError,
// ExitOK/ExitUsage/ExitValidation/ExitAdapter), duplicated rather than
// imported because those helpers are unexported in internal/onboardcmd and
// this package's work order forbids editing it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/domain"
)

// Exit codes, matching internal/onboardcmd's contract: 3 for everything the
// operator's data or the record's admissibility is responsible for (a bad
// column, a non-canonical CIDR, a refusal from the service, a pending
// result, a subnet still waiting for its parent's binding); 4 for everything outside the reviewed record (a table.csv that
// cannot be opened, a ledger or NetBox failure while resolving a parent).
const (
	ExitOK         = 0
	ExitUsage      = 2
	ExitValidation = 3
	ExitAdapter    = 4
)

const helpText = `usage: platform-ipam adopt <command> [flags]

commands:
  plan     validate a reviewed table; print a verdict per record; write nothing
             plan records.csv
  apply    re-run plan, then adopt one network at a time (parents before subnets)
             apply records.csv --operator <subject>
  abandon  withdraw an uncommitted adoption that can never commit; fence, clear,
           delete -- takes no table
             abandon --allocation-id <id> --operator <subject> --reason <text>

adopt is a sibling of onboard, not a subcommand of it: it opens the ledger and
the cloud observer, and it never runs a migration.

input: a CSV (or TSV, by .tsv/.txt extension) table with a header row -- see
the internal/adoptcmd package comment for the exact columns and why
internal/onboard's reader was not reused for it. An unrecognized column, a
missing required column, or a column this package derives itself
(prefix_length, address_family, pool, domain) is an error. abandon takes no
table at all: any positional argument to it is a usage error.

--operator <subject> is required on apply and on abandon. It is the audit
subject of the person running this adoption (recorded on ADOPT_PLANNED,
ADOPT_COMMITTED and, for abandon, ADOPT_ABANDONED as the actor) and nothing
else: it is never looked up in the identity file and it grants no authority by
itself. apply's acting principal that actually reserves the space is resolved
from configuration instead, from the identity whose tenant_id matches the
record and whose scope (accounts, environments, regions) covers it exactly; if
none does, or more than one does, the record is refused rather than guessed
at. abandon takes no tenant at all -- it acts on the ledger's own hold, not as
a tenant (docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md).

apply re-runs plan for every record before writing anything and refuses to
start -- writing nothing -- if any record's plan verdict is a refusal. It then
adopts records one at a time, vpc rows before subnet rows regardless of the
file's own order, and stops at the first refusal, error, or pending (202)
result: there is no "continue on error", because an adoption has no undo
(docs/decisions/0010-ADOPTING_EXISTING_NETWORKS_AS_ALLOCATIONS.md). Re-running
apply over a file it already adopted converges: every already-committed
record replays with no new write.

a VPC and its subnets take two runs. The service exempts a parent VPC from its
subnet's overlap rule only on the evidence of the parent's binding, and an
adopted VPC is born RESERVED and unbound: the platform cannot tag somebody
else's VPC. So a subnet whose parent has no binding yet -- including a parent
adopted a moment ago in the same run -- is reported as waiting_for_parent and
is not attempted; it does not stop the run. Once the owning team has tagged the
VPC with both platform tags and the worker has observed it, the parent is
ACTIVE; re-run apply then, and the parent replays while its subnets are adopted.

abandon (ADR 0012) withdraws an *uncommitted* adoption whose reviewed record
was wrong and cannot be made to match the ground truth, so it fences its
overlap domain forever (every reservation there answers 503 domain_busy) with
no way to finish. What abandon is NOT: it never touches a committed
allocation -- that is ownership, and DELETE /v1/allocations/{id} (release) is
the answer there, absolutely, in every state a committed allocation can be in;
it never deletes an AWS resource or a NetBox prefix (it clears a
half-converted prefix's ownership fields back to imported occupancy, keeping
the import tag, batch and source -- the prefix survives); and it is not an
undo of a real adoption -- nothing it acts on was ever owned. Its two ids come
from apply's own report (a pending, 202 outcome carries allocation_id and
operation_id) or from an open adoption_stuck finding naming the allocation.
--operation-id is optional and, when given, must name that allocation's own
ADOPT operation. --dry-run runs every check and the same inventory search,
writes nothing at all, and prints what a real run would find and would do;
combined with any other flag it can never reach the write path. Every outcome
that reaches the service -- success, a refusal, an uncertain infrastructure
failure -- prints one JSON report on stdout, exactly like plan and apply; a
refusal from before the service was reached (a usage error) prints none.

exit codes:
  0 ok (every record adopted or replayed; an abandon that fenced, cleared and
    deleted, or a dry run that completed)
  2 usage
  3 validation -- plan/apply: bad input, a refusal, a pending result, or a
    subnet waiting for its parent's binding; abandon: a refusal the operator
    must act on (unknown allocation, committed, wrong or already-terminal
    operation, an abandon already in progress, the inventory's own refusal)
  4 adapter -- plan/apply: the table could not be opened, or a ledger/NetBox/
    cloud failure; abandon: an uncertain or infrastructure outcome (the
    inventory did not say whether it cleared the prefix, the ledger could not
    be reached); re-running abandon is always safe after this exit
`

// CheckArgs validates args -- a recognized subcommand with its required
// positional table path and flags -- without any configuration, ledger or
// service: it is a pure function of args. cmd/platform-ipam/main.go calls it
// before opening the ledger connection, because "adopt" (unlike "client" and
// "onboard") otherwise dispatches only after settings, the ledger and
// configuration are already built (docs/WORK_PLAN.md package F4: adopt needs
// the ledger and the cloud observer, so it shares api/worker's construction).
// Without this split, "platform-ipam adopt" with no arguments -- or any other
// plain usage mistake -- would report a database or NetBox configuration
// error before ever reaching adoptcmd's own usage message, and would refuse
// to even print it in an environment with no database configured at all.
// ok reports whether args are well-formed enough to proceed to a real run;
// when it is false, code is already the correct process exit code and the
// usage text (or, for "help", the help text) has already been written.
func CheckArgs(args []string, stdout, stderr io.Writer) (code int, ok bool) {
	if len(args) == 0 {
		fmt.Fprint(stderr, helpText)
		return ExitUsage, false
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, helpText)
		return ExitOK, false
	}
	if err := checkArgs(args); err != nil {
		return usageFail(stderr, err), false
	}
	return ExitOK, true
}

// checkArgs is CheckArgs's logic without the printing, reused by Main so
// there is exactly one definition of what a well-formed plan/apply
// invocation looks like.
func checkArgs(args []string) error {
	switch args[0] {
	case "help", "-h", "--help":
		return nil
	case "plan":
		positional, flagArgs := splitPositional(args[1:])
		set := newFlagSet("plan")
		if err := parseFlags(set, flagArgs); err != nil {
			return err
		}
		if len(positional) != 1 {
			return usagef("plan: exactly one table path is required")
		}
		return nil
	case "apply":
		positional, flagArgs := splitPositional(args[1:], "operator")
		set := newFlagSet("apply")
		operator := set.String("operator", "", "audit subject of the person running this adoption (required)")
		if err := parseFlags(set, flagArgs); err != nil {
			return err
		}
		if len(positional) != 1 {
			return usagef("apply: exactly one table path is required")
		}
		if *operator == "" {
			return usagef("apply: --operator is required")
		}
		return nil
	case "abandon":
		return checkAbandonArgs(args[1:])
	default:
		return usagef("unknown command %q", args[0])
	}
}

// Main runs one adopt invocation and returns the process exit code. cfg
// supplies the identities the acting principal is resolved from; svc is
// everything adopt needs from the service (real in cmd/platform-ipam/main.go,
// a fake in tests). Main re-validates args with checkArgs on its own, so it
// remains correct when called directly (as every test in this package does)
// without going through CheckArgs first.
func Main(ctx context.Context, args []string, cfg domain.Config, svc Service, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, helpText)
		return ExitUsage
	}
	if args[0] != "help" && args[0] != "-h" && args[0] != "--help" {
		if err := checkArgs(args); err != nil {
			return usageFail(stderr, err)
		}
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, helpText)
		return ExitOK
	case "plan":
		return runPlan(ctx, rest, cfg, svc, stdout, stderr)
	case "apply":
		return runApply(ctx, rest, cfg, svc, stdout, stderr)
	case "abandon":
		return runAbandon(ctx, rest, svc, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "platform-ipam adopt: unknown command %q\n\n%s", command, helpText)
		return ExitUsage
	}
}

// --- flag/usage plumbing, in the style of internal/onboardcmd.go ---

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func parseFlags(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		return usagef("%s: %v", set.Name(), err)
	}
	return nil
}

// splitPositional separates this subcommand's known flags (and their values)
// from bare positional arguments, exactly as internal/onboardcmd's own
// splitPositional does and for the same reason: "plan records.csv" and
// "apply records.csv --operator alice" put the positional table path before
// the flags, which flag.FlagSet.Parse alone cannot handle (it stops at the
// first non-flag argument).
func splitPositional(args []string, names ...string) (positional, flagArgs []string) {
	known := make(map[string]bool, len(names))
	for _, n := range names {
		known[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		flagArgs = append(flagArgs, a)
		if !known[name] || strings.Contains(a, "=") {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return positional, flagArgs
}

func usageFail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "platform-ipam adopt: %v\n\n%s", err, helpText)
	return ExitUsage
}

// --- plan ---

func runPlan(ctx context.Context, args []string, cfg domain.Config, svc Service, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args)
	set := newFlagSet("plan")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 1 {
		return usageFail(stderr, usagef("plan: exactly one table path is required"))
	}

	f, err := os.Open(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt plan: %v\n", err)
		return ExitAdapter
	}
	defer f.Close()
	records, err := ReadRecords(positional[0], f)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt plan: %v\n", err)
		return ExitValidation
	}

	ordered := orderParentsFirst(records)
	pl := newParentLookup()
	pl.noteFileRows(ordered)
	if err := pl.seedFromLedger(ctx, svc, distinctTenants(ordered)); err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt plan: %v\n", err)
		return ExitAdapter
	}

	// plan takes no --operator flag: the pin's Operator field is required by
	// the service, but plan's own verdict never depends on which subject it
	// is, so a fixed placeholder keeps `plan` runnable without deciding who
	// will eventually run `apply`. It is never written anywhere plan itself
	// touches, since plan calls PlanAdoption, which never persists anything.
	const planOperatorPlaceholder = "adopt-plan"
	report := evaluateAll(ctx, svc, cfg, ordered, pl, planOperatorPlaceholder)

	if err := printJSON(stdout, report); err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt plan: writing report: %v\n", err)
		return ExitAdapter
	}
	printPlanSummary(stderr, report)
	if report.HasRefusals() {
		return ExitValidation
	}
	return ExitOK
}

func printPlanSummary(stderr io.Writer, rep Report) {
	counts := map[Verdict]int{}
	for _, r := range rep.Records {
		counts[r.Verdict]++
	}
	fmt.Fprintf(stderr, "platform-ipam adopt plan: %d record(s): %d would adopt, %d would replay, %d pending, %d deferred on parent, %d refused\n",
		len(rep.Records), counts[VerdictWouldAdopt], counts[VerdictWouldReplay], counts[VerdictPending], counts[VerdictDeferred], counts[VerdictRefused])
}

// --- apply ---

func runApply(ctx context.Context, args []string, cfg domain.Config, svc Service, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "operator")
	set := newFlagSet("apply")
	operator := set.String("operator", "", "audit subject of the person running this adoption (required)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 1 {
		return usageFail(stderr, usagef("apply: exactly one table path is required"))
	}
	if *operator == "" {
		return usageFail(stderr, usagef("apply: --operator is required"))
	}

	f, err := os.Open(positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt apply: %v\n", err)
		return ExitAdapter
	}
	defer f.Close()
	records, err := ReadRecords(positional[0], f)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt apply: %v\n", err)
		return ExitValidation
	}

	report, err := performApply(ctx, svc, cfg, records, *operator)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt apply: %v\n", err)
		return ExitAdapter
	}

	if err := printJSON(stdout, report); err != nil {
		fmt.Fprintf(stderr, "platform-ipam adopt apply: writing report: %v\n", err)
		return ExitAdapter
	}

	if report.Plan.HasRefusals() {
		printPlanSummary(stderr, report.Plan)
		fmt.Fprintln(stderr, "platform-ipam adopt apply: refusing to start; plan reported refusals above -- resolve them and re-run plan first")
		return ExitValidation
	}

	adopted, failed, notAttempted, waiting, adapterFailure := 0, 0, 0, 0, false
	for _, o := range report.Outcomes {
		switch o.Status {
		case OutcomeAdopted:
			adopted++
		case OutcomeFailed:
			failed++
			if o.Adapter {
				adapterFailure = true
			}
		case OutcomeNotAttempted:
			notAttempted++
		case OutcomeWaitingForParent:
			waiting++
		}
	}
	fmt.Fprintf(stderr, "platform-ipam adopt apply: %d adopted, %d failed, %d not attempted, %d waiting for a parent binding, %d total\n",
		adopted, failed, notAttempted, waiting, len(report.Outcomes))
	if waiting > 0 {
		fmt.Fprintln(stderr, "platform-ipam adopt apply: subnets wait until their parent VPC is ACTIVE -- tagged by its owning team and observed by the worker; re-run apply then (already adopted records replay)")
	}

	switch {
	case adapterFailure:
		return ExitAdapter
	case failed > 0 || notAttempted > 0 || waiting > 0:
		return ExitValidation
	default:
		return ExitOK
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
