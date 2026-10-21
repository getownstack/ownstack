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

// helmStub puts a fake helm on PATH that answers `history` with history and
// records every invocation.
func helmStub(t *testing.T, history string) (*Kube, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "calls") + "\"\n" +
		"case \"$1\" in\n  history) " + history + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	return &Kube{Exec: &Exec{Console: NewConsole(&out)}}, filepath.Join(dir, "calls")
}

func helmCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		calls = append(calls, strings.Fields(line)[0])
	}
	return calls
}

func TestHelmUpgradeRecoversInterruptedRelease(t *testing.T) {
	tests := map[string]struct {
		history string
		want    []string
	}{
		"not installed":       {"echo 'Error: release: not found' >&2; exit 1", []string{"history", "upgrade"}},
		"deployed":            {`echo '[{"revision":1,"status":"deployed"}]'`, []string{"history", "upgrade"}},
		"interrupted upgrade": {`echo '[{"revision":1,"status":"deployed"},{"revision":2,"status":"pending-upgrade"}]'`, []string{"history", "rollback", "upgrade"}},
		"interrupted install": {`echo '[{"revision":1,"status":"pending-install"}]'`, []string{"history", "uninstall", "upgrade"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			k, calls := helmStub(t, tt.history)
			if err := k.HelmUpgrade(context.Background(), Chart{Release: "jenkins", Name: "jenkins", Repo: "https://charts.example", Version: "1.0.0", Namespace: "jenkins"}); err != nil {
				t.Fatal(err)
			}
			if got := helmCalls(t, calls); strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("helm calls %v, want %v", got, tt.want)
			}
		})
	}
}
