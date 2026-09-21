package onboardcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/migrate"
)

// This file tests `platform-ipam onboard progress` (progress.go,
// docs/WORK_PLAN.md package M3b3,
// docs/decisions/0015-REVIEWED_MIGRATION_PLAN_AND_DERIVED_PROGRESS.md), in
// the shape assess_test.go tests assess.go.
//
// --- fixtures ---
//
// Every account id below is either the all-zero account or a repdigit (all
// twelve digits identical), the repository's own synthetic convention --
// see TestProgressFixturesUseOnlySyntheticAccountIDs below, this package's
// own version of assess_test.go's TestAssessFixturesUseOnlySyntheticAccountIDs
// and internal/migrate's TestFixturesUseOnlySyntheticAccountIDs. Every CIDR
// is RFC 1918 space.

// progressVPCPresentCSV observes exactly the VPC every plan fixture below
// names as its subject ("aws:111111111111:eu-central-1:vpc-1111111111111111"):
// coverage is complete for the account/region, and the subject reads
// SubjectObserved.
const progressVPCPresentCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
111111111111,10.10.0.0/16,eu-central-1,vpc,vpc-1111111111111111,alpha-vpc,available,true,vpc-cidr-assoc-aaa1,2026-09-20T00:00:00Z
`

// progressVPCGoneCSV observes a DIFFERENT VPC in the SAME account and
// region as every plan fixture's subject, so coverage is complete for that
// account/region while the subject itself is absent: SubjectNotObserved
// (ADR 0015's "the pair that matters most" -- internal/migrate's own
// TestSubjectFactNotObservedUnderCompleteCoverageAgainstUnknownUnderIncomplete
// pins the same pair one layer down).
const progressVPCGoneCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
111111111111,10.20.0.0/16,eu-central-1,vpc,vpc-9999999999999999,other-vpc,available,true,vpc-cidr-assoc-bbb1,2026-09-20T00:00:00Z
`

const progressAccountsJSON = `{"Accounts":[{"Id":"111111111111","Name":"Alpha","Status":"ACTIVE"}]}`

const progressRunJSON = `{
  "script_version": "2",
  "started_at": "2026-09-20T00:00:00Z",
  "finished_at": "2026-09-20T00:01:00Z",
  "role_name": "PlatformIpamReadOnly",
  "management_account_used": false,
  "management_account_id": null,
  "configured_regions": ["eu-central-1"],
  "accounts": [
    {
      "account_id": "111111111111",
      "account_name": "Alpha",
      "credential_source": "assumed-role",
      "regions_attempted": "known",
      "not_attempted_reason": null,
      "region_source": "configured",
      "regions": [
        {"region": "eu-central-1", "outcome": "succeeded", "row_count": 1, "observed_at": "2026-09-20T00:00:00Z"}
      ]
    }
  ]
}
`

// progressPlanYAML is one move -- replace, subject
// aws:111111111111:eu-central-1:vpc-1111111111111111, target
// tenant-a/alloc-a -- written as block YAML with a comment, so the YAML
// path (not JSON dressed up with a .yaml extension) is what every test
// below actually exercises.
const progressPlanYAML = `# reviewed migration plan -- test fixture, not a real customer's
version: 1
plan_id: test-plan
waves:
  - id: wave-1
    name: Wave 1
    owner: alice
moves:
  - subject:
      account_id: "111111111111"
      region: eu-central-1
      vpc_id: vpc-1111111111111111
    disposition: replace
    wave: wave-1
    owner: alice
    target:
      tenant_id: tenant-a
      allocation_key: alloc-a
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "111111111111"
      prefix_length: 16
`

// progressEvidenceEmptyJSON is an operator-scoped export that saw no
// allocation at all for tenant-a/alloc-a: ADR 0015's "none" ("the
// allocation evidence could have seen an allocation for this tenant and
// key and contains none").
const progressEvidenceEmptyJSON = `{"read_at": "2026-09-21T00:00:00Z", "scope": "operator", "allocations": []}`

// progressEvidenceActiveJSON is an operator-scoped export carrying an
// ACTIVE, verified allocation whose immutable fields equal
// progressPlanYAML's target exactly -- ADR 0015's "active" fact.
const progressEvidenceActiveJSON = `{
  "read_at": "2026-09-21T00:00:00Z",
  "scope": "operator",
  "allocations": [
    {
      "tenant_id": "tenant-a",
      "allocation_key": "alloc-a",
      "scope": "vpc",
      "environment": "prod",
      "region": "eu-central-1",
      "account_id": "111111111111",
      "prefix_length": 16,
      "state": "ACTIVE",
      "binding_verified_at": "2026-09-21T00:00:00Z"
    }
  ]
}
`

