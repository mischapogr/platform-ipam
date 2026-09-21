package service

// Package H4, part two: docs/FINDINGS.md indexes every code non-test source
// can raise. Nothing enforced that until this test: parse cmd/ and
// internal/, collect the finding code named at every workerFinding( call,
// every (*Service).flagStuckHold( call and every Code: field of a
// domain.Finding{} literal, and fail if the document is missing one or
// names one the source never raises. In the style of adopt_test.go's
// TestCommittedAllocationsComeIntoBeingInThreePlaces: a flat AST walk, not a
// data-flow analysis, over sources this package's own house style keeps
// simple enough for one.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory (the package
// directory Go sets for `go test`, here or under a private copy used to
// prepare this change) until it finds go.mod, so the test locates
// docs/FINDINGS.md correctly whether it runs from the real checkout or from
// a snapshot copy that carries cmd/, internal/ and docs/ but not the whole
// repository root's other directories.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the repository root: no go.mod found above the test's working directory")
		}
		dir = parent
	}
}

// scanFindingCodesInScope collects every finding code literal raised within
// one function scope (a *ast.FuncDecl body or a *ast.FuncLit body) and
// recurses into any nested function literal as a scope of its own -- the
// two raising sites this project uses, workerFinding(st, a, code, ...) and
// s.flagStuckHold(ctx, a, code), plus a bare domain.Finding{Code: code}
// literal, each sit inside the anonymous closure passed to
// (domain.Ledger).Update rather than in the named method around it, and a
// variable named "code" in one closure must never resolve against an
// assignment in a different one.
func scanFindingCodesInScope(body *ast.BlockStmt, codes map[string]bool) {
	if body == nil {
		return
	}
	// Pass 1: every string literal ever assigned to a bare identifier in
	// THIS scope (both `:=` and `=` are *ast.AssignStmt in go/ast). A
	// variable can be reassigned across branches (e.g. `code := "a"; if ...
	// { code = "b" }`), so every value it is ever set to is kept, not just
	// the last: workerFinding(st, a, code, ...) can run under either.
	literals := map[string][]string{}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, isFuncLit := n.(*ast.FuncLit); isFuncLit {
			return false // a scope of its own; pass 2 recurses into it separately
		}
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || i >= len(assign.Rhs) {
				continue
			}
			lit, ok := assign.Rhs[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, err := strconv.Unquote(lit.Value); err == nil {
				literals[id.Name] = append(literals[id.Name], v)
			}
		}
		return true
	})

	resolve := func(e ast.Expr) []string {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				if s, err := strconv.Unquote(v.Value); err == nil {
					return []string{s}
				}
			}
		case *ast.Ident:
			return literals[v.Name]
		}
		return nil
	}
	add := func(values []string) {
		for _, v := range values {
			if v != "" {
				codes[v] = true
			}
		}
	}

	// Pass 2: every raising call or literal in THIS scope, recursing into
	// any nested function literal as a scope of its own.
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			scanFindingCodesInScope(x.Body, codes)
			return false
		case *ast.CallExpr:
			var name string
			switch f := x.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if (name == "workerFinding" || name == "flagStuckHold") && len(x.Args) > 2 {
				add(resolve(x.Args[2]))
			}
		case *ast.CompositeLit:
			sel, ok := x.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "domain" || sel.Sel.Name != "Finding" {
				return true
			}
			for _, elt := range x.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Code" {
					continue
				}
				add(resolve(kv.Value))
			}
		}
		return true
	})
}

// collectRaisedFindingCodes parses every non-test .go file under cmd/ and
// internal/ from the repository root and returns the set of finding codes
// the source can raise.
func collectRaisedFindingCodes(t *testing.T) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	codes := map[string]bool{}
	fset := token.NewFileSet()
	seen := 0
	for _, dir := range []string{"cmd", "internal"} {
		full := filepath.Join(root, dir)
		if _, err := os.Stat(full); err != nil {
			t.Fatalf("expected %s to exist under the repository root %s: %v", dir, root, err)
		}
		if err := filepath.WalkDir(full, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			seen++
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				scanFindingCodesInScope(fn.Body, codes)
			}
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", full, err)
		}
	}
	if seen == 0 {
		t.Fatal("no non-test Go sources found under cmd/ or internal/")
	}
	return codes
}

// documentedFindingCodes parses the "## <code>" headings docs/FINDINGS.md
// uses -- one section per code, in the style established by this file's own
// prose -- and returns the set of codes it documents.
func documentedFindingCodes(t *testing.T) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "FINDINGS.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	heading := regexp.MustCompile(`(?m)^## ([a-z][a-z0-9_]*)\s*$`)
	out := map[string]bool{}
	for _, m := range heading.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s documented no finding codes (no \"## code\" heading matched)", path)
	}
	return out
}

// TestDocumentedFindingCodesMatchTheSource is docs/FINDINGS.md's own honesty
// check (package H4, part two): every code the source can raise must be
// documented, and the document must name nothing the source does not raise.
func TestDocumentedFindingCodesMatchTheSource(t *testing.T) {
	raised := collectRaisedFindingCodes(t)
	documented := documentedFindingCodes(t)

	var missing, extra []string
	for code := range raised {
		if !documented[code] {
			missing = append(missing, code)
		}
	}
	for code := range documented {
		if !raised[code] {
			extra = append(extra, code)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/FINDINGS.md is missing codes the source raises: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("docs/FINDINGS.md documents codes the source never raises: %v", extra)
	}
	if len(raised) == 0 {
		t.Fatal("the source raised no finding codes at all -- the collector itself is broken")
	}
}
