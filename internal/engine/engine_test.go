package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

func newEngine(t *testing.T, src string) *Engine {
	t.Helper()
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	store := secrets.NewStore(filepath.Join(t.TempDir(), "secrets.env"))
	store.Getenv = func(k string) string { return providertest.Env[k] }
	return New(m, store, stack.NewConsole(&bytes.Buffer{}))
}

func stageIDs(e *Engine) string {
	var ids []string
	for _, s := range e.stages() {
		ids = append(ids, s.id)
	}
	return strings.Join(ids, ",")
}

func TestStageOrder(t *testing.T) {
	e := newEngine(t, providertest.Manifest)
	if got := stageIDs(e); got != "compute,dns,ingress,vcs,images,environments,cicd" {
		t.Errorf("stages = %s", got)
	}
}

func TestApplyRejectsInvalidManifest(t *testing.T) {
	e := newEngine(t, strings.Replace(providertest.Manifest, "provider: jenkins", "provider: woodpecker", 1))
	err := e.Apply(context.Background(), ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "spec.cicd.provider") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryPreflight(t *testing.T) {
	e := newEngine(t, providertest.Manifest) // harbor + jenkins + the rest, about 4 GB
	if err := e.memoryPreflight(ApplyOptions{MemTotalMiB: 1642}); err == nil || !strings.Contains(err.Error(), "Harbor") {
		t.Fatalf("1.6 GB server should be refused with the heavy parts named, got %v", err)
	}
	if err := e.memoryPreflight(ApplyOptions{MemTotalMiB: 1642, AllowLowMemory: true}); err != nil {
		t.Fatalf("override ignored: %v", err)
	}
	if err := e.memoryPreflight(ApplyOptions{MemTotalMiB: 8192}); err != nil {
		t.Fatalf("8 GB server refused: %v", err)
	}
	starter := strings.NewReplacer("provider: harbor", "provider: zot", "provider: jenkins", "provider: gh-actions").Replace(providertest.Manifest)
	if err := newEngine(t, starter).memoryPreflight(ApplyOptions{MemTotalMiB: 1642}); err != nil {
		t.Fatalf("Starter stack should fit 1.6 GB: %v", err)
	}
}

// plan's server checks use apply's memory rule, and only manifest issues and
// failed checks would stop apply; missing secrets are asked for.
func TestPlanProblems(t *testing.T) {
	memory := func(checks []stack.Check) (stack.Check, bool) {
		for _, c := range checks {
			if c.Name == "memory" {
				return c, true
			}
		}
		return stack.Check{}, false
	}
	if c, ok := memory(serverChecks(1642, 4000)); !ok || c.State != stack.Fail {
		t.Errorf("1.6 GB for 4 GB: %+v", c)
	}
	if c, ok := memory(serverChecks(8192, 4000)); !ok || c.State != stack.OK {
		t.Errorf("8 GB for 4 GB: %+v", c)
	}
	if c, ok := memory(serverChecks(0, 4000)); ok {
		t.Errorf("unknown memory checked: %+v", c)
	}

	plan := Plan{
		Issues:  []manifest.Issue{{Path: "spec.domain", Message: "x"}},
		Server:  []stack.Check{{Name: "root", State: stack.Fail}, {Name: "arch", State: stack.OK}},
		Missing: []string{"GitHub access token"},
	}
	if got := plan.Problems(); got != 2 {
		t.Errorf("problems = %d, want 2", got)
	}
}
