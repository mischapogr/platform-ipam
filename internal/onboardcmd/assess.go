package onboardcmd

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/onboard"
	yaml "go.yaml.in/yaml/v3"
)

// This file implements `platform-ipam onboard assess` (docs/WORK_PLAN.md
// package M1b3, docs/decisions/0014-RESOURCE_AWARE_OVERLAP_ASSESSMENT.md): an
// offline, read-only report of which observed AWS VPC CIDR associations
// conflict with which others, and for which part of the estate no complete
// statement can be made. The pure comparison lives in internal/assess
// (package M1b2); this file only decodes the organization inventory's files
// (and the reviewed matrix/ownership/fixed/decisions inputs) into that
// package's Input and Options, and renders the Report it returns.
//
// Like drift.go, assess never writes to NetBox -- unlike drift, it never
// calls NetBox AT ALL, and it never opens the ledger, never loads pools
// configuration, and never reads an IPAM_ environment variable (ADR 0014,
// "Where it lives": "runAssess does not call newAdapter, reads no IPAM_
// variable, and a test runs it with the environment empty"). See
// TestAssessSourceImportsExcludeAdapterPackages and
// TestAssessRunsWithEmptyEnvironment in assess_test.go for the two
// properties this file's own tests hold it to.
//
// It writes nowhere but stdout, stderr and the single --out path a flag may
// name (ADR 0014, "Where it lives"): no file, no NetBox call, no ledger
// write, ever.

// --- flags and dispatch ---

