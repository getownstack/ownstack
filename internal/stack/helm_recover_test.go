package stack

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRecoveryAction(t *testing.T) {
	rev := func(n int, status string) helmRevision { return helmRevision{Revision: n, Status: status} }
	tests := map[string]struct {
		history []helmRevision
		action  string
		target  int
	}{
		"deployed":                  {[]helmRevision{rev(1, "superseded"), rev(2, "deployed")}, "", 0},
		"failed":                    {[]helmRevision{rev(1, "deployed"), rev(2, "failed")}, "", 0},
		"interrupted install":       {[]helmRevision{rev(1, "pending-install")}, "uninstall", 0},
		"interrupted first upgrade": {[]helmRevision{rev(1, "pending-upgrade")}, "uninstall", 0},
		"interrupted upgrade":       {[]helmRevision{rev(1, "superseded"), rev(2, "deployed"), rev(3, "pending-upgrade")}, "rollback", 2},
		"last good before failures": {[]helmRevision{rev(1, "deployed"), rev(2, "failed"), rev(3, "pending-upgrade")}, "rollback", 1},
		"never deployed":            {[]helmRevision{rev(1, "failed"), rev(2, "failed"), rev(3, "pending-upgrade")}, "rollback", 2},
		"interrupted rollback":      {[]helmRevision{rev(4, "superseded"), rev(5, "deployed"), rev(6, "pending-rollback")}, "rollback", 5},
	}
	for name, tt := range tests {
		action, target := recoveryAction(tt.history)
		if action != tt.action || target != tt.target {
			t.Errorf("%s: got %q %d, want %q %d", name, action, target, tt.action, tt.target)
		}
	}
}

// helmStub puts a fake helm on PATH that answers `history` with history,
// runs upgrade for `upgrade`, writes a chart archive for `pull`, and records
// every invocation. The state directory is a temporary one.
func helmStub(t *testing.T, history, upgrade string) (*Kube, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	t.Setenv("OWNSTACK_DIR", filepath.Join(dir, "state"))
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "calls") + "\"\n" +
		"case \"$1\" in\n" +
		"  history) " + history + " ;;\n" +
		"  upgrade) " + upgrade + " ;;\n" +
		"  pull) while [ $# -gt 0 ]; do [ \"$1\" = --destination ] && dest=$2; shift; done; echo chart > \"$dest/pulled.tgz\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	return &Kube{Exec: &Exec{Console: NewConsole(&out)}}, filepath.Join(dir, "calls")
}

// helmCalls returns the recorded invocations, full and by subcommand.
func helmCalls(t *testing.T, path string) (lines, verbs []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		lines = append(lines, line)
		verbs = append(verbs, strings.Fields(line)[0])
	}
	return lines, verbs
}

var testChart = Chart{Release: "jenkins", Name: "jenkins", Repo: "https://charts.example", Version: "1.0.0", Namespace: "jenkins"}

func TestHelmUpgradeRecoversInterruptedRelease(t *testing.T) {
	tests := map[string]struct {
		history string
		want    []string
	}{
		"not installed":       {"echo 'Error: release: not found' >&2; exit 1", []string{"pull", "history", "upgrade"}},
		"deployed":            {`echo '[{"revision":1,"status":"deployed"}]'`, []string{"pull", "history", "upgrade"}},
		"interrupted upgrade": {`echo '[{"revision":1,"status":"deployed"},{"revision":2,"status":"pending-upgrade"}]'`, []string{"pull", "history", "rollback", "upgrade"}},
		"interrupted install": {`echo '[{"revision":1,"status":"pending-install"}]'`, []string{"pull", "history", "uninstall", "upgrade"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			k, calls := helmStub(t, tt.history, "true")
			if err := k.HelmUpgrade(context.Background(), testChart); err != nil {
				t.Fatal(err)
			}
			if _, got := helmCalls(t, calls); strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("helm calls %v, want %v", got, tt.want)
			}
		})
	}
}

// The chart is pulled once into the state directory; every upgrade installs
// that archive, and a failing upgrade is not retried.
func TestHelmUpgradeUsesThePulledChart(t *testing.T) {
	k, calls := helmStub(t, "exit 1", "true")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := k.HelmUpgrade(ctx, testChart); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(StateFile("charts"), "jenkins-1.0.0.tgz")
	if data, err := os.ReadFile(archive); err != nil || string(data) != "chart\n" {
		t.Fatalf("archive %s: %q %v", archive, data, err)
	}
	lines, verbs := helmCalls(t, calls)
	if strings.Join(verbs, " ") != "pull history upgrade history upgrade" {
		t.Fatalf("helm calls %v", lines)
	}
	if !strings.HasPrefix(lines[0], "pull jenkins --repo https://charts.example --version 1.0.0 --destination "+StateFile("charts")) {
		t.Errorf("pull = %s", lines[0])
	}
	if want := "upgrade --install jenkins " + archive + " --namespace jenkins --create-namespace --wait --timeout 600s"; lines[2] != want || lines[4] != want {
		t.Errorf("upgrades:\n%s\n%s\nwant:\n%s", lines[2], lines[4], want)
	}
	if entries, _ := os.ReadDir(StateFile("charts")); len(entries) != 1 {
		t.Errorf("charts dir holds %v, want only the archive", entries)
	}

	k, calls = helmStub(t, "exit 1", "exit 1")
	if err := k.HelmUpgrade(ctx, testChart); err == nil {
		t.Fatal("failing upgrade succeeded")
	}
	if _, verbs := helmCalls(t, calls); strings.Join(verbs, " ") != "pull history upgrade" {
		t.Errorf("helm calls %v, want one upgrade", verbs)
	}
}
