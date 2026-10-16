package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/secrets"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
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
	if got := stageIDs(e); got != "compute,dns,ingress,vcs,images,environments,alerts,cicd" {
		t.Errorf("stages = %s", got)
	}
}

func TestMinimalStackStages(t *testing.T) {
	src := `
apiVersion: ownstack.io/v1
kind: Stack
metadata: {name: tiny}
spec:
  domain: example.com
  environments: [prod]
  compute: {provider: own-vps, settings: {host: 203.0.113.10}}
  dns: {provider: manual}
  ingress: {provider: traefik, settings: {acmeEmail: ops@example.com}}
`
	e := newEngine(t, src)
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := stageIDs(e); got != "compute,environments,dns,ingress" {
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
