package ownvps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/providertest"
)

func TestK3sAction(t *testing.T) {
	for _, tc := range []struct {
		installed, want string
		action          string
		err             string
	}{
		{"v1.33.4+k3s1", "v1.33.4+k3s1", k3sNone, ""},
		{"v1.33.4+k3s1", "1.33.4+k3s1", k3sNone, ""},
		{"v1.33.4+k3s1", "v1.33.5+k3s1", k3sUpgrade, ""},
		{"v1.33.4+k3s1", "v1.33.4+k3s2", k3sUpgrade, ""},
		{"v1.33.4+k3s1", "1.34.1+k3s1", k3sUpgrade, ""},
		{"v1.34.0-rc1+k3s1", "v1.34.0+k3s1", k3sUpgrade, ""},
		{"v1.33.4+k3s1", "v1.35.0+k3s1", "", "one minor version at a time: run curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=v1.34 sh -s - and apply again"},
		{"v1.33.4+k3s1", "v1.33.3+k3s1", "", "does not support downgrades"},
		{"v1.33.4+k3s2", "v1.33.4+k3s1", "", "does not support downgrades"},
		{"v1.33.4+k3s1", "v1.32.9+k3s1", "", "does not support downgrades"},
		{"v1.33.4+k3s1", "v2.0.0+k3s1", "", "another major version"},
		{"garbage", "v1.33.4+k3s1", "", "cannot read the installed k3s version"},
	} {
		action, err := k3sAction(tc.installed, tc.want)
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s -> %q: %v", tc.installed, tc.want, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s -> %q: err = %v, want %q", tc.installed, tc.want, err, tc.err)
		case err == nil && action != tc.action:
			t.Errorf("%s -> %q: action = %q, want %q", tc.installed, tc.want, action, tc.action)
		}
	}
}

// The release pins one exact k3s build, and the install script installs it.
func TestK3sIsPinned(t *testing.T) {
	r, ok := parseK3sVersion(k3sVersion)
	if !ok || r.pre || r.build < 1 || !strings.HasPrefix(k3sVersion, "v") {
		t.Errorf("k3sVersion %s is not a k3s release build", k3sVersion)
	}
	if k3sInstallScript != "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='"+k3sVersion+"' sh -s -" {
		t.Errorf("install = %s", k3sInstallScript)
	}
}

func TestNodeReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	nodes := filepath.Join(dir, "nodes.json")
	stub := "#!/bin/sh\n[ -f " + nodes + " ] && cat " + nodes + " || { echo 'The connection to the server was refused' >&2; exit 1; }\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	rt, out := providertest.Runtime(t, Spec)

	if ok, detail := nodeReady(context.Background(), rt); ok || !strings.Contains(detail, "connection to the server was refused") {
		t.Errorf("API server down: ok=%v detail=%q", ok, detail)
	}
	node := func(ready string) string {
		return `{"items":[{"metadata":{"name":"box"},"status":{"conditions":[{"type":"Ready","status":"` + ready + `"}],"nodeInfo":{"kubeletVersion":"v1.36.5+k3s1"}}}]}`
	}
	os.WriteFile(nodes, []byte(node("False")), 0o600)
	if ok, detail := nodeReady(context.Background(), rt); ok || detail != "no ready node yet" {
		t.Errorf("node not ready: ok=%v detail=%q", ok, detail)
	}
	os.WriteFile(nodes, []byte(node("True")), 0o600)
	if ok, _ := nodeReady(context.Background(), rt); !ok || !strings.Contains(out.String(), "Node box is ready (v1.36.5+k3s1)") {
		t.Errorf("ready node: ok=%v output %q", ok, out.String())
	}
}

