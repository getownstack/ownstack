package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/ghactions"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// Providers read only other aspects' contract outputs, never their manifest
// entries. Reading the manifest directly is allowed only for the provider's
// own aspect (rt.Aspect()); rt.Setting, rt.Secret and rt.Values do that.
func TestProvidersReadOnlyTheirOwnAspect(t *testing.T) {
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
		for _, leak := range manifestLeaks(fset, f) {
			t.Errorf("%s reads another aspect's manifest entry; consume its contract output instead", leak)
		}
	}

	// The guard itself catches the pattern it is for.
	src := `package p
func f(rt *stack.Runtime) bool {
	_ = rt.Manifest.Settings(rt.Catalog, rt.Aspect())["host"]
	return rt.Manifest.Settings(rt.Catalog, "vcs")["demoApp"] == "true"
}`
	f, err := parser.ParseFile(fset, "leak.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if leaks := manifestLeaks(fset, f); len(leaks) != 1 || !strings.HasPrefix(leaks[0], "leak.go:4:") {
		t.Errorf("guard missed the leak: %v", leaks)
	}
}

// manifestLeaks finds Manifest.Settings, Component and SecretRef calls whose
// aspect argument is not rt.Aspect().
func manifestLeaks(fset *token.FileSet, f *ast.File) []string {
	// The argument index that names the aspect.
	aspectArg := map[string]int{"Settings": 1, "Component": 0, "SecretRef": 1}
	var leaks []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		i, watched := aspectArg[sel.Sel.Name]
		if !ok || recv.Sel.Name != "Manifest" || !watched {
			return true
		}
		if i >= len(call.Args) || !isOwnAspect(call.Args[i]) {
			leaks = append(leaks, fmt.Sprintf("%s: Manifest.%s", fset.Position(call.Pos()), sel.Sel.Name))
		}
		return true
	})
	return leaks
}

func isOwnAspect(arg ast.Expr) bool {
	call, ok := arg.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Aspect" && len(call.Args) == 0
}

// GitHub Actions takes the cluster and demo repos from the VCS output, not
// from the github provider's settings.
func TestGHActionsUsesVCSOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]string{"login": "acme"})
		case r.URL.Path == "/user/repos":
			json.NewEncoder(w).Encode([]map[string]any{
				{"name": "infra", "owner": map[string]string{"login": "acme"}},
				{"name": "shop", "owner": map[string]string{"login": "acme"}},
			})
		case strings.HasSuffix(r.URL.Path, "/contents/.github/workflows/ownstack.yml"):
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rt, _ := providertest.Runtime(t)
	m, err := manifest.Parse([]byte(strings.Replace(providertest.Manifest, "provider: jenkins", "provider: gh-actions", 1)))
	if err != nil {
		t.Fatal(err)
	}
	rt.Manifest = m
	// The manifest's vcs settings say ownstack-cluster and lumen-yard.
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: srv.URL, Token: "t", Username: "acme",
		ClusterRepo: "infra", DemoRepo: "demo-x"}
	var got []string
	for _, step := range (ghactions.Provider{}).Plan(context.Background(), rt.For("cicd")) {
		got = append(got, step.Action+" "+step.Target)
	}
	plan := strings.Join(got, "\n")
	if !strings.Contains(plan, "configure acme/shop") || strings.Contains(plan, "acme/infra") || !strings.Contains(plan, "ensure acme/demo-x") ||
		strings.Contains(plan, "lumen-yard") {
		t.Errorf("plan:\n%s", plan)
	}

	rt.Out.VCS.DemoRepo = ""
	for _, step := range (ghactions.Provider{}).Plan(context.Background(), rt.For("cicd")) {
		if step.Action == "ensure" && strings.Contains(step.Target, "acme/") {
			t.Errorf("demo step without a demo repo: %+v", step)
		}
	}
}
