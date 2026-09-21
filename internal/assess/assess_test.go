package assess

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCollectorFixtureScenario replays, at the Input level, the exact
// end-to-end scenario ADR 0014's evidence paragraph names for
// tests/aws/test_org_inventory.sh: account 111111111111 has one VPC with an
// associated 10.0.0.0/16 and 10.1.0.0/16, one subnet, and a disassociated
// range that is filtered out (so it never reaches this package's Input at
// all -- the collector filters CidrBlockState.State == "associated" before
// this package ever sees a row), while account 222222222222 fails
// assume-role. This package owns none of the CSV/bash machinery -- that is
// M1b1's and M1b3's -- so this test builds the Input a correct mapping
// would produce and checks this package's half of the promise: zero
// conflicts (the two ranges belong to one VPC), coverage incomplete because
// of the failed account, and the incomplete statement rendered. It then
// adds one synthetic second account that also holds 10.0.0.0/16 and checks
// that produces exactly one equal-cidr conflict at impact unknown, matching
// the record's own worked example word for word.
func TestCollectorFixtureScenario(t *testing.T) {
	assoc1, assoc2 := "vpc-cidr-assoc-aaa", "vpc-cidr-assoc-bbb"
	obsAt := "2026-09-20T00:00:00Z"
	baseRecords := []ResourceRecord{
		{AccountID: "111111111111", AccountName: "one", Region: "eu-central-1", Type: TypeVPC, ResourceID: "vpc-0000000000000001", CIDR: "10.0.0.0/16",
			AssociationID: &assoc1, Primary: TriTrue, ObservedAt: &obsAt, SourceFile: "networks.csv", SourceRow: 1},
		{AccountID: "111111111111", AccountName: "one", Region: "eu-central-1", Type: TypeVPC, ResourceID: "vpc-0000000000000001", CIDR: "10.1.0.0/16",
			AssociationID: &assoc2, Primary: TriFalse, ObservedAt: &obsAt, SourceFile: "networks.csv", SourceRow: 2},
		{AccountID: "111111111111", AccountName: "one", Region: "eu-central-1", Type: TypeSubnet, ResourceID: "subnet-0000000000000001", CIDR: "10.0.1.0/24", ParentID: "vpc-0000000000000001",
			ObservedAt: &obsAt, SourceFile: "networks.csv", SourceRow: 3},
		// The disassociated range never appears here: the collector already
		// filtered it before this package's Input existed.
	}
	failures := []FailureRow{
		{AccountID: "222222222222", AccountName: "two", Stage: StageAssumeRole, Error: "got condition check failed", SourceFile: "failures.csv", SourceRow: 1},
	}
	accounts := []AccountRecord{
		{AccountID: "111111111111", Name: "one", Status: "ACTIVE"},
		{AccountID: "222222222222", Name: "two", Status: "ACTIVE"},
	}
	run := &RunRecord{StartedAt: "2026-09-20T00:00:00Z", FinishedAt: "2026-09-20T00:01:00Z", RoleName: "PlatformIpamReadOnly", SourceFile: "run.json",
		Attempts: []RunAttempt{{AccountID: "111111111111", Region: "eu-central-1", Outcome: AttemptSucceeded}}}

	in := Input{Records: baseRecords, Failures: failures, Accounts: accounts, Run: run}
	report := mustAssess(t, in, Options{})

	if len(report.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want zero: the two ranges belong to one VPC", report.Conflicts)
	}
	if report.Coverage.Complete {
		t.Fatalf("Coverage.Complete = true, want false: account 222222222222 failed assume-role")
	}
	assertIncompleteTemplateChosen(t, report)
	assertNoForbiddenPhrase(t, report)

	// Add a synthetic second account that also holds 10.0.0.0/16.
	extended := in
	extendedAssoc := "vpc-cidr-assoc-ccc"
	extended.Records = append(append([]ResourceRecord{}, baseRecords...), ResourceRecord{
		AccountID: "333333333333", AccountName: "three", Region: "eu-central-1", Type: TypeVPC, ResourceID: "vpc-0000000000000099", CIDR: "10.0.0.0/16",
		AssociationID: &extendedAssoc, Primary: TriTrue, ObservedAt: &obsAt, SourceFile: "networks.csv", SourceRow: 4,
	})
	extended.Accounts = append(append([]AccountRecord{}, accounts...), AccountRecord{AccountID: "333333333333", Name: "three", Status: "ACTIVE"})
	extendedRun := *run
	extendedRun.Attempts = append(append([]RunAttempt{}, run.Attempts...), RunAttempt{AccountID: "333333333333", Region: "eu-central-1", Outcome: AttemptSucceeded})
	extended.Run = &extendedRun

	extReport := mustAssess(t, extended, Options{})
	var equalCIDR []Conflict
	for _, c := range extReport.Conflicts {
		if c.Kind == KindEqualCIDR {
			equalCIDR = append(equalCIDR, c)
		}
	}
	if len(equalCIDR) != 1 {
		t.Fatalf("equal-cidr conflicts = %+v, want exactly one (vpc-...0001 and vpc-...0099 both hold 10.0.0.0/16)", equalCIDR)
	}
	if equalCIDR[0].Impact != ImpactUnknown {
		t.Errorf("Impact = %q, want unknown (no matrix supplied)", equalCIDR[0].Impact)
	}
}

// TestFixturesUseOnlySyntheticAccountIDs is ADR 0014's confidentiality
// guard: "A test asserts that no fixture in the package contains a
// twelve-digit account id other than the all-zero one." This package's own
// fixtures (unlike the record's smallest example) need more than one
// synthetic account to exercise "two VPCs in different accounts" and the
// hundred-account scale bound, so the narrowest reading that both satisfies
// the guard's purpose -- catching a real inventory pasted in as a
// convenient test case -- and matches every other test package in this
// repository's own convention (internal/onboard, internal/onboardcmd,
// internal/netbox: see e.g. internal/onboardcmd/drift_test.go) is: every
// hardcoded twelve-digit STRING LITERAL in this package's test sources must
// be the all-zero account or a repdigit (all twelve digits identical), both
// of which are unambiguously synthetic on sight. A computed id (this
// package's own estate generator derives up to 200 distinct account ids
// arithmetically from a literal base) is not a copy-pasted real account,
// so only literal strings are scanned. This is a decision the record's
// smaller worked example did not have to make; see the package-level
// report to the lead.
func TestFixturesUseOnlySyntheticAccountIDs(t *testing.T) {
	twelveDigits := regexp.MustCompile(`[0-9]{12}`)
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read .: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, match := range twelveDigits.FindAllString(lit.Value, -1) {
				if !isSyntheticAccountID(match) {
					t.Errorf("%s: literal %s contains a non-synthetic-looking twelve-digit sequence %q", name, lit.Value, match)
				}
			}
			return true
		})
	}
}

func isSyntheticAccountID(id string) bool {
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
