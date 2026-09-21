package assess

import (
	"strings"
	"testing"
)

// intp is a small helper: *int literal for RunAttempt.RowCount.
func intp(n int) *int { return &n }

// TestRowCountFewerThanRecordedIsIncomplete: run.json says a region produced
// 2 rows, only 1 is present -- a truncated or hand-filtered networks.csv
// beside an intact run record. This is docs/WORK_PLAN.md M1c's own words:
// "so a truncated or hand-filtered networks file beside an intact run
// record still reports complete: true" -- which this test proves no longer
// happens.
func TestRowCountFewerThanRecordedIsIncomplete(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(2)},
		}},
	}
	report := mustAssess(t, in, Options{})

	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false (1 row present, 2 recorded)")
	}
	if len(report.Coverage.RowCountShort) != 1 {
		t.Fatalf("RowCountShort = %+v, want exactly one entry", report.Coverage.RowCountShort)
	}
	got := report.Coverage.RowCountShort[0]
	if got.AccountID != "111111111111" || got.Region != "eu-central-1" || got.Recorded != 2 || got.Present != 1 {
		t.Fatalf("RowCountShort[0] = %+v, want {111111111111 eu-central-1 2 1}", got)
	}
	if len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("RowCountExceeded = %+v, want none", report.Coverage.RowCountExceeded)
	}
	// Counted in the summary's incomplete pairs, exactly like
	// Failed/Partial/NotAttempted (ADR 0014 M1c: "counted in the summary's
	// incomplete pairs").
	if report.Summary.IncompleteAccounts != 1 || report.Summary.IncompleteRegionPairs != 1 {
		t.Fatalf("IncompleteAccounts/IncompleteRegionPairs = %d/%d, want 1/1",
			report.Summary.IncompleteAccounts, report.Summary.IncompleteRegionPairs)
	}
	assertIncompleteTemplateChosen(t, report)
	assertNoForbiddenPhrase(t, report)
	// A shortfall is reported through the coverage list alone; it does not
	// also need a Note (only the surplus direction does).
	for _, n := range report.Notes {
		if n.Kind == NoteRowCountExceedsRecorded {
			t.Fatalf("a shortfall produced a %s note: %+v", NoteRowCountExceedsRecorded, n)
		}
	}
}

// TestRowCountMoreThanRecordedIsADataErrorNote: more rows are present than
// run.json's own row_count -- self-contradictory, reported as a note, not
// counted in the incomplete-pairs summary, but it still makes the report
// not complete (ADR 0014 M1c: "MORE rows than recorded is a data error
// worth a note and also not complete").
func TestRowCountMoreThanRecordedIsADataErrorNote(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records: []ResourceRecord{
			vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
			vpcRec("111111111111", "eu-central-1", "vpc-b", "10.1.0.0/16", "networks.csv", 2),
		},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(1)},
		}},
	}
	report := mustAssess(t, in, Options{})

	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false (2 rows present, 1 recorded)")
	}
	if len(report.Coverage.RowCountShort) != 0 {
		t.Fatalf("RowCountShort = %+v, want none", report.Coverage.RowCountShort)
	}
	if len(report.Coverage.RowCountExceeded) != 1 {
		t.Fatalf("RowCountExceeded = %+v, want exactly one entry", report.Coverage.RowCountExceeded)
	}
	got := report.Coverage.RowCountExceeded[0]
	if got.AccountID != "111111111111" || got.Region != "eu-central-1" || got.Recorded != 1 || got.Present != 2 {
		t.Fatalf("RowCountExceeded[0] = %+v, want {111111111111 eu-central-1 1 2}", got)
	}
	// NOT counted in the incomplete pairs (unlike the shortfall case): the
	// gap here is a data error, not an unread account/region.
	if report.Summary.IncompleteAccounts != 0 || report.Summary.IncompleteRegionPairs != 0 {
		t.Fatalf("IncompleteAccounts/IncompleteRegionPairs = %d/%d, want 0/0 (a surplus is not an incomplete-pairs gap)",
			report.Summary.IncompleteAccounts, report.Summary.IncompleteRegionPairs)
	}
	found := false
	for _, n := range report.Notes {
		if n.Kind == NoteRowCountExceedsRecorded {
			found = true
			if !strings.Contains(n.Message, "111111111111") || !strings.Contains(n.Message, "eu-central-1") {
				t.Errorf("note message %q does not name the account and region", n.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no %s note; notes = %+v", NoteRowCountExceedsRecorded, report.Notes)
	}
}

// TestRowCountMissingDegradesRatherThanGuessing: an older run.json
// (script_version 1) recorded no row_count at all for a succeeded attempt.
// ADR 0014's own rule -- "it never guesses" -- applies here exactly as it
// does to a missing association_id or observed_at column: no mismatch is
// reported, and input_limits names the degradation.
func TestRowCountMissingDegradesRatherThanGuessing(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded}, // no RowCount
		}},
	}
	report := mustAssess(t, in, Options{})

	if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("row count mismatch reported from a nil RowCount: short=%+v exceeded=%+v",
			report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
	}
	if !report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true: an absent row_count must not itself make the report incomplete")
	}
	found := false
	for _, l := range report.InputLimits {
		if l == LimitRowCountMissing {
			found = true
		}
	}
	if !found {
		t.Fatalf("InputLimits = %v, want %s", report.InputLimits, LimitRowCountMissing)
	}
}

