package assess

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

func renderBoth(t *testing.T, report Report) (jsonBytes, textBytes []byte) {
	t.Helper()
	var jb, tb bytes.Buffer
	if err := WriteReportJSON(&jb, report); err != nil {
		t.Fatalf("WriteReportJSON: %v", err)
	}
	if err := WriteReportText(&tb, report); err != nil {
		t.Fatalf("WriteReportText: %v", err)
	}
	return jb.Bytes(), tb.Bytes()
}

// TestDeterminismAcrossRuns: two runs over one input are byte-identical, in
// both encodings.
func TestDeterminismAcrossRuns(t *testing.T) {
	in := genEstate(estateParams{accounts: 8, regionsPerAccount: 2, vpcsPerAccountRegion: 4, subnetsPerVPC: 3, conflictFraction: 0.3, unreadAccounts: 1, partialRegions: 1, includeRun: true, seed: 3})
	opts := Options{InputFiles: []InputFile{{Path: "networks.csv", Content: []byte("x")}}}

	r1 := mustAssess(t, in, opts)
	r2 := mustAssess(t, in, opts)
	j1, t1 := renderBoth(t, r1)
	j2, t2 := renderBoth(t, r2)
	if !bytes.Equal(j1, j2) {
		t.Errorf("JSON output differs across two runs over the same input")
	}
	if !bytes.Equal(t1, t2) {
		t.Errorf("text output differs across two runs over the same input")
	}
}

// TestDeterminismAcrossInputOrder: a run over the same input with its data
// rows shuffled produces the same report, except for the row numbers inside
// each side, which must name the rows the records now occupy (ADR 0014's
// own wording for this property).
func TestDeterminismAcrossInputOrder(t *testing.T) {
	in := genEstate(estateParams{accounts: 8, regionsPerAccount: 2, vpcsPerAccountRegion: 4, subnetsPerVPC: 3, conflictFraction: 0.3, unreadAccounts: 1, partialRegions: 1, includeRun: true, seed: 4})

	base := mustAssess(t, in, Options{})

	rng := rand.New(rand.NewSource(9))
	for trial := 0; trial < 5; trial++ {
		shuffled := shuffleInputPreservingRowNumbers(rng, in)
		got := mustAssess(t, shuffled, Options{})

		if got.Summary.TotalRelationships != base.Summary.TotalRelationships {
			t.Fatalf("trial %d: TotalRelationships = %d, want %d", trial, got.Summary.TotalRelationships, base.Summary.TotalRelationships)
		}
		if len(got.Conflicts) != len(base.Conflicts) {
			t.Fatalf("trial %d: %d conflicts, want %d", trial, len(got.Conflicts), len(base.Conflicts))
		}
		for i := range base.Conflicts {
			if got.Conflicts[i].ID != base.Conflicts[i].ID {
				t.Fatalf("trial %d: conflict[%d].ID = %s, want %s (order must not depend on input row order)", trial, i, got.Conflicts[i].ID, base.Conflicts[i].ID)
			}
		}
	}
}

// shuffleInputPreservingRowNumbers permutes the order records/failures
// appear in Input, without changing any record's own SourceFile/SourceRow --
// exactly "shuffled inputs" in the sense ADR 0014 means (the row numbers
// name the rows the records occupy in their own file; only the order the
// records were handed to Assess in changes).
func shuffleInputPreservingRowNumbers(rng *rand.Rand, in Input) Input {
	records := append([]ResourceRecord(nil), in.Records...)
	rng.Shuffle(len(records), func(i, j int) { records[i], records[j] = records[j], records[i] })
	failures := append([]FailureRow(nil), in.Failures...)
	rng.Shuffle(len(failures), func(i, j int) { failures[i], failures[j] = failures[j], failures[i] })
	accounts := append([]AccountRecord(nil), in.Accounts...)
	rng.Shuffle(len(accounts), func(i, j int) { accounts[i], accounts[j] = accounts[j], accounts[i] })
	var run *RunRecord
	if in.Run != nil {
		r := *in.Run
		r.Attempts = append([]RunAttempt(nil), in.Run.Attempts...)
		rng.Shuffle(len(r.Attempts), func(i, j int) { r.Attempts[i], r.Attempts[j] = r.Attempts[j], r.Attempts[i] })
		run = &r
	}
	return Input{Records: records, Failures: failures, Accounts: accounts, Run: run}
}