// runAssess takes no context.Context, exactly like runParse, runRenderConfig
// and runRenderFixture (and unlike runPlan, runApply and runDrift, which all
// call NetBox): assess performs no I/O that could ever need to be cancelled
// beyond reading local files, which os.ReadFile and os.Stdin do not offer a
// context for anyway.
func runAssess(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args,
		"inventory", "networks", "failures", "accounts", "run",
		"matrix", "ownership", "fixed", "decisions",
		"format", "out", "stamp", "max-relationships")
	set := newFlagSet("assess")
	inventory := set.String("inventory", "",
		"collector output directory: shorthand for --networks DIR/networks.csv --failures DIR/failures.csv --accounts DIR/accounts.json --run DIR/run.json\n"+
			"                 an explicit --failures/--accounts/--run/--networks always wins; a file --inventory would imply that does not exist on disk is silently treated as not given (run.json in particular is legitimately absent from an interrupted collection)")
	var networksList []string
	set.Var(stringListFlag{&networksList}, "networks",
		"networks input, repeatable; any format `onboard plan` reads (CSV/TSV/paste/.xlsx, or \"-\" for stdin text), through the same reader and normalizer")
	failuresFlag := set.String("failures", "", "failures.csv path")
	accountsFlag := set.String("accounts", "", "accounts.json path")
	runFlag := set.String("run", "", "run.json path")
	matrixFlag := set.String("matrix", "", "reviewed connectivity matrix path (YAML or JSON)")
	ownershipFlag := set.String("ownership", "", "ownership table path (YAML or JSON)")
	fixedFlag := set.String("fixed", "", "fixed (non-AWS) ranges table path (YAML or JSON)")
	decisionsFlag := set.String("decisions", "", "decisions file path (YAML or JSON), keyed by conflict id")
	formatFlag := set.String("format", "text", "json or text")
	outFlag := set.String("out", "", "additionally write the report to this path (stdout always carries the same bytes)")
	stampFlag := set.String("stamp", "", "archival label carried into the report verbatim -- the only field in the report that did not come out of an input file; never derived from the wall clock")
	maxRelationshipsFlag := set.Int("max-relationships", 0, "refuse (exit 4) rather than produce a report of more than this many relationships; 0 means the engine's own default (100000)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 0 {
		return usageFail(stderr, usagef("assess: unexpected argument(s) %v", positional))
	}
	switch *formatFlag {
	case "json", "text":
	default:
		return usageFail(stderr, usagef("assess: --format must be json or text, got %q", *formatFlag))
	}

	networksInputs, failuresPath, accountsPath, runPath := resolveAssessInputs(*inventory, networksList, *failuresFlag, *accountsFlag, *runFlag)

	// No network data is not an empty estate. Without this the engine is handed
	// zero records and answers, truthfully about nothing, that no conflicting
	// relationship was observed -- and with a run record and an account list
	// beside it, that the scope was read completely.
	if len(networksInputs) == 0 {
		if *inventory != "" {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s holds no networks.csv, so no network data was read and no report can be produced\n", *inventory)
			return ExitAdapter
		}
		return usageFail(stderr, usagef("assess: no network data: give --inventory DIR or at least one --networks input"))
	}

	var inputFiles []assess.InputFile
	addDigest := func(path string, content []byte) {
		inputFiles = append(inputFiles, assess.InputFile{Path: path, Content: content})
	}

	// --- networks (repeatable), through onboard's own reader and normalizer ---
	var mergedTable onboard.Table
	haveNetworks := false
	for _, name := range networksInputs {
		data, table, err := readNetworksTable(name)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: reading %s: %v\n", name, err)
			return ExitAdapter
		}
		addDigest(name, data)
		table = stampSourceFile(table, name)
		if table.Kind != onboard.KindNetworks {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s resolved as a %s table, not a networks table (needs cidr, account_id and region columns); no report can be produced\n", name, table.Kind)
			return ExitAdapter
		}
		// A row the normalizer dropped is a resource nobody compared. The import
		// can leave such a row behind and say so; a report that is allowed to
		// say "complete" cannot, so it refuses and names every such row.
		if dropped := errorDiagnostics(table.Diagnostics); len(dropped) != 0 {
			for _, d := range dropped {
				fmt.Fprintf(stderr, "platform-ipam onboard assess: %s:%d: %s\n", name, d.Row, d.Message)
			}
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %d row(s) of %s could not be read, so the resources they describe would be missing from every comparison; no report can be produced\n", len(dropped), name)
			return ExitAdapter
		}
		if !haveNetworks {
			mergedTable, haveNetworks = table, true
			continue
		}
		mergedTable = concatTables(mergedTable, table)
	}

	records, err := buildResourceRecords(mergedTable.Networks)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard assess: %v\n", err)
		return ExitAdapter
	}

	// --- accounts.json ---
	var accountRecords []assess.AccountRecord
	if accountsPath != "" {
		data, err := os.ReadFile(accountsPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: reading %s: %v\n", accountsPath, err)
			return ExitAdapter
		}
		addDigest(accountsPath, data)
		accountRecords, err = decodeAccountsJSON(accountsPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %v\n", err)
			return ExitAdapter
		}
	}

	// --- failures.csv ---
	var failureRows []assess.FailureRow
	if failuresPath != "" {
		data, err := os.ReadFile(failuresPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: reading %s: %v\n", failuresPath, err)
			return ExitAdapter
		}
		addDigest(failuresPath, data)
		failureRows, err = decodeFailuresCSV(failuresPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %v\n", err)
			return ExitAdapter
		}
	}

	// --- run.json ---
	var runRecord *assess.RunRecord
	if runPath != "" {
		data, err := os.ReadFile(runPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: reading %s: %v\n", runPath, err)
			return ExitAdapter
		}
		addDigest(runPath, data)
		rr, err := decodeRunJSON(runPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %v\n", err)
			return ExitAdapter
		}
		runRecord = &rr
	}

	// --- matrix / ownership / fixed / decisions: YAML (or JSON) reviewed inputs ---
	var matrix *assess.Matrix
	if *matrixFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*matrixFlag)
		if err == nil {
			addDigest(*matrixFlag, raw)
			var m assess.Matrix
			m, err = assess.DecodeMatrix(bytes.NewReader(jsonBytes))
			matrix = &m
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s: %v\n", *matrixFlag, err)
			return ExitAdapter
		}
	}

	ownership := assess.Ownership{}
	if *ownershipFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*ownershipFlag)
		if err == nil {
			addDigest(*ownershipFlag, raw)
			ownership, err = assess.DecodeOwnership(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s: %v\n", *ownershipFlag, err)
			return ExitAdapter
		}
	}

	var fixed []assess.FixedRange
	if *fixedFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*fixedFlag)
		if err == nil {
			addDigest(*fixedFlag, raw)
			fixed, err = assess.DecodeFixed(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s: %v\n", *fixedFlag, err)
			return ExitAdapter
		}
		// DecodeFixed's JSON schema (cidr, description, owner) has no source
		// tracking -- a customer's --fixed table names no file of its own to
		// point back into -- so the command supplies it here from the ONE
		// file it was read from and the row's own ordinal position in that
		// file's decoded list, the closest honest analogue this format has to
		// a CSV row number (ADR 0014 does not define this; see the report to
		// the lead).
		for i := range fixed {
			fixed[i].SourceFile = *fixedFlag
			fixed[i].SourceRow = i + 1
		}
	}

	var decisions map[string]assess.Decision
	if *decisionsFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*decisionsFlag)
		if err == nil {
			addDigest(*decisionsFlag, raw)
			decisions, err = assess.DecodeDecisions(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: %s: %v\n", *decisionsFlag, err)
			return ExitAdapter
		}
	}

	in := assess.Input{Records: records, Accounts: accountRecords, Failures: failureRows, Run: runRecord}
	opts := assess.Options{
		Matrix: matrix, Ownership: ownership, Fixed: fixed, Decisions: decisions,
		Stamp: *stampFlag, MaxRelationships: *maxRelationshipsFlag, InputFiles: inputFiles,
	}

	report, err := assess.Assess(in, opts)
	if err != nil {
		// *assess.MissingFieldError and *assess.TooManyRelationshipsError are
		// the only two errors Assess ever returns (see its own doc comment);
		// both mean "no report exists", which is exactly what exit 4 names
		// for this command (ADR 0014, "Where it lives": "The package's
		// existing taxonomy calls 4 the adapter code; assess has no adapter,
		// and the help text says plainly that it uses 4 for 'no report
		// exists'"). Nothing is written to stdout in this case.
		fmt.Fprintf(stderr, "platform-ipam onboard assess: %v\n", err)
		return ExitAdapter
	}

	var buf bytes.Buffer
	switch *formatFlag {
	case "json":
		err = assess.WriteReportJSON(&buf, report)
	case "text":
		err = assess.WriteReportText(&buf, report)
	}
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard assess: rendering report: %v\n", err)
		return ExitAdapter
	}
	// stdout and --out always carry byte-identical content: assess never
	// picks one over the other (ADR 0014's own worked test list: "--out and
	// stdout carry identical bytes"). Nothing else is ever written to stdout
	// -- no free-text summary of the command's own (that goes to stderr
	// below), so stdout is the report and only the report.
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard assess: writing stdout: %v\n", err)
		return ExitAdapter
	}
	if *outFlag != "" {
		if err := os.WriteFile(*outFlag, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard assess: writing %s: %v\n", *outFlag, err)
			return ExitAdapter
		}
	}

	fmt.Fprintf(stderr, "platform-ipam onboard assess: %s\n", report.Summary.Sentence)
	if report.Clean() {
		return ExitOK
	}
	return ExitValidation
}

