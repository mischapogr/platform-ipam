package config

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// TestNewAuthOnlyConstructedForAPIMode is package H5's source-parsing proof,
// in the style of internal/service's "three places" tests, that
// cmd/platform-ipam/main.go never constructs transport.NewAuth for "adopt":
// in oidc mode NewAuth performs OIDC discovery over the network against the
// configured issuer (internal/transport/auth.go), so an adopt run -- a
// manual, one-shot, irreversible action (ADR 0010) -- must never depend on
// the identity provider being reachable. The evidence is structural, not
// behavioural: the file's single transport.NewAuth call site must sit
// directly inside an `if mode == "api"` block and nothing looser, so neither
// "adopt" nor "worker" can ever reach it, today or after an unrelated edit to
// this file's control flow that a manual review might miss.
func TestNewAuthOnlyConstructedForAPIMode(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../../cmd/platform-ipam/main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}

	var calls []*ast.CallExpr
	var ifStmts []*ast.IfStmt
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewAuth" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "transport" {
					calls = append(calls, x)
				}
			}
		case *ast.IfStmt:
			ifStmts = append(ifStmts, x)
		}
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("expected exactly one transport.NewAuth call site in cmd/platform-ipam/main.go, found %d", len(calls))
	}
	callPos := calls[0].Pos()

	// Every if-statement whose THEN body (not its condition, not its else
	// branch) textually contains the call, by token position.
	var containing []*ast.IfStmt
	for _, s := range ifStmts {
		if s.Body.Pos() <= callPos && callPos <= s.Body.End() {
			containing = append(containing, s)
		}
	}
	if len(containing) == 0 {
		t.Fatal("transport.NewAuth is constructed unconditionally: not inside any if statement's body")
	}
	innermost := containing[0]
	for _, s := range containing[1:] {
		if (s.Body.End() - s.Body.Pos()) < (innermost.Body.End() - innermost.Body.Pos()) {
			innermost = s
		}
	}
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, innermost.Cond); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), `mode == "api"`; got != want {
		t.Fatalf("transport.NewAuth's innermost guard renders as %q, want %q -- \"adopt\" (and \"worker\") must never reach it", got, want)
	}
}