// TestConflictIDStableAcrossUnrelatedChanges: a conflict's id is unchanged
// when an unrelated VPC is added, when a matrix is supplied, when ownership
// is supplied, and when the observation time changes.
func TestConflictIDStableAcrossUnrelatedChanges(t *testing.T) {
	base := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	}}
	baseReport := mustAssess(t, base, Options{})
	if len(baseReport.Conflicts) != 1 {
		t.Fatalf("baseline conflicts = %+v, want exactly one", baseReport.Conflicts)
	}
	wantID := baseReport.Conflicts[0].ID

	t.Run("unrelated VPC added", func(t *testing.T) {
		in := base
		in.Records = append(append([]ResourceRecord{}, base.Records...),
			vpcRec("333333333333", "us-west-2", "vpc-c", "172.16.0.0/16", "networks.csv", 3))
		report := mustAssess(t, in, Options{})
		if c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b"); c == nil || c.ID != wantID {
			t.Errorf("got %+v, want id %s unchanged", c, wantID)
		}
	})

	t.Run("matrix supplied", func(t *testing.T) {
		matrix := Matrix{Version: 1, Groups: []Group{{ID: "g", Members: []string{"111111111111", "222222222222"}}}}
		report := mustAssess(t, base, Options{Matrix: &matrix})
		if c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b"); c == nil || c.ID != wantID {
			t.Errorf("got %+v, want id %s unchanged", c, wantID)
		}
	})

	t.Run("ownership supplied", func(t *testing.T) {
		ownership := Ownership{Entries: []OwnershipEntry{{AccountID: "111111111111", Product: "widgets", Environment: "prod", Owner: "team-a"}}}
		report := mustAssess(t, base, Options{Ownership: ownership})
		if c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b"); c == nil || c.ID != wantID {
			t.Errorf("got %+v, want id %s unchanged", c, wantID)
		}
	})

	t.Run("observation time changed", func(t *testing.T) {
		in := base
		obsAt := "2026-01-01T00:00:00Z"
		in.Records = append([]ResourceRecord{}, base.Records...)
		in.Records[0].ObservedAt = &obsAt
		report := mustAssess(t, in, Options{})
		if c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b"); c == nil || c.ID != wantID {
			t.Errorf("got %+v, want id %s unchanged", c, wantID)
		}
	})
}

// TestConflictIDOrderIndependent: ConflictID(k, a, b) == ConflictID(k, b, a).
func TestConflictIDOrderIndependent(t *testing.T) {
	a, b := "aws:111111111111:us-east-1:vpc-a:10.0.0.0/16", "aws:222222222222:us-east-1:vpc-b:10.0.0.0/16"
	if ConflictID(KindEqualCIDR, a, b) != ConflictID(KindEqualCIDR, b, a) {
		t.Errorf("ConflictID is order-dependent")
	}
}

// TestConflictsAreTraceableToTheirSourceRecords is the head property named
// first in ADR 0014's evidence paragraph: a test reads the produced report,
// opens each side's named file at its named row, and asserts that the row
// yields that resource id and that CIDR. Here "opening a file at a row"
// means looking the (file, row) pair up in the original Input, since this
// package never reads a file itself -- the property under test is that the
// Side's SourceFile/SourceRow correctly name the record that produced it,
// and that the relationship can be re-derived from those two records alone.
func TestConflictsAreTraceableToTheirSourceRecords(t *testing.T) {
	in := genEstate(estateParams{accounts: 6, regionsPerAccount: 2, vpcsPerAccountRegion: 3, subnetsPerVPC: 2, conflictFraction: 0.4, includeRun: true, seed: 11})
	report := mustAssess(t, in, Options{})
	if len(report.Conflicts) == 0 {
		t.Fatalf("expected at least one conflict in the generated estate")
	}

	bySourceRow := map[int]ResourceRecord{}
	for _, r := range in.Records {
		if r.Type == TypeVPC {
			bySourceRow[r.SourceRow] = r
		}
	}

	for _, c := range report.Conflicts {
		if len(c.Sides) != 2 {
			t.Fatalf("conflict %s has %d sides", c.ID, len(c.Sides))
		}
		var resolved [2]ResourceRecord
		for i, s := range c.Sides {
			rec, ok := bySourceRow[s.SourceRow]
			if !ok || rec.SourceFile != s.SourceFile {
				t.Fatalf("conflict %s side %d names %s:%d, which does not resolve to an input record", c.ID, i, s.SourceFile, s.SourceRow)
			}
			if rec.ResourceID != s.VPCID || rec.CIDR != s.CIDR {
				t.Fatalf("conflict %s side %d: source record is %+v, does not match side %+v", c.ID, i, rec, s)
			}
			resolved[i] = rec
		}
		// Re-derive the relationship from the two records alone.
		gotKind := rederiveKind(t, resolved[0], resolved[1])
		if gotKind != c.Kind {
			t.Errorf("conflict %s: re-derived kind %s from source records, report says %s", c.ID, gotKind, c.Kind)
		}
	}
}

func rederiveKind(t *testing.T, a, b ResourceRecord) Kind {
	t.Helper()
	as, ae, ok1 := cidrInterval(a.CIDR)
	bs, be, ok2 := cidrInterval(b.CIDR)
	if !ok1 || !ok2 {
		t.Fatalf("could not parse CIDRs %s / %s", a.CIDR, b.CIDR)
	}
	if as == bs && ae == be {
		return KindEqualCIDR
	}
	return KindContains
}