// --- test helpers ---

// runProgressArgs runs runProgress directly (not through Main -- progress
// needs no context.Context, see progress.go) and returns the exit code and
// both streams as strings, mirroring assess_test.go's runAssessArgs.
func runProgressArgs(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = runProgress(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func decodeProgressReport(t *testing.T, jsonText string) migrate.Report {
	t.Helper()
	var r migrate.Report
	if err := json.Unmarshal([]byte(jsonText), &r); err != nil {
		t.Fatalf("decoding report JSON: %v\n%s", err, jsonText)
	}
	return r
}

// assertProgressReportProse mirrors internal/migrate/report_test.go's own
// assertNoForbiddenPhrase and assertNoPercentSign, scanning THIS report's
// two renderings from the command's own stdout rather than an in-process
// migrate.Report value: Summary.Sentence and Notes (decoded from the JSON
// rendering) plus the full text rendering for every member of
// migrate.ForbiddenPhrases(), and both renderings in full for any "%"
// character.
func assertProgressReportProse(t *testing.T, jsonText, textText string) {
	t.Helper()
	r := decodeProgressReport(t, jsonText)
	ownProse := strings.ToLower(r.Summary.Sentence + " " + strings.Join(r.Notes, " "))
	lowerText := strings.ToLower(textText)
	for _, phrase := range migrate.ForbiddenPhrases() {
		if strings.Contains(ownProse, phrase) {
			t.Errorf("progress report's own prose contains forbidden phrase %q: %q", phrase, ownProse)
		}
		if strings.Contains(lowerText, phrase) {
			t.Errorf("text output contains forbidden phrase %q\n%s", phrase, textText)
		}
	}
	if strings.Contains(jsonText, "%") {
		t.Errorf("JSON output contains a %% character:\n%s", jsonText)
	}
	if strings.Contains(textText, "%") {
		t.Errorf("text output contains a %% character:\n%s", textText)
	}
}

// --- the record's three fixtures ---

// TestProgressExitsThreeOnIncompleteEvidence: ADR 0015's evidence
// paragraph, first fixture -- no allocation evidence supplied at all, so
// evidence.complete is false and the target fact is unknown (not none: no
// evidence file could have seen it).
func TestProgressExitsThreeOnIncompleteEvidence(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeProgressReport(t, stdout)
	if report.Evidence.Complete {
		t.Errorf("Evidence.Complete = true, want false (no --allocations supplied)")
	}
	if len(report.Moves) != 1 || report.Moves[0].TargetFact != migrate.TargetUnknown {
		t.Fatalf("Moves = %+v, want one move with target_fact unknown", report.Moves)
	}
	if report.Moves[0].SubjectFact != migrate.SubjectObserved {
		t.Fatalf("Moves[0].SubjectFact = %s, want observed (the subject IS in networks.csv)", report.Moves[0].SubjectFact)
	}
}

// TestProgressExitsThreeWhenNothingHasMovedYet is ADR 0015's own warning,
// verbatim: "its clean sibling ... exits 3 as well -- because nothing has
// moved yet ... so that nobody builds a fixture that reaches 0 by leaving
// evidence out." Evidence is FULLY supplied (operator scope, which covers
// every tenant) and coverage is complete, but the evidence export saw no
// allocation for the plan's target key yet, and the subject VPC is still
// observed (nothing has actually moved).
func TestProgressExitsThreeWhenNothingHasMovedYet(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceEmptyJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeProgressReport(t, stdout)
	if !report.Evidence.Complete {
		t.Errorf("Evidence.Complete = false, want true (operator scope covers tenant-a, coverage is complete)")
	}
	if len(report.Moves) != 1 || report.Moves[0].TargetFact != migrate.TargetNone {
		t.Fatalf("Moves = %+v, want one move with target_fact none", report.Moves)
	}
	if report.Moves[0].SubjectFact != migrate.SubjectObserved {
		t.Fatalf("Moves[0].SubjectFact = %s, want observed (nothing has moved yet)", report.Moves[0].SubjectFact)
	}
	if report.Moves[0].FullyEvidenced {
		t.Errorf("Moves[0].FullyEvidenced = true, want false")
	}
}

// TestProgressExitsZeroWhenEveryMoveAffirmative is ADR 0015's third
// fixture: "every move's three facts are affirmative, is the only one that
// exits 0." The subject is not-observed under complete coverage (a
// different VPC in the same account/region keeps coverage complete) and
// the operator-scoped evidence carries a verified ACTIVE allocation whose
// immutable fields equal the plan's target exactly.
func TestProgressExitsZeroWhenEveryMoveAffirmative(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCGoneCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceActiveJSON),
		"--format", "json",
	)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0); stderr=%s\nstdout=%s", code, stderr, stdout)
	}
	report := decodeProgressReport(t, stdout)
	if !report.Evidence.Complete {
		t.Errorf("Evidence.Complete = false, want true")
	}
	if len(report.Moves) != 1 || !report.Moves[0].FullyEvidenced {
		t.Fatalf("Moves = %+v, want one move fully evidenced", report.Moves)
	}
	if report.Moves[0].SubjectFact != migrate.SubjectNotObserved {
		t.Errorf("SubjectFact = %s, want not-observed", report.Moves[0].SubjectFact)
	}
	if report.Moves[0].TargetFact != migrate.TargetActive {
		t.Errorf("TargetFact = %s, want active", report.Moves[0].TargetFact)
	}
	if report.Summary.Derived.UnplannedConflicts != 0 {
		t.Errorf("UnplannedConflicts = %d, want 0", report.Summary.Derived.UnplannedConflicts)
	}
}