// resolveAssessInputs applies --inventory's shorthand: its networks.csv is
// prepended to any explicit --networks entries (both are read; --inventory
// is not exclusive with supplying extra --networks inputs of your own), and
// its failures.csv/accounts.json/run.json fill in only the flags the
// operator left empty. A file --inventory would imply that is not actually
// on disk is dropped rather than attempted -- an explicit --failures,
// --accounts or --run naming a path that does not exist is still a hard
// error (an operator who names a file expects it to be read); only the
// convenience shorthand degrades silently, and run.json in particular is
// legitimately absent from an interrupted collector run (ADR 0014,
// scripts/aws/org-inventory.sh's own comment: "an interrupted run ... leaves
// no run.json at all").
func resolveAssessInputs(inventoryDir string, networks []string, failures, accounts, run string) (networksOut []string, failuresOut, accountsOut, runOut string) {
	networksOut = append([]string{}, networks...)
	failuresOut, accountsOut, runOut = failures, accounts, run
	if inventoryDir == "" {
		return networksOut, failuresOut, accountsOut, runOut
	}
	if p := filepath.Join(inventoryDir, "networks.csv"); pathExists(p) {
		networksOut = append([]string{p}, networksOut...)
	}
	if failuresOut == "" {
		if p := filepath.Join(inventoryDir, "failures.csv"); pathExists(p) {
			failuresOut = p
		}
	}
	if accountsOut == "" {
		if p := filepath.Join(inventoryDir, "accounts.json"); pathExists(p) {
			accountsOut = p
		}
	}
	if runOut == "" {
		if p := filepath.Join(inventoryDir, "run.json"); pathExists(p) {
			runOut = p
		}
	}
	return networksOut, failuresOut, accountsOut, runOut
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readNetworksTable reads one --networks input, returning both its raw bytes
// (for the report's input digest, ADR 0014 "Identity and determinism": "the
// SHA-256 of its contents") and the onboard.Table those bytes decode to. It
// mirrors readInput's stdin-vs-file split (name "-" means stdin text,
// design section 2) but, unlike readInput, keeps the raw bytes in memory
// instead of decoding straight from the open file or os.Stdin: assess must
// be able to name exactly what it read, and readInput's other callers
// (parse, plan, apply) never needed that.
func readNetworksTable(name string) ([]byte, onboard.Table, error) {
	if name == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, onboard.Table{}, fmt.Errorf("stdin: %w", err)
		}
		t, err := onboard.ReadText("stdin", bytes.NewReader(data), onboard.ReadOptions{})
		return data, t, err
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, onboard.Table{}, err
	}
	t, err := onboard.ReadTable(name, bytes.NewReader(data), int64(len(data)), onboard.ReadOptions{})
	return data, t, err
}

