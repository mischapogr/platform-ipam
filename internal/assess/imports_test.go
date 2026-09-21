package assess

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packageFiles parses every non-test .go file of dir, mirroring
// internal/service/adopt_test.go's helper of the same name and purpose.
func packageFiles(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatalf("no sources found in %s", dir)
	}
	return out
}

// TestImportsAreStandardLibraryOnly is ADR 0014's import-graph property:
// this package "imports nothing from internal/service, internal/netbox,
// internal/storage, internal/config, internal/cloud, internal/transport" --
// in fact nothing beyond the Go standard library at all, which is what makes
// "never calls NetBox or AWS" a property of the import graph rather than of
// care (package doc.go). A standard-library import path never contains a
// dot before its first slash; every non-standard import this codebase uses
// does (github.com/...), so that is the test.
func TestImportsAreStandardLibraryOnly(t *testing.T) {
	for name, f := range packageFiles(t, ".") {
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			first := path
			if i := strings.Index(path, "/"); i >= 0 {
				first = path[:i]
			}
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is not a standard-library package", name, path)
			}
		}
	}
}
