package onboardcmd

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/migrate"
	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// This file implements `platform-ipam onboard progress` (docs/WORK_PLAN.md
// package M3b3, docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md):
// an offline, read-only report deriving three independent facts per planned
// move -- subject, target, conflicts -- from a reviewed migration plan, the
// same organization inventory `onboard assess` reads, and an authenticated
// allocation evidence export. It writes nowhere but stdout, stderr and a
// single --out path.
//
// Like assess.go, this file is in the `onboard` family for ADR 0014's
// reason exactly (ADR 0015, "Where it lives": "it needs strictly less than
// every other member -- no ledger, no NetBox, no cloud credentials, no
// pools configuration"): runProgress never calls newAdapter, reads no
// IPAM_ variable, and a test runs it with the environment empty -- see
// TestProgressRunsWithEmptyEnvironment and
// TestProgressSourceImportsExcludeAdapterPackages in progress_test.go,
// mirroring assess_test.go's own two guards for this file.
//
// The assessment's own input flags (--inventory, --networks, --failures,
// --accounts, --run, --matrix, --ownership, --fixed, --decisions) are read
// EXACTLY as assess.go's runAssess reads them: this file calls the very
// same unexported readers (resolveAssessInputs, readNetworksTable,
// buildResourceRecords, decodeAccountsJSON, decodeFailuresCSV,
// decodeRunJSON, readYAMLOrJSONFile, stampSourceFile, concatTables,
// errorDiagnostics, pathExists) rather than a copy of their logic, so the
// two commands can never silently drift apart in how an input decodes (ADR
// 0015, "M3b3 ... the record's own reading"). assess.go itself is
// untouched -- no shared helper was factored out of it, so every one of its
// own tests is unaffected byte for byte; see the report to the lead for why
// "call the same functions" (docs/WORK_PLAN.md's own fallback wording) was
// chosen over extracting a new shared entry point.

