// Package onboardcmd implements the `platform-ipam onboard` process mode
// (docs/ONBOARDING_IMPORT.md section 2, ADR 0007): parse, plan, apply and the
// two render steps that turn tables people already have into NetBox
// occupancy, config fragments and a fake-cloud fixture. It is a process mode,
// not a CLI verb (internal/cli holds no policy and there is no endpoint for
// this to call), and it never opens the ledger database: Main is dispatched
// in cmd/platform-ipam/main.go before storage.NewPostgresLedger is even
// constructed, exactly like the "client" mode.
//
// This package composes the building blocks in internal/onboard (pure) and
// internal/netbox (the adapter). It does no table parsing, validation or
// rendering of its own -- it only wires flags, environment, configuration
// and the NetBox client around those calls, and decides what apply writes.
package onboardcmd

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/config"
	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// Exit codes. Two categories beyond usage, per docs/WORK_PLAN.md section 2's
// contract for this package ("distinct non-zero codes for validation errors
// vs. NetBox/transport errors"): ExitValidation covers everything the
// operator's data or table is responsible for (bad rows, a table.json that
// does not decode, a plan/apply refusal); ExitAdapter covers everything the
// environment around NetBox and configuration is responsible for (a config
// file that does not load, NetBox unreachable, an EnsureOccupancy refusal).
const (
	ExitOK         = 0
	ExitUsage      = 2
	ExitValidation = 3
	ExitAdapter    = 4
)

const helpText = `usage: platform-ipam onboard <command> [flags]

commands:
  parse          any supported input -> canonical table JSON
                   parse <input>... --out table.json [--sheet NAME]
  plan           validate a table; print a report; write nothing
                   plan table.json --domain ID
  apply          re-run plan, then write NetBox occupancy
                   apply table.json --domain ID --batch NAME [--source TEXT] [--aws-objects] [--refresh]
  render-config  print YAML config fragments for human review
                   render-config table.json [--role-name N] [--default-region R ...]
  render-fixture print a fake-cloud observation fixture
                   render-fixture table.json --domain ID [--generation G]
  drift          compare the NetBox AWS plugin's accounts to cloud_coverage
                   drift --domain ID   (read-only; see docs/NETBOX_AWS_PLUGIN.md)
  remove         remove one imported prefix once every contributor is known, covered and observed absent
                   remove CIDR --domain ID [--inventory DIR | --networks IN...] [--failures F] [--accounts A] [--run R]
                          [--apply --id NETBOX_ID] [--out PATH]
                   (dry run by default; writes to NetBox only with --apply and the NetBox id a dry run
                   reported -- see docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md)
  assess         offline overlap report over the organization inventory's own records
                   assess [--inventory DIR | --networks IN...] [--failures F] [--accounts A] [--run R]
                          [--matrix M] [--ownership O] [--fixed X] [--decisions D]
                          [--format json|text] [--out PATH] [--stamp S] [--max-relationships N]
                   (read-only; no ledger, no NetBox, no AWS, no pools configuration, no IPAM_ variable
                   -- see docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md)
  progress       offline progress report deriving three facts per planned move from a reviewed plan
                   progress --plan P... [--inventory DIR | --networks IN...] [--failures F] [--accounts A] [--run R]
                          [--matrix M] [--ownership O] [--fixed X] [--decisions D] [--allocations E...]
                          [--format json|text] [--out PATH] [--stamp S] [--max-relationships N]
                   (read-only; no ledger, no NetBox, no AWS, no pools configuration, no IPAM_ variable
                   -- see docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md)

parse accepts several inputs of the SAME table kind (concatenated); "-" reads
a text table (CSV/TSV/paste) from stdin. render-config, render-fixture,
drift and assess never write into the repository (assess writes only stdout,
stderr and its own single --out path, if given), and drift never writes to
NetBox either; apply refuses to run if plan reports any error, and re-running
it after a successful apply reports every entry "unchanged" and performs no
writes.

apply --aws-objects additionally creates AWS Account, VPC and Subnet objects
in netbox-aws-vpc-plugin, linked to the prefixes apply just wrote (or found
already unmanaged) -- docs/WORK_PLAN.md package N3, ADR 0009. It probes the
plugin before any occupancy write and refuses (exit 4) if the plugin is not
installed. Without the flag, apply's behaviour is byte-for-byte unchanged, so
an installation without the plugin is unaffected.

apply --refresh additively updates platform_import_contributors on every
networks-table row that plan reports already-unmanaged -- docs/WORK_PLAN.md
package M9b3, ADR 0016. It adds a contributor the table names that the
prefix does not carry yet, and refreshes an already-named contributor's
observation time and last-seen batch; it never removes a contributor a
table happens not to name (a table is a scope, not a census), and it writes
at all only when the contributor set actually grows or the prefix carried no
list yet (a prefix imported before this existed gains a reconstructed list,
flagged as such). It never touches the description, tags, status, tenant, or
any other operator-owned field, and it refuses a prefix that carries any
ownership field or a contributor list this version cannot read. Only a
networks table can carry contributors, so --refresh is a no-op for a ranges
table. Without the flag, apply's behaviour is byte-for-byte unchanged.

assess reads --matrix, --ownership, --fixed and --decisions as YAML or JSON
(converted internally; internal/assess itself decodes only JSON) and its
--networks inputs through the same reader and normalizer as parse and plan,
but it never loads a table.json and never touches any of the environment
variables below: unlike every other onboard subcommand, it needs none of
them, by design (ADR 0014).

progress reads the same --inventory/--networks/--failures/--accounts/--run
and --matrix/--ownership/--fixed/--decisions inputs assess does, calling
assess's own comparison engine in process, plus one or more --plan files (a
reviewed migration.yaml, merged and structurally validated) and zero or more
--allocations files (an authenticated allocation evidence export); it needs
none of the environment variables below either, by the same design (ADR
0015). It never writes an address: a plan document has no field for one.

environment (plan, apply, and drift only -- never assess):
  IPAM_CONFIG_FILE    pools configuration path (default examples/config/pools.yaml)
  IPAM_IDENTITY_FILE  identity mapping file (optional)
  IPAM_ENVIRONMENT    development, stage, or prod (default development)
  IPAM_NETBOX_URL     NetBox base URL (required)
  IPAM_NETBOX_TOKEN   NetBox API token (required)

onboard never opens the ledger database: no IPAM_DATABASE_URL is read.

exit codes:
  0 ok   2 usage
  3 validation (bad input, or plan/apply/assess reported an error; for
    assess: coverage incomplete, or at least one conflict is confirmed --
    the report is still written in full; for progress: evidence is not
    complete, or some move is blocked, mismatched, unclaimed or unplanned,
    or not yet fully evidenced -- the report is still written in full)
  4 adapter (configuration, NetBox, or transport failure; for assess and
    progress, which have no adapter, this means no report exists at all: an
    input missing or unreadable, a required column absent, a
    matrix/ownership/fixed/decisions file that does not decode, more
    relationships than --max-relationships allows, or, for progress only, a
    plan that does not decode or fails a structural refusal -- nothing is
    written to stdout in this case)
`

