package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestProviderSourceNeverConstructsADeleteOnOperations is a source-parsing
// guard, in the style of internal/service's own structural tests
// (TestCommittedAllocationsComeIntoBeingInThreePlaces and its neighbours):
// package H8c's ADR 0013 review makes the provider's non-cancellation a
// property of every future change, not just of today's code, so it is
// pinned at the AST level rather than only demonstrated by the timeout test
// above. It parses every non-test .go file in this module (both this
// package and internal/client) and fails if any call expression takes both
// http.MethodDelete and a string literal argument mentioning "operations" --
// the shape every DELETE call in this module takes today
// (internal/client/client.go's DeleteAllocation: doJSON(ctx,
// http.MethodDelete, "/v1/allocations/"+...)). The provider must never issue
// DELETE /v1/operations/{operation_id}: that is the reservation's own tenant
// exit (ADR 0013), never something this provider performs on the consumer's
// behalf -- not on its own create timeout (see the comment on Create's
// operationCtx), not on destroy, not anywhere.
func TestProviderSourceNeverConstructsADeleteOnOperations(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".") && path != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			hasMethodDelete, mentionsOperations := false, false
			for _, arg := range call.Args {
				switch a := arg.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := a.X.(*ast.Ident); ok && pkg.Name == "http" && a.Sel.Name == "MethodDelete" {
						hasMethodDelete = true
					}
				case *ast.BasicLit:
					if a.Kind == token.STRING && strings.Contains(strings.ToLower(a.Value), "operations") {
						mentionsOperations = true
					}
				case *ast.BinaryExpr:
					// A path built as a literal prefix plus a suffix, exactly
					// DeleteAllocation's own "/v1/allocations/"+url.PathEscape(id)
					// shape: walk the binary expression's operands for a
					// string literal mentioning "operations".
					ast.Inspect(a, func(sub ast.Node) bool {
						lit, ok := sub.(*ast.BasicLit)
						if ok && lit.Kind == token.STRING && strings.Contains(strings.ToLower(lit.Value), "operations") {
							mentionsOperations = true
						}
						return true
					})
				}
			}
			if hasMethodDelete && mentionsOperations {
				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: a call passes both http.MethodDelete and a string literal mentioning \"operations\" -- the provider must never issue DELETE /v1/operations (ADR 0013)", pos.Filename, pos.Line)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