// runProgress takes no context.Context, for the same reason runAssess does
// not (assess.go's own doc comment): it performs no I/O that could ever
// need to be cancelled beyond reading local files.
func runProgress(args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositional(args,
		"plan", "inventory", "networks", "failures", "accounts", "run",
		"matrix", "ownership", "fixed", "decisions", "allocations",
		"format", "out", "stamp", "max-relationships")
	set := newFlagSet("progress")
	var planList []string
	set.Var(stringListFlag{&planList}, "plan",
		"reviewed migration plan (YAML or JSON), repeatable; required")
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
	var allocationsList []string
	set.Var(stringListFlag{&allocationsList}, "allocations",
		"allocation evidence export (YAML or JSON, read_at/scope/allocations), repeatable; an authenticated read handed to this command, never fetched by it")
	formatFlag := set.String("format", "text", "json or text")
	outFlag := set.String("out", "", "additionally write the report to this path (stdout always carries the same bytes)")
	stampFlag := set.String("stamp", "", "archival label carried into the report verbatim -- the only field in the report that did not come out of an input file; never derived from the wall clock")
	maxRelationshipsFlag := set.Int("max-relationships", 0, "refuse (exit 4) rather than produce a report of more than this many relationships; 0 means the engine's own default (100000); passed straight through to the assessment engine")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 0 {
		return usageFail(stderr, usagef("progress: unexpected argument(s) %v", positional))
	}
	switch *formatFlag {
	case "json", "text":
	default:
		return usageFail(stderr, usagef("progress: --format must be json or text, got %q", *formatFlag))
	}
	if len(planList) == 0 {
		return usageFail(stderr, usagef("progress: no plan: give at least one --plan"))
	}

	// --- the plan: decode every --plan file, merge, validate ---
	//
	// A decode failure or a structural refusal is exit 4, "no report", and
	// nothing is written to stdout (ADR 0015, "Where it lives"): both
	// *migrate.DecodeError and *migrate.StructuralError already name the
	// file (and, for a structural refusal, the key) their Error() method
	// formats, so this command adds nothing beyond its own prefix.
	var planFiles []assess.InputFile
	var plans []migrate.Plan
	for _, path := range planList {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", path, err)
			return ExitAdapter
		}
		planFiles = append(planFiles, assess.InputFile{Path: path, Content: raw})
		p, err := migrate.DecodePlan(bytes.NewReader(raw), path)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
			return ExitAdapter
		}
		plans = append(plans, p)
	}
	plan := migrate.Merge(plans...)
	if err := migrate.Validate(plan); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
		return ExitAdapter
	}

	// --- the assessment's own inputs, exactly as assess.go reads them ---

	networksInputs, failuresPath, accountsPath, runPath := resolveAssessInputs(*inventory, networksList, *failuresFlag, *accountsFlag, *runFlag)

	// No network data is not an empty estate -- see assess.go's own comment
	// at the identical check; this command must not lie about being able to
	// make a complete statement any more than assess itself may.
	if len(networksInputs) == 0 {
		if *inventory != "" {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s holds no networks.csv, so no network data was read and no report can be produced\n", *inventory)
			return ExitAdapter
		}
		return usageFail(stderr, usagef("progress: no network data: give --inventory DIR or at least one --networks input"))
	}

	var assessInputFiles []assess.InputFile
	addAssessDigest := func(path string, content []byte) {
		assessInputFiles = append(assessInputFiles, assess.InputFile{Path: path, Content: content})
	}

	// --- networks (repeatable), through onboard's own reader and normalizer ---
	var mergedTable onboard.Table
	haveNetworks := false
	for _, name := range networksInputs {
		data, table, err := readNetworksTable(name)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", name, err)
			return ExitAdapter
		}
		addAssessDigest(name, data)
		table = stampSourceFile(table, name)
		if table.Kind != onboard.KindNetworks {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s resolved as a %s table, not a networks table (needs cidr, account_id and region columns); no report can be produced\n", name, table.Kind)
			return ExitAdapter
		}
		if dropped := errorDiagnostics(table.Diagnostics); len(dropped) != 0 {
			for _, d := range dropped {
				fmt.Fprintf(stderr, "platform-ipam onboard progress: %s:%d: %s\n", name, d.Row, d.Message)
			}
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %d row(s) of %s could not be read, so the resources they describe would be missing from every comparison; no report can be produced\n", len(dropped), name)
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
		fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
		return ExitAdapter
	}

	// --- accounts.json ---
	var accountRecords []assess.AccountRecord
	if accountsPath != "" {
		data, err := os.ReadFile(accountsPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", accountsPath, err)
			return ExitAdapter
		}
		addAssessDigest(accountsPath, data)
		accountRecords, err = decodeAccountsJSON(accountsPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
			return ExitAdapter
		}
	}

	// --- failures.csv ---
	var failureRows []assess.FailureRow
	if failuresPath != "" {
		data, err := os.ReadFile(failuresPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", failuresPath, err)
			return ExitAdapter
		}
		addAssessDigest(failuresPath, data)
		failureRows, err = decodeFailuresCSV(failuresPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
			return ExitAdapter
		}
	}

	// --- run.json ---
	var runRecord *assess.RunRecord
	if runPath != "" {
		data, err := os.ReadFile(runPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", runPath, err)
			return ExitAdapter
		}
		addAssessDigest(runPath, data)
		rr, err := decodeRunJSON(runPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
			return ExitAdapter
		}
		runRecord = &rr
	}

	// --- matrix / ownership / fixed / decisions: YAML (or JSON) reviewed inputs ---
	var matrix *assess.Matrix
	if *matrixFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*matrixFlag)
		if err == nil {
			addAssessDigest(*matrixFlag, raw)
			var m assess.Matrix
			m, err = assess.DecodeMatrix(bytes.NewReader(jsonBytes))
			matrix = &m
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s: %v\n", *matrixFlag, err)
			return ExitAdapter
		}
	}

	ownership := assess.Ownership{}
	if *ownershipFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*ownershipFlag)
		if err == nil {
			addAssessDigest(*ownershipFlag, raw)
			ownership, err = assess.DecodeOwnership(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s: %v\n", *ownershipFlag, err)
			return ExitAdapter
		}
	}

	var fixed []assess.FixedRange
	if *fixedFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*fixedFlag)
		if err == nil {
			addAssessDigest(*fixedFlag, raw)
			fixed, err = assess.DecodeFixed(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s: %v\n", *fixedFlag, err)
			return ExitAdapter
		}
		for i := range fixed {
			fixed[i].SourceFile = *fixedFlag
			fixed[i].SourceRow = i + 1
		}
	}

	var decisions map[string]assess.Decision
	if *decisionsFlag != "" {
		raw, jsonBytes, err := readYAMLOrJSONFile(*decisionsFlag)
		if err == nil {
			addAssessDigest(*decisionsFlag, raw)
			decisions, err = assess.DecodeDecisions(bytes.NewReader(jsonBytes))
		}
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %s: %v\n", *decisionsFlag, err)
			return ExitAdapter
		}
	}

	in := assess.Input{Records: records, Accounts: accountRecords, Failures: failureRows, Run: runRecord}
	assessOpts := assess.Options{
		Matrix: matrix, Ownership: ownership, Fixed: fixed, Decisions: decisions,
		Stamp: *stampFlag, MaxRelationships: *maxRelationshipsFlag, InputFiles: assessInputFiles,
	}

	report, err := assess.Assess(in, assessOpts)
	if err != nil {
		// *assess.MissingFieldError and *assess.TooManyRelationshipsError are
		// the only two errors Assess ever returns; both mean "no report
		// exists" here exactly as they do for assess.go's own runAssess.
		fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
		return ExitAdapter
	}

	// --- allocation evidence (zero or more --allocations files) ---
	var evidence []migrate.DerivedEvidenceFile
	for _, path := range allocationsList {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: reading %s: %v\n", path, err)
			return ExitAdapter
		}
		ev, err := migrate.DecodeEvidence(bytes.NewReader(raw), path)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: %v\n", err)
			return ExitAdapter
		}
		evidence = append(evidence, toDerivedEvidenceFile(ev, raw))
	}

	migrateOpts := migrate.Options{Stamp: *stampFlag, PlanFiles: planFiles}
	progressReport := migrate.Derive(plan, report, in, evidence, migrateOpts)

	var buf bytes.Buffer
	switch *formatFlag {
	case "json":
		err = migrate.WriteReportJSON(&buf, progressReport)
	case "text":
		err = migrate.WriteReportText(&buf, progressReport)
	}
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard progress: rendering report: %v\n", err)
		return ExitAdapter
	}
	// stdout and --out always carry byte-identical content, exactly as
	// assess.go's runAssess promises for its own report.
	if _, err := stdout.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard progress: writing stdout: %v\n", err)
		return ExitAdapter
	}
	if *outFlag != "" {
		if err := os.WriteFile(*outFlag, buf.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard progress: writing %s: %v\n", *outFlag, err)
			return ExitAdapter
		}
	}

	fmt.Fprintf(stderr, "platform-ipam onboard progress: %s\n", progressReport.Summary.Sentence)
	if progressClean(progressReport) {
		return ExitOK
	}
	return ExitValidation
}