// Main runs one onboard invocation and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, helpText)
		return ExitUsage
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, helpText)
		return ExitOK
	case "parse":
		return runParse(rest, stdout, stderr)
	case "plan":
		return runPlan(ctx, rest, stdout, stderr)
	case "apply":
		return runApply(ctx, rest, stdout, stderr)
	case "render-config":
		return runRenderConfig(rest, stdout, stderr)
	case "render-fixture":
		return runRenderFixture(rest, stdout, stderr)
	case "drift":
		return runDrift(ctx, rest, stdout, stderr)
	case "remove":
		return runRemove(ctx, rest, stdout, stderr)
	case "assess":
		// No ctx: assess calls neither NetBox nor the ledger (see assess.go's
		// own package comment), so it takes the same (args, stdout, stderr)
		// shape as runParse, runRenderConfig and runRenderFixture below.
		return runAssess(rest, stdout, stderr)
	case "progress":
		// No ctx, for the same reason assess takes none (progress.go's own
		// package comment).
		return runProgress(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "platform-ipam onboard: unknown command %q\n\n%s", command, helpText)
		return ExitUsage
	}
}

// --- shared flag/usage plumbing, in the style of internal/cli/cli.go ---

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

// parseFlags parses only; unlike internal/cli's helper it does not reject
// trailing positional arguments, because every onboard subcommand takes at
// least one (a table.json path, or parse's input list).
func parseFlags(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		return usagef("%s: %v", set.Name(), err)
	}
	return nil
}

// splitPositional separates this subcommand's known flags (and their
// values) from bare positional arguments. Design section 2 puts the
// positional table.json (or parse's input list) *before* its flags --
// "plan table.json --domain ID" -- but the standard library's
// flag.FlagSet.Parse stops at the first argument that is not a flag, which
// would leave everything after table.json unparsed. Knowing each
// subcommand's exact flag names (rather than guessing from a "-" prefix)
// also means a positional value that happens to start with "-" is never
// mistaken for a flag.
func splitPositional(args []string, names ...string) (positional, flagArgs []string) {
	return splitPositionalFlags(args, nil, names)
}

// splitPositionalFlags is splitPositional's general form: boolNames lists
// this subcommand's boolean flags (apply's --aws-objects is the only one so
// far). A boolean flag never carries a separate value token -- "apply
// table.json --aws-objects --batch b1" would otherwise have "--batch"
// swallowed as --aws-objects's "value" and handed to flag.Parse as if it
// were one, producing a confusing "invalid boolean value \"--batch\"" error
// instead of parsing --batch normally -- so boolNames is recognized as a
// known flag name but excluded from the value-consuming logic below.
func splitPositionalFlags(args []string, boolNames, names []string) (positional, flagArgs []string) {
	known := make(map[string]bool, len(names)+len(boolNames))
	for _, n := range names {
		known[n] = true
	}
	isBool := make(map[string]bool, len(boolNames))
	for _, n := range boolNames {
		isBool[n] = true
		known[n] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		// A bare "-" is not a flag -- design section 2 and the help text
		// use it as the stdin marker for parse's input list -- and Go's own
		// flag.Parse agrees (parseOne treats anything shorter than two
		// bytes as "not a flag" and stops there). Without this check "-"
		// fell into the flagArgs branch below, was rejected by flag.Parse
		// as an unknown flag before parsing even reached --out, and
		// "platform-ipam onboard parse - --out table.json" -- exactly the
		// invocation the help text advertises -- failed with "at least one
		// input is required".
		if a == "-" || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		flagArgs = append(flagArgs, a)
		if !known[name] || strings.Contains(a, "=") || isBool[name] {
			// Not one of this subcommand's flags (e.g. -h, or a typo), or
			// already carries its value as --name=value, or is a boolean
			// flag that never takes a separate value token: let flag.Parse
			// report an unknown-flag error itself, or take the value as is.
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return positional, flagArgs
}

// usageFail writes a usage error and the command's help text to stderr and
// returns ExitUsage, mirroring internal/cli.Main's usageError handling.
func usageFail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "platform-ipam onboard: %v\n\n%s", err, helpText)
	return ExitUsage
}

// stringListFlag collects a repeatable string flag, e.g. --default-region.
type stringListFlag struct{ values *[]string }

func (f stringListFlag) String() string {
	if f.values == nil {
		return ""
	}
	return strings.Join(*f.values, ",")
}
func (f stringListFlag) Set(v string) error {
	*f.values = append(*f.values, v)
	return nil
}

// --- table.json I/O ---

