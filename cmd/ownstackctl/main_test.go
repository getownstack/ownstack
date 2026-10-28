package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func runCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func keys(m map[string]any) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// validate --json is what the designer runs on every change: always exit 0,
// with the issues, the written manifest, resources, and secrets.
func TestValidateJSON(t *testing.T) {
	out, err := runCmd(t, `{"apiVersion":"ownstack.io/v2","domain":"example.com"}`, "validate", "--json", "-f", "-")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if keys(doc) != "issues,manifest,resources,secrets,valid" || doc["valid"] != false {
		t.Errorf("document = %s", out)
	}
	var paths []string
	for _, issue := range doc["issues"].([]any) {
		paths = append(paths, issue.(map[string]any)["path"].(string))
	}
	if !slices.Equal(paths, []string{"compute.host", "ingress.acmeEmail", "vcs.owner"}) {
		t.Errorf("issues at %v", paths)
	}
	if doc["manifest"] != "apiVersion: ownstack.io/v2\ndomain: example.com\n" {
		t.Errorf("manifest = %q", doc["manifest"])
	}
	resources := doc["resources"].(map[string]any)
	if keys(resources) != "diskGiB,memoryMiB,minServerMiB" || resources["memoryMiB"].(float64) == 0 ||
		resources["minServerMiB"].(float64) >= resources["memoryMiB"].(float64) {
		t.Errorf("resources = %v", resources)
	}
	secrets := doc["secrets"].([]any)
	first := secrets[0].(map[string]any)
	if len(secrets) != 3 || keys(first) != "aspect,env,generate,helpURL,key,label" || first["env"] != "CLOUDFLARE_API_TOKEN" {
		t.Errorf("secrets = %v", secrets)
	}

	// Unreadable input is an issue too, still with exit 0.
	out, err = runCmd(t, "a: [", "validate", "--json", "-f", "-")
	if err != nil || !strings.Contains(out, `"valid": false`) || !strings.Contains(out, `"path": ""`) {
		t.Errorf("parse error: %v %s", err, out)
	}
}

func TestValidateExamples(t *testing.T) {
	for _, name := range []string{"starter", "standard"} {
		path := filepath.Join("..", "..", "examples", name+".yaml")
		if out, err := runCmd(t, "", "validate", "-f", path); err != nil || !strings.Contains(out, "is valid") {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
	bad := filepath.Join(t.TempDir(), "ownstack.yaml")
	os.WriteFile(bad, []byte("apiVersion: ownstack.io/v1\n"), 0o644)
	if out, err := runCmd(t, "", "validate", "-f", bad); err == nil || !strings.Contains(out, "no longer read") {
		t.Errorf("v1 manifest: %v %s", err, out)
	}
}

// The catalog the designer reads (catalog v2).
func TestCatalogJSON(t *testing.T) {
	out, err := runCmd(t, "", "catalog")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if keys(doc) != "aspects,memoryFitRatio,presets,providers" {
		t.Errorf("catalog keys = %s", keys(doc))
	}
	var aspects []string
	for _, a := range doc["aspects"].([]any) {
		aspects = append(aspects, a.(map[string]any)["id"].(string)+"="+a.(map[string]any)["default"].(string))
	}
	if strings.Join(aspects, " ") != "compute=own-vps dns=cloudflare ingress=traefik vcs=github images=zot cicd=gh-actions" {
		t.Errorf("aspects = %v", aspects)
	}
	presets, _ := json.Marshal(doc["presets"])
	if string(presets) != `[{"components":{},"id":"starter","minMemoryGB":2,"name":"Starter"},{"components":{"cicd":"jenkins","images":"harbor"},"id":"standard","minMemoryGB":4,"name":"Standard"}]` {
		t.Errorf("presets = %s", presets)
	}
	for _, p := range doc["providers"].([]any) {
		for k := range p.(map[string]any) {
			if !slices.Contains([]string{"id", "aspect", "name", "mono", "hosting", "summary", "params", "resources"}, k) {
				t.Errorf("provider field %s", k)
			}
		}
	}
}

func TestRemovedCommandsAndFlags(t *testing.T) {
	for _, args := range [][]string{
		{"up"}, {"doctor"},
		{"apply", "--no-verify"}, {"apply", "--secrets-file", "x"}, {"apply", "--no-prompt"}, {"apply", "--health-url", "x"},
		{"apply", "--no-health-reports"}, {"apply", "--dir", "x"}, {"plan", "--secrets-file", "x"},
		{"validate", "ownstack.yaml"},
	} {
		if _, err := runCmd(t, "", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

// -f defaults to the manifest the last apply saved.
func TestManifestDefaultsToTheStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OWNSTACK_DIR", dir)
	if _, err := runCmd(t, "", "validate"); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "ownstack.yaml")+" not found") {
		t.Errorf("err = %v", err)
	}
	data, _ := os.ReadFile("../../examples/starter.yaml")
	os.WriteFile(filepath.Join(dir, "ownstack.yaml"), data, 0o644)
	if _, err := runCmd(t, "", "validate"); err != nil {
		t.Error(err)
	}
}