// TestRowCountAcrossSeveralNetworksInputsSums: several --networks inputs
// contribute rows for the same account and region under one run record; the
// present count is their sum, not just the last file's.
func TestRowCountAcrossSeveralNetworksInputsSums(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records: []ResourceRecord{
			vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks-1.csv", 1),
			vpcRec("111111111111", "eu-central-1", "vpc-b", "10.1.0.0/16", "networks-2.csv", 1),
		},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(2)},
		}},
	}
	report := mustAssess(t, in, Options{})
	if !report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true: 1 row from each of two files sums to the recorded 2")
	}
	if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("unexpected mismatch: short=%+v exceeded=%+v", report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
	}
}

// TestRowCountSameFileTwiceDoesNotFakeASurplus: the identical physical row
// -- same source file, same source row -- read twice (the same --networks
// file named on the command line more than once) must not be counted
// twice: that would manufacture a surplus that was never actually
// collected. docs/WORK_PLAN.md M1c: "the SAME file passed twice ... must
// not fake a surplus."
func TestRowCountSameFileTwiceDoesNotFakeASurplus(t *testing.T) {
	row := vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{row, row}, // the very same file+row, read twice
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(1)},
		}},
	}
	report := mustAssess(t, in, Options{})
	if !report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true: the duplicated (file,row) must count once, not twice")
	}
	if len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("RowCountExceeded = %+v, want none: repeating one input must not fake a surplus", report.Coverage.RowCountExceeded)
	}

	// Contrast: two DIFFERENT rows in the SAME file both count -- a
	// duplicate row in one file was still a row the collector wrote.
	in2 := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records: []ResourceRecord{
			vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
			vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 2), // same CIDR, DIFFERENT row: a true duplicate observation, still a written row
		},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(2)},
		}},
	}
	report2 := mustAssess(t, in2, Options{})
	if !report2.Coverage.Complete {
		t.Fatalf("Coverage.Complete = false, want true: two distinct rows (even a duplicate observation) both count as present")
	}
	if report2.Summary.DuplicateObservations != 1 {
		t.Fatalf("DuplicateObservations = %d, want 1 (the engine's own dedup collapses the association, but the row count above must not)", report2.Summary.DuplicateObservations)
	}
}

// TestRowCountUnlistedAccountRegionIsSkipped: rows exist for an
// account/region the run record names no attempt for at all. There is no
// recorded row_count to compare against, so no mismatch is reported --
// this is a different question from whether that account/region was ever
// meant to be attempted, which the existing coverage lists already answer.
func TestRowCountUnlistedAccountRegionIsSkipped(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run:      &RunRecord{SourceFile: "run.json"}, // no attempts at all
	}
	report := mustAssess(t, in, Options{})
	if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("a row-count mismatch was reported for an account/region absent from run.json: short=%+v exceeded=%+v",
			report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
	}
}