// --- structural and decode refusals: exit 4, nothing on stdout ---

// progressPlanMissingTargetYAML is a "replace" move with no target -- ADR
// 0015's RefusalMissingTarget.
const progressPlanMissingTargetYAML = `version: 1
waves:
  - id: wave-1
    name: Wave 1
moves:
  - subject:
      account_id: "111111111111"
      region: eu-central-1
      vpc_id: vpc-1111111111111111
    disposition: replace
    wave: wave-1
`

func TestProgressPlanStructuralRefusalExitsFourWithNothingOnStdout(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanMissingTargetYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
	)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want ExitAdapter (4); stderr=%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (no report on a plan refusal)", stdout)
	}
	if !strings.Contains(stderr, "missing_target") {
		t.Errorf("stderr = %q, want it to name the missing_target refusal", stderr)
	}
}

// progressPlanCIDRJSON writes a "cidr" key under a target -- ADR 0015's
// worked example ("an operator who writes cidr: under a target gets an
// error naming the field").
const progressPlanCIDRJSON = `{
  "version": 1,
  "waves": [{"id": "wave-1", "name": "Wave 1"}],
  "moves": [{
    "subject": {"account_id": "111111111111", "region": "eu-central-1", "vpc_id": "vpc-1111111111111111"},
    "disposition": "replace",
    "wave": "wave-1",
    "target": {"tenant_id": "tenant-a", "allocation_key": "alloc-a", "cidr": "10.10.0.0/16"}
  }]
}`

func TestProgressPlanWithCIDRKeyExitsFour(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.json", progressPlanCIDRJSON),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
	)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want ExitAdapter (4); stderr=%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "cidr") {
		t.Errorf("stderr = %q, want it to name the cidr field", stderr)
	}
}

// progressPlanUnquotedAccountIDYAML: account_id written without quotes, the
// YAML-integer trap DecodePlan refuses by ordinary Go type-checking.
const progressPlanUnquotedAccountIDYAML = `version: 1
waves:
  - id: wave-1
    name: Wave 1
moves:
  - subject:
      account_id: 111111111111
      region: eu-central-1
      vpc_id: vpc-1111111111111111
    disposition: retire
    wave: wave-1
`

func TestProgressPlanWithUnquotedAccountIDExitsFour(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanUnquotedAccountIDYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
	)
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want ExitAdapter (4); stderr=%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

// --- usage: no plan at all ---

// TestProgressNoPlanExitsTwo supplies otherwise-complete, valid assess
// inputs (so a mutant that deletes the explicit no-plan check would run to
// completion and exit 0 or 3, not 2 -- this test would then fail rather
// than pass by coincidence).
func TestProgressNoPlanExitsTwo(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
	)
	if code != ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage (2); stderr=%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "no plan") {
		t.Errorf("stderr = %q, want it to say no plan was given", stderr)
	}
}