// loadTable reads and decodes a table.json produced by "parse". Table has no
// custom JSON tags, so the encoding is the Go standard library's ordinary,
// deterministic struct encoding -- stable across runs, and read back exactly
// as written.
func loadTable(path string) (onboard.Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return onboard.Table{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var t onboard.Table
	if err := json.Unmarshal(data, &t); err != nil {
		return onboard.Table{}, fmt.Errorf("%s: invalid table JSON: %w", path, err)
	}
	return t, nil
}

func rowCount(t onboard.Table) int {
	switch t.Kind {
	case onboard.KindAccounts:
		return len(t.Accounts)
	case onboard.KindNetworks:
		return len(t.Networks)
	case onboard.KindRanges:
		return len(t.Ranges)
	}
	return 0
}

func diagnosticsHaveError(diags []onboard.Diagnostic) bool {
	for _, d := range diags {
		if d.Level == onboard.LevelError {
			return true
		}
	}
	return false
}

// --- parse ---

// readInput turns one named input into a Table. "-" is read as a text table
// from stdin (design section 2: "use `-` for stdin text"); stdin has no
// random access, so only ReadText's formats (CSV/TSV/paste) are available
// that way -- a real .xlsx must be a named file. Anything else is opened by
// path and handed to onboard.ReadTable, which picks the format from its
// extension.
func readInput(name string, opts onboard.ReadOptions) (onboard.Table, error) {
	if name == "-" {
		return onboard.ReadText("stdin", os.Stdin, opts)
	}
	f, err := os.Open(name)
	if err != nil {
		return onboard.Table{}, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return onboard.Table{}, fmt.Errorf("%s: %w", name, err)
	}
	return onboard.ReadTable(name, f, info.Size(), opts)
}

func concatTables(a, b onboard.Table) onboard.Table {
	a.Accounts = append(a.Accounts, b.Accounts...)
	a.Networks = append(a.Networks, b.Networks...)
	a.Ranges = append(a.Ranges, b.Ranges...)
	a.Diagnostics = append(a.Diagnostics, b.Diagnostics...)
	return a
}

// stampSourceFile records which input a table's networks rows came from
// (ADR 0014, docs/WORK_PLAN.md M1b1): concatTables merges several inputs
// whose SourceRow each restarts at 1 in its own file (internal/onboard's
// shiftRows/remapXLSXRowNumbers renumber a row to the position an operator
// sees within ITS OWN input, knowing nothing of any other input being
// concatenated alongside it), so a merged table cannot say on its own
// whether row 12 is row 12 of the first input or of the second.
// NetworkRow.SourceFile carries that distinction from here on.
//
// It is applied here, in runParse, right after each input is read and
// before any concatenation -- not inside internal/onboard's readers, which
// stay pure format decoders with no opinion on what a caller will call their
// input; not inside concatTables itself, which would then need to know each
// side's file name too, for no benefit over stamping at the read site. Every
// input is stamped, including the first: a single-input parse gains a
// correct, non-empty SourceFile as a side effect, which is strictly more
// useful than leaving it blank, and changes nothing else about a
// single-input run's Networks rows or Plan's output (Plan, Finding and
// WriteEntry never read SourceFile -- see the M1b1 report for why that
// scope was deliberately left alone).
func stampSourceFile(t onboard.Table, name string) onboard.Table {
	for i := range t.Networks {
		t.Networks[i].SourceFile = name
	}
	return t
}

func runParse(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "out", "sheet")
	set := newFlagSet("parse")
	out := set.String("out", "", "output path for the canonical table JSON (required)")
	sheet := set.String("sheet", "", "worksheet name for .xlsx input (default: first sheet)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	inputs := positional
	if len(inputs) == 0 {
		return usageFail(stderr, usagef("parse: at least one input is required"))
	}
	if *out == "" {
		return usageFail(stderr, usagef("parse: --out is required"))
	}

	opts := onboard.ReadOptions{Sheet: *sheet}
	var merged onboard.Table
	var firstName string
	for i, name := range inputs {
		t, err := readInput(name, opts)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard parse: %v\n", err)
			return ExitValidation
		}
		t = stampSourceFile(t, name)
		if i == 0 {
			merged, firstName = t, name
			continue
		}
		// Multiple inputs of the same table kind are concatenated; different
		// kinds are a usage error naming them (design section 2), because
		// silently picking one table's shape over another would drop rows
		// without saying so.
		if t.Kind != merged.Kind {
			return usageFail(stderr, usagef(
				"parse: %q is a %s table but %q is a %s table; every input must be the same table kind",
				name, t.Kind, firstName, merged.Kind))
		}
		merged = concatTables(merged, t)
	}

	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard parse: encoding table: %v\n", err)
		return ExitAdapter
	}
	data = append(data, '\n')
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard parse: writing %s: %v\n", *out, err)
		return ExitAdapter
	}

	hasError := diagnosticsHaveError(merged.Diagnostics)
	fmt.Fprintf(stderr, "platform-ipam onboard parse: wrote %s (%s table, %d rows, %d diagnostics%s)\n",
		*out, merged.Kind, rowCount(merged), len(merged.Diagnostics), errorSuffix(hasError))
	if hasError {
		// The file is still written -- the operator inspects it and decides
		// what to fix -- but the process reports failure so a pipeline stops.
		return ExitValidation
	}
	return ExitOK
}

func errorSuffix(hasError bool) string {
	if hasError {
		return ", including at least one error"
	}
	return ""
}

// --- shared config/NetBox wiring for plan and apply ---

// settings holds only the environment onboard needs. It deliberately does
// not reuse config.Settings.Validate: that method unconditionally requires
// IPAM_DATABASE_URL (and validates auth-mode/AWS-mode/OIDC settings this
// process mode never touches), which would either force onboard to invent a
// database it never opens or weaken Validate for every other mode. Instead
// this package reads the same IPAM_* variable names Settings.Environment
// does, and validates only what plan and apply actually use: an environment
// value config.Load can act on, and (since NetBox is only needed once a
// write path is taken) a NetBox origin and token.
type settings struct {
	ConfigFile   string
	IdentityFile string
	Environment  string
	NetBoxURL    string
	NetBoxToken  string
}

func loadSettings() settings {
	get := func(key, fallback string) string {
		if v := os.Getenv("IPAM_" + key); v != "" {
			return v
		}
		return fallback
	}
	return settings{
		ConfigFile:   get("CONFIG_FILE", "examples/config/pools.yaml"),
		IdentityFile: get("IDENTITY_FILE", ""),
		Environment:  get("ENVIRONMENT", "development"),
		NetBoxURL:    get("NETBOX_URL", ""),
		NetBoxToken:  get("NETBOX_TOKEN", ""),
	}
}

func (s settings) validate() error {
	switch s.Environment {
	case "development", "stage", "prod":
	default:
		return fmt.Errorf("IPAM_ENVIRONMENT must be development, stage, or prod")
	}
	if s.NetBoxURL == "" {
		return fmt.Errorf("IPAM_NETBOX_URL is required")
	}
	if s.NetBoxToken == "" {
		return fmt.Errorf("IPAM_NETBOX_TOKEN is required")
	}
	return nil
}

