package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// starter is the test stack (Standard) with the Starter providers.
var starter = strings.Replace(providertest.Manifest, "images: {provider: harbor}\ncicd: {provider: jenkins}\n", "", 1)

func newEngine(t *testing.T, src string) *Engine {
	t.Helper()
	t.Setenv("OWNSTACK_DIR", t.TempDir())
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
	for _, src := range []string{providertest.Manifest, starter} {
		if got := stageIDs(newEngine(t, src)); got != "compute,dns,ingress,vcs,images,environments,cicd" {
			t.Errorf("stages = %s", got)
		}
	}
}

func TestApplyRejectsInvalidManifest(t *testing.T) {
	e := newEngine(t, strings.Replace(providertest.Manifest, "provider: jenkins", "provider: woodpecker", 1))
	err := e.Apply(context.Background(), ApplyOptions{})
	if err == nil || !strings.Contains(err.Error(), "cicd.provider") {
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
	if err := e.memoryPreflight(ApplyOptions{MemTotalMiB: 3800}); err != nil {
		t.Fatalf("Standard refused on a 4 GB server: %v", err)
	}
	if err := newEngine(t, starter).memoryPreflight(ApplyOptions{MemTotalMiB: 1642}); err != nil {
		t.Fatalf("Starter stack should fit 1.6 GB: %v", err)
	}
}

// Secrets and resources follow the providers the manifest picks, defaults
// included.
func TestSecretsAndResources(t *testing.T) {
	var envs []string
	for _, s := range newEngine(t, starter).Secrets() {
		envs = append(envs, s.Aspect+"."+s.Key+"="+s.Env)
	}
	if want := []string{"dns.apiToken=CLOUDFLARE_API_TOKEN", "vcs.token=GITHUB_TOKEN", "images.password=ZOT_PASSWORD"}; !slices.Equal(envs, want) {
		t.Errorf("secrets = %v", envs)
	}
	starterMiB := newEngine(t, starter).Resources().MemoryMiB
	standardMiB := newEngine(t, providertest.Manifest).Resources().MemoryMiB
	if starterMiB == 0 || standardMiB < starterMiB+2000 {
		t.Errorf("memory: Starter %d MiB, Standard %d MiB", starterMiB, standardMiB)
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
		Issues:  []manifest.Issue{{Path: "domain", Message: "x"}},
		Server:  []stack.Check{{Name: "root", State: stack.Fail}, {Name: "arch", State: stack.OK}},
		Missing: []string{"GitHub access token"},
	}
	if got := plan.Problems(); got != 2 {
		t.Errorf("problems = %d, want 2", got)
	}
}
