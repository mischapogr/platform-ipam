package assess

import (
	"strings"
	"testing"
)

// TestCoverageIncompleteRoutes is the table-driven test ADR 0014's evidence
// paragraph requires: "a table-driven test over every route to
// incompleteness -- a failed assume-role row, a failed describe row, a
// region that is both partial and populated, an ACTIVE account in
// accounts.json that was never attempted, and a missing run.json -- asserts
// coverage.complete is false, that the incomplete template was chosen, and
// that the output contains no member of the forbidden list."
func TestCoverageIncompleteRoutes(t *testing.T) {
	tests := []struct {
		name string
		in   Input
	}{
		{
			name: "failed assume-role row",
			in: Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
				Failures: []FailureRow{{AccountID: "111111111111", Stage: StageAssumeRole, Error: "AccessDenied", SourceFile: "failures.csv", SourceRow: 1}},
				Run:      &RunRecord{SourceFile: "run.json"},
			},
		},
		{
			name: "failed describe row, whole account otherwise empty",
			in: Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
				Failures: []FailureRow{{AccountID: "111111111111", Region: "us-east-1", Stage: StageDescribe, Error: "Throttling", SourceFile: "failures.csv", SourceRow: 1}},
				Run:      &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptFailed, Stage: "describe"}}},
			},
		},
		{
			name: "region both partial and populated",
			in: Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
				Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
				Failures: []FailureRow{{AccountID: "111111111111", Region: "us-east-1", Stage: StageDescribe, Error: "Throttling", SourceFile: "failures.csv", SourceRow: 1}},
				Run:      &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptFailed, Stage: "describe"}}},
			},
		},
		{
			name: "ACTIVE account never attempted",
			in: Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}, {AccountID: "222222222222", Status: "ACTIVE"}},
				Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
				Run:      &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}}},
			},
		},
		{
			name: "missing run.json",
			in: Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
				Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := mustAssess(t, tc.in, Options{})
			if report.Coverage.Complete {
				t.Fatalf("Coverage.Complete = true, want false")
			}
			assertIncompleteTemplateChosen(t, report)
			assertNoForbiddenPhrase(t, report)
		})
	}
}

func assertIncompleteTemplateChosen(t *testing.T, report Report) {
	t.Helper()
	if !strings.Contains(report.Summary.Sentence, "this report cannot make a complete statement for") {
		t.Errorf("sentence = %q, want the incomplete template's fixed substring", report.Summary.Sentence)
	}
	if strings.Contains(report.Summary.Sentence, "no conflicting relationship was observed in the scope read") {
		t.Errorf("sentence = %q, must never contain the complete template's sentence", report.Summary.Sentence)
	}
}

func assertNoForbiddenPhrase(t *testing.T, report Report) {
	t.Helper()
	var jsonBuf, textBuf strings.Builder
	if err := WriteReportJSON(&jsonBuf, report); err != nil {
		t.Fatalf("WriteReportJSON: %v", err)
	}
	if err := WriteReportText(&textBuf, report); err != nil {
		t.Fatalf("WriteReportText: %v", err)
	}
	for _, phrase := range ForbiddenCompletePhrases() {
		if strings.Contains(strings.ToLower(jsonBuf.String()), phrase) {
			t.Errorf("JSON output contains forbidden phrase %q", phrase)
		}
		if strings.Contains(strings.ToLower(textBuf.String()), phrase) {
			t.Errorf("text output contains forbidden phrase %q", phrase)
		}
	}
}

// TestForbiddenPhraseDenyListAcrossGeneratedIncompleteReports generates many
// incomplete-coverage reports over the estate generator and scans every one,
// rather than relying on a single hand-picked example.
func TestForbiddenPhraseDenyListAcrossGeneratedIncompleteReports(t *testing.T) {
	for seed := int64(0); seed < 15; seed++ {
		in := genEstate(estateParams{
			accounts: 6, regionsPerAccount: 2, vpcsPerAccountRegion: 3, subnetsPerVPC: 2,
			conflictFraction: 0.3, unreadAccounts: 1, partialRegions: 1, includeRun: true, seed: seed,
		})
		report := mustAssess(t, in, Options{})
		if report.Coverage.Complete {
			t.Fatalf("seed %d: Coverage.Complete = true, want false (estate has an unread account)", seed)
		}
		assertIncompleteTemplateChosen(t, report)
		assertNoForbiddenPhrase(t, report)
	}
}

// TestReadEmptyDistinguishedFromNotAttempted proves "read and empty" is
// told apart from "not read": an account-region with a successful attempt
// and zero rows lands in ReadEmpty, while one with no attempt at all lands
// in NotAttempted, even though both contribute zero ResourceRecords.
func TestReadEmptyDistinguishedFromNotAttempted(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}, {AccountID: "222222222222", Status: "ACTIVE"}},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}, // read, found nothing
			// 222222222222 has no attempt recorded at all: not attempted.
		}},
	}
	report := mustAssess(t, in, Options{})

	foundReadEmpty := false
	for _, e := range report.Coverage.ReadEmpty {
		if e.AccountID == "111111111111" && e.Region == "us-east-1" {
			foundReadEmpty = true
		}
	}
	if !foundReadEmpty {
		t.Errorf("ReadEmpty = %+v, want 111111111111/us-east-1", report.Coverage.ReadEmpty)
	}
	foundNotAttempted := false
	for _, e := range report.Coverage.NotAttempted {
		if e.AccountID == "222222222222" {
			foundNotAttempted = true
		}
	}
	if !foundNotAttempted {
		t.Errorf("NotAttempted = %+v, want 222222222222", report.Coverage.NotAttempted)
	}
}