// adapter bundles the loaded configuration and NetBox client plan and apply
// share, so apply's "re-run plan" does not risk building them differently
// from a standalone plan invocation.
type adapter struct {
	cfg domain.Config
	inv *netbox.Client
}

func newAdapter() (adapter, error) {
	s := loadSettings()
	if err := s.validate(); err != nil {
		return adapter{}, err
	}
	cfg, err := config.Load(s.ConfigFile, s.IdentityFile, s.Environment)
	if err != nil {
		return adapter{}, fmt.Errorf("loading pools configuration: %w", err)
	}
	inv, err := netbox.New(netbox.Config{BaseURL: s.NetBoxURL, Token: s.NetBoxToken, Domains: cfg.Domains, Pools: cfg.Pools})
	if err != nil {
		return adapter{}, fmt.Errorf("invalid NetBox adapter configuration: %w", err)
	}
	return adapter{cfg: cfg, inv: inv}, nil
}

func resolveDomain(cfg domain.Config, domainID string) (domain.Domain, bool) {
	for _, d := range cfg.Domains {
		if d.ID == domainID {
			return d, true
		}
	}
	return domain.Domain{}, false
}

// plan takes a real inventory snapshot and calls onboard.Plan. It only calls
// NetBox when the domain and its VRF are actually configured: an unknown
// domain or a domain without a VRF is refused by onboard.Plan itself, before
// it ever looks at snap, and asking NetBox for an unrestricted, unscoped
// snapshot in that case would be both wasted and dangerous (Client.Snapshot
// treats a zero VRF ID as "every VRF", not "none"). A genuine Snapshot
// failure is returned as an error here, never folded into an empty
// snapshot: an unreadable inventory is not an empty one (design section 5),
// and Plan's own RuleIncompleteSnapshot check only protects against a
// snapshot that came back marked incomplete, not one that never came back.
func (a adapter) plan(ctx context.Context, table onboard.Table, domainID string) (onboard.Report, error) {
	var snap domain.InventorySnapshot
	if d, ok := resolveDomain(a.cfg, domainID); ok && d.Backend.VRFID != 0 {
		s, err := a.inv.Snapshot(ctx, d)
		if err != nil {
			return onboard.Report{}, fmt.Errorf("taking NetBox inventory snapshot for domain %q: %w", domainID, err)
		}
		snap = s
	}
	return onboard.Plan(table, a.cfg, domainID, snap), nil
}

// --- plan ---

func writeReportJSON(stdout io.Writer, report onboard.Report) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func printReportSummary(stderr io.Writer, report onboard.Report) {
	counts := map[onboard.Level]int{}
	for _, f := range report.Findings {
		counts[f.Level]++
	}
	fmt.Fprintf(stderr, "platform-ipam onboard: %d error(s), %d warning(s), %d info, write-set %d\n",
		counts[onboard.LevelError], counts[onboard.LevelWarning], counts[onboard.LevelInfo], len(report.Writes))
}

func runPlan(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "domain")
	set := newFlagSet("plan")
	domainID := set.String("domain", "", "overlap domain id (required)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	rest := positional
	if len(rest) != 1 {
		return usageFail(stderr, usagef("plan: exactly one table.json path is required"))
	}
	if *domainID == "" {
		return usageFail(stderr, usagef("plan: --domain is required"))
	}

	table, err := loadTable(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard plan: %v\n", err)
		return ExitValidation
	}

	a, err := newAdapter()
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard plan: %v\n", err)
		return ExitAdapter
	}
	report, err := a.plan(ctx, table, *domainID)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard plan: %v\n", err)
		return ExitAdapter
	}

	if err := writeReportJSON(stdout, report); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard plan: writing report: %v\n", err)
		return ExitAdapter
	}
	printReportSummary(stderr, report)
	if report.HasErrors() {
		return ExitValidation
	}
	return ExitOK
}

// --- apply ---

// applyResultLine is one JSON-Lines record printed to stdout per WriteEntry,
// in the order onboard.Plan produced them, and -- since package M9b3 -- per
// already-unmanaged CIDR --refresh attempted, in the order onboard.Plan's
// findings carry them. Refresh and Reconstructed are both omitted (false)
// for an ordinary create/unchanged line, so the JSON shape of every line
// apply already printed is unchanged.
type applyResultLine struct {
	Kind          string `json:"kind"`
	CIDR          string `json:"cidr,omitempty"`
	Start         string `json:"start_address,omitempty"`
	End           string `json:"end_address,omitempty"`
	Action        string `json:"action"`
	NetBoxID      string `json:"netbox_id"`
	SourceRows    []int  `json:"source_rows"`
	Refresh       bool   `json:"refresh,omitempty"`
	Reconstructed bool   `json:"reconstructed,omitempty"`
}

func entryLabel(e onboard.WriteEntry) string {
	if e.Kind == onboard.WriteRange {
		return e.StartAddress + "-" + e.EndAddress
	}
	return e.CIDR
}

