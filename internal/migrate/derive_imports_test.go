package migrate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// myOwnNonTestSourceFileNames lists the file-name prefixes package M3b2
// owns within internal/migrate (the lead's task brief: "in files that
// cannot collide with M3b1's (derive*.go, evidence*.go, report*.go and
// their tests; M3b1 owns plan*.go)"). internal/migrate is a SHARED
// directory once M3b1 and M3b2 both land: a naive "every non-test .go file
// in this directory" scan would wrongly also police M3b1's own files (whose
// decoder legitimately imports the repository's YAML library), so this
// package's own import-graph guard must filter to files it owns before
// parsing, not just skip _test.go files the way
// internal/assess/imports_test.go's single-owner packageFiles can.
var myOwnNonTestSourceFileNames = []string{"derive", "evidence", "report"}

func isMyOwnNonTestSourceFile(name string) bool {
	if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
		return false
	}
	for _, prefix := range myOwnNonTestSourceFileNames {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// myOwnPackageFiles parses every non-test .go file THIS package (M3b2) owns
// in dir, by the file-name convention above -- unlike
// internal/assess/imports_test.go's packageFiles, which may assume it owns
// every non-test file in its directory because internal/assess has no
// sibling package sharing its directory.
func myOwnPackageFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isMyOwnNonTestSourceFile(name) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("no sources found in %s matching %v", dir, myOwnNonTestSourceFileNames)
	}
	return out
}

// TestNonTestSourceImportsAssessAndStandardLibraryOnly is package M3b2's own
// version of ADR 0014's import-graph property
// (internal/assess/imports_test.go's TestImportsAreStandardLibraryOnly),
// widened by exactly one path: the lead's task brief for this package
// states "it may import internal/assess and the standard library and
// NOTHING else from this module -- a source-parsing test asserts it."
//
// This scans only THIS PACKAGE'S OWN non-test .go files (myOwnPackageFiles
// above), never M3b1's plan*.go, which legitimately imports the
// repository's YAML library for its decoder -- a constraint that is this
// package's own, not the whole shared internal/migrate directory's. Test
// files are not scanned here and are free to import
// internal/assess/estategen for the end-to-end fixture ADR 0015's evidence
// paragraph names ("finally end to end, from
// internal/assess/estategen's generated estate") -- the record's own
// precedent (TestImportsAreStandardLibraryOnly,
// TestAssessSourceImportsExcludeAdapterPackages) scopes this guard to
// production source, not tests; see the report to the lead for this
// reading.
func TestNonTestSourceImportsAssessAndStandardLibraryOnly(t *testing.T) {
	const allowedModulePath = "github.com/mischapogr/platform-ipam/internal/assess"
	for name, f := range myOwnPackageFiles(t, ".") {
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == allowedModulePath {
				continue
			}
			first := path
			if i := strings.Index(path, "/"); i >= 0 {
				first = path[:i]
			}
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is neither the standard library nor %q", name, path, allowedModulePath)
			}
		}
	}
}
