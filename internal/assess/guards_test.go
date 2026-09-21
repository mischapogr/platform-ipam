package assess

import "testing"

// readWhole is an input whose coverage is complete: the account list and the
// run record are both supplied, and every region the records name was read.
func readWhole(records ...ResourceRecord) Input {
	in := Input{Records: records, Run: &RunRecord{SourceFile: "run.json"}}
	seenAccount, seenRegion := map[string]bool{}, map[AccountRegion]bool{}
	for _, r := range records {
		if !seenAccount[r.AccountID] {
			seenAccount[r.AccountID] = true
			in.Accounts = append(in.Accounts, AccountRecord{AccountID: r.AccountID, Status: "ACTIVE"})
		}
		if ar := (AccountRegion{AccountID: r.AccountID, Region: r.Region}); !seenRegion[ar] {
			seenRegion[ar] = true
			in.Run.Attempts = append(in.Run.Attempts, RunAttempt{AccountID: r.AccountID, Region: r.Region, Outcome: AttemptSucceeded})
		}
	}
	return in
}

func assessed(t *testing.T, in Input, opts Options) Report {
	t.Helper()
	opts.Stamp = "2026-09-20T12:00:00Z"
	report, err := Assess(in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// Two associations of one VPC are one resource's own address space, whatever
// the rows say about them, and two fixed ranges are two statements a reviewer
// made: neither pair is a relationship between two VPCs.
func TestOnlyTwoDifferentVPCsOrAVPCAndAFixedRangeConflict(t *testing.T) {
	sameVPC := readWhole(
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.1.0/24", "networks.csv", 2))
	if report := assessed(t, sameVPC, Options{}); len(report.Conflicts) != 0 {
		t.Fatalf("one VPC conflicts with itself: %+v", report.Conflicts)
	}
	fixedOnly := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "172.16.0.0/16", "networks.csv", 1))
	report := assessed(t, fixedOnly, Options{Fixed: []FixedRange{{CIDR: "10.0.0.0/8"}, {CIDR: "10.1.0.0/16"}}})
	if len(report.Conflicts) != 0 || report.Summary.FixedRelationships != 0 {
		t.Fatalf("two fixed ranges conflict with each other: %+v", report.Conflicts)
	}
}

// A CIDR with host bits set is not a block anybody associated, and a fixed
// range written twice is one range.
func TestMalformedAndRepeatedRangesAreNotCompared(t *testing.T) {
	in := readWhole(
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.5/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2))
	report := assessed(t, in, Options{})
	if len(report.Conflicts) != 0 {
		t.Fatalf("a non-canonical CIDR was compared: %+v", report.Conflicts)
	}
	found := false
	for _, n := range report.Notes {
		found = found || (n.Kind == NoteInvalidCIDR && n.SourceRow == 1)
	}
	if !found {
		t.Fatalf("no %s note names row 1: %+v", NoteInvalidCIDR, report.Notes)
	}

	twice := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1))
	report = assessed(t, twice, Options{Fixed: []FixedRange{{CIDR: "10.0.0.0/8", Description: "a"}, {CIDR: "10.0.0.0/8", Description: "b"}}})
	if len(report.Conflicts) != 1 || report.Summary.FixedRelationships != 1 {
		t.Fatalf("a fixed range written twice gave %d conflicts, %d counted", len(report.Conflicts), report.Summary.FixedRelationships)
	}
}

// A describe failure in a region that returned no rows is a failed region. It
// is partial only when rows from it are in the table.
func TestARegionIsPartialOnlyWhenItAlsoReturnedRows(t *testing.T) {
	in := readWhole(vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1))
	in.Failures = []FailureRow{{AccountID: "111111111111", Region: "eu-west-1", Stage: StageDescribe, Error: "Throttling"}}
	report := assessed(t, in, Options{})
	if len(report.Coverage.Partial) != 0 || len(report.Coverage.Failed) != 1 {
		t.Fatalf("partial = %+v, failed = %+v", report.Coverage.Partial, report.Coverage.Failed)
	}
}

// Members of one group must communicate by definition, and a shared service
// must be reachable from everything that is assigned at all; neither needs a
// pair written out.
func TestOneGroupAndSharedServicesAreConfirmedWithoutAListedPair(t *testing.T) {
	in := readWhole(
		vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1),
		vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2))
	cases := map[string]Matrix{
		"one group":        {Version: 1, Groups: []Group{{ID: "prod", Members: []string{"111111111111", "222222222222"}}}},
		"a shared service": {Version: 1, Groups: []Group{{ID: "prod", Members: []string{"111111111111"}}, {ID: "dns", Members: []string{"222222222222"}}}, SharedServices: []string{"dns"}},
	}
	for name, matrix := range cases {
		t.Run(name, func(t *testing.T) {
			report := assessed(t, in, Options{Matrix: &matrix})
			if len(report.Conflicts) != 1 || report.Conflicts[0].Impact != ImpactConfirmed {
				t.Fatalf("conflicts = %+v", report.Conflicts)
			}
		})
	}
}

// Clean is what the command's exit status turns on: complete coverage and no
// confirmed conflict, and never one without the other.
func TestCleanNeedsCompleteCoverageAndNoConfirmedConflict(t *testing.T) {
	a := vpcRec("111111111111", "us-east-1", "vpc-a", "10.0.0.0/16", "networks.csv", 1)
	b := vpcRec("222222222222", "us-east-1", "vpc-b", "10.0.0.0/16", "networks.csv", 2)
	apart := vpcRec("222222222222", "us-east-1", "vpc-b", "10.9.0.0/16", "networks.csv", 2)
	together := Matrix{Version: 1, Groups: []Group{{ID: "prod", Members: []string{"111111111111", "222222222222"}}}}

	if report := assessed(t, readWhole(a, apart), Options{Matrix: &together}); !report.Clean() {
		t.Fatalf("complete coverage and no conflict is not clean: %+v", report.Coverage)
	}
	if report := assessed(t, readWhole(a, b), Options{Matrix: &together}); report.Clean() {
		t.Fatal("a confirmed conflict is clean")
	}
	if report := assessed(t, readWhole(a, b), Options{}); !report.Clean() {
		t.Fatal("a conflict of unknown impact alone must not fail the run; the report lists it")
	}
	unread := readWhole(a, apart)
	unread.Run = nil
	report := assessed(t, unread, Options{Matrix: &together})
	if report.Clean() {
		t.Fatal("incomplete coverage is clean")
	}
	noAccounts := readWhole(a, apart)
	noAccounts.Accounts = nil
	report = assessed(t, noAccounts, Options{})
	if report.Clean() {
		t.Fatal("coverage without an account list is clean")
	}
	found := false
	for _, limit := range report.InputLimits {
		found = found || limit == LimitExpectedAccountsUnknown
	}
	if !found {
		t.Fatalf("input_limits = %v, want %s", report.InputLimits, LimitExpectedAccountsUnknown)
	}
}
