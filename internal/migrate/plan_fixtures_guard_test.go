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
)

// TestPlanFixturesUseOnlySyntheticAccountIDs is the same confidentiality
// guard internal/assess's TestFixturesUseOnlySyntheticAccountIDs applies
// (ADR 0014, "Confidentiality": "A test asserts that no fixture in the
// package contains a twelve-digit account id other than the all-zero one"),
// applied here because this package's own fixtures carry account ids too
// (a migration plan's Subject.AccountID and Target.AccountID). It follows
// the same narrowest reading internal/assess's own comment settles on:
// every hardcoded twelve-digit STRING LITERAL in this package's test
// sources must be the all-zero account or a repdigit (all twelve digits
// identical).
//
// Named and helper-named with a "Plan" prefix -- TestPlanFixturesUseOnly
// SyntheticAccountIDs and isSyntheticPlanAccountID, not internal/assess's
// bare TestFixturesUseOnlySyntheticAccountIDs and isSyntheticAccountID --
// for the same reason plan_imports_test.go's test and helper are
// "Plan"-prefixed: the lead's brief asks M3b2 to "add the same AST guard
// test internal/assess has" in this SAME package, and two independently
// written private copies cannot coordinate a shared name in advance. See
// the report to the lead.
func TestPlanFixturesUseOnlySyntheticAccountIDs(t *testing.T) {
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
				if !isSyntheticPlanAccountID(match) {
					t.Errorf("%s: literal %s contains a non-synthetic-looking twelve-digit sequence %q", name, lit.Value, match)
				}
			}
			return true
		})
	}
}

func isSyntheticPlanAccountID(id string) bool {
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
