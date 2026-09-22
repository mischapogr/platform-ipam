package onboardcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mischapogr/platform-ipam/internal/assess"
	"github.com/mischapogr/platform-ipam/internal/domain"
	"github.com/mischapogr/platform-ipam/internal/netbox"
	"github.com/mischapogr/platform-ipam/internal/onboard"
)

// This file implements `platform-ipam onboard remove` (docs/WORK_PLAN.md
// package M9b4, docs/decisions/0016-SOURCE_AWARE_REFRESH_AND_REMOVAL_OF_IMPORTED_OCCUPANCY.md):
// the first command in this repository that deletes something the platform
// does not own. It is built in the shape of `adopt abandon` (package H2c):
// a dry run by default, one named prefix (a domain and a CIDR), one JSON
// report always, no `--all` and no sweep -- and unlike abandon, it defaults
// to the SAFE direction: nothing is deleted unless --apply is given
// together with the NetBox id a dry run just reported, because ADR 0016's
// own fourth evidence rule is "a reviewed, explicit invocation" and its own
// words are "the tool refuses only what it can decide mechanically" -- the
// freshness judgement is the operator's, made by reading the report between
// the two runs.
//
// remove reads the evidence collection being acted on exactly as `onboard
// assess` does (package M1b3): --inventory or the individual
// --networks/--failures/--accounts/--run flags, through the very same
// decoders (resolveAssessInputs, readNetworksTable, decodeAccountsJSON,
// decodeFailuresCSV, decodeRunJSON, buildResourceRecords -- all defined in
// assess.go, this file adds no second reading of any of them) and the same
// engine, internal/assess.Assess, for its Coverage and its input digests.
// What differs from assess is the question asked of that evidence: assess
// asks whether two VPCs conflict, remove asks whether every one of this ONE
// prefix's contributors is known, covered and observed absent.
//
// remove never opens the ledger, exactly like every other onboard
// subcommand (cmd/platform-ipam/main.go dispatches "onboard" before
// storage.NewPostgresLedger is constructed) -- ADR 0016's own "Where it
// lives" section names the cost of that choice and accepts it.

// --- flags and dispatch ---

func runRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	positional, flagArgs := splitPositionalFlags(args, []string{"apply"},
		[]string{"domain", "inventory", "networks", "failures", "accounts", "run", "id", "out"})
	set := newFlagSet("remove")
	domainID := set.String("domain", "", "overlap domain id (required)")
	inventory := set.String("inventory", "", "collector output directory, exactly as `onboard assess --inventory` reads one")
	var networksList []string
	set.Var(stringListFlag{&networksList}, "networks", "networks input, repeatable, through the same reader `onboard assess --networks` uses")
	failuresFlag := set.String("failures", "", "failures.csv path")
	accountsFlag := set.String("accounts", "", "accounts.json path")
	runFlag := set.String("run", "", "run.json path")
	applyFlag := set.Bool("apply", false, "remove the prefix; without this flag remove only reports what it would do and writes nothing to NetBox")
	idFlag := set.String("id", "", "the NetBox id a prior dry run reported for this CIDR; required with --apply, and must still match a fresh read")
	outFlag := set.String("out", "", "additionally write the report to this path (stdout always carries the same bytes)")
	if err := parseFlags(set, flagArgs); err != nil {
		return usageFail(stderr, err)
	}
	if len(positional) != 1 {
		return usageFail(stderr, usagef("remove: exactly one CIDR is required (got %d); remove acts on one named prefix, never a sweep", len(positional)))
	}
	cidr := positional[0]
	if *domainID == "" {
		return usageFail(stderr, usagef("remove: --domain is required"))
	}
	if *applyFlag && strings.TrimSpace(*idFlag) == "" {
		return usageFail(stderr, usagef("remove: --apply requires --id, the NetBox id a dry run reported for %s", cidr))
	}

	networksInputs, failuresPath, accountsPath, runPath := resolveAssessInputs(*inventory, networksList, *failuresFlag, *accountsFlag, *runFlag)
	if len(networksInputs) == 0 {
		return usageFail(stderr, usagef("remove: no evidence collection: give --inventory DIR or at least one --networks input"))
	}

	var inputFiles []assess.InputFile
	addDigest := func(path string, content []byte) {
		inputFiles = append(inputFiles, assess.InputFile{Path: path, Content: content})
	}

	var mergedTable onboard.Table
	haveNetworks := false
	for _, name := range networksInputs {
		data, table, err := readNetworksTable(name)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: reading %s: %v\n", name, err)
			return ExitAdapter
		}
		addDigest(name, data)
		table = stampSourceFile(table, name)
		if table.Kind != onboard.KindNetworks {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: %s resolved as a %s table, not a networks table\n", name, table.Kind)
			return ExitAdapter
		}
		if dropped := errorDiagnostics(table.Diagnostics); len(dropped) != 0 {
			for _, d := range dropped {
				fmt.Fprintf(stderr, "platform-ipam onboard remove: %s:%d: %s\n", name, d.Row, d.Message)
			}
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
		fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
		return ExitAdapter
	}

	var accountRecords []assess.AccountRecord
	if accountsPath != "" {
		data, err := os.ReadFile(accountsPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: reading %s: %v\n", accountsPath, err)
			return ExitAdapter
		}
		addDigest(accountsPath, data)
		accountRecords, err = decodeAccountsJSON(accountsPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
			return ExitAdapter
		}
	}

	var failureRows []assess.FailureRow
	if failuresPath != "" {
		data, err := os.ReadFile(failuresPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: reading %s: %v\n", failuresPath, err)
			return ExitAdapter
		}
		addDigest(failuresPath, data)
		failureRows, err = decodeFailuresCSV(failuresPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
			return ExitAdapter
		}
	}

	var runRecord *assess.RunRecord
	if runPath != "" {
		data, err := os.ReadFile(runPath)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: reading %s: %v\n", runPath, err)
			return ExitAdapter
		}
		addDigest(runPath, data)
		rr, err := decodeRunJSON(runPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
			return ExitAdapter
		}
		runRecord = &rr
	}

	in := assess.Input{Records: records, Accounts: accountRecords, Failures: failureRows, Run: runRecord}
	assessReport, err := assess.Assess(in, assess.Options{InputFiles: inputFiles})
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
		return ExitAdapter
	}

	a, err := newAdapter()
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
		return ExitAdapter
	}
	d, ok := resolveDomain(a.cfg, *domainID)
	if !ok {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: unknown domain %q\n", *domainID)
		return ExitValidation
	}

	detail, err := a.inv.ReadOccupancy(ctx, d, cidr)
	if err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
		return ExitAdapter
	}

	report := buildRemoveReport(*domainID, detail, records, runRecord, assessReport, *applyFlag, *idFlag)

	if *applyFlag && report.Removable {
		result, err := a.inv.DeleteOccupancy(ctx, d, cidr, detail.ID)
		if err != nil {
			report.Refusals = append(report.Refusals, removeRefusal{Code: "delete_failed", Message: err.Error()})
			report.Removable = false
			if writeErr := writeRemoveReport(stdout, *outFlag, report); writeErr != nil {
				fmt.Fprintf(stderr, "platform-ipam onboard remove: writing report: %v\n", writeErr)
				return ExitAdapter
			}
			fmt.Fprintf(stderr, "platform-ipam onboard remove: %v\n", err)
			return removeExit(err)
		}
		report.Removed = true
		report.NetBoxID = result.ID
	}

	if err := writeRemoveReport(stdout, *outFlag, report); err != nil {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: writing report: %v\n", err)
		return ExitAdapter
	}

	if report.Removed {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: removed prefix %s (%s)\n", cidr, report.NetBoxID)
		return ExitOK
	}
	if report.Removable {
		fmt.Fprintf(stderr, "platform-ipam onboard remove: dry run for %s: removable; re-run with --apply --id %s to remove it\n", cidr, report.NetBoxID)
		return ExitOK
	}
	fmt.Fprintf(stderr, "platform-ipam onboard remove: %s is not removable: %d refusal(s)\n", cidr, len(report.Refusals))
	return ExitValidation
}

// removeExit classifies a DeleteOccupancy error the way abandonExit already
// classifies AbandonAdoption's: an error carrying domain.ErrInventoryUncertain
// or netbox.ErrRemovalUncertain got no answer from NetBox and may or may not
// have deleted the prefix, so it is ExitAdapter -- infrastructure, not a
// decision; everything else (owned, not imported, id mismatch, a 412
// conflict, a failed read-back) is a decision this adapter reached on
// evidence it read, so it is ExitValidation, the same code the dry run
// itself returns for a refusal.
func removeExit(err error) int {
	if errors.Is(err, domain.ErrInventoryUncertain) || errors.Is(err, netbox.ErrRemovalUncertain) {
		return ExitAdapter
	}
	return ExitValidation
}

