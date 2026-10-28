package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// envStub answers kubectl for the environments stage. Calls and applied
// input are written to the stub dir.
const envStub = `#!/bin/sh
echo "$*" >> "$OWNSTACK_STUB_DIR/calls"
case "$*" in
  "apply "*) cat >> "$OWNSTACK_STUB_DIR/applied" ;;
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
	rt.Out.Images = &stack.ImagesOutput{Host: "registry.example.com", Project: "apps", PushUsername: "ci", PushPassword: "registry-pw-value"}
	return rt, out, dir
}

// One kubectl call server-side applies every environment's namespace, pull
// secret, and default service account (C8).
func TestEnvironmentsApply(t *testing.T) {
	rt, out, dir := envRuntime(t, envStub)
	if err := (environments{rt: rt}).apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "apply --server-side --force-conflicts --field-manager=ownstack -f -\n" {
		t.Fatalf("kubectl calls:\n%s", calls)
	}
	applied, _ := os.ReadFile(filepath.Join(dir, "applied"))
	var list struct {
		Kind  string
		Items []struct {
			Kind     string
			Type     string
			Data     map[string]string
			Metadata struct {
				Name, Namespace string
				Labels          map[string]string
			}
			ImagePullSecrets []struct{ Name string }
		}
	}
	if err := json.Unmarshal(applied, &list); err != nil || list.Kind != "List" {
		t.Fatalf("applied %s: %v", applied, err)
	}
	var got []string
	for _, item := range list.Items {
		got = append(got, item.Kind+" "+item.Metadata.Namespace+"/"+item.Metadata.Name)
		switch item.Kind {
		case "Namespace":
			if item.Metadata.Labels[podSecurityEnforce] != "baseline" || item.Metadata.Labels[podSecurityWarn] != "baseline" {
				t.Errorf("namespace %s labels = %v", item.Metadata.Name, item.Metadata.Labels)
			}
		case "Secret":
			config, _ := base64.StdEncoding.DecodeString(item.Data[".dockerconfigjson"])
			if item.Type != "kubernetes.io/dockerconfigjson" || stack.PullSecretMismatch(string(config), "registry.example.com", "ci", "registry-pw-value") != "" {
				t.Errorf("pull secret in %s: %s %s", item.Metadata.Namespace, item.Type, config)
			}
		case "ServiceAccount":
			if len(item.ImagePullSecrets) != 1 || item.ImagePullSecrets[0].Name != "registry-credentials" {
				t.Errorf("service account in %s pulls with %v", item.Metadata.Namespace, item.ImagePullSecrets)
			}
		}
	}
	want := []string{
		"Namespace /prod", "Secret prod/registry-credentials", "ServiceAccount prod/default",
		"Namespace /dev", "Secret dev/registry-credentials", "ServiceAccount dev/default",
		"Namespace /qa", "Secret qa/registry-credentials", "ServiceAccount qa/default",
	}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("applied:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(out.String(), "registry-pw-value") || !strings.Contains(out.String(), "Environments ready: prod, dev, qa") {
		t.Errorf("output:\n%s", out.String())
	}
}

// A failing apply fails the stage; a registry without a credential fails it
// before anything is applied.
func TestEnvironmentsApplyFails(t *testing.T) {
	rt, _, _ := envRuntime(t, "#!/bin/sh\necho 'error: the server could not find the requested resource' >&2\nexit 1\n")
	if err := (environments{rt: rt}).apply(context.Background()); err == nil || !strings.Contains(err.Error(), "could not find the requested resource") {
		t.Errorf("err = %v", err)
	}

	rt, _, dir := envRuntime(t, envStub)
	rt.Out.Images.PushUsername = ""
	if err := (environments{rt: rt}).apply(context.Background()); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls")); err == nil {
		t.Error("applied without a credential")
	}
}

func TestEnvironmentsStatus(t *testing.T) {
	stub := `#!/bin/sh
case "$*" in
  "get namespace dev -o name"|"get namespace qa -o name") echo namespace/x ;;
  *) exit 1 ;;
esac
`
	rt, _, _ := envRuntime(t, stub)
	rt.Out.Images = nil
	want := map[string]string{"dev": stack.OK, "qa": stack.OK, "prod": stack.Fail}
	checks := (environments{rt: rt}).status(context.Background())
	if len(checks) != 3 {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if c.State != want[c.Name] {
			t.Errorf("%s: %+v", c.Name, c)
		}
	}
}
