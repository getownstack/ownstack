package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

func TestNamespaceObject(t *testing.T) {
	labels := func(obj map[string]any) map[string]string {
		return obj["metadata"].(map[string]any)["labels"].(map[string]string)
	}
	for _, tc := range []struct {
		name    string
		current map[string]string
		want    map[string]string
	}{
		{"new namespace", nil, map[string]string{podSecurityEnforce: "baseline", podSecurityWarn: "baseline"}},
		{"privileged by hand", map[string]string{podSecurityEnforce: "privileged"}, map[string]string{podSecurityEnforce: "baseline", podSecurityWarn: "baseline"}},
		{"stricter level kept", map[string]string{podSecurityEnforce: "restricted", podSecurityWarn: "restricted"}, map[string]string{podSecurityEnforce: "restricted", podSecurityWarn: "restricted"}},
	} {
		obj := namespaceObject("prod", tc.current)
		if obj["kind"] != "Namespace" || obj["metadata"].(map[string]any)["name"] != "prod" {
			t.Errorf("%s: %v", tc.name, obj)
		}
		if got := labels(obj); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: labels = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// envStub answers kubectl for the environments stage: prod exists with a
// privileged label and one pod that baseline would reject; dev and qa do not
// exist yet. Calls, applied objects, and patches are written to the stub dir.
const envStub = `#!/bin/sh
echo "$*" >> "$OWNSTACK_STUB_DIR/calls"
case "$*" in
  "get namespace prod -o json")
    echo '{"metadata":{"name":"prod","labels":{"kubernetes.io/metadata.name":"prod","pod-security.kubernetes.io/enforce":"privileged"}}}' ;;
  "get namespace "*) exit 1 ;;
  "label --dry-run=server --overwrite namespace prod pod-security.kubernetes.io/enforce=baseline")
    echo "namespace/prod labeled (server dry run)"
    echo 'Warning: existing pods in namespace "prod" violate the new PodSecurity enforce level "baseline:latest"' >&2
    echo 'Warning: legacy-0: hostPath volumes' >&2 ;;
  "apply -f -") cat >> "$OWNSTACK_STUB_DIR/applied"; echo >> "$OWNSTACK_STUB_DIR/applied" ;;
  "get serviceaccount default -n "*) echo '{"metadata":{"resourceVersion":"5"},"imagePullSecrets":[{"name":"mine"}]}' ;;
  "patch serviceaccount default -n "*) echo "$*" >> "$OWNSTACK_STUB_DIR/patches" ;;
  *) exit 1 ;;
esac
`

// envRuntime returns a runtime whose kubectl is script, its console output,
// and the stub directory.
func envRuntime(t *testing.T, script string) (*stack.Runtime, *strings.Builder, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OWNSTACK_STUB_DIR", dir)
	rt, _ := providertest.Runtime(t)
	out := &strings.Builder{}
	rt.Console = stack.NewConsole(out)
	rt.Exec.Console = rt.Console
	rt.Out.Images = &stack.ImagesOutput{Host: "registry.example.com", Project: "apps", PushUsername: "ci", PushPassword: "registry-pw-value", PullSecretName: "registry-credentials"}
	return rt, out, dir
}

func TestEnvironmentsApply(t *testing.T) {
	rt, out, dir := envRuntime(t, envStub)
	if err := (environments{rt: rt}).apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Every namespace is applied with the Pod Security labels.
	applied, _ := os.ReadFile(filepath.Join(dir, "applied"))
	namespaces := 0
	dec := json.NewDecoder(strings.NewReader(string(applied)))
	for dec.More() {
		var obj struct {
			Kind     string
			Metadata struct {
				Name   string
				Labels map[string]string
			}
		}
		if err := dec.Decode(&obj); err != nil {
			t.Fatal(err)
		}
		if obj.Kind != "Namespace" {
			continue
		}
		namespaces++
		if obj.Metadata.Labels[podSecurityEnforce] != "baseline" || obj.Metadata.Labels[podSecurityWarn] != "baseline" {
			t.Errorf("namespace %s labels = %v", obj.Metadata.Name, obj.Metadata.Labels)
		}
	}
	if namespaces != 3 {
		t.Errorf("applied %d namespaces, want 3", namespaces)
	}

	// The existing namespace is dry-run first and its violations reported.
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if n := strings.Count(string(calls), "label --dry-run=server"); n != 1 {
		t.Errorf("%d dry runs, want 1 (only prod existed):\n%s", n, calls)
	}
	if !strings.Contains(out.String(), "prod now enforces Pod Security baseline") || !strings.Contains(out.String(), "legacy-0: hostPath volumes") {
		t.Errorf("violations not reported:\n%s", out.String())
	}

	// The default service accounts pull with the secret, keeping their own.
	patches, _ := os.ReadFile(filepath.Join(dir, "patches"))
	if strings.Count(string(patches), `{"name":"mine"},{"name":"registry-credentials"}`) != 3 {
		t.Errorf("patches:\n%s", patches)
	}
	if strings.Contains(out.String(), "registry-pw-value") || strings.Contains(string(patches), "registry-pw-value") {
		t.Errorf("a credential leaked:\n%s\n%s", out.String(), patches)
	}
}

// A default service account that never appears costs a warning, not the run.
func TestDefaultServiceAccountMissing(t *testing.T) {
	defer func(a int, d time.Duration) { serviceAccountAttempts, serviceAccountDelay = a, d }(serviceAccountAttempts, serviceAccountDelay)
	serviceAccountAttempts, serviceAccountDelay = 3, 0
	stub := strings.Replace(envStub, `echo '{"metadata":{"resourceVersion":"5"},"imagePullSecrets":[{"name":"mine"}]}' ;;`,
		`echo 'Error from server (NotFound): serviceaccounts "default" not found' >&2; exit 1 ;;`, 1)
	rt, out, dir := envRuntime(t, stub)
	if err := (environments{rt: rt}).apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if n := strings.Count(string(calls), "get serviceaccount default -n prod"); n != 3 {
		t.Errorf("asked %d times for the prod account, want 3", n)
	}
	if !strings.Contains(out.String(), "warning: the default service account in prod does not pull with registry-credentials") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestEnvironmentsStatusPodSecurity(t *testing.T) {
	stub := `#!/bin/sh
case "$*" in
  "get namespace dev -o json") echo '{"metadata":{"labels":{"pod-security.kubernetes.io/enforce":"baseline"}}}' ;;
  "get namespace qa -o json") echo '{"metadata":{"labels":{"pod-security.kubernetes.io/enforce":"restricted"}}}' ;;
  "get namespace prod -o json") echo '{"metadata":{"labels":{"kubernetes.io/metadata.name":"prod"}}}' ;;
  *) exit 1 ;;
esac
`
	rt, _, _ := envRuntime(t, stub)
	rt.Out.Images = nil
	want := map[string]string{"dev": stack.OK, "qa": stack.OK, "prod": stack.Warn}
	checks := (environments{rt: rt}).status(context.Background())
	if len(checks) != 3 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if c.State != want[c.Name] {
			t.Errorf("%s: %+v", c.Name, c)
		}
		if c.Name == "prod" && !strings.Contains(c.Detail, "Pod Security is not enforced") {
			t.Errorf("prod detail = %q", c.Detail)
		}
	}
}