func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositionalFlags(args, []string{"aws-objects", "refresh"}, []string{"domain", "batch", "source"})
	set := newFlagSet("apply")
	domainID := set.String("domain", "", "overlap domain id (required)")
	batch := set.String("batch", "", "import batch name, written to every entry (required)")
	source := set.String("source", "", "free-form source label written to every entry, e.g. the input file name")
	awsObjects := set.Bool("aws-objects", false,
		"additionally create AWS Account, VPC and Subnet objects in netbox-aws-vpc-plugin, linked to the imported prefixes")
	refresh := set.Bool("refresh", false,
		"additively update platform_import_contributors on every already-unmanaged prefix the table still names (ADR 0016); without it apply is unchanged")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	rest := positional
	if len(rest) != 1 {
		return usageFail(stderr, usagef("apply: exactly one table.json path is required"))
	}
	if *domainID == "" || *batch == "" {
		return usageFail(stderr, usagef("apply: --domain and --batch are required"))
	}

	table, err := loadTable(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard apply: %v\n", err)
		return ExitValidation
	}

	a, err := newAdapter()
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard apply: %v\n", err)
		return ExitAdapter
	}

	// --aws-objects probes the plugin FIRST, before any occupancy write
	// (docs/WORK_PLAN.md package N3): a missing plugin must never leave a
	// partial import (prefixes written, plugin objects impossible) behind.
	if *awsObjects {
		if err := a.inv.ProbeAWSPlugin(ctx); err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: --aws-objects: %v\n", err)
			return ExitAdapter
		}
	}

	// apply never trusts a previous plan run: it re-plans this table against
	// the real configuration and a fresh NetBox snapshot before writing
	// anything, so a table.json edited or reused since it was last checked
	// cannot slip an unvalidated row into NetBox.
	report, err := a.plan(ctx, table, *domainID)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard apply: %v\n", err)
		return ExitAdapter
	}
	if report.HasErrors() {
		printReportSummary(stderr, report)
		fmt.Fprintln(stderr, "platform-ipam onboard apply: refusing to write; plan reported errors above -- resolve them and re-run plan first")
		return ExitValidation
	}

	// report.HasErrors() is false, so onboard.Plan did not refuse on an
	// unknown domain or a domain without a VRF: resolveDomain must succeed.
	d, _ := resolveDomain(a.cfg, *domainID)

	created, unchanged := 0, 0
	enc := json.NewEncoder(stdout)
	for _, entry := range report.Writes {
		occ, err := entryOccupancy(table, entry, *batch, *source)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: entry %s (source rows %v): %v\n", entryLabel(entry), entry.SourceRows, err)
			return ExitAdapter
		}
		result, err := a.inv.EnsureOccupancy(ctx, d, occ)
		if err != nil {
			// Any ErrOccupancy* (or a transport failure) stops the run here:
			// entries already written stay written -- apply is idempotent, so
			// a re-run after the offending row is fixed reports them
			// unchanged -- but nothing after this entry is attempted.
			fmt.Fprintf(stderr, "platform-ipam onboard apply: entry %s (source rows %v): %v\n", entryLabel(entry), entry.SourceRows, err)
			return ExitAdapter
		}
		if result.Action == netbox.OccupancyCreated {
			created++
		} else {
			unchanged++
		}
		line := applyResultLine{
			Kind: string(entry.Kind), CIDR: entry.CIDR, Start: entry.StartAddress, End: entry.EndAddress,
			Action: result.Action, NetBoxID: result.ID, SourceRows: entry.SourceRows,
		}
		if err := enc.Encode(line); err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: writing result: %v\n", err)
			return ExitAdapter
		}
	}
	fmt.Fprintf(stderr, "platform-ipam onboard apply: batch %q: %d created, %d unchanged, %d total\n", *batch, created, unchanged, len(report.Writes))

	// --refresh (docs/WORK_PLAN.md package M9b3, ADR 0016) is a second,
	// independent write phase over a DIFFERENT set of CIDRs than the loop
	// above: report.Writes never contains a CIDR Plan found already
	// unmanaged (planPrefixCandidates drops it with RuleAlreadyUnmanaged),
	// which is exactly the set --refresh acts on. Gated entirely behind the
	// flag, so omitting it makes this function issue not one additional
	// NetBox request -- the byte-for-byte property ADR 0016 requires.
	if *refresh {
		counts, err := applyRefresh(ctx, a, d, table, report, *batch, *source, enc)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: --refresh: %v\n", err)
			return ExitAdapter
		}
		fmt.Fprintf(stderr, "platform-ipam onboard apply: --refresh: %d written (%d reconstructed), %d unchanged, %d total\n",
			counts.Written, counts.Reconstructed, counts.Unchanged, counts.Total)
	}

	if *awsObjects {
		counts, err := applyAWSObjects(ctx, a, d, table, enc, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: %v\n", err)
			return ExitAdapter
		}
		fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: %d created, %d updated, %d unchanged, %d total\n",
			counts.Created, counts.Updated, counts.Unchanged, counts.Total)
	}
	return ExitOK
}

// --- apply --refresh ---

// refreshCounts tallies every RefreshOccupancy call applyRefresh made, for
// the summary line printed after it runs. Reconstructed counts the subset
// of Written whose prefix carried no contributor list at all before this
// call (ADR 0016's reconstruct path); it is not a separate action.
type refreshCounts struct {
	Written, Reconstructed, Unchanged, Total int
}

// applyRefresh is ADR 0016's additive refresh (docs/WORK_PLAN.md package
// M9b3): for every CIDR onboard.Plan already reported RuleAlreadyUnmanaged
// for -- the report the same *table has just re-validated -- it builds the
// table's own would-be contributor list for that CIDR (the same
// construction entryOccupancy's create path uses, via networkContributors)
// and calls netbox.RefreshOccupancy, which decides on its own evidence
// whether anything needs writing.
//
// It only ever touches a networks table's rows: a ranges table's CIDR
// candidates go through the same RuleAlreadyUnmanaged rule
// (internal/onboard/plan.go's planPrefixCandidates is shared), but
// entryOccupancy never builds contributors for one either, because the
// canonical ranges table has no account/region/resource columns to build
// them from -- so this returns immediately, having made no request, for
// anything but table.Kind == onboard.KindNetworks.
func applyRefresh(ctx context.Context, a adapter, d domain.Domain, table onboard.Table, report onboard.Report, batch, source string, enc *json.Encoder) (refreshCounts, error) {
	var counts refreshCounts
	if table.Kind != onboard.KindNetworks {
		return counts, nil
	}
	for _, f := range report.Findings {
		if f.Rule != onboard.RuleAlreadyUnmanaged {
			continue
		}
		rows := networkRowsForCIDR(table.Networks, f.CIDR)
		contributors := networkContributors(rows, batch)
		result, err := a.inv.RefreshOccupancy(ctx, d, f.CIDR, source, contributors)
		if err != nil {
			return counts, fmt.Errorf("entry %s (source rows %v): %w", f.CIDR, f.Rows, err)
		}
		counts.Total++
		switch result.Action {
		case netbox.RefreshWritten:
			counts.Written++
			if result.Reconstructed {
				counts.Reconstructed++
			}
		default:
			counts.Unchanged++
		}
		line := applyResultLine{
			Kind: string(onboard.WritePrefix), CIDR: f.CIDR, Action: result.Action,
			NetBoxID: result.ID, SourceRows: f.Rows, Refresh: true, Reconstructed: result.Reconstructed,
		}
		if err := enc.Encode(line); err != nil {
			return counts, fmt.Errorf("writing refresh result: %w", err)
		}
	}
	return counts, nil
}

