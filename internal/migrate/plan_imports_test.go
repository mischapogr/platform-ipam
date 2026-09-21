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

// planPackageFiles parses every non-test .go file of dir, mirroring
// internal/assess/imports_test.go's packageFiles helper of the same
// purpose. Named with a "plan" prefix (unlike assess's own "packageFiles")
// because this package will gain more files from other work-plan packages
// (M3b2, M3b3) that may want a helper of their own with the same purpose in
// the same package namespace; see this file's own doc comment.
func planPackageFiles(t *testing.T, dir string) map[string]*ast.File {
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

// forbiddenMigrateImports is ADR 0015's import-graph property for this
// package ("The plan has nowhere to write a CIDR": "the engine lives in a
// package that imports neither internal/netbox, internal/storage,
// internal/service, internal/cloud, internal/transport nor the AWS SDK").
// Unlike internal/assess's TestImportsAreStandardLibraryOnly, this is a
// denylist rather than an allowlist: this package's own cut (M3b1) is
// explicitly permitted to import go.yaml.in/yaml/v3, the repository's
// existing YAML dependency, which a stdlib-only test would have to special-
// case around. See plan_decode.go's doc comment for why that import is
// this package's own decision to make.
var forbiddenMigrateImportPrefixes = []string{
	"github.com/mischapogr/platform-ipam/internal/netbox",
	"github.com/mischapogr/platform-ipam/internal/storage",
	"github.com/mischapogr/platform-ipam/internal/service",
	"github.com/mischapogr/platform-ipam/internal/cloud",
	"github.com/mischapogr/platform-ipam/internal/transport",
}

// TestPlanPackageImportsExcludeAdapterPackages is this file's own test, named
// with a "Plan" prefix (rather than the bare "TestImportsExcludeAdapter
// Packages" a mirror of internal/assess's test name would suggest) because
// M3b2's own worked test list also calls for "a source-parsing import test
// as internal/assess has" in this SAME package -- two independently written
// test functions in one Go package must not share a name, and this package
// has no way to coordinate that with M3b2's private copy in advance (see
// the report to the lead).
func TestPlanPackageImportsExcludeAdapterPackages(t *testing.T) {
	for name, f := range planPackageFiles(t, ".") {
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range forbiddenMigrateImportPrefixes {
				if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
					t.Errorf("%s imports %q, which ADR 0015 forbids this package from importing", name, path)
				}
			}
			if path == "github.com/aws/aws-sdk-go-v2" || strings.HasPrefix(path, "github.com/aws/aws-sdk-go-v2/") ||
				strings.HasPrefix(path, "github.com/aws/aws-sdk-go/") {
				t.Errorf("%s imports %q, the AWS SDK, which ADR 0015 forbids", name, path)
			}
		}
	}
}
