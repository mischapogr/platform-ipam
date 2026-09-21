package onboardcmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mischapogr/platform-ipam/internal/assess"
)

// --- fixtures ---
//
// Every account id below is either the all-zero account or a repdigit (all
// twelve digits identical), the repository's own synthetic convention (see
// internal/assess/assess_test.go's TestFixturesUseOnlySyntheticAccountIDs,
// whose account-id guard TestAssessFixturesUseOnlySyntheticAccountIDs below
// extends to this package's own fixtures). Every CIDR is RFC 1918 space.

const networksCleanCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
111111111111,10.10.0.0/16,eu-central-1,vpc,vpc-1111111111111111,alpha-vpc,available,true,vpc-cidr-assoc-aaa1,2026-09-20T00:00:00Z
`

const accountsCleanJSON = `{"Accounts":[{"Id":"111111111111","Name":"Alpha","Status":"ACTIVE"}]}`

const failuresEmptyCSV = `account_id,account_name,region,stage,error
`

const runCleanJSON = `{
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

// networksConflictCSV: two different VPCs, two different accounts, the same
// CIDR -- an equal-cidr conflict.
const networksConflictCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
222222222222,10.20.0.0/16,eu-central-1,vpc,vpc-2222222222222222,beta-vpc,available,true,vpc-cidr-assoc-bbb1,2026-09-20T00:00:00Z
333333333333,10.20.0.0/16,eu-central-1,vpc,vpc-3333333333333333,gamma-vpc,available,true,vpc-cidr-assoc-ccc1,2026-09-20T00:00:00Z
`

const accountsConflictJSON = `{"Accounts":[{"Id":"222222222222","Name":"Beta","Status":"ACTIVE"},{"Id":"333333333333","Name":"Gamma","Status":"ACTIVE"}]}`

const runConflictJSON = `{
  "script_version": "2",
  "started_at": "2026-09-20T00:00:00Z",
  "finished_at": "2026-09-20T00:01:00Z",
  "role_name": "PlatformIpamReadOnly",
  "management_account_used": false,
  "management_account_id": null,
  "configured_regions": ["eu-central-1"],
  "accounts": [
    {"account_id": "222222222222", "account_name": "Beta", "credential_source": "assumed-role",
     "regions_attempted": "known", "not_attempted_reason": null, "region_source": "configured",
     "regions": [{"region": "eu-central-1", "outcome": "succeeded", "row_count": 1, "observed_at": "2026-09-20T00:00:00Z"}]},
    {"account_id": "333333333333", "account_name": "Gamma", "credential_source": "assumed-role",
     "regions_attempted": "known", "not_attempted_reason": null, "region_source": "configured",
     "regions": [{"region": "eu-central-1", "outcome": "succeeded", "row_count": 1, "observed_at": "2026-09-20T00:00:00Z"}]}
  ]
}
`

// matrixConflictYAML puts each side of the conflict in its own group and
// requires them to communicate, so the equal-cidr conflict above is
// impact confirmed. Written as block-style YAML with a comment, per the
// task's requirement to test the YAML path with real YAML, not JSON dressed
// up with a .yaml extension.
const matrixConflictYAML = `# reviewed connectivity matrix -- test fixture, not a real customer's
version: 1
groups:
  - id: g-beta
    members:
      - "222222222222"
  - id: g-gamma
    members:
      - "333333333333"
must_communicate:
  - [g-beta, g-gamma]
must_stay_isolated: []
shared_services: []
`

// runPartialJSON: one account, one region, outcome partial -- and, in the
// tests that use it, deliberately NO --failures flag is passed at all. ADR
// 0014's amendment exists because the FIRST implementation of internal/assess
// derived coverage incompleteness from failures.csv alone, so a partial
// region recorded only in run.json (failures.csv being optional) produced
// coverage.complete == true. That defect is fixed in internal/assess itself
// (M1b2, reviewed); TestRunJSONPartialWithNoFailuresFileStillIncomplete below
// pins the same property at this command's own boundary, in case a future
// change to this file's decoder reintroduces it independently.
const runPartialJSON = `{
  "script_version": "2",
  "started_at": "2026-09-20T00:00:00Z",
  "finished_at": "2026-09-20T00:01:00Z",
  "role_name": "PlatformIpamReadOnly",
  "management_account_used": false,
  "management_account_id": null,
  "configured_regions": ["eu-central-1"],
  "accounts": [
    {"account_id": "111111111111", "account_name": "Alpha", "credential_source": "assumed-role",
     "regions_attempted": "known", "not_attempted_reason": null, "region_source": "configured",
     "regions": [{"region": "eu-central-1", "outcome": "partial", "stage": "describe", "row_count": 1, "observed_at": "2026-09-20T00:00:00Z"}]}
  ]
}
`

const networksPartialCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
111111111111,10.30.0.0/16,eu-central-1,vpc,vpc-1111111111111111,alpha-vpc,available,true,vpc-cidr-assoc-aaa1,2026-09-20T00:00:00Z
`

// networksOldCSV has none of ADR 0014's two new columns; networksNewCSV
// does, and (deliberately) names the SAME CIDR under a different account, so
// the resulting equal-cidr conflict's two Sides let a test inspect the
// column-presence mapping directly: nil for the old file's side, populated
// for the new file's.
const networksOldCSV = `account_id,cidr,region,type,resource_id,name,state,primary
111111111111,10.40.0.0/16,eu-central-1,vpc,vpc-old-0001,old-vpc,available,true
`

const networksNewCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
222222222222,10.40.0.0/16,eu-central-1,vpc,vpc-new-0001,new-vpc,available,true,vpc-cidr-assoc-new1,2026-09-20T01:00:00Z
`

// networksMissingTypeCSV has no "type" column at all -- every row's Type is
// "", which assess.Validate refuses (MissingFieldError), exit 4.
const networksMissingTypeCSV = `account_id,cidr,region
111111111111,10.50.0.0/16,eu-central-1
`

const accountsBadJSON = `{"Accounts": [`

const failuresMissingColumnCSV = `account_id,account_name,region,stage
111111111111,Alpha,,assume-role
`

const runBadJSON = `{not valid json`

const matrixBadYAML = `version: [1, 2
`

// ownershipUnquotedLeadingZeroYAML is the trap the task names: an unquoted
// account id, and specifically one whose digits are all valid octal digits
// (an all-zero id, this package's own synthetic convention), decodes as a
// YAML/JSON *number*, not the string OwnershipEntry.AccountID expects.
const ownershipUnquotedLeadingZeroYAML = `- account_id: 000000000000
  product: p1
  environment: prod
  owner: alice
`

// ownershipQuotedLeadingZeroYAML is the same account id, quoted, which reads
// back as the string it must be.
const ownershipQuotedLeadingZeroYAML = `- account_id: "000000000000"
  product: p1
  environment: prod
  owner: alice
`

const fixedBadYAML = `- cidr: [unterminated
`

const decisionsBadYAML = `c-abc: {decision: unterminated
`

// --- helpers ---

func mustWrite(t *testing.T, dir, name, content string) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, name), content)
}

// runAssessArgs runs runAssess directly (not through Main -- assess needs no
// context.Context, see assess.go) and returns the exit code and both
// streams as strings.
func runAssessArgs(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = runAssess(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func decodeReport(t *testing.T, jsonText string) assess.Report {
	t.Helper()
	var r assess.Report
	if err := json.Unmarshal([]byte(jsonText), &r); err != nil {
		t.Fatalf("decoding report JSON: %v\n%s", err, jsonText)
	}
	return r
}

// --- clean run ---

func TestAssessCleanRunExitsZero(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON),
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if !report.Coverage.Complete {
		t.Errorf("Coverage.Complete = false, want true")
	}
	if len(report.Conflicts) != 0 {
		t.Errorf("Conflicts = %+v, want none", report.Conflicts)
	}
	if !report.Clean() {
		t.Errorf("Report.Clean() = false, want true")
	}
}

// --- row_count cross-check (package M1c) ---

// networksCleanHeaderOnlyCSV is networksCleanCSV truncated to its header
// alone: the same shape a truncated or hand-filtered networks file has
// beside an otherwise-intact run.json. runCleanJSON's own row_count for
// this account/region is 1 (it was written to describe the ONE row
// networksCleanCSV carries), so pairing it with zero rows present is
// exactly package M1c's scenario.
const networksCleanHeaderOnlyCSV = `account_id,cidr,region,type,resource_id,name,state,primary,association_id,observed_at
`

// TestAssessRowCountShortFromTruncatedNetworksFile exercises the exact
// defect docs/WORK_PLAN.md's M1c package names -- "a truncated or
// hand-filtered networks file beside an intact run record still reports
// complete: true" -- end to end through runAssess (and therefore through
// decodeRunJSON's mapping of run.json's row_count onto assess.RunAttempt,
// which package M1c is what makes it reach the engine at all).
func TestAssessRowCountShortFromTruncatedNetworksFile(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanHeaderOnlyCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON), // still says row_count 1
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false (0 rows present, run.json recorded 1)")
	}
	if len(report.Coverage.RowCountShort) != 1 {
		t.Fatalf("RowCountShort = %+v, want exactly one entry", report.Coverage.RowCountShort)
	}
	got := report.Coverage.RowCountShort[0]
	if got.AccountID != "111111111111" || got.Region != "eu-central-1" || got.Recorded != 1 || got.Present != 0 {
		t.Fatalf("RowCountShort[0] = %+v, want {111111111111 eu-central-1 1 0}", got)
	}
}

// TestAssessRowCountExceededFromExtraRow is the mirror: two rows are
// present for the one account/region run.json's row_count says is 1.
func TestAssessRowCountExceededFromExtraRow(t *testing.T) {
	dir := t.TempDir()
	extraRowCSV := networksCleanCSV + "111111111111,10.11.0.0/16,eu-central-1,vpc,vpc-1111111111111112,alpha-vpc-2,available,true,vpc-cidr-assoc-aaa2,2026-09-20T00:00:00Z\n"
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", extraRowCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON), // still says row_count 1
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false (2 rows present, run.json recorded 1)")
	}
	if len(report.Coverage.RowCountExceeded) != 1 {
		t.Fatalf("RowCountExceeded = %+v, want exactly one entry", report.Coverage.RowCountExceeded)
	}
	found := false
	for _, n := range report.Notes {
		if n.Kind == assess.NoteRowCountExceedsRecorded {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s note; notes = %+v", assess.NoteRowCountExceedsRecorded, report.Notes)
	}
}

// TestAssessRowCountMatchesRecordedStaysClean: the ordinary case -- what
// TestAssessCleanRunExitsZero already covers -- must NOT gain a spurious
// LimitRowCountMissing now that decodeRunJSON maps the field: a present,
// matching row_count must not itself appear as a degradation.
func TestAssessRowCountMatchesRecordedStaysClean(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON),
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	for _, l := range report.InputLimits {
		if l == assess.LimitRowCountMissing {
			t.Fatalf("InputLimits = %v, must not carry %s when row_count matched", report.InputLimits, assess.LimitRowCountMissing)
		}
	}
	if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("unexpected row-count mismatch: short=%+v exceeded=%+v", report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
	}
}

// --- confirmed conflict ---

func TestAssessConfirmedConflictExitsValidationWithCompleteCoverage(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksConflictCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsConflictJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runConflictJSON),
		"--matrix", mustWrite(t, dir, "matrix.yaml", matrixConflictYAML),
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if !report.Coverage.Complete {
		t.Errorf("Coverage.Complete = false, want true: this scenario has no coverage gap, only a confirmed conflict")
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if report.Conflicts[0].Impact != assess.ImpactConfirmed {
		t.Errorf("Impact = %q, want confirmed", report.Conflicts[0].Impact)
	}
	if report.Clean() {
		t.Errorf("Report.Clean() = true, want false: a confirmed conflict must never be clean")
	}
}

// --- incomplete coverage, no conflict ---

func TestAssessIncompleteCoverageNoConflictExitsValidation(t *testing.T) {
	dir := t.TempDir()
	accountsTwo := `{"Accounts":[{"Id":"111111111111","Name":"Alpha","Status":"ACTIVE"},{"Id":"444444444444","Name":"Delta","Status":"ACTIVE"}]}`
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsTwo),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON), // covers only 111111111111
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false: account 444444444444 was never attempted")
	}
	if len(report.Conflicts) != 0 {
		t.Errorf("Conflicts = %+v, want none", report.Conflicts)
	}
	assertIncompleteTemplateAndNoForbiddenPhrase(t, stdout, args)
}

// assertIncompleteTemplateAndNoForbiddenPhrase re-renders the SAME
// underlying report as text (by re-running with --format text) and checks
// both encodings: the incomplete template's fixed substring is present, the
// complete template's is absent, and none of
// assess.ForbiddenCompletePhrases() appears in either encoding -- ADR 0014's
// own two-encoders-one-value guarantee means this is one property checked
// twice, not two independent claims.
func assertIncompleteTemplateAndNoForbiddenPhrase(t *testing.T, jsonStdout string, jsonArgs []string) {
	t.Helper()
	report := decodeReport(t, jsonStdout)
	if !strings.Contains(report.Summary.Sentence, "this report cannot make a complete statement for") {
		t.Errorf("sentence = %q, want the incomplete template's fixed substring", report.Summary.Sentence)
	}
	if strings.Contains(report.Summary.Sentence, "no conflicting relationship was observed in the scope read") {
		t.Errorf("sentence = %q, must never contain the complete template's sentence", report.Summary.Sentence)
	}
	textArgs := make([]string, len(jsonArgs))
	copy(textArgs, jsonArgs)
	for i, a := range textArgs {
		if a == "json" {
			textArgs[i] = "text"
		}
	}
	_, textStdout, _ := runAssessArgs(textArgs...)
	for _, phrase := range assess.ForbiddenCompletePhrases() {
		if strings.Contains(jsonStdout, phrase) {
			t.Errorf("json output contains forbidden phrase %q", phrase)
		}
		if strings.Contains(textStdout, phrase) {
			t.Errorf("text output contains forbidden phrase %q", phrase)
		}
	}
}

// --- run.json records partial/failed with NO --failures file ---

func TestRunJSONPartialWithNoFailuresFileStillIncomplete(t *testing.T) {
	dir := t.TempDir()
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksPartialCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--run", mustWrite(t, dir, "run.json", runPartialJSON),
		// deliberately no --failures
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false: run.json itself records a partial region, and failures.csv being absent must not paper over that (ADR 0014's dated amendment)")
	}
	if len(report.Coverage.Partial) != 1 {
		t.Errorf("Coverage.Partial = %+v, want exactly one entry", report.Coverage.Partial)
	}
}

// --- old-format networks mixed with new-format ---

func TestAssessOldAndNewFormatNetworksFilesMixed(t *testing.T) {
	dir := t.TempDir()
	oldPath := mustWrite(t, dir, "old.csv", networksOldCSV)
	newPath := mustWrite(t, dir, "new.csv", networksNewCSV)
	code, stdout, stderr := runAssessArgs("--networks", oldPath, "--networks", newPath, "--format", "json")
	if code != ExitValidation {
		// No accounts/run/failures supplied at all, so coverage is
		// necessarily incomplete; the point of this test is the mapping, not
		// the exit code, but it must still be 3 (a report was produced) and
		// never 4.
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one (both files name 10.40.0.0/16)", report.Conflicts)
	}
	c := report.Conflicts[0]
	if len(c.Sides) != 2 {
		t.Fatalf("Sides = %+v, want two", c.Sides)
	}
	var oldSide, newSide *assess.Side
	for i := range c.Sides {
		switch c.Sides[i].SourceFile {
		case oldPath:
			oldSide = &c.Sides[i]
		case newPath:
			newSide = &c.Sides[i]
		}
	}
	if oldSide == nil || newSide == nil {
		t.Fatalf("could not find both sides by SourceFile in %+v (old=%s new=%s)", c.Sides, oldPath, newPath)
	}
	if oldSide.AssociationID != nil {
		t.Errorf("old-format side AssociationID = %v, want nil (no such column in that file)", *oldSide.AssociationID)
	}
	if oldSide.ObservedAt != nil {
		t.Errorf("old-format side ObservedAt = %v, want nil", *oldSide.ObservedAt)
	}
	if newSide.AssociationID == nil || *newSide.AssociationID != "vpc-cidr-assoc-new1" {
		t.Errorf("new-format side AssociationID = %v, want \"vpc-cidr-assoc-new1\"", newSide.AssociationID)
	}
	if newSide.ObservedAt == nil || *newSide.ObservedAt != "2026-09-20T01:00:00Z" {
		t.Errorf("new-format side ObservedAt = %v, want the recorded instant", newSide.ObservedAt)
	}
	foundAssocLimit, foundObservedLimit := false, false
	for _, l := range report.InputLimits {
		if l == assess.LimitAssociationIDMissing {
			foundAssocLimit = true
		}
		if l == assess.LimitObservationTimeMissing {
			foundObservedLimit = true
		}
	}
	if !foundAssocLimit || !foundObservedLimit {
		t.Errorf("InputLimits = %v, want both association-id-missing and observation-time-missing (from the old-format row)", report.InputLimits)
	}
}

// --- missing required column: no report at all ---

func TestAssessMissingRequiredColumnExitsAdapterWithNoStdout(t *testing.T) {
	dir := t.TempDir()
	code, stdout, stderr := runAssessArgs("--networks", mustWrite(t, dir, "networks.csv", networksMissingTypeCSV))
	if code != ExitAdapter {
		t.Fatalf("exit = %d, want ExitAdapter (4); stderr=%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: no report exists when a required column is missing", stdout)
	}
	if stderr == "" {
		t.Errorf("stderr is empty, want an explanation")
	}
}

// --- unreadable / undecodable inputs, one flag at a time ---

func TestAssessUnreadableOrUndecodableInputsExitAdapter(t *testing.T) {
	dir := t.TempDir()
	goodNetworks := mustWrite(t, dir, "networks_good.csv", networksCleanCSV)
	goodAccounts := mustWrite(t, dir, "accounts_good.json", accountsCleanJSON)
	goodFailures := mustWrite(t, dir, "failures_good.csv", failuresEmptyCSV)
	goodRun := mustWrite(t, dir, "run_good.json", runCleanJSON)
	badAccounts := mustWrite(t, dir, "accounts_bad.json", accountsBadJSON)
	badFailures := mustWrite(t, dir, "failures_bad.csv", failuresMissingColumnCSV)
	badRun := mustWrite(t, dir, "run_bad.json", runBadJSON)
	badMatrix := mustWrite(t, dir, "matrix_bad.yaml", matrixBadYAML)
	badOwnership := mustWrite(t, dir, "ownership_bad.yaml", ownershipUnquotedLeadingZeroYAML)
	badFixed := mustWrite(t, dir, "fixed_bad.yaml", fixedBadYAML)
	badDecisions := mustWrite(t, dir, "decisions_bad.yaml", decisionsBadYAML)
	missing := filepath.Join(dir, "does-not-exist.csv")

	base := func() []string {
		return []string{"--networks", goodNetworks, "--accounts", goodAccounts, "--failures", goodFailures, "--run", goodRun}
	}

	tests := []struct {
		name string
		args []string
	}{
		{"networks file does not exist", []string{"--networks", missing, "--accounts", goodAccounts, "--failures", goodFailures, "--run", goodRun}},
		{"accounts.json malformed", append(base(), "--accounts", badAccounts)},
		{"failures.csv missing a required column", append(base(), "--failures", badFailures)},
		{"run.json malformed", append(base(), "--run", badRun)},
		{"matrix malformed YAML", append(base(), "--matrix", badMatrix)},
		{"ownership unquoted leading-zero account id (YAML integer trap)", append(base(), "--ownership", badOwnership)},
		{"fixed malformed YAML", append(base(), "--fixed", badFixed)},
		{"decisions malformed YAML", append(base(), "--decisions", badDecisions)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runAssessArgs(tc.args...)
			if code != ExitAdapter {
				t.Fatalf("exit = %d, want ExitAdapter (4); stdout=%q stderr=%s", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

// TestAssessOwnershipAcceptsQuotedLeadingZeroAccountID is the positive half
// of the YAML-integer trap: the SAME account id, quoted, must decode
// successfully.
func TestAssessOwnershipAcceptsQuotedLeadingZeroAccountID(t *testing.T) {
	dir := t.TempDir()
	code, _, stderr := runAssessArgs(
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--ownership", mustWrite(t, dir, "ownership.yaml", ownershipQuotedLeadingZeroYAML),
		"--format", "json",
	)
	if code == ExitAdapter {
		t.Fatalf("exit = ExitAdapter (4), want a report to be produced; stderr=%s", stderr)
	}
}

// --- usage errors ---

func TestAssessUsageErrors(t *testing.T) {
	dir := t.TempDir()
	good := mustWrite(t, dir, "networks.csv", networksCleanCSV)
	tests := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--networks", good, "--nonexistent-flag", "x"}},
		{"invalid --format", []string{"--networks", good, "--format", "yaml"}},
		{"stray positional argument", []string{"unexpected-positional", "--networks", good}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, _ := runAssessArgs(tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want ExitUsage (2)", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on a usage error", stdout)
			}
		})
	}
}

// --- the owner's sentence, with both a confirmed conflict and missing coverage ---

func TestAssessOwnerSentenceCountsConflictsAndGaps(t *testing.T) {
	dir := t.TempDir()
	accountsThree := `{"Accounts":[
		{"Id":"222222222222","Name":"Beta","Status":"ACTIVE"},
		{"Id":"333333333333","Name":"Gamma","Status":"ACTIVE"},
		{"Id":"444444444444","Name":"Delta","Status":"ACTIVE"}
	]}`
	args := []string{
		"--networks", mustWrite(t, dir, "networks.csv", networksConflictCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsThree),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runConflictJSON), // covers only 222222222222 and 333333333333; 444444444444 is never mentioned
		"--matrix", mustWrite(t, dir, "matrix.yaml", matrixConflictYAML),
		"--format", "json",
	}
	code, stdout, stderr := runAssessArgs(args...)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false: account 444444444444 was never attempted")
	}
	if report.Summary.TotalRelationships != 1 {
		t.Errorf("TotalRelationships = %d, want 1", report.Summary.TotalRelationships)
	}
	confirmed := 0
	for _, ic := range report.Summary.ByImpact {
		if ic.Impact == assess.ImpactConfirmed {
			confirmed = ic.Count
		}
	}
	if confirmed != 1 {
		t.Errorf("confirmed count = %d, want 1", confirmed)
	}
	if report.Summary.IncompleteAccounts != 1 {
		t.Errorf("IncompleteAccounts = %d, want 1 (account 444444444444)", report.Summary.IncompleteAccounts)
	}
	if !strings.Contains(report.Summary.Sentence, "1 VPC/CIDR relationship(s)") {
		t.Errorf("sentence = %q, want it to count the one relationship", report.Summary.Sentence)
	}
	if !strings.Contains(report.Summary.Sentence, "1 confirmed") {
		t.Errorf("sentence = %q, want it to say 1 confirmed", report.Summary.Sentence)
	}
	if !strings.Contains(report.Summary.Sentence, "this report cannot make a complete statement for 1 account(s)") {
		t.Errorf("sentence = %q, want it to name exactly one incomplete account", report.Summary.Sentence)
	}
}

// --- traceability: every conflict's two sides name a real input row ---

func TestAssessConflictsAreTraceableToTheirSourceRows(t *testing.T) {
	dir := t.TempDir()
	networksPath := mustWrite(t, dir, "networks.csv", networksConflictCSV)
	code, stdout, stderr := runAssessArgs("--networks", networksPath, "--format", "json")
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	report := decodeReport(t, stdout)
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}

	f, err := os.Open(networksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	header := records[0]
	cidrIdx, acctIdx := -1, -1
	for i, h := range header {
		switch h {
		case "cidr":
			cidrIdx = i
		case "account_id":
			acctIdx = i
		}
	}

	for _, side := range report.Conflicts[0].Sides {
		if side.SourceFile != networksPath {
			t.Errorf("side.SourceFile = %q, want %q", side.SourceFile, networksPath)
			continue
		}
		// SourceRow is 1-based over the WHOLE FILE, header included (see
		// internal/onboard/read.go's shiftRows: "the row an operator sees in
		// the source: the header counts"), so file row N is records[N-1] and
		// the valid data-row range is [2, len(records)].
		if side.SourceRow < 2 || side.SourceRow > len(records) {
			t.Fatalf("side.SourceRow = %d out of range for a %d-line file (header + %d data rows)", side.SourceRow, len(records), len(records)-1)
		}
		row := records[side.SourceRow-1]
		if row[cidrIdx] != side.CIDR {
			t.Errorf("row %d cidr = %q, want side.CIDR %q", side.SourceRow, row[cidrIdx], side.CIDR)
		}
		if row[acctIdx] != side.AccountID {
			t.Errorf("row %d account_id = %q, want side.AccountID %q", side.SourceRow, row[acctIdx], side.AccountID)
		}
	}
}

// --- --out and stdout carry identical bytes ---

func TestAssessOutAndStdoutAreByteIdentical(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "report.json")
	code, stdout, stderr := runAssessArgs(
		"--networks", mustWrite(t, dir, "networks.csv", networksConflictCSV),
		"--matrix", mustWrite(t, dir, "matrix.yaml", matrixConflictYAML),
		"--format", "json",
		"--out", outPath,
	)
	if code != ExitValidation {
		t.Fatalf("exit = %d, want ExitValidation (3); stderr=%s", code, stderr)
	}
	fileBytes, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading --out file: %v", err)
	}
	if string(fileBytes) != stdout {
		t.Errorf("--out content differs from stdout:\n--out:\n%s\nstdout:\n%s", fileBytes, stdout)
	}
}

// --- determinism: two runs, and a permutation of the networks inputs' order ---

func TestAssessDeterministicAcrossRunsAndInputOrder(t *testing.T) {
	dir := t.TempDir()
	oldPath := mustWrite(t, dir, "old.csv", networksOldCSV)
	newPath := mustWrite(t, dir, "new.csv", networksNewCSV)

	_, first, _ := runAssessArgs("--networks", oldPath, "--networks", newPath, "--format", "json")
	_, second, _ := runAssessArgs("--networks", oldPath, "--networks", newPath, "--format", "json")
	if first != second {
		t.Errorf("two runs over the same input produced different output")
	}
	_, reordered, _ := runAssessArgs("--networks", newPath, "--networks", oldPath, "--format", "json")
	if first != reordered {
		t.Errorf("reordering the --networks inputs changed the output:\nfirst:\n%s\nreordered:\n%s", first, reordered)
	}
}

// --- empty environment: no IPAM_ variable, no adapter construction ---

func TestAssessRunsWithEmptyEnvironment(t *testing.T) {
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
		"assess",
		"--networks", mustWrite(t, dir, "networks.csv", networksCleanCSV),
		"--accounts", mustWrite(t, dir, "accounts.json", accountsCleanJSON),
		"--failures", mustWrite(t, dir, "failures.csv", failuresEmptyCSV),
		"--run", mustWrite(t, dir, "run.json", runCleanJSON),
		"--format", "json",
	}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0) with an empty environment; stderr=%s", code, stderr.String())
	}
	report := decodeReport(t, stdout.String())
	if !report.Clean() {
		t.Errorf("Report.Clean() = false, want true")
	}
}

// --- help text names assess ---

func TestMainHelpTextNamesAssess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), []string{"help"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (0)", code)
	}
	if !strings.Contains(stdout.String(), "assess") {
		t.Errorf("help text does not mention assess:\n%s", stdout.String())
	}
}

// --- source-parsing property: assess.go imports no adapter package ---

// TestAssessSourceImportsExcludeAdapterPackages is this package's own
// version of internal/assess's TestImportsAreStandardLibraryOnly and
// internal/config's TestOperatorRoleOnlyConstructedByConfigLoad "three
// places" style: rather than banning every non-stdlib import (onboardcmd.go
// itself legitimately imports internal/config and internal/netbox for plan,
// apply and drift), it parses ONLY assess.go and asserts it imports none of
// the packages that would let it open the ledger, call NetBox, load pools
// configuration, call AWS, or serve HTTP transport -- ADR 0014, "Where it
// lives": "never open the ledger; never construct a NetBox client; never
// call AWS". This is a property of the import graph, checked at the source
// level, not just of the tests that happen to exercise it.
func TestAssessSourceImportsExcludeAdapterPackages(t *testing.T) {
	forbidden := []string{
		"internal/config",
		"internal/netbox",
		"internal/storage",
		"internal/service",
		"internal/cloud",
		"internal/transport",
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "assess.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse assess.go: %v", err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbidden {
			if strings.Contains(path, bad) {
				t.Errorf("assess.go imports %q, which assess must never depend on", path)
			}
		}
	}
}

// TestAssessSourceHasNoEnvironmentRead is a companion source-parsing check:
// assess.go must never call os.Getenv/os.LookupEnv, because doing so even
// once would be exactly the H5-style trap ADR 0014 names by name ("assess
// must not repeat it"). loadSettings/newAdapter (onboardcmd.go) call
// os.Getenv freely for plan/apply/drift; assess.go must call it never.
func TestAssessSourceHasNoEnvironmentRead(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "assess.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse assess.go: %v", err)
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
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name == "os" && (sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
			t.Errorf("assess.go calls os.%s -- assess must read no environment variable", sel.Sel.Name)
		}
		return true
	})
}

// --- confidentiality: this file's own fixtures are synthetic ---

// TestAssessFixturesUseOnlySyntheticAccountIDs extends
// internal/assess/assess_test.go's own guard (Go string literals only) to
// this package's fixtures, which are Go string constants holding CSV/JSON/
// YAML text rather than typed Go values -- the twelve-digit sequences that
// matter here live inside those constants' string bodies, which the literal
// scan below already covers (a Go raw string literal IS an *ast.BasicLit of
// kind STRING), so no separate testdata-directory walk is needed: this
// package, unlike internal/assess, keeps every fixture inline (see
// mustWrite's callers throughout this file) rather than in a testdata
// directory.
func TestAssessFixturesUseOnlySyntheticAccountIDs(t *testing.T) {
	twelveDigits := regexp.MustCompile(`[0-9]{12}`)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "assess_test.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse assess_test.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, match := range twelveDigits.FindAllString(lit.Value, -1) {
			if !isSyntheticAccountIDForAssessTests(match) {
				t.Errorf("literal in assess_test.go contains a non-synthetic-looking twelve-digit sequence %q", match)
			}
		}
		return true
	})
}

func isSyntheticAccountIDForAssessTests(id string) bool {
	if id == "000000000000" {
		return true
	}
	for i := 1; i < len(id); i++ {
		if id[i] != id[0] {
			return false
		}
	}
	return true
}
