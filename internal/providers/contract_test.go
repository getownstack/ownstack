package providers

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Providers never read the manifest directly. They read their own settings
// through rt.Setting, rt.Secret and rt.Values, the stack's own settings
// through rt.Domain, rt.AdminDomain, rt.AdminHost, rt.Environments and
// rt.Production, and other aspects only through their contract outputs.
// That is what keeps providers swappable.
func TestProvidersDoNotReadTheManifest(t *testing.T) {
	files, err := filepath.Glob("*/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no provider sources found: %v", err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "providertest/") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range manifestReads(fset, f) {
			t.Errorf("%s reads the manifest; use the runtime's accessors or a contract output instead", leak)
		}
	}

	// The guard itself catches the pattern it is for.
	src := `package p
func f(rt *stack.Runtime) bool {
	_ = rt.Setting("host")
	return rt.Manifest.Settings(rt.Catalog, "vcs")["demoApp"] == "true"
}`
	f, err := parser.ParseFile(fset, "leak.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if leaks := manifestReads(fset, f); len(leaks) != 1 || !strings.HasPrefix(leaks[0], "leak.go:4:") {
		t.Errorf("guard missed the leak: %v", leaks)
	}
}

// manifestReads finds every selector of a Manifest field.
func manifestReads(fset *token.FileSet, f *ast.File) []string {
	var leaks []string
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Manifest" {
			leaks = append(leaks, fmt.Sprintf("%s: %s", fset.Position(sel.Pos()), "Manifest"))
		}
		return true
	})
	return leaks
}