// --- apply --aws-objects ---

// awsObjectCounts tallies every plugin (or dcim.Region) object
// applyAWSObjects ensured, for the summary line printed after it runs.
type awsObjectCounts struct {
	Created, Updated, Unchanged, Total int
}

func (c *awsObjectCounts) record(action string) {
	c.Total++
	switch action {
	case netbox.AWSObjectCreated:
		c.Created++
	case netbox.AWSObjectUpdated:
		c.Updated++
	default:
		c.Unchanged++
	}
}

// awsObjectResultLine is one JSON-Lines record per plugin object
// applyAWSObjects ensures, printed to the same stdout stream as
// applyResultLine, after every occupancy result line.
type awsObjectResultLine struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Action string `json:"action"`
}

// applyAWSObjects ensures AWS Account, VPC and Subnet objects in the plugin
// for a table apply --aws-objects just imported (docs/WORK_PLAN.md package
// N3, ADR 0009): every plugin object it creates or confirms points at a
// prefix EnsureOccupancy already wrote as unmanaged occupancy, one call
// earlier in the same apply run -- this function never creates or modifies
// a prefix itself. Called only when apply --aws-objects is set, and only
// after the occupancy loop above has finished successfully.
//
// For a networks table it walks table.Networks directly, not
// report.Writes: a row whose CIDR was already present in NetBox is dropped
// from the write set by onboard.Plan's RuleAlreadyUnmanaged (design section
// 5, "makes apply repeatable"), but its plugin objects still need linking,
// exactly as docs/WORK_PLAN.md package N3 requires ("including rows whose
// prefix already existed and was therefore dropped from the write set").
func applyAWSObjects(ctx context.Context, a adapter, d domain.Domain, table onboard.Table, enc *json.Encoder, stderr io.Writer) (awsObjectCounts, error) {
	var counts awsObjectCounts

	emit := func(kind, key, action string) error {
		counts.record(action)
		return enc.Encode(awsObjectResultLine{Kind: kind, Key: key, Action: action})
	}

	regionIDs := map[string]int{}
	ensureRegion := func(name string) (int, error) {
		if name == "" {
			return 0, nil
		}
		if id, ok := regionIDs[name]; ok {
			return id, nil
		}
		res, err := a.inv.EnsureAWSRegion(ctx, name)
		if err != nil {
			return 0, fmt.Errorf("aws region %s: %w", name, err)
		}
		id, err := strconv.Atoi(res.ID)
		if err != nil {
			return 0, fmt.Errorf("aws region %s: non-numeric NetBox id %q", name, res.ID)
		}
		regionIDs[name] = id
		if err := emit("aws-region", name, res.Action); err != nil {
			return 0, err
		}
		return id, nil
	}

	accountIDs := map[string]int{}
	ensureAccount := func(accountID, name string) (int, error) {
		if accountID == "" {
			return 0, nil
		}
		if id, ok := accountIDs[accountID]; ok {
			return id, nil
		}
		res, err := a.inv.EnsureAWSAccount(ctx, netbox.AWSAccountSpec{AccountID: accountID, Name: name})
		if err != nil {
			return 0, fmt.Errorf("aws account %s: %w", accountID, err)
		}
		id, err := strconv.Atoi(res.ID)
		if err != nil {
			return 0, fmt.Errorf("aws account %s: non-numeric NetBox id %q", accountID, res.ID)
		}
		accountIDs[accountID] = id
		if err := emit("aws-account", accountID, res.Action); err != nil {
			return 0, err
		}
		return id, nil
	}

	switch table.Kind {
	case onboard.KindAccounts:
		// "Accounts tables: ensure AWSAccount objects (id + name)"
		// (docs/WORK_PLAN.md package N3): this is the one place an account
		// name is actually known, since a networks row carries only an
		// account id.
		for _, row := range table.Accounts {
			if _, err := ensureAccount(row.AccountID, row.AccountName); err != nil {
				return counts, fmt.Errorf("source row %d: %w", row.SourceRow, err)
			}
		}
		return counts, nil
	case onboard.KindRanges:
		// A ranges table carries no account or resource id to link a
		// plugin object to: nothing to do.
		return counts, nil
	}

	// table.Kind == onboard.KindNetworks from here on.
	vpcNetBoxID := map[string]int{}
	// Pass 1: every VPC row, so pass 2 can link a subnet to a VPC this same
	// apply run just ensured regardless of the two rows' order in the table
	// (NetworkRow.Type is "not validated" and rows are not guaranteed
	// sorted by it).
	for _, row := range table.Networks {
		if row.ResourceID == "" {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: row %d has no resource id; skipped\n", row.SourceRow)
			continue
		}
		if row.Type != "vpc" {
			continue
		}
		regionID, err := ensureRegion(row.Region)
		if err != nil {
			return counts, fmt.Errorf("source row %d: %w", row.SourceRow, err)
		}
		accountID, err := ensureAccount(row.AccountID, "")
		if err != nil {
			return counts, fmt.Errorf("source row %d: %w", row.SourceRow, err)
		}
		if accountID == 0 {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: vpc row %d (%s) has no account id; skipped\n", row.SourceRow, row.ResourceID)
			continue
		}
		res, err := a.inv.EnsureAWSVPC(ctx, d, netbox.AWSVPCSpec{
			VPCID: row.ResourceID, Name: row.Name, OwnerAccountNetBoxID: accountID,
			RegionNetBoxID: regionID, PrimaryCIDR: row.CIDR,
		})
		if err != nil {
			return counts, fmt.Errorf("aws vpc %s (source row %d): %w", row.ResourceID, row.SourceRow, err)
		}
		vpcID, err := strconv.Atoi(res.ID)
		if err != nil {
			return counts, fmt.Errorf("aws vpc %s: non-numeric NetBox id %q", row.ResourceID, res.ID)
		}
		vpcNetBoxID[row.ResourceID] = vpcID
		if err := emit("aws-vpc", row.ResourceID, res.Action); err != nil {
			return counts, err
		}
	}

	// Pass 2: every subnet row.
	for _, row := range table.Networks {
		if row.ResourceID == "" || row.Type != "subnet" {
			continue
		}
		regionID, err := ensureRegion(row.Region)
		if err != nil {
			return counts, fmt.Errorf("source row %d: %w", row.SourceRow, err)
		}
		accountID, err := ensureAccount(row.AccountID, "")
		if err != nil {
			return counts, fmt.Errorf("source row %d: %w", row.SourceRow, err)
		}
		if accountID == 0 {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: subnet row %d (%s) has no account id; skipped\n", row.SourceRow, row.ResourceID)
			continue
		}
		vpcID, ok := vpcNetBoxID[row.ParentID]
		if !ok && row.ParentID != "" {
			// The parent VPC is not part of this table's own vpc rows: it
			// may already exist in the plugin from an earlier apply run
			// ("when that VPC ... already exists in the plugin",
			// docs/WORK_PLAN.md package N3).
			existing, found, err := a.inv.LookupAWSVPC(ctx, row.ParentID)
			if err != nil {
				return counts, fmt.Errorf("looking up parent vpc %s for source row %d: %w", row.ParentID, row.SourceRow, err)
			}
			if found {
				id, err := strconv.Atoi(existing.ID)
				if err != nil {
					return counts, fmt.Errorf("aws vpc %s: non-numeric NetBox id %q", row.ParentID, existing.ID)
				}
				vpcID = id
				vpcNetBoxID[row.ParentID] = id
			}
		}
		if vpcID == 0 {
			fmt.Fprintf(stderr, "platform-ipam onboard apply: aws objects: subnet row %d (%s) has no parent vpc %q in this table or the plugin; skipped\n",
				row.SourceRow, row.ResourceID, row.ParentID)
			continue
		}
		res, err := a.inv.EnsureAWSSubnet(ctx, d, netbox.AWSSubnetSpec{
			SubnetID: row.ResourceID, Name: row.Name, VPCNetBoxID: vpcID, OwnerAccountNetBoxID: accountID,
			RegionNetBoxID: regionID, CIDR: row.CIDR, AvailabilityZone: row.AZID,
		})
		if err != nil {
			return counts, fmt.Errorf("aws subnet %s (source row %d): %w", row.ResourceID, row.SourceRow, err)
		}
		if err := emit("aws-subnet", row.ResourceID, res.Action); err != nil {
			return counts, err
		}
	}

	return counts, nil
}