// toDerivedEvidenceFile maps one decoded migrate.EvidenceFile (DecodeEvidence's
// own raw, field-for-field reading of an allocation evidence export) onto
// migrate.DerivedEvidenceFile, the shape M3b2's derivation actually reads
// (ADR 0015, "Tenancy, authentication and the allocation evidence"):
// Scope and ReadAt carried verbatim (an empty Scope is not refused here --
// ADR 0015 says an export "whose scope field is absent is treated as
// covering nothing", which is evidenceIndex's job to act on, not a decode-
// or command-level refusal); Path and Content are the file's own path and
// exact bytes, for the report's digest; each EvidenceAllocation maps field
// by field onto a DerivedEvidenceAllocation, with "" mapping to nil for the
// verified-at stamp exactly as domain.Binding.VerifiedAt itself is nil when
// unset (BindingVerifiedAt's own doc comment in plan_evidence.go).
func toDerivedEvidenceFile(ev migrate.EvidenceFile, raw []byte) migrate.DerivedEvidenceFile {
	allocations := make([]migrate.DerivedEvidenceAllocation, 0, len(ev.Allocations))
	for _, a := range ev.Allocations {
		var prefixLength int
		if a.PrefixLength != nil {
			prefixLength = *a.PrefixLength
		}
		var verifiedAt *string
		if a.BindingVerifiedAt != "" {
			v := a.BindingVerifiedAt
			verifiedAt = &v
		}
		allocations = append(allocations, migrate.DerivedEvidenceAllocation{
			TenantID:            a.TenantID,
			AllocationKey:       a.AllocationKey,
			Scope:               a.Scope,
			Environment:         a.Environment,
			Region:              a.Region,
			AccountID:           a.AccountID,
			PrefixLength:        prefixLength,
			ParentAllocationKey: a.ParentAllocationKey,
			State:               a.State,
			VerifiedAt:          verifiedAt,
		})
	}
	return migrate.DerivedEvidenceFile{
		Scope:       ev.Scope,
		ReadAt:      ev.ReadAt,
		Allocations: allocations,
		Path:        ev.SourceFile,
		Content:     raw,
	}
}