// TestRenumberedRowsChangeTraceability: a mutation that renumbers the input
// rows must change the numbers a Conflict's sides report, so the test above
// cannot pass against fabricated provenance.
func TestRenumberedRowsChangeTraceability(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	c := findConflict(t, report, KindEqualCIDR, "vpc-a", "vpc-b")
	if c == nil {
		t.Fatalf("no conflict found")
	}
	originalRows := map[int]bool{c.Sides[0].SourceRow: true, c.Sides[1].SourceRow: true}

	renumbered := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 41),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 42),
	}}
	report2 := mustAssess(t, renumbered, Options{})
	c2 := findConflict(t, report2, KindEqualCIDR, "vpc-a", "vpc-b")
	if c2 == nil {
		t.Fatalf("no conflict found after renumbering")
	}
	if originalRows[c2.Sides[0].SourceRow] || originalRows[c2.Sides[1].SourceRow] {
		t.Errorf("row numbers did not change after renumbering the input: %+v", c2.Sides)
	}
}

// TestMissingRequiredFieldRefusesTheReport covers ADR 0014's refusal list:
// cidr, account_id, region, type, resource_id.
func TestMissingRequiredFieldRefusesTheReport(t *testing.T) {
	base := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	fields := []struct {
		name   string
		break_ func(r *ResourceRecord)
	}{
		{"cidr", func(r *ResourceRecord) { r.CIDR = "" }},
		{"account_id", func(r *ResourceRecord) { r.AccountID = "" }},
		{"region", func(r *ResourceRecord) { r.Region = "" }},
		{"type", func(r *ResourceRecord) { r.Type = "" }},
		{"resource_id", func(r *ResourceRecord) { r.ResourceID = "" }},
	}
	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			r := base
			f.break_(&r)
			_, err := Assess(Input{Records: []ResourceRecord{r}}, Options{})
			if err == nil {
				t.Fatalf("Assess succeeded with %s missing, want a refusal error", f.name)
			}
			mfe, ok := err.(*MissingFieldError)
			if !ok {
				t.Fatalf("error = %v (%T), want *MissingFieldError", err, err)
			}
			if mfe.Field != f.name {
				t.Errorf("MissingFieldError.Field = %q, want %q", mfe.Field, f.name)
			}
		})
	}
}

// TestMissingAssociationIDDegrades checks the input_limits/null-side
// behaviour: missing association ids degrade, they never refuse, and the
// limit is named.
func TestMissingAssociationIDDegrades(t *testing.T) {
	in := Input{Records: []ResourceRecord{
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2),
	}}
	report := mustAssess(t, in, Options{})
	found := false
	for _, l := range report.InputLimits {
		if l == LimitAssociationIDMissing {
			found = true
		}
	}
	if !found {
		t.Errorf("InputLimits = %v, want %q", report.InputLimits, LimitAssociationIDMissing)
	}
	for _, c := range report.Conflicts {
		for _, s := range c.Sides {
			if s.AssociationID != nil {
				t.Errorf("side %+v has a non-nil AssociationID, want nil (never guessed)", s)
			}
		}
	}
}

// TestTooManyRelationshipsRefuses exercises --max-relationships.
func TestTooManyRelationshipsRefuses(t *testing.T) {
	var records []ResourceRecord
	for i := 0; i < 30; i++ {
		records = append(records, vpcRec(accountIDFor(i), "us-east-1", vpcIDFor(i), "10.0.0.0/16", "networks.csv", i+1))
	}
	// 30 VPCs sharing one CIDR = C(30,2) = 435 pairwise relationships.
	_, err := Assess(Input{Records: records}, Options{MaxRelationships: 100})
	if err == nil {
		t.Fatalf("Assess succeeded, want a refusal: 435 relationships exceeds the cap of 100")
	}
	tmr, ok := err.(*TooManyRelationshipsError)
	if !ok {
		t.Fatalf("error = %v (%T), want *TooManyRelationshipsError", err, err)
	}
	if tmr.Count != 435 {
		t.Errorf("Count = %d, want 435", tmr.Count)
	}
}

func accountIDFor(i int) string { return padAccount(i) }
func vpcIDFor(i int) string     { return "vpc-" + padAccount(i) }

func padAccount(i int) string {
	s := "000000000000" + itoa(i)
	return s[len(s)-12:]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestReachabilityDisclaimerAlwaysPresentAndNeverContradicted.
func TestReachabilityDisclaimerAlwaysPresentAndNeverContradicted(t *testing.T) {
	report := mustAssess(t, Input{}, Options{})
	if len(report.Notes) == 0 || report.Notes[0].Kind != NoteReachability {
		t.Fatalf("Notes[0] = %+v, want the reachability disclaimer first", report.Notes)
	}
	forbidden := []string{"reachable", "unreachable", " ready"}
	j, tx := renderBoth(t, report)
	for _, phrase := range forbidden {
		if strings.Contains(strings.ToLower(string(j)), phrase) {
			t.Errorf("JSON output contains %q", phrase)
		}
		if strings.Contains(strings.ToLower(string(tx)), phrase) {
			t.Errorf("text output contains %q", phrase)
		}
	}
}