// TestCompleteCoverageProducesCompleteTemplate is the mirror case: a fully
// read estate with no failures selects the complete template and the
// mechanical Complete field is true.
func TestCompleteCoverageProducesCompleteTemplate(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run:      &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}}},
	}
	report := mustAssess(t, in, Options{})
	if !report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true")
	}
	if !strings.Contains(report.Summary.Sentence, "no conflicting relationship was observed in the scope read") {
		t.Errorf("sentence = %q, want the complete template (no conflicts in this fixture)", report.Summary.Sentence)
	}
}

// TestInactiveAccountNotRequiredForCompleteness: accounts.json's ACTIVE
// filter means an INACTIVE account's absence from run.json never blocks
// completeness.
func TestInactiveAccountNotRequiredForCompleteness(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}, {AccountID: "999999999999", Status: "SUSPENDED"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run:      &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}}},
	}
	report := mustAssess(t, in, Options{})
	if !report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true: a SUSPENDED account is not in the expected-account set")
	}
}

// failures.csv and accounts.json are optional inputs, and the run record is
// evidence in its own right. A region the run record calls failed or partial is
// not a region that was read, whatever else was or was not supplied, and
// without the account list nobody can name an account the run never visited.
// Each of these reported complete before the run record's own outcomes were
// read.
func TestCoverageIsNeverCompleteOnTheRunRecordsOwnEvidence(t *testing.T) {
	accounts := []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}}
	records := []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)}
	run := func(outcome AttemptOutcome) *RunRecord {
		return &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded},
			{AccountID: "111111111111", Region: "eu-west-1", Outcome: outcome, Stage: "describe"},
		}}
	}
	tests := map[string]struct {
		in    Input
		check func(t *testing.T, c Coverage)
	}{
		"a failed region and no failures file": {Input{Accounts: accounts, Records: records, Run: run(AttemptFailed)}, func(t *testing.T, c Coverage) {
			if len(c.Failed) != 1 || c.Failed[0].Region != "eu-west-1" || c.Failed[0].Stage != StageDescribe {
				t.Fatalf("failed = %+v", c.Failed)
			}
		}},
		"a partial region and no failures file": {Input{Accounts: accounts, Records: records, Run: run(AttemptPartial)}, func(t *testing.T, c Coverage) {
			if len(c.Partial) != 1 || c.Partial[0].Region != "eu-west-1" {
				t.Fatalf("partial = %+v", c.Partial)
			}
		}},
		"an outcome this version does not know": {Input{Accounts: accounts, Records: records, Run: run("skipped")}, func(t *testing.T, c Coverage) {
			if len(c.Failed) != 1 || !strings.Contains(c.Failed[0].Error, `"skipped"`) {
				t.Fatalf("failed = %+v", c.Failed)
			}
		}},
		"no account list": {Input{Records: records, Run: run(AttemptSucceeded)}, func(t *testing.T, c Coverage) {
			if !c.AccountsMissing {
				t.Fatal("accounts_missing is false with no account list")
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := Assess(tc.in, Options{Stamp: "2026-09-20T12:00:00Z"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Coverage.Complete {
				t.Fatalf("coverage is complete: %+v", report.Coverage)
			}
			tc.check(t, report.Coverage)
			if !strings.Contains(report.Summary.Sentence, "cannot make a complete statement") {
				t.Fatalf("the complete template was chosen: %q", report.Summary.Sentence)
			}
		})
	}

	// A failure row and the run record naming the same region are one failure.
	both := Input{Accounts: accounts, Records: records, Run: run(AttemptFailed),
		Failures: []FailureRow{{AccountID: "111111111111", Region: "eu-west-1", Stage: StageDescribe, Error: "Throttling"}}}
	report, err := Assess(both, Options{Stamp: "2026-09-20T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Coverage.Failed) != 1 || report.Coverage.Failed[0].Error != "Throttling" {
		t.Fatalf("failed = %+v", report.Coverage.Failed)
	}

	// And the complete case still is: everything supplied, everything read.
	whole := Input{Accounts: accounts, Records: records, Run: &RunRecord{Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}}}}
	if report, err = Assess(whole, Options{Stamp: "2026-09-20T12:00:00Z"}); err != nil || !report.Coverage.Complete {
		t.Fatalf("complete = %v, err = %v: %+v", report.Coverage.Complete, err, report.Coverage)
	}
	for _, limit := range report.InputLimits {
		if limit == LimitExpectedAccountsUnknown {
			t.Fatal("the account list was supplied and the report says it was not")
		}
	}
}

// The order of the --fixed table's rows is not evidence. A conflict against a
// fixed range keeps its id, and the report its bytes, whichever order the rows
// were written in.
func TestFixedRangeOrderDoesNotChangeTheReport(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run:      &RunRecord{Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded}}},
	}
	a := FixedRange{CIDR: "10.0.0.0/8", Description: "on-premises", SourceFile: "fixed.json", SourceRow: 1}
	b := FixedRange{CIDR: "10.0.0.0/16", Description: "partner", SourceFile: "fixed.json", SourceRow: 2}
	render := func(fixed []FixedRange) string {
		report, err := Assess(in, Options{Stamp: "2026-09-20T12:00:00Z", Fixed: fixed})
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := WriteReportJSON(&out, report); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	b2 := b
	b2.SourceRow, a.SourceRow = 1, 2
	first, second := render([]FixedRange{{CIDR: a.CIDR, Description: a.Description, SourceFile: a.SourceFile, SourceRow: 1}, b}), render([]FixedRange{b2, a})
	strip := func(s string) string {
		return strings.NewReplacer(`"source_row": 1`, "", `"source_row": 2`, "").Replace(s)
	}
	if strip(first) != strip(second) {
		t.Fatalf("the order of the fixed rows changed the report:\n%s\n---\n%s", first, second)
	}
}