func withConfigFile(t *testing.T, content *string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rancher", "k3s", "config.yaml")
	old := k3sConfig
	k3sConfig = path
	t.Cleanup(func() { k3sConfig = old })
	if content != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestEnsureK3sConfig(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		content *string
		changed bool
	}{
		{"missing file", nil, true},
		{"empty file", str(""), true},
		{"both present", str("disable:\n  - traefik\ntls-san:\n  - 203.0.113.10\n"), false},
		{"comma-separated strings", str("disable: servicelb, traefik\ntls-san: example.com,203.0.113.10\n"), false},
		{"comma-separated list items", str("disable: [\"servicelb,traefik\"]\ntls-san: [203.0.113.10]\n"), false},
		{"other settings only", str("write-kubeconfig-mode: \"0644\"\ndisable: servicelb\n"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := withConfigFile(t, tc.content)
			changed, err := ensureK3sConfig("203.0.113.10")
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.changed {
				t.Errorf("changed = %v, want %v", changed, tc.changed)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.changed {
				if string(data) != *tc.content {
					t.Errorf("unchanged config was rewritten:\n%s", data)
				}
				return
			}
			if !strings.HasPrefix(string(data), "# Managed in part by ownstackctl.\n") {
				t.Errorf("no header:\n%s", data)
			}
			if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
				t.Errorf("mode = %v", info.Mode().Perm())
			}
			var config map[string]any
			if err := yaml.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(items(config["disable"]), ","), "traefik") || !contains(items(config["tls-san"]), "203.0.113.10") {
				t.Errorf("config = %v", config)
			}
			if tc.content != nil && strings.Contains(*tc.content, "write-kubeconfig-mode") && config["write-kubeconfig-mode"] != "0644" {
				t.Errorf("other settings lost: %v", config)
			}
			if tc.content != nil && strings.Contains(*tc.content, "servicelb") && !contains(items(config["disable"]), "servicelb") {
				t.Errorf("existing disable entry lost: %v", config["disable"])
			}
			// A second run finds nothing to change, so k3s is not restarted.
			if changed, err := ensureK3sConfig("203.0.113.10"); err != nil || changed {
				t.Errorf("second run changed=%v err=%v", changed, err)
			}
		})
	}
}

// k3s trims disabled components but not TLS SANs, so " 203.0.113.10" is not
// the server's address.
func TestTLSSANIsNotTrimmed(t *testing.T) {
	content := "disable: traefik\ntls-san: example.com, 203.0.113.10\n"
	withConfigFile(t, &content)
	if changed, err := ensureK3sConfig("203.0.113.10"); err != nil || !changed {
		t.Errorf("changed=%v err=%v, want the address added", changed, err)
	}
}

func items(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, item := range v {
			out = append(out, item.(string))
		}
		return out
	}
	return nil
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestParseDF(t *testing.T) {
	out := `Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/sda1         40470732 12345678  26037340      33% /`
	total, avail, ok := parseDF(out)
	if !ok || total != 40470732 || avail != 26037340 {
		t.Errorf("total=%d avail=%d ok=%v", total, avail, ok)
	}
	if _, _, ok := parseDF("Filesystem 1024-blocks Used Available Capacity Mounted on"); ok {
		t.Error("header only parsed")
	}
}

func TestParseMeminfo(t *testing.T) {
	info := parseMeminfo("MemTotal:        2014412 kB\nMemFree:          123456 kB\nMemAvailable:    1048576 kB\nSwapTotal:             0 kB\nHugePages_Total:       0\n")
	if info["MemTotal"] != 2014412 || info["MemAvailable"] != 1048576 || info["SwapTotal"] != 0 || len(info) != 5 {
		t.Errorf("info = %v", info)
	}
}

func TestHelmInstallIsPinned(t *testing.T) {
	if strings.Contains(helmInstallScript, "/main/") || strings.Count(helmInstallScript, helmVersion) != 2 ||
		!strings.Contains(helmInstallScript, "DESIRED_VERSION="+helmVersion) || strings.Contains(helmInstallScript, "VERIFY_CHECKSUM") {
		t.Errorf("helm install = %s", helmInstallScript)
	}
	if !strings.HasPrefix(helmVersion, "v3.") {
		t.Errorf("helm %s: ownstackctl and the pinned charts use Helm 3", helmVersion)
	}
}
