package ghactions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// runScript extracts the shell of the build step.
func runScript(t *testing.T) string {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(Workflow), &wf); err != nil {
		t.Fatalf("workflow is not valid YAML: %v", err)
	}
	for _, step := range wf.Jobs["deploy"].Steps {
		if step.Run != "" {
			return step.Run
		}
	}
	t.Fatal("no run step")
	return ""
}

func TestWorkflowScriptParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.sh")
	os.WriteFile(path, []byte(runScript(t)), 0o600)
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// The image tag logic runs for real: branch names with slashes and other
// characters must become a valid tag.
func TestWorkflowImageTag(t *testing.T) {
	script := runScript(t)
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ref=") || strings.HasPrefix(trimmed, "tag=") {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("tag lines not found: %v", lines)
	}
	cmd := exec.Command("bash", "-c", strings.Join(lines, "\n")+"\necho \"$tag\"")
	cmd.Env = append(os.Environ(), "GITHUB_REF_NAME=feature/new ui!", "GITHUB_SHA=0123456789abcdef")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "feature-new-ui--0123456" {
		t.Errorf("tag = %q", got)
	}
}