// --- report shape ---

type removeReport struct {
	ReportVersion int                          `json:"report_version"`
	Domain        string                       `json:"domain"`
	CIDR          string                       `json:"cidr"`
	NetBoxID      string                       `json:"netbox_id,omitempty"`
	Apply         bool                         `json:"apply"`
	Removable     bool                         `json:"removable"`
	Removed       bool                         `json:"removed"`
	Description   string                       `json:"description"`
	Contributors  []removeContributorEvidence  `json:"contributors"`
	Coverage      []removeAccountRegionOutcome `json:"coverage"`
	Run           *removeRunSummary            `json:"run,omitempty"`
	Refusals      []removeRefusal              `json:"refusals"`
	Inputs        []assess.InputDigest         `json:"inputs,omitempty"`
}

type removeRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// removeContributorEvidence is one contributor as the prefix carries it,
// plus rule 3's own per-contributor absence argument: whether the evidence
// collection observed it absent, and why (or why not).
type removeContributorEvidence struct {
	domain.Contributor
	ObservedAbsent bool   `json:"observed_absent"`
	Reason         string `json:"reason"`
}

// removeAccountRegionOutcome is one (account, region) pair among the
// contributors', and what the run record and coverage say about it.
type removeAccountRegionOutcome struct {
	AccountID string `json:"account_id"`
	Region    string `json:"region"`
	Outcome   string `json:"outcome"`
	Complete  bool   `json:"complete"`
}

type removeRunSummary struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	SourceFile string `json:"source_file"`
}

func writeRemoveReport(stdout io.Writer, outPath string, report removeReport) error {
	buf, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	if _, err := stdout.Write(buf); err != nil {
		return err
	}
	if outPath != "" {
		return os.WriteFile(outPath, buf, 0o644)
	}
	return nil
}

// --- evidence and refusals ---