// --- tenancy: evidence scoped to a tenant other than a target's ---

// TestProgressEvidenceScopedToOtherTenantExitsThreeWithTargetUnknown is ADR
// 0015's scope rule: "a tenant export covers exactly that tenant, and every
// move whose target names another tenant is unknown -- never none."
func TestProgressEvidenceScopedToOtherTenantExitsThreeWithTargetUnknown(t *testing.T) {
	const evidenceOtherTenantJSON = `{
  "read_at": "2026-09-21T00:00:00Z",
  "scope": "tenant-other",
  "allocations": [
    {"tenant_id": "tenant-other", "allocation_key": "alloc-x", "state": "ACTIVE"}
  ]
}`
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", evidenceOtherTenantJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeProgressReport(t, stdout)
	if len(report.Moves) != 1 || report.Moves[0].TargetFact != migrate.TargetUnknown {
		t.Fatalf("Moves = %+v, want one move with target_fact unknown (scope tenant-other never sees tenant-a)", report.Moves)
	}
	if report.Evidence.Complete {
		t.Errorf("Evidence.Complete = true, want false")
	}
}

// progressAccountsTwoJSON adds a second account, "222222222222", never
// mentioned in progressVPCGoneCSV/progressVPCPresentCSV at all.
const progressAccountsTwoJSON = `{"Accounts":[{"Id":"111111111111","Name":"Alpha","Status":"ACTIVE"},{"Id":"222222222222","Name":"Beta","Status":"ACTIVE"}]}`

// progressRunOneAccountNotAttemptedJSON: account 111111111111/eu-central-1
// succeeded (the plan subject's own account/region is fully covered), but
// account 222222222222 was never attempted at all -- an unrelated gap that
// makes the embedded assessment's own coverage.complete false without
// touching the plan's subject or target in any way.
const progressRunOneAccountNotAttemptedJSON = `{
  "script_version": "2",
  "started_at": "2026-09-20T00:00:00Z",
  "finished_at": "2026-09-20T00:01:00Z",
  "role_name": "PlatformIpamReadOnly",
  "management_account_used": false,
  "management_account_id": null,
  "configured_regions": ["eu-central-1"],
  "accounts": [
    {
      "account_id": "111111111111",
      "account_name": "Alpha",
      "credential_source": "assumed-role",
      "regions_attempted": "known",
      "not_attempted_reason": null,
      "region_source": "configured",
      "regions": [
        {"region": "eu-central-1", "outcome": "succeeded", "row_count": 1, "observed_at": "2026-09-20T00:00:00Z"}
      ]
    },
    {
      "account_id": "222222222222",
      "account_name": "Beta",
      "credential_source": "assumed-role",
      "regions_attempted": "unknown",
      "not_attempted_reason": "AccessDenied",
      "region_source": "configured",
      "regions": []
    }
  ]
}
`

// TestProgressExitsThreeWhenAssessmentCoverageIncompleteElsewhere is the
// reason Evidence.Complete must be its OWN top-level check rather than
// something progressClean infers only from FullyEvidenced and
// UnplannedConflicts: ADR 0015 defines evidence.complete as true only when
// the EMBEDDED ASSESSMENT'S OWN coverage.complete is also true (evidence.go's
// idx.complete: "true if and only if the embedded assessment's
// coverage.complete is true, at least one allocation evidence file was
// supplied, and the union of those files' scopes covers every tenant the
// plan's targets name"). Here the plan's own subject and target are both
// fully evidenced -- an unrelated account (222222222222) was simply never
// attempted, which the plan never mentions at all -- so a rule that only
// checked FullyEvidenced and UnplannedConflicts would wrongly exit 0 for an
// estate the assessment itself admits it could not read completely. This
// test is this package's own mutation fixture for progressClean's
// Evidence.Complete check (see the report to the lead's mutation table).
func TestProgressExitsThreeWhenAssessmentCoverageIncompleteElsewhere(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCGoneCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsTwoJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunOneAccountNotAttemptedJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceActiveJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s\nstdout=%s", code, stderr, stdout)
	}
	report := decodeProgressReport(t, stdout)
	if len(report.Moves) != 1 || !report.Moves[0].FullyEvidenced {
		t.Fatalf("Moves = %+v, want the one move fully evidenced despite the unrelated gap", report.Moves)
	}
	if report.Summary.Derived.UnplannedConflicts != 0 {
		t.Fatalf("UnplannedConflicts = %d, want 0", report.Summary.Derived.UnplannedConflicts)
	}
	if report.Evidence.Complete {
		t.Errorf("Evidence.Complete = true, want false (the embedded assessment's own coverage is not complete)")
	}
	if report.Assessment.Coverage.Complete {
		t.Errorf("Assessment.Coverage.Complete = true, want false (account 222222222222 was never attempted)")
	}
}