// TestRowCountNoRunRecordUnchanged: with no run.json at all, the existing
// attempted-set-unknown/RunMissing behaviour is exactly what it was before
// this package; rowCountMismatches contributes nothing new.
func TestRowCountNoRunRecordUnchanged(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
	}
	report := mustAssess(t, in, Options{})
	if !report.Coverage.RunMissing {
		t.Fatal("RunMissing = false, want true")
	}
	if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
		t.Fatalf("row-count lists are non-empty with no run.json: short=%+v exceeded=%+v",
			report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
	}
	found := false
	for _, l := range report.InputLimits {
		if l == LimitRowCountMissing {
			found = true
		}
	}
	if found {
		t.Fatal("LimitRowCountMissing must not be named when run.json itself is absent (that is LimitAttemptedSetUnknown's job)")
	}
}

// TestRowCountPartialOutcomeIsAlsoCrossChecked: a partial region's own
// row_count is the VPC rows only (the subnet call failed), and it is
// cross-checked exactly like a succeeded one.
func TestRowCountPartialOutcomeIsAlsoCrossChecked(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Failures: []FailureRow{{AccountID: "111111111111", Region: "eu-central-1", Stage: StageDescribe, Error: "Throttling"}},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptPartial, Stage: "describe", RowCount: intp(3)},
		}},
	}
	report := mustAssess(t, in, Options{})
	if len(report.Coverage.RowCountShort) != 1 {
		t.Fatalf("RowCountShort = %+v, want one entry (1 present, 3 recorded)", report.Coverage.RowCountShort)
	}
}

// TestRowCountIgnoredOnFailedOrNotAttempted: row_count is meaningless for a
// failed or not_attempted outcome (the real collector never writes it
// there -- see scripts/aws/org-inventory.sh's scan_account, which only
// attaches row_count to the "succeeded" and "partial" branches), but a
// hand-edited or malformed run.json could carry one anyway. It must never
// be cross-checked: Failed/NotAttempted already name the gap through their
// own coverage lists, and comparing a row_count that was never meant to be
// authoritative would double-report or misreport it.
func TestRowCountIgnoredOnFailedOrNotAttempted(t *testing.T) {
	for _, outcome := range []AttemptOutcome{AttemptFailed, AttemptNotAttempted} {
		t.Run(string(outcome), func(t *testing.T) {
			in := Input{
				Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
				Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
					{AccountID: "111111111111", Region: "eu-central-1", Outcome: outcome, Stage: "describe", RowCount: intp(5)},
				}},
			}
			report := mustAssess(t, in, Options{})
			if len(report.Coverage.RowCountShort) != 0 || len(report.Coverage.RowCountExceeded) != 0 {
				t.Fatalf("outcome %s: row-count mismatch reported despite the attempt never having succeeded or been partial: short=%+v exceeded=%+v",
					outcome, report.Coverage.RowCountShort, report.Coverage.RowCountExceeded)
			}
		})
	}
}

// TestRowCountShortCountsAsImplicatedVPC: RowCountShort is a genuine
// coverage gap like Failed/Partial/NotAttempted, so a VPC whose
// account/region falls inside it is implicated too.
func TestRowCountShortCountsAsImplicatedVPC(t *testing.T) {
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records:  []ResourceRecord{vpcRec("111111111111", "eu-central-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)},
		Run: &RunRecord{SourceFile: "run.json", Attempts: []RunAttempt{
			{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded, RowCount: intp(2)},
		}},
	}
	report := mustAssess(t, in, Options{})
	if report.Summary.ImplicatedVPCs != 1 {
		t.Fatalf("ImplicatedVPCs = %d, want 1 (the one VPC present in the short account/region)", report.Summary.ImplicatedVPCs)
	}
}

// Records that name no source row are counted one by one: two of them are two
// rows, not one row seen twice.
func TestRowCountDoesNotFoldRecordsThatNameNoSource(t *testing.T) {
	two := 2
	in := Input{
		Accounts: []AccountRecord{{AccountID: "111111111111", Status: "ACTIVE"}},
		Records: []ResourceRecord{
			vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "", 0),
			vpcRec("111111111111", "us-east-1", "vpc-b", "10.1.0.0/16", "", 0),
		},
		Run: &RunRecord{Attempts: []RunAttempt{{AccountID: "111111111111", Region: "us-east-1", Outcome: AttemptSucceeded, RowCount: &two}}},
	}
	report, err := Assess(in, Options{Stamp: "2026-09-21T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Coverage.RowCountShort) != 0 || !report.Coverage.Complete {
		t.Fatalf("coverage = %+v", report.Coverage)
	}
}