// buildRemoveReport is the pure decision at this command's centre: it takes
// what NetBox says about the one prefix (detail), what the evidence
// collection says (records, runRecord, assessReport.Coverage) and decides
// every one of ADR 0016's four evidence rules plus its five outright
// refusals, without ever calling NetBox again. It always evaluates every
// rule -- it never stops at the first failure -- because the dry run's
// whole purpose is to show an operator the complete picture in one report.
func buildRemoveReport(domainID string, detail netbox.OccupancyDetail, records []assess.ResourceRecord, runRecord *assess.RunRecord, assessReport assess.Report, apply bool, wantID string) removeReport {
	// Refusals, Contributors and Coverage are seeded as non-nil, empty
	// slices rather than left at their zero value: a report that never
	// appends to one of them (the happy path has no refusals; a prefix
	// with no contributors would have none to walk) must still encode
	// `[]`, not JSON `null` -- the same contract adopt.Report's own
	// Records already holds (internal/adoptcmd/plan.go's Report is built
	// with `make([]RecordResult, len(records))` for exactly this reason).
	// A caller that ranges over `refusals`/`contributors`/`coverage`
	// without a null check -- this package's own end-to-end tests did --
	// must never see null on the happy path.
	report := removeReport{
		ReportVersion: 1, Domain: domainID, CIDR: detail.CIDR, NetBoxID: detail.ID,
		Apply: apply, Description: detail.Description, Inputs: assessReport.Inputs,
		Refusals: []removeRefusal{}, Contributors: []removeContributorEvidence{}, Coverage: []removeAccountRegionOutcome{},
	}
	if runRecord != nil {
		report.Run = &removeRunSummary{StartedAt: runRecord.StartedAt, FinishedAt: runRecord.FinishedAt, SourceFile: runRecord.SourceFile}
	}

	refuse := func(code, message string) {
		report.Refusals = append(report.Refusals, removeRefusal{Code: code, Message: message})
	}

	// --- outright refusals, whatever the evidence says ---
	if detail.Owned {
		refuse("prefix_owned", fmt.Sprintf("prefix %s carries an ownership field; refusing to remove a platform-managed object", detail.CIDR))
	}
	if !detail.Imported {
		refuse("not_imported", fmt.Sprintf("prefix %s does not carry the %s tag; not ours to remove", detail.CIDR, netbox.ImportedTag))
	}
	if detail.PoolConflict != "" {
		refuse("pool_prefix", detail.PoolConflict)
	}
	if detail.Contributors != nil && len(detail.Contributors) == 0 {
		// A non-nil, zero-length list, as distinct from no list at all
		// (rule 1, below): ADR 0016's own words, "an empty list is not an
		// argument that nothing contributes; it is a list nobody wrote".
		refuse("empty_contributor_list", fmt.Sprintf("prefix %s carries an empty contributor list", detail.CIDR))
	}
	if detail.Contributors != nil && len(detail.Contributors) > 0 && !descriptionMatchesImport(detail) {
		refuse("description_edited", fmt.Sprintf(
			"prefix %s's description does not match what the import wrote (an operator wrote it, or removed the import's own text): "+
				"for a prefix created by package M9c or later this checks the description's own truncation-safe fingerprint "+
				"(withDescriptionFingerprint/descriptionFingerprintConsistent, internal/onboardcmd); for one created before it "+
				"(no fingerprint present) this falls back to package M9b4's original check, that the account and resource-id "+
				"segments the import would generate from the contributor list are still present verbatim -- domain.Contributor "+
				"carries no Name and no per-row free-text description, so neither check can see an edit confined to the name "+
				"or free-text portions of the description, and says so nowhere but here: this is the narrowest honest rule the "+
				"stored data supports",
			detail.CIDR))
	}

	// --- rule 1: every contributor known ---
	if detail.Unreadable {
		refuse("contributor_unreadable", fmt.Sprintf("prefix %s's contributor list holds something this version cannot decode", detail.CIDR))
	}
	if detail.Contributors == nil && !detail.Unreadable {
		refuse("contributor_unknown", fmt.Sprintf("prefix %s carries no contributor list at all (imported before ADR 0016, and never refreshed)", detail.CIDR))
	}
	if detail.Reconstructed {
		refuse("contributor_reconstructed", fmt.Sprintf("prefix %s's contributor list was reconstructed by a refresh, not written by the import that created the prefix, and can never be proven complete", detail.CIDR))
	}
	for _, c := range detail.Contributors {
		if c.ObservedAt == nil {
			refuse("contributor_stale_source", fmt.Sprintf("contributor %s has a null observed_at (a pre-ADR-0016 or pre-M1b1 source) and can never be removal evidence", c.Identity))
		}
		if c.ResourceID == "" {
			refuse("contributor_degenerate_identity", fmt.Sprintf("contributor %s has no resource_id (a degenerate identity) and can never be proven absent", c.Identity))
		}
	}

	// --- rules 2 and 3: coverage and absence, per contributor ---
	seenAR := map[assess.AccountRegion]bool{}
	for _, c := range detail.Contributors {
		ar := assess.AccountRegion{AccountID: c.AccountID, Region: c.Region}
		if !seenAR[ar] {
			seenAR[ar] = true
			outcome, complete := accountRegionCoverage(assessReport.Coverage, runRecord, ar)
			report.Coverage = append(report.Coverage, removeAccountRegionOutcome{AccountID: ar.AccountID, Region: ar.Region, Outcome: outcome, Complete: complete})
			if !complete {
				refuse("coverage_incomplete", fmt.Sprintf("account %s region %s: %s", ar.AccountID, ar.Region, outcome))
			}
		}

		absent, reason := observedAbsent(records, c)
		if absent && c.ObservedAt != nil && runRecord != nil {
			if stale, staleReason := observationStaleAgainstFinish(*c.ObservedAt, runRecord.FinishedAt); stale {
				absent, reason = false, staleReason
			}
		}
		report.Contributors = append(report.Contributors, removeContributorEvidence{Contributor: c, ObservedAbsent: absent, Reason: reason})
		if !absent {
			refuse("not_observed_absent", fmt.Sprintf("contributor %s: %s", c.Identity, reason))
		}
	}
	sortAccountRegionOutcomes(report.Coverage)

	// --- rule 4: a reviewed, explicit invocation ---
	if apply && wantID != detail.ID {
		refuse("netbox_id_mismatch", fmt.Sprintf("--id %s does not match the NetBox id %s a fresh read finds at %s; the review is stale", wantID, detail.ID, detail.CIDR))
	}

	report.Removable = len(report.Refusals) == 0
	return report
}

