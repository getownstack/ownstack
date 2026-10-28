package stack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPullSecretMismatch(t *testing.T) {
	const server, user, pass = "harbor.admin.example.com", "robot$ownstack-ci", "s3cr3t-value"
	current, err := DockerConfigJSON(server, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := DockerConfigJSON("https://index.docker.io/v1/", "acme", pass)
	for _, tc := range []struct {
		name, raw, server, user, pass, want string
	}{
		{"match", current, server, user, pass, ""},
		{"rotated password", current, server, user, "new-value", "different password"},
		{"new robot", current, server, "robot$other", pass, "different username"},
		{"password unknown here", current, server, user, "", ""},
		{"other registry", current, "zot.admin.example.com", user, pass, "no entry for zot.admin.example.com"},
		{"docker hub key", hub, "https://index.docker.io/v1/", "acme", pass, ""},
		{"auth only", `{"auths":{"r.example.com":{"auth":"dTpw"}}}`, "r.example.com", "u", "p", ""},
		{"garbage", "not json", server, user, pass, "unreadable"},
	} {
		got := PullSecretMismatch(tc.raw, tc.server, tc.user, tc.pass)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, pass) || strings.Contains(got, "new-value") {
			t.Errorf("%s: reason contains a password: %q", tc.name, got)
		}
	}
}

func TestDockerConfigJSON(t *testing.T) {
	for _, tc := range []struct{ server, user, pass string }{
		{"harbor.admin.example.com", "robot$ownstack-ci", `p"a:ss\w`},
		{"https://index.docker.io/v1/", "acme", "token"},
	} {
		raw, err := DockerConfigJSON(tc.server, tc.user, tc.pass)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
		}
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		entry, ok := config.Auths[tc.server]
		if !ok || len(config.Auths) != 1 {
			t.Fatalf("auths = %v, want only %s", config.Auths, tc.server)
		}
		decoded, _ := base64.StdEncoding.DecodeString(entry.Auth)
		if entry.Username != tc.user || entry.Password != tc.pass || string(decoded) != tc.user+":"+tc.pass {
			t.Errorf("%s: entry = %+v, auth = %q", tc.server, entry, decoded)
		}
	}
}

// Secrets are applied with data, never stringData: server-side apply tracks
// the fields it sends, and stringData is not stored.
func TestSecretObject(t *testing.T) {
	obj := SecretObject("dev", "registry-credentials", "kubernetes.io/dockerconfigjson", map[string]string{".dockerconfigjson": `{"auths":{}}`}, nil)
	data := obj["data"].(map[string]string)
	decoded, _ := base64.StdEncoding.DecodeString(data[".dockerconfigjson"])
	if string(decoded) != `{"auths":{}}` || obj["stringData"] != nil || obj["type"] != "kubernetes.io/dockerconfigjson" {
		t.Errorf("secret = %v", obj)
	}
	if meta := obj["metadata"].(map[string]any); meta["namespace"] != "dev" || meta["labels"] != nil {
		t.Errorf("metadata = %v", meta)
	}
	if obj := SecretObject("x", "y", "", nil, map[string]string{"a": "b"}); obj["type"] != "Opaque" || obj["metadata"].(map[string]any)["labels"] == nil {
		t.Errorf("opaque secret = %v", obj)
	}
}

// kubectlStub puts a fake kubectl on PATH that records its arguments and
// stdin.
func kubectlStub(t *testing.T) (*Kube, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "calls") + "\"\ncat >> \"" + filepath.Join(dir, "stdin") + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	return &Kube{Exec: &Exec{Console: NewConsole(&out)}}, dir
}

func TestApplyIsOneServerSideList(t *testing.T) {
	k, dir := kubectlStub(t)
	ns := map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "dev"}}
	sa := map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "default", "namespace": "dev"}}
	if err := k.Apply(context.Background(), ns, sa); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "apply --server-side --force-conflicts --field-manager=ownstack -f -\n" {
		t.Errorf("calls = %q", calls)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	var list struct {
		APIVersion, Kind string
		Items            []struct{ Kind string }
	}
	if err := json.Unmarshal(stdin, &list); err != nil {
		t.Fatalf("%v: %s", err, stdin)
	}
	if list.APIVersion != "v1" || list.Kind != "List" || len(list.Items) != 2 || list.Items[0].Kind != "Namespace" || list.Items[1].Kind != "ServiceAccount" {
		t.Errorf("applied %s", stdin)
	}
}