// progressClean reports whether r is a "clean" progress report in the sense
// this command needs to choose between exit 0 and exit 3, mirroring
// assess.Report.Clean's role for `onboard assess` (ADR 0014) and named for
// it, but computed here rather than as a migrate.Report method because
// package migrate is M3b1/M3b2's cut, not this command's to extend (the
// lead's brief: change internal/migrate only for a defect shown by a
// failing test).
//
// ADR 0015's own words are "0 is a report whose evidence is complete and in
// which nothing is blocked, stale, mismatched, unclaimed or unplanned"
// (Where it lives), but its worked test list is more exact and this
// function follows the test list: "a third fixture, in which every move's
// three facts are affirmative, is the only one that exits 0" (the evidence
// paragraph). "Every move's three facts affirmative" is exactly
// migrate.MoveResult.FullyEvidenced (subject not-observed, target active,
// every claimed conflict resolved as stale, no unclaimed conflict) for
// EVERY move, which is what this function checks -- not the five words
// literally, because the five words are self-contradictory: FullyEvidenced
// itself REQUIRES a move's claimed, resolved conflicts to read
// migrate.ResolveStale (derive_facts.go's own doc comment: an absent
// conflict id is the record's definition of a move having actually
// resolved what it claimed), so a report containing any move that
// successfully resolved a conflict it claimed can never have zero stale
// resolves -- requiring resolved_stale == 0 would make exit 0 unreachable
// by the exact plans the record's own worked example describes. This is a
// decision the record's text does not reconcile; see the report to the
// lead.
//
// Read as "every move fully evidenced, plus nothing unplanned", the other
// four of the five words are still covered, three of them as a
// CONSEQUENCE of FullyEvidenced rather than as an independent check:
// "mismatched" (a fully evidenced move's target is always active, never
// mismatched), "unclaimed" (fully evidenced requires zero, by definition),
// and "blocked" (a fully evidenced move's subject is not-observed, and the
// current assessment can never report a conflict touching a subject it did
// not observe -- internal/assess/conflict.go's sideFrom builds every
// non-fixed Side from an observed association, so a subject absent from
// Input.Records can never be a conflict's side -- meaning
// KeepBlockedByConflict is always false whenever every move is fully
// evidenced). "Unplanned" cannot be implied by any per-move fact (it names
// conflicts that touch NO move's subject at all), so it is the one term
// this function checks directly rather than through FullyEvidenced.
func progressClean(r migrate.Report) bool {
	if !r.Evidence.Complete {
		return false
	}
	// A plan that plans nothing has evidenced nothing: zero moves is not a
	// migration whose every move is affirmative, it is an empty document.
	if r.Summary.Derived.MovesTotal == 0 || r.Summary.Derived.FullyEvidenced != r.Summary.Derived.MovesTotal {
		return false
	}
	if r.Summary.Derived.UnplannedConflicts != 0 {
		return false
	}
	return true
}