// progressVPCGoneWithUnplannedConflictCSV extends progressVPCGoneCSV with
// two MORE VPCs, in the SAME account and region, sharing one CIDR --
// an equal-cidr conflict between two subjects NEITHER of which
// progressPlanYAML's move names at all.
const progressVPCGoneWithUnplannedConflictCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
111111111111,10.20.0.0/16,eu-central-1,vpc,vpc-9999999999999999,other-vpc,available,true,vpc-cidr-assoc-bbb1,2026-09-20T00:00:00Z
111111111111,10.30.0.0/16,eu-central-1,vpc,vpc-8888888888888888,unplanned-a,available,true,vpc-cidr-assoc-ccc1,2026-09-20T00:00:00Z
111111111111,10.30.0.0/16,eu-central-1,vpc,vpc-7777777777777777,unplanned-b,available,true,vpc-cidr-assoc-ddd1,2026-09-20T00:00:00Z
`

// progressRunThreeRowsJSON: run.json's own row_count for account
// 111111111111/eu-central-1 must match the number of networks.csv rows this
// run was actually given for that account/region (package M1c,
// rowCountMismatches) -- progressVPCGoneWithUnplannedConflictCSV has three,
// not progressRunJSON's one, so this fixture exists to keep the embedded
// assessment's own coverage.complete true and isolate the unplanned-conflict
// check from an unrelated row-count mismatch.
const progressRunThreeRowsJSON = `{
  "script_version": "2",
  "started_at": "2026-09-20T00:00:00Z",
  "finished_at": "2026-09-20T00:01:00Z",
  "role_name": "PlatformIpamReadOnly",
  "management_account_used": false,
  "management_account_id": null,
  "configured_regions": ["eu-central-1"],
  "accounts": [
    {
      "account_id": "111111111111",
      "account_name": "Alpha",
      "credential_source": "assumed-role",
      "regions_attempted": "known",
      "not_attempted_reason": null,
      "region_source": "configured",
      "regions": [
        {"region": "eu-central-1", "outcome": "succeeded", "row_count": 3, "observed_at": "2026-09-20T00:00:00Z"}
      ]
    }
  ]
}
`

// TestProgressExitsThreeWhenUnplannedConflictExists is this package's own
// mutation fixture for progressClean's UnplannedConflicts check (see the
// report to the lead's mutation table): the plan's one move is fully
// evidenced on its own (subject not-observed, target active, matching
// progressEvidenceActiveJSON), so a rule that checked only FullyEvidenced
// and Evidence.Complete would wrongly exit 0 here -- but the current
// assessment also reports an equal-cidr conflict between two OTHER VPCs
// that no move in the plan names at all (ADR 0015, "What is derived": "a
// conflict the assessment reports on a subject no move mentions is
// unplanned"), which must keep this report out of exit 0.
func TestProgressExitsThreeWhenUnplannedConflictExists(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCGoneWithUnplannedConflictCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunThreeRowsJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceActiveJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s\nstdout=%s", code, stderr, stdout)
	}
	report := decodeProgressReport(t, stdout)
	if len(report.Moves) != 1 || !report.Moves[0].FullyEvidenced {
		t.Fatalf("Moves = %+v, want the one move fully evidenced on its own", report.Moves)
	}
	if !report.Evidence.Complete {
		t.Errorf("Evidence.Complete = false, want true")
	}
	if len(report.Unplanned) != 1 {
		t.Fatalf("Unplanned = %+v, want exactly one unplanned conflict", report.Unplanned)
	}
	if report.Summary.Derived.UnplannedConflicts != 1 {
		t.Errorf("Summary.Derived.UnplannedConflicts = %d, want 1", report.Summary.Derived.UnplannedConflicts)
	}
}

// --- forbidden-phrase and no-% scan, both formats, an incomplete run ---

func TestProgressIncompleteRunProseHasNoForbiddenPhraseOrPercent(t *testing.T) {
	dir := t.TempDir()
	planPath := mustWrite(t, dir, "plan.yaml", progressPlanYAML)
	networksPath := mustWrite(t, dir, "networks.csv", progressVPCPresentCSV)
	accountsPath := mustWrite(t, dir, "accounts.json", progressAccountsJSON)
	runPath := mustWrite(t, dir, "run.json", progressRunJSON)

	_, jsonStdout, jsonStderr := runProgressArgs(
		"--plan", planPath, "--networks", networksPath, "--accounts", accountsPath, "--run", runPath,
		"--format", "json",
	)
	_, textStdout, textStderr := runProgressArgs(
		"--plan", planPath, "--networks", networksPath, "--accounts", accountsPath, "--run", runPath,
		"--format", "text",
	)
	if strings.Contains(strings.ToLower(jsonStderr), "forbidden") {
		t.Fatalf("test setup: stderr unexpectedly mentions forbidden: %s", jsonStderr)
	}
	assertProgressReportProse(t, jsonStdout, textStdout)
	_ = textStderr
}

// --- determinism: two runs, and a permutation of --plan/--allocations order ---

const progressPlanSubject2YAML = `version: 1
waves:
  - id: wave-1
    name: Wave 1
    owner: alice