// entryOccupancy builds the netbox.Occupancy for one WriteEntry: the merged
// description (name, account(s), resource id(s), and every preserved
// unknown-column/description value, one line per source row once several
// rows collapsed into it) and, for a prefix built from a networks row, the
// AWS fields -- but only when the collapsed group has exactly one row that
// carries them, since several rows with different accounts (design section
// 5's duplicate-CIDR case) makes "the" account ambiguous and EnsureOccupancy
// already refuses AWS fields on a range outright (they have no account
// column in the canonical table to begin with).
func entryOccupancy(table onboard.Table, entry onboard.WriteEntry, batch, source string) (netbox.Occupancy, error) {
	switch table.Kind {
	case onboard.KindNetworks:
		rows := networkRowsForCIDR(table.Networks, entry.CIDR)
		if len(rows) == 0 {
			return netbox.Occupancy{}, fmt.Errorf("no network row in the table matches %s", entry.CIDR)
		}
		accountID, region, resourceID := networkAWSFields(rows)
		return netbox.Occupancy{
			CIDR: entry.CIDR, Description: mergeNetworkDescription(rows), Batch: batch, Source: source,
			AWSAccountID: accountID, AWSRegion: region, AWSResourceID: resourceID,
			Contributors: networkContributors(rows, batch),
		}, nil
	case onboard.KindRanges:
		switch entry.Kind {
		case onboard.WritePrefix:
			rows := rangeCIDRRowsForCIDR(table.Ranges, entry.CIDR)
			if len(rows) == 0 {
				return netbox.Occupancy{}, fmt.Errorf("no range row in the table matches %s", entry.CIDR)
			}
			return netbox.Occupancy{CIDR: entry.CIDR, Description: mergeRangeDescription(rows), Batch: batch, Source: source}, nil
		case onboard.WriteRange:
			rows := rangeRowsForSpan(table.Ranges, entry.StartAddress, entry.EndAddress)
			if len(rows) == 0 {
				return netbox.Occupancy{}, fmt.Errorf("no range row in the table matches %s-%s", entry.StartAddress, entry.EndAddress)
			}
			return netbox.Occupancy{StartAddress: entry.StartAddress, EndAddress: entry.EndAddress,
				Description: mergeRangeDescription(rows), Batch: batch, Source: source}, nil
		}
	}
	return netbox.Occupancy{}, fmt.Errorf("unsupported write entry for a %s table", table.Kind)
}

// networkRowsForCIDR re-derives the group of NetworkRows that produced one
// WriteEntry, the same way onboard.Plan's planPrefixCandidates grouped them:
// by canonical CIDR string, not by SourceRow (one source row can contribute
// several NetworkRows when its CIDR cell held several networks).
func networkRowsForCIDR(rows []onboard.NetworkRow, cidr string) []onboard.NetworkRow {
	var out []onboard.NetworkRow
	for _, r := range rows {
		if canonicalPrefix(r.CIDR) == cidr {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceRow < out[j].SourceRow })
	return out
}

func rangeCIDRRowsForCIDR(rows []onboard.RangeRow, cidr string) []onboard.RangeRow {
	var out []onboard.RangeRow
	for _, r := range rows {
		if r.CIDR != "" && canonicalPrefix(r.CIDR) == cidr {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceRow < out[j].SourceRow })
	return out
}

func rangeRowsForSpan(rows []onboard.RangeRow, start, end string) []onboard.RangeRow {
	var out []onboard.RangeRow
	for _, r := range rows {
		if r.StartAddress == "" || r.EndAddress == "" {
			continue
		}
		s, errS := netip.ParseAddr(r.StartAddress)
		e, errE := netip.ParseAddr(r.EndAddress)
		if errS != nil || errE != nil {
			continue
		}
		if s.String() == start && e.String() == end {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceRow < out[j].SourceRow })
	return out
}