// descriptionMatchesImport decides ADR 0016's description_edited refusal:
// does the prefix's current description still look like what the import
// wrote, rather than something an operator has since typed over it.
//
// Package M9c changed what a NEW prefix's description contains
// (mergeNetworkDescription, onboardcmd.go): it no longer rolls up the
// contributors' account and resource ids, so expectedContributorDescription
// below -- built entirely from those two Contributor fields -- can no
// longer be found inside a new-format description at all, and checking it
// unconditionally would refuse every prefix M9c creates as "edited" the
// moment it is read back. The fingerprint mergeNetworkDescription now
// embeds in the description itself (withDescriptionFingerprint,
// onboardcmd.go) is checked first and, when present, decides the question
// on its own: it is a self-consistency check over the description's own
// text, so it needs nothing reconstructed from Contributor and stays
// correct across a truncation.
//
// A prefix created before this package shipped (M9b1-M9b4) carries no
// fingerprint at all -- it predates withDescriptionFingerprint -- so this
// falls back to the original, unmodified check for those: does the
// description still CONTAIN the account/resource-id segments
// expectedContributorDescription reconstructs from the stored contributor
// list. Every existing test and fixture built against that shape
// (internal/onboardcmd/remove_test.go's baseDetail, and any prefix a real
// install already imported under M9b1-M9b4 before upgrading) keeps working
// unchanged.
func descriptionMatchesImport(detail netbox.OccupancyDetail) bool {
	// A syntactically present new-format suffix is decisive. If its digest
	// disagrees with the body, the legacy containment rule must not rescue
	// it: a purpose name can itself contain the old account/resource-id
	// sentence, even though the new import never generated that roll-up.
	if descriptionFingerprintPattern.MatchString(detail.Description) {
		return descriptionFingerprintConsistent(detail.Description)
	}
	legacy := expectedContributorDescription(detail.Contributors)
	if legacy == "" {
		// Nothing reconstructable either way (every contributor lacks both
		// an account id and a resource id, e.g. a degenerate identity) --
		// this check has no evidence to compare and must not manufacture a
		// false "edited" from an empty string, which is contained in
		// everything.
		return true
	}
	return strings.Contains(detail.Description, legacy)
}

// descriptionFingerprintConsistent reports whether description ends with
// onboardcmd.go's withDescriptionFingerprint suffix AND that suffix still
// matches the body it is attached to -- a self-consistency check, not a
// lookup: it needs no copy of what the original body should have been, only
// that the body and its own fingerprint still agree, which any edit to
// either one breaks. A description with no such suffix at all (never
// written with one, or the whole thing replaced) returns false and falls
// back to descriptionMatchesImport's legacy path.
func descriptionFingerprintConsistent(description string) bool {
	loc := descriptionFingerprintPattern.FindStringSubmatchIndex(description)
	if loc == nil {
		return false
	}
	body := description[:loc[0]]
	got := description[loc[2]:loc[3]]
	return got == descriptionFingerprint(body)
}

// expectedContributorDescription reconstructs the parts of a PRE-M9c
// mergeNetworkDescription's output that a stored contributor list CAN
// reproduce: the "account(s) ..." and "resource id(s) ..." segments, built
// with the exact same uniqueNonEmpty/plural/join(" | ") that generation
// used. domain.Contributor carries no Name and no per-row free-text
// description (ADR 0016 defines exactly identity, account_id, region, type,
// resource_id, parent_id, association_id, observed_at, the two batch fields
// and source_file/source_row -- nothing else), so this can never
// reconstruct the Name segment or any per-row source-label segment of the
// original description, both of which a real import commonly carries.
// descriptionMatchesImport therefore checks CONTAINMENT (this
// reconstruction must appear verbatim inside the stored description), not
// equality: the narrowest honest rule this command can apply, given what
// Contributor actually stores. It still refuses precisely when the
// evidence-bearing text -- the account ids and resource ids a removal's
// argument is actually built on -- has been altered or removed, which is
// what "an operator wrote it" is meant to catch; it does not, and cannot,
// detect an edit confined to the Name or free-text portions, since nothing
// here has a copy of what those originally said. Package M9c narrowed this
// to a fallback for a prefix created before it (see
// descriptionMatchesImport); it is otherwise unchanged from M9b4.
func expectedContributorDescription(contributors []domain.Contributor) string {
	var parts []string
	if accounts := uniqueNonEmpty(len(contributors), func(i int) string { return contributors[i].AccountID }); len(accounts) > 0 {
		parts = append(parts, plural("account", len(accounts))+" "+strings.Join(accounts, ", "))
	}
	if resourceIDs := uniqueNonEmpty(len(contributors), func(i int) string { return contributors[i].ResourceID }); len(resourceIDs) > 0 {
		parts = append(parts, plural("resource id", len(resourceIDs))+" "+strings.Join(resourceIDs, ", "))
	}
	return strings.Join(parts, " | ")
}

