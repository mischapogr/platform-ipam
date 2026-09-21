package migrate

import (
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

// TestFixturesUseOnlySyntheticAccountIDs is this package's own version of
// internal/assess/assess_test.go's TestFixturesUseOnlySyntheticAccountIDs
// (ADR 0015, "Determinism, the output and confidentiality": "the package
// repeats internal/assess's mechanical guard that no fixture contains a
// twelve-digit account id other than the all-zero one"). Every hardcoded
// twelve-digit string literal in this package's own _test.go sources must
// be the all-zero account or a repdigit (all twelve digits identical),
// exactly as assess's own guard defines "unambiguously synthetic on sight".
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

// --- shared test fixture builders ---

// vpcRec builds one synthetic assess.ResourceRecord VPC row.
func vpcRec(accountID, region, vpcID, cidr, sourceFile string, sourceRow int) assess.ResourceRecord {
	assoc := "assoc-" + vpcID
	obs := "2026-09-20T00:00:00Z"
	return assess.ResourceRecord{
		AccountID: accountID, Region: region, Type: assess.TypeVPC, ResourceID: vpcID, CIDR: cidr,
		AssociationID: &assoc, Primary: assess.TriTrue, ObservedAt: &obs,
		SourceFile: sourceFile, SourceRow: sourceRow,
	}
}

// readWhole builds an assess.Input whose coverage is complete for every
// account and region the given records name, mirroring
// internal/assess/guards_test.go's helper of the same name and purpose.
func readWhole(records ...assess.ResourceRecord) assess.Input {
	in := assess.Input{Records: records, Run: &assess.RunRecord{SourceFile: "run.json"}}
	seenAccount, seenRegion := map[string]bool{}, map[assess.AccountRegion]bool{}
	for _, r := range records {
		if !seenAccount[r.AccountID] {
			seenAccount[r.AccountID] = true
			in.Accounts = append(in.Accounts, assess.AccountRecord{AccountID: r.AccountID, Status: "ACTIVE"})
		}
		ar := assess.AccountRegion{AccountID: r.AccountID, Region: r.Region}
		if !seenRegion[ar] {
			seenRegion[ar] = true
			in.Run.Attempts = append(in.Run.Attempts, assess.RunAttempt{AccountID: r.AccountID, Region: r.Region, Outcome: assess.AttemptSucceeded})
		}
	}
	return in
}

// mustAssess runs internal/assess.Assess and fails the test on error.
func mustAssess(t *testing.T, in assess.Input, opts assess.Options) assess.Report {
	t.Helper()
	report, err := assess.Assess(in, opts)
	if err != nil {
		t.Fatalf("assess.Assess: %v", err)
	}
	return report
}

func strPtr(s string) *string { return &s }

func subject(accountID, region, vpcID string) Subject {
	return Subject{AccountID: accountID, Region: region, VPCID: vpcID}
}