moves:
  - subject:
      account_id: "111111111111"
      region: eu-central-1
      vpc_id: vpc-2222222222222222
    disposition: replace
    wave: wave-1
    owner: alice
    target:
      tenant_id: tenant-b
      allocation_key: alloc-b
      scope: vpc
      environment: prod
      region: eu-central-1
      account_id: "111111111111"
      prefix_length: 20
`

const progressEvidenceSubject2ActiveJSON = `{
  "read_at": "2026-09-21T00:00:00Z",
  "scope": "operator",
  "allocations": [
    {
      "tenant_id": "tenant-b",
      "allocation_key": "alloc-b",
      "scope": "vpc",
      "environment": "prod",
      "region": "eu-central-1",
      "account_id": "111111111111",
      "prefix_length": 20,
      "state": "ACTIVE",
      "binding_verified_at": "2026-09-21T00:00:00Z"
    }
  ]
}
`

func TestProgressDeterministicAcrossRunsAndFileOrder(t *testing.T) {
	dir := t.TempDir()
	planA := mustWrite(t, dir, "plan-a.yaml", progressPlanYAML)
	planB := mustWrite(t, dir, "plan-b.yaml", progressPlanSubject2YAML)
	evA := mustWrite(t, dir, "evidence-a.json", progressEvidenceActiveJSON)
	evB := mustWrite(t, dir, "evidence-b.json", progressEvidenceSubject2ActiveJSON)
	networksPath := mustWrite(t, dir, "networks.csv", progressVPCGoneCSV)
	accountsPath := mustWrite(t, dir, "accounts.json", progressAccountsJSON)
	runPath := mustWrite(t, dir, "run.json", progressRunJSON)

	run := func(planFirst, planSecond, evFirst, evSecond string) (int, string) {
		code, stdout, stderr := runProgressArgs(
			"--plan", planFirst, "--plan", planSecond,
			"--networks", networksPath, "--accounts", accountsPath, "--run", runPath,
			"--allocations", evFirst, "--allocations", evSecond,
			"--format", "json",
		)
		if code != ExitOK {
			t.Fatalf("exit = %d, want ExitOK (0); stderr=%s", code, stderr)
		}
		return code, stdout
	}

	_, out1 := run(planA, planB, evA, evB)
	_, out2 := run(planA, planB, evA, evB)
	if out1 != out2 {
		t.Fatalf("two runs over identical input produced different output:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", out1, out2)
	}

	_, out3 := run(planB, planA, evB, evA)
	if out1 != out3 {
		t.Fatalf("permuting --plan and --allocations file order changed the output:\n--- original order ---\n%s\n--- permuted order ---\n%s", out1, out3)
	}

	report := decodeProgressReport(t, out1)
	if len(report.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", report.Moves)
	}
	for _, m := range report.Moves {
		if !m.FullyEvidenced {
			t.Errorf("move %s: FullyEvidenced = false, want true", m.Subject)
		}
	}
}

// --- empty environment ---

// TestProgressRunsWithEmptyEnvironment mirrors assess_test.go's
// TestAssessRunsWithEmptyEnvironment: progress needs no IPAM_ variable
// (ADR 0015, "Where it lives"), pinned through Main's own dispatch (not
// runProgress directly) so the property holds for the command an operator
// actually runs.
func TestProgressRunsWithEmptyEnvironment(t *testing.T) {
	var cleared []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "IPAM_") {
			cleared = append(cleared, name)
		}
	}
	saved := make(map[string]string, len(cleared))
	for _, name := range cleared {
		saved[name] = os.Getenv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for name, v := range saved {
			os.Setenv(name, v)
		}
	})

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{
		"progress",
		"--plan", mustWrite(t, dir, "plan.yaml", progressPlanYAML),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCGoneCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceActiveJSON),
		"--format", "json",
	}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0) with an empty environment; stderr=%s", code, stderr.String())
	}
	report := decodeProgressReport(t, stdout.String())
	if len(report.Moves) != 1 || !report.Moves[0].FullyEvidenced {
		t.Fatalf("Moves = %+v, want one move fully evidenced", report.Moves)
	}
}

// --- help text names progress ---

func TestMainHelpTextNamesProgress(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"help"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0)", code)
	}
	if !strings.Contains(stdout.String(), "progress") {
		t.Errorf("help text does not mention progress:\n%s", stdout.String())
	}
}

// --- source-parsing properties: progress.go imports no adapter package,
// reads no environment variable ---

// TestProgressSourceImportsExcludeAdapterPackages is this package's own
// version of TestAssessSourceImportsExcludeAdapterPackages, scoped to
// progress.go: it parses ONLY progress.go and asserts it imports none of
// the packages that would let it open the ledger, call NetBox, load pools
// configuration, call AWS, or serve HTTP transport (ADR 0015, "Where it
// lives": "never open the ledger; never construct a NetBox client; never
// call AWS").
func TestProgressSourceImportsExcludeAdapterPackages(t *testing.T) {
	forbidden := []string{
		"internal/config",
		"internal/netbox",
		"internal/storage",
		"internal/service",
		"internal/cloud",
		"internal/transport",
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "progress.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse progress.go: %v", err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbidden {
			if strings.Contains(path, bad) {
				t.Errorf("progress.go imports %q, which progress must never depend on", path)
			}
		}
	}
}

// TestProgressSourceHasNoEnvironmentRead is progress.go's own version of
// TestAssessSourceHasNoEnvironmentRead: progress.go must never call
// os.Getenv/os.LookupEnv.
func TestProgressSourceHasNoEnvironmentRead(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "progress.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse progress.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "os" {
			return true
		}
		if sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv" {
			t.Errorf("progress.go calls os.%s, which it must never do", sel.Sel.Name)
		}
		return true
	})
}

// TestProgressFixturesUseOnlySyntheticAccountIDs is this package's own
// version of TestAssessFixturesUseOnlySyntheticAccountIDs, scoped to
// progress_test.go (ADR 0015, "Determinism, the output and
// confidentiality": "the package repeats internal/assess's mechanical
// guard that no fixture contains a twelve-digit account id other than the
// all-zero one").
func TestProgressFixturesUseOnlySyntheticAccountIDs(t *testing.T) {
	twelveDigits := regexp.MustCompile(`[0-9]{12}`)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "progress_test.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse progress_test.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, match := range twelveDigits.FindAllString(lit.Value, -1) {
			if !isSyntheticAccountIDForAssessTests(match) {
				t.Errorf("literal in progress_test.go contains a non-synthetic-looking twelve-digit sequence %q", match)
			}
		}
		return true
	})
}

// A plan with no moves cannot be the report that exits 0: it has evidenced
// nothing, however complete the evidence around it is.
func TestProgressWithNoMovesIsNotClean(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runProgressArgs(
		"--plan", mustWrite(t, dir, "plan.yaml", "version: 1\nplan_id: empty\nwaves: []\nmoves: []\n"),
		"--networks", mustWrite(t, dir, "networks.csv", progressVPCPresentCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", progressAccountsJSON),
		"--run", mustWrite(t, dir, "run.json", progressRunJSON),
		"--allocations", mustWrite(t, dir, "evidence.json", progressEvidenceActiveJSON),
		"--format", "json",
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	if report := decodeProgressReport(t, stdout); report.Summary.Derived.MovesTotal != 0 {
		t.Fatalf("moves_total = %d, want 0", report.Summary.Derived.MovesTotal)
	}
}
