package ghactions

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

type workflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func workflowSteps(t *testing.T) []workflowStep {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(Workflow), &wf); err != nil {
		t.Fatalf("workflow is not valid YAML: %v", err)
	}
	return wf.Jobs["deploy"].Steps
}

// Images are built for the server's platform. Emulation is set up by a
// pinned action only when the runner's architecture differs, and the build
// stays a plain docker build and push (no buildx --push, which would push an
// index with attestations).
func TestWorkflowBuildsForTheServerPlatform(t *testing.T) {
	script := runScript(t)
	if !strings.Contains(script, `docker build --platform "${PLATFORM:-linux/amd64}" `) || !strings.Contains(script, `docker push "$image"`) {
		t.Error("build does not target the server's platform")
	}
	if strings.Contains(Workflow, "buildx") || strings.Contains(Workflow, "--push") {
		t.Error("use docker build and docker push")
	}
	var qemu *workflowStep
	steps := workflowSteps(t)
	for i, step := range steps {
		if step.Env["PLATFORM"] != "" && step.Env["PLATFORM"] != "${{ vars.OWNSTACK_PLATFORM }}" {
			t.Errorf("PLATFORM = %q", step.Env["PLATFORM"])
		}
		if strings.HasPrefix(step.Uses, "docker/setup-qemu-action@") {
			qemu = &steps[i]
		}
	}
	if qemu == nil {
		t.Fatal("no QEMU step")
	}
	// The job later holds the deploy credential: third-party actions are
	// pinned to a commit, and so is the image they run.
	if !regexp.MustCompile(`@[0-9a-f]{40}$`).MatchString(qemu.Uses) {
		t.Errorf("QEMU action not pinned to a commit: %s", qemu.Uses)
	}
	if image := qemu.With["image"]; image == "" || strings.HasSuffix(image, ":latest") {
		t.Errorf("binfmt image not pinned: %q", image)
	}
	if !strings.Contains(qemu.If, "vars.OWNSTACK_PLATFORM || 'linux/amd64'") || !strings.Contains(qemu.If, "runner.arch") {
		t.Errorf("QEMU condition = %q", qemu.If)
	}
}

// Hooks get the same environment as with Jenkins: the hook runs for real
// and prints what it received.
func TestWorkflowHookEnvironment(t *testing.T) {
	var block []string
	in := false
	for _, line := range strings.Split(runScript(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "APPLICATION_ID=") {
			in = true
		}
		if in {
			block = append(block, trimmed)
			if strings.Contains(trimmed, "before-deployment.sh") {
				break
			}
		}
	}
	if len(block) == 0 {
		t.Fatal("hook invocation not found")
	}
	dir := t.TempDir()
	hook := "env | grep -E '^(APPLICATION_ID|RELEASE_NAME|NAMESPACE|IMAGE|IMAGE_REGISTRY|IMAGE_REPOSITORY_PROJECT|IMAGE_TAG|IMAGE_PULL_SECRET|OWNSTACK_DOMAIN)=' | sort\n"
	if err := os.WriteFile(filepath.Join(dir, "before-deployment.sh"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", strings.Join(block, "\n"))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "dir=" + dir, "app=shop", "image=registry.example.com/apps/shop:main-0123456",
		"NAMESPACE=prod", "REGISTRY=registry.example.com", "PROJECT=apps", "tag=main-0123456",
		"PULL_SECRET=registry-credentials", "DOMAIN=example.com",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := strings.Join([]string{
		"APPLICATION_ID=shop", "IMAGE=registry.example.com/apps/shop:main-0123456", "IMAGE_PULL_SECRET=registry-credentials",
		"IMAGE_REGISTRY=registry.example.com", "IMAGE_REPOSITORY_PROJECT=apps", "IMAGE_TAG=main-0123456",
		"NAMESPACE=prod", "OWNSTACK_DOMAIN=example.com", "RELEASE_NAME=shop",
	}, "\n")
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("hook environment:\n%s\nwant:\n%s", got, want)
	}
}