// canonicalPrefix returns netip's canonical string form of an IPv4 CIDR, or
// "" if raw does not parse as one. It exists only to compare against
// WriteEntry.CIDR (itself produced this way by onboard.Plan), so an invalid
// row -- already reported as its own Finding by Plan -- is simply excluded
// from every group rather than erroring a second time here.
func canonicalPrefix(raw string) string {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return ""
	}
	return p.String()
}

// networkContributors builds ADR 0016's contributor list for one collapsed
// group: one entry per source row, so that the identity of every VPC sharing
// a CIDR survives a collapse that used to keep only a sentence in the
// description. It is written on create only (internal/netbox's
// EnsureOccupancy); nothing here updates an existing prefix, which is package
// M9b3's work.
//
// The construction itself lives in internal/onboard (onboard.ContributorsForRows,
// package M9b2), not here: Plan's contributor findings need the exact same
// construction to compare a table's rows against what a prefix already
// carries, and internal/onboard cannot import this package (onboardcmd
// already imports onboard), so the one function both callers share has to
// live on the onboard side. This wrapper keeps the name every existing test
// in this package already calls.
func networkContributors(rows []onboard.NetworkRow, batch string) []domain.Contributor {
	return onboard.ContributorsForRows(rows, batch)
}

func networkAWSFields(rows []onboard.NetworkRow) (accountID, region, resourceID string) {
	var withAccount []onboard.NetworkRow
	for _, r := range rows {
		if r.AccountID != "" {
			withAccount = append(withAccount, r)
		}
	}
	if len(withAccount) == 1 {
		return withAccount[0].AccountID, withAccount[0].Region, withAccount[0].ResourceID
	}
	return "", "", ""
}

func mergeNetworkDescription(rows []onboard.NetworkRow) string {
	var parts []string
	if names := uniqueNonEmpty(len(rows), func(i int) string { return rows[i].Name }); len(names) > 0 {
		parts = append(parts, strings.Join(names, "; "))
	}
	if accounts := uniqueNonEmpty(len(rows), func(i int) string { return rows[i].AccountID }); len(accounts) > 0 {
		parts = append(parts, plural("account", len(accounts))+" "+strings.Join(accounts, ", "))
	}
	if resourceIDs := uniqueNonEmpty(len(rows), func(i int) string { return rows[i].ResourceID }); len(resourceIDs) > 0 {
		parts = append(parts, plural("resource id", len(resourceIDs))+" "+strings.Join(resourceIDs, ", "))
	}
	multi := len(rows) > 1
	for _, r := range rows {
		if r.Description == "" {
			continue
		}
		parts = append(parts, sourceLabel(multi, r.SourceRow, r.Description))
	}
	return strings.Join(parts, " | ")
}

func mergeRangeDescription(rows []onboard.RangeRow) string {
	var parts []string
	if owners := uniqueNonEmpty(len(rows), func(i int) string { return rows[i].Owner }); len(owners) > 0 {
		parts = append(parts, plural("owner", len(owners))+" "+strings.Join(owners, ", "))
	}
	if sources := uniqueNonEmpty(len(rows), func(i int) string { return rows[i].Source }); len(sources) > 0 {
		parts = append(parts, plural("source", len(sources))+" "+strings.Join(sources, ", "))
	}
	multi := len(rows) > 1
	for _, r := range rows {
		if r.Description == "" {
			continue
		}
		parts = append(parts, sourceLabel(multi, r.SourceRow, r.Description))
	}
	return strings.Join(parts, " | ")
}

func sourceLabel(multi bool, sourceRow int, description string) string {
	if !multi {
		return description
	}
	return fmt.Sprintf("source row %d: %s", sourceRow, description)
}

// plural renders a count-appropriate field label. Both nouns used here take
// a plain trailing "s", so this does not need a pluralization dependency.
func plural(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// uniqueNonEmpty collects the distinct non-empty values of get(0..n-1), in
// first-seen order.
func uniqueNonEmpty(n int, get func(i int) string) []string {
	seen := map[string]bool{}
	var out []string
	for i := 0; i < n; i++ {
		v := get(i)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// --- render-config ---

func runRenderConfig(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "role-name", "default-region")
	set := newFlagSet("render-config")
	roleName := set.String("role-name", "", "IAM role name used when a row has no role_arn (default: PlatformIpamReadOnly)")
	var regions []string
	set.Var(stringListFlag{&regions}, "default-region", "fallback region for a row with no regions; repeatable")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	rest := positional
	if len(rest) != 1 {
		return usageFail(stderr, usagef("render-config: exactly one table.json path is required"))
	}

	table, err := loadTable(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard render-config: %v\n", err)
		return ExitValidation
	}
	out, err := onboard.RenderConfig(table, onboard.ConfigOptions{RoleName: *roleName, DefaultRegions: regions})
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard render-config: %v\n", err)
		return ExitValidation
	}
	if _, err := stdout.Write(out); err != nil {
		return ExitAdapter
	}
	return ExitOK
}

// --- render-fixture ---

func runRenderFixture(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args, "domain", "generation")
	set := newFlagSet("render-fixture")
	domainID := set.String("domain", "", "overlap domain id (required)")
	generation := set.String("generation", "", "observation generation label (default: import-<unix-timestamp>)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	rest := positional
	if len(rest) != 1 {
		return usageFail(stderr, usagef("render-fixture: exactly one table.json path is required"))
	}
	if *domainID == "" {
		return usageFail(stderr, usagef("render-fixture: --domain is required"))
	}

	table, err := loadTable(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard render-fixture: %v\n", err)
		return ExitValidation
	}
	gen := *generation
	if gen == "" {
		gen = fmt.Sprintf("import-%d", time.Now().Unix())
	}
	out, err := onboard.RenderFixture(table, *domainID, gen)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard render-fixture: %v\n", err)
		return ExitValidation
	}
	if _, err := stdout.Write(out); err != nil {
		return ExitAdapter
	}
	return ExitOK
}