// --- mapping onboard.NetworkRow onto assess.ResourceRecord ---

// buildResourceRecords maps every row of a networks Table onto
// assess.ResourceRecord, field by field. It is the one place that resolves
// the model gap docs/WORK_PLAN.md's M1b1 review left for this package: an
// AssociationID/ObservedAt pointer is nil when the row's own SOURCE FILE had
// no such column (NetworkRow.AssociationIDColumnPresent /
// ObservedAtColumnPresent, added to internal/onboard by this package), and a
// pointer to the (possibly empty) cell value when the column existed.
//
// internal/onboard's own account_name column is not carried: NetworkRow has
// no dedicated field for it (knownColumns[KindNetworks] does not include
// colAccountName, pre-existing behaviour unrelated to this package --
// account_name already lands in Description as free text for every import,
// not only for assess), so assess.ResourceRecord.AccountName is left empty
// here. This is a decision the record did not make explicitly; see the
// report to the lead.
func buildResourceRecords(rows []onboard.NetworkRow) ([]assess.ResourceRecord, error) {
	records := make([]assess.ResourceRecord, 0, len(rows))
	for _, row := range rows {
		rec := mapNetworkRow(row)
		switch rec.Type {
		case assess.TypeVPC, assess.TypeSubnet, "":
			// "" is left for assess.Validate (called inside assess.Assess) to
			// report as a *assess.MissingFieldError naming "type" -- the same
			// refusal every other missing required field produces, so a
			// caller sees one consistent error shape rather than two.
		default:
			return nil, fmt.Errorf("%s:%d: type %q is neither \"vpc\" nor \"subnet\"; this resource cannot be classified, so no report can be produced (this is not one of ADR 0014's named degradations, which is why it refuses rather than being silently excluded from the sweep)",
				row.SourceFile, row.SourceRow, row.Type)
		}
		records = append(records, rec)
	}
	return records, nil
}

// mapNetworkRow converts one onboard.NetworkRow to one assess.ResourceRecord.
// The conversion itself lives in internal/onboard (onboard.MapNetworkRow,
// package M9b2): Plan's ADR 0016 contributor findings need the identical
// mapping onboard assess uses here, so that a reviewer holding an assess
// conflict and a prefix's contributor list can join them by the same identity
// string with no mapping table, and internal/onboard cannot import this
// package to get it (onboardcmd already imports onboard). This wrapper keeps
// the name this file's own tests already call.
func mapNetworkRow(row onboard.NetworkRow) assess.ResourceRecord {
	return onboard.MapNetworkRow(row)
}

// --- accounts.json ---

// rawAccountsFile mirrors exactly the shape `aws organizations list-accounts
// --output json` produces (docs/AWS_ORGANIZATION_INVENTORY.md: "the raw `aws
// organizations list-accounts` response"), which carries many fields this
// command does not use (Arn, Email, JoinedMethod, JoinedTimestamp, ...). No
// json.Decoder.DisallowUnknownFields is used here: unlike the reviewed
// matrix/ownership/fixed/decisions inputs, which are a schema this project
// defines and should reject typos in, accounts.json is a foreign AWS
// response this command must stay forward-compatible with.
type rawAccountsFile struct {
	Accounts []rawAccountEntry `json:"Accounts"`
}