// accountRegionCoverage answers ADR 0014's amended coverage rule (incl.
// package M1c's row-count cross-check), narrowed to ONE contributing account
// and region, from the assess.Report.Coverage this command's own call to
// assess.Assess already computed over the whole collection -- never a
// second implementation of computeCoverage, which is unexported and stays
// that way. "complete" here means this one pair, not the whole estate:
// Report.Coverage.Complete is the wrong question for a removal, which must
// judge only the accounts and regions its own contributors depend on.
func accountRegionCoverage(cov assess.Coverage, run *assess.RunRecord, ar assess.AccountRegion) (outcome string, complete bool) {
	if cov.RunMissing {
		return "run.json missing", false
	}
	if cov.AccountsMissing {
		return "accounts.json missing", false
	}
	for _, f := range cov.Failed {
		if f.AccountID == ar.AccountID && (f.Region == ar.Region || f.Region == "") {
			return "failed: " + f.Error, false
		}
	}
	for _, p := range cov.Partial {
		if p.AccountID == ar.AccountID && p.Region == ar.Region {
			return "partial", false
		}
	}
	for _, n := range cov.NotAttempted {
		if n.AccountID == ar.AccountID && (n.Region == ar.Region || n.Region == "") {
			return "not attempted", false
		}
	}
	for _, r := range cov.RowCountShort {
		if r.AccountID == ar.AccountID && r.Region == ar.Region {
			return fmt.Sprintf("row count short: run.json recorded %d, %d present", r.Recorded, r.Present), false
		}
	}
	// A positive statement, not merely "named by no gap list": a
	// contributing account/region the run record never mentions at all
	// (not ACTIVE in accounts.json, or simply absent from the run) is
	// UNKNOWN, not complete, and none of Coverage's gap lists above are
	// built to name an account the run record is silent about entirely.
	if run != nil {
		for _, a := range run.Attempts {
			if a.AccountID == ar.AccountID && a.Region == ar.Region && a.Outcome == assess.AttemptSucceeded {
				return "succeeded", true
			}
		}
	}
	return "not listed in the run record", false
}

// observedAbsent is ADR 0016's evidence rule 3, read literally: "no row
// [carries] the contributor's resource id in its account and region."
func observedAbsent(records []assess.ResourceRecord, c domain.Contributor) (absent bool, reason string) {
	for _, r := range records {
		if r.AccountID == c.AccountID && r.Region == c.Region && r.ResourceID == c.ResourceID {
			return false, fmt.Sprintf("still present in the collection (%s row %d)", r.SourceFile, r.SourceRow)
		}
	}
	return true, "no row in the collection names this resource id in this account and region"
}

// observationStaleAgainstFinish is ADR 0016's other half of rule 3: "each
// contributor's stored observed_at must also be older than that
// collection's finished_at, so a removal cannot be argued from a collection
// that predates the evidence that created the contributor." Both timestamps
// are RFC 3339 (run.json's own shape, docs/AWS_ORGANIZATION_INVENTORY.md);
// a timestamp this cannot parse is treated as making the argument unusable
// rather than silently skipped, because "never guess" is this project's
// rule for exactly this kind of comparison.
func observationStaleAgainstFinish(observedAt, finishedAt string) (stale bool, reason string) {
	obs, errObs := time.Parse(time.RFC3339, observedAt)
	fin, errFin := time.Parse(time.RFC3339, finishedAt)
	if errObs != nil || errFin != nil {
		return true, "observed_at or the collection's finished_at is not a readable RFC 3339 timestamp; the absence argument cannot be dated"
	}
	if !obs.Before(fin) {
		return true, fmt.Sprintf("observed_at %s is not older than this collection's finished_at %s; the absence argument cannot be dated as newer evidence", observedAt, finishedAt)
	}
	return false, ""
}

func sortAccountRegionOutcomes(list []removeAccountRegionOutcome) {
	// A stable, deterministic order (account, then region) so two runs over
	// the same evidence produce byte-identical reports, matching every
	// other sorted list in this package and in internal/assess.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if a.AccountID < b.AccountID || (a.AccountID == b.AccountID && a.Region <= b.Region) {
				break
			}
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}