type rawAccountEntry struct {
	Id     string `json:"Id"`
	Name   string `json:"Name"`
	Status string `json:"Status"`
}

func decodeAccountsJSON(path string, data []byte) ([]assess.AccountRecord, error) {
	var raw rawAccountsFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: decoding accounts.json: %w", path, err)
	}
	out := make([]assess.AccountRecord, 0, len(raw.Accounts))
	for _, a := range raw.Accounts {
		out = append(out, assess.AccountRecord{AccountID: a.Id, Name: a.Name, Status: a.Status})
	}
	return out, nil
}

// --- failures.csv ---

// decodeFailuresCSV reads failures.csv, header `account_id,account_name,
// region,stage,error` (scripts/aws/org-inventory.sh). It is tolerant of
// column order and of extra columns (matched by name, not position) but
// strict about the five it reads: every one of them must be present in the
// header, or the whole file is refused, because a failures.csv missing one
// of its own defined columns is not a file this command recognizes at all.
func decodeFailuresCSV(path string, data []byte) ([]assess.FailureRow, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	idx := make(map[string]int, len(records[0]))
	for i, h := range records[0] {
		idx[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, col := range []string{"account_id", "account_name", "region", "stage", "error"} {
		if _, ok := idx[col]; !ok {
			return nil, fmt.Errorf("%s: missing required column %q (header: %s)", path, col, strings.Join(records[0], ","))
		}
	}
	cell := func(row []string, col string) string {
		i := idx[col]
		if i >= len(row) {
			return ""
		}
		return row[i]
	}

	out := make([]assess.FailureRow, 0, len(records)-1)
	for i, row := range records[1:] {
		out = append(out, assess.FailureRow{
			AccountID:   cell(row, "account_id"),
			AccountName: cell(row, "account_name"),
			Region:      cell(row, "region"),
			Stage:       assess.FailureStage(cell(row, "stage")),
			Error:       cell(row, "error"),
			SourceFile:  path,
			SourceRow:   i + 1,
		})
	}
	return out, nil
}

// --- run.json ---

// rawRunFile mirrors run.json's shape exactly as
// scripts/aws/org-inventory.sh writes it and
// docs/AWS_ORGANIZATION_INVENTORY.md documents it. Like rawAccountsFile, no
// DisallowUnknownFields: run.json's own script_version field exists
// precisely because its shape is expected to evolve, and this command must
// keep reading an older or newer run.json that adds a field it does not yet
// know about.
type rawRunFile struct {
	ScriptVersion         string          `json:"script_version"`
	StartedAt             string          `json:"started_at"`
	FinishedAt            string          `json:"finished_at"`
	RoleName              string          `json:"role_name"`
	ManagementAccountUsed bool            `json:"management_account_used"`
	ManagementAccountID   *string         `json:"management_account_id"`
	ConfiguredRegions     []string        `json:"configured_regions"`
	Accounts              []rawRunAccount `json:"accounts"`
}

type rawRunAccount struct {
	AccountID          string         `json:"account_id"`
	AccountName        string         `json:"account_name"`
	CredentialSource   string         `json:"credential_source"`
	RegionsAttempted   string         `json:"regions_attempted"`
	NotAttemptedReason *string        `json:"not_attempted_reason"`
	RegionSource       string         `json:"region_source"`
	Regions            []rawRunRegion `json:"regions"`
}

type rawRunRegion struct {
	Region     string `json:"region"`
	Outcome    string `json:"outcome"`
	Stage      string `json:"stage"`
	RowCount   *int   `json:"row_count"`
	Reason     string `json:"reason"`
	ObservedAt string `json:"observed_at"`
}

func decodeRunJSON(path string, data []byte) (assess.RunRecord, error) {
	var raw rawRunFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return assess.RunRecord{}, fmt.Errorf("%s: decoding run.json: %w", path, err)
	}

	var attempts []assess.RunAttempt
	for _, acc := range raw.Accounts {
		if acc.RegionsAttempted == "unknown" && len(acc.Regions) == 0 {
			// "regions_attempted: unknown with no region entries" (the whole
			// account's region list itself was never obtained) maps to ONE
			// account-level attempt: docs/AWS_ORGANIZATION_INVENTORY.md's own
			// words, "nothing is invented", so this is the one honest way to
			// tell the engine "this whole account was not attempted" without
			// naming a region that was never even discovered. computeCoverage
			// (internal/assess) treats a not_attempted entry with an empty
			// Region as exactly that: a whole-account gap.
			attempts = append(attempts, assess.RunAttempt{AccountID: acc.AccountID, Region: "", Outcome: assess.AttemptNotAttempted})
			continue
		}
		for _, r := range acc.Regions {
			// Outcome is cast verbatim, not validated against
			// assess.AttemptSucceeded/Failed/Partial/NotAttempted: an outcome
			// word this command's own rawRunRegion does not otherwise
			// interpret is exactly the case ADR 0014's amendment describes --
			// "any word this version does not know" -- and internal/assess's
			// own coverage.go already turns an unrecognized AttemptOutcome
			// into a failed entry rather than silently ignoring it. Refusing
			// here instead would make this command less forward-compatible
			// with a future collector than the engine it calls already is.
			attempts = append(attempts, assess.RunAttempt{
				AccountID: acc.AccountID,
				Region:    r.Region,
				Outcome:   assess.AttemptOutcome(r.Outcome),
				Stage:     r.Stage,
				// RowCount is carried straight through (package M1c): r.RowCount
				// is already *int, nil for an outcome that never carries one
				// (failed, not_attempted) and nil for a run.json written before
				// row_count existed (script_version 1) -- both cases
				// internal/assess.rowCountMismatches and computeInputLimits
				// treat identically ("never guess"), so no decision is made
				// here about which of the two nil means.
				RowCount: r.RowCount,
			})
		}
	}

	return assess.RunRecord{
		StartedAt:             raw.StartedAt,
		FinishedAt:            raw.FinishedAt,
		RoleName:              raw.RoleName,
		ManagementCredentials: raw.ManagementAccountUsed,
		ConfiguredRegions:     raw.ConfiguredRegions,
		ScriptVersion:         raw.ScriptVersion,
		Attempts:              attempts,
		SourceFile:            path,
	}, nil
}

// --- YAML (or JSON) reviewed inputs: matrix, ownership, fixed, decisions ---

// readYAMLOrJSONFile reads path and converts its contents from YAML to JSON,
// using go.yaml.in/yaml/v3 -- the repository's existing YAML dependency
// (go.mod); no dependency is added, and go.mod/go.sum are never touched.
// internal/assess is stdlib-only by design (see its own
// TestImportsAreStandardLibraryOnly) and its DecodeMatrix/DecodeOwnership/
// DecodeFixed/DecodeDecisions accept only JSON, so this command performs the
// conversion the record describes ("the command converting the YAML
// documents this record describes before handing them over", ADR 0014's
// dated amendment) rather than the engine.
//
// Every valid JSON document is already valid YAML 1.2, so a caller who
// already has a JSON file for one of these inputs needs no special
// handling: it decodes here unchanged and re-encodes to the same JSON.
//
// The YAML-integer trap: an unquoted, all-digit scalar (an account id
// without quotes, in particular one with a leading zero, which some YAML
// resolvers additionally read as octal) decodes as a YAML/JSON *number*, not
// a string. Every field this command reads that value into (AccountID,
// group member entries, a CIDR) is a Go string, so encoding/json's decoder
// -- which never silently converts a JSON number into a string field --
// refuses with an error naming the offending field. This is deliberately
// not special-cased with extra detection code: the ordinary strict decode
// path already refuses it, honestly and by construction, for any bare
// numeric value in a string position, not only account ids and not only
// leading-zero ones. See TestAssessMatrixRejectsUnquotedLeadingZeroAccountID
// and TestAssessOwnershipAcceptsQuotedLeadingZeroAccountID in
// assess_test.go.
func readYAMLOrJSONFile(path string) (raw []byte, converted []byte, err error) {
	raw, err = os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var generic interface{}
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return raw, nil, fmt.Errorf("%s: not valid YAML or JSON: %w", path, err)
	}
	converted, err = json.Marshal(generic)
	if err != nil {
		return raw, nil, fmt.Errorf("%s: re-encoding as JSON: %w", path, err)
	}
	return raw, converted, nil
}

// errorDiagnostics returns the diagnostics that cost the table a row or a cell.
func errorDiagnostics(all []onboard.Diagnostic) []onboard.Diagnostic {
	var out []onboard.Diagnostic
	for _, d := range all {
		if d.Level == onboard.LevelError {
			out = append(out, d)
		}
	}
	return out
}
