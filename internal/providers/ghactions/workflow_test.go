package ghactions

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type workflowStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	If   string            `yaml:"if"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

type workflowJob struct {
	Needs       string            `yaml:"needs"`
	If          string            `yaml:"if"`
	RunsOn      string            `yaml:"runs-on"`
	Outputs     map[string]string `yaml:"outputs"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Steps []workflowStep `yaml:"steps"`
}

type workflowDoc struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
		WorkflowDispatch struct {
			Inputs map[string]struct {
				Required bool   `yaml:"required"`
				Default  string `yaml:"default"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	} `yaml:"on"`
	Concurrency any                    `yaml:"concurrency"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

func parseWorkflow(t *testing.T) workflowDoc {
	t.Helper()
	var wf workflowDoc
	if err := yaml.Unmarshal([]byte(Workflow), &wf); err != nil {
		t.Fatalf("workflow is not valid YAML: %v", err)
	}
	return wf
}

func workflowSteps(t *testing.T) []workflowStep {
	t.Helper()
	return parseWorkflow(t).Jobs["deploy"].Steps
}

// jobScript is the shell of a job's first run step.
func jobScript(t *testing.T, job string) string {
	t.Helper()
	for _, step := range parseWorkflow(t).Jobs[job].Steps {
		if step.Run != "" {
			return step.Run
		}
	}
	t.Fatalf("job %s has no run step", job)
	return ""
}

// runScript is the shell of the build and deploy step.
func runScript(t *testing.T) string {
	t.Helper()
	return jobScript(t, "deploy")
}

// scriptBlock returns the script's lines from the first one (trimmed) that
// starts with start through the next one that is exactly end (top level).
func scriptBlock(t *testing.T, script, start, end string) string {
	t.Helper()
	var block []string
	for _, line := range strings.Split(script, "\n") {
		if len(block) == 0 && !strings.HasPrefix(strings.TrimSpace(line), start) {
			continue
		}
		block = append(block, line)
		if len(block) > 1 && line == end {
			return strings.Join(block, "\n")
		}
	}
	t.Fatalf("no block from %q to %q", start, end)
	return ""
}

func needUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs the workflow's shell")
	}
}

func TestWorkflowScriptParses(t *testing.T) {
	for _, job := range []string{"resolve", "deploy"} {
		path := filepath.Join(t.TempDir(), job+".sh")
		os.WriteFile(path, []byte(jobScript(t, job)), 0o600)
		if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("%s: bash -n: %v\n%s", job, err, out)
		}
	}
}

// Pushes to any branch start the workflow; resolve decides whether it
// deploys. Manual runs may pick an environment and an image tag to promote.
func TestWorkflowTriggers(t *testing.T) {
	wf := parseWorkflow(t)
	if got := wf.On.Push.Branches; len(got) != 1 || got[0] != "**" {
		t.Errorf("push branches = %v, want [**]", got)
	}
	inputs := wf.On.WorkflowDispatch.Inputs
	for _, name := range []string{"environment", "tag"} {
		in, ok := inputs[name]
		if !ok || in.Required || in.Default != "" {
			t.Errorf("workflow_dispatch input %s = %+v (present %v), want optional and empty", name, in, ok)
		}
	}
	if len(inputs) != 2 {
		t.Errorf("inputs = %v", inputs)
	}
	if wf.Concurrency != nil {
		t.Errorf("workflow-level concurrency %v would serialize unrelated branches; deploy is grouped per environment", wf.Concurrency)
	}
}

// The deploy job runs only for a resolved environment, one at a time per
// environment, and deploys into it.
func TestWorkflowDeployJob(t *testing.T) {
	wf := parseWorkflow(t)
	resolve, deploy := wf.Jobs["resolve"], wf.Jobs["deploy"]
	if resolve.RunsOn != "ubuntu-latest" {
		t.Errorf("resolve runs on %q", resolve.RunsOn)
	}
	// resolve holds no credentials and checks nothing out.
	for _, step := range resolve.Steps {
		if step.Uses != "" {
			t.Errorf("resolve uses %s", step.Uses)
		}
		for name, value := range step.Env {
			if strings.Contains(value, "secrets.") {
				t.Errorf("resolve reads a secret: %s=%s", name, value)
			}
		}
		if strings.Contains(step.Run, "${{") {
			t.Error("resolve interpolates expressions into its script; pass them through env")
		}
	}
	if resolve.Outputs["environment"] == "" || resolve.Outputs["production"] == "" {
		t.Errorf("resolve outputs = %v", resolve.Outputs)
	}
	if deploy.Needs != "resolve" || !strings.Contains(deploy.If, "needs.resolve.outputs.environment != ''") {
		t.Errorf("deploy needs %q if %q", deploy.Needs, deploy.If)
	}
	if !strings.Contains(deploy.Concurrency.Group, "${{ needs.resolve.outputs.environment }}") || deploy.Concurrency.CancelInProgress {
		t.Errorf("deploy concurrency = %+v", deploy.Concurrency)
	}
	var env map[string]string
	for _, step := range deploy.Steps {
		if step.Run != "" {
			env = step.Env
			if strings.Contains(step.Run, "${{") {
				t.Error("deploy interpolates expressions into its script; pass them through env")
			}
		}
	}
	for name, want := range map[string]string{
		"NAMESPACE":              "${{ needs.resolve.outputs.environment }}",
		"PRODUCTION_ENVIRONMENT": "${{ needs.resolve.outputs.production }}",
		"PROMOTE_TAG":            "${{ inputs.tag }}",
	} {
		if env[name] != want {
			t.Errorf("deploy env %s = %q, want %q", name, env[name], want)
		}
	}
	script := runScript(t)
	if !strings.Contains(script, `--set-string environment="$NAMESPACE" --set-string productionEnvironment="$PRODUCTION_ENVIRONMENT"`) {
		t.Error("helm does not get environment and productionEnvironment")
	}
}

// resolveRun runs the resolve job's script and returns its outputs.
func resolveRun(t *testing.T, env map[string]string) (outputs map[string]string, log string, err error) {
	t.Helper()
	needUnix(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	cmd := exec.Command("bash", "-c", jobScript(t, "resolve"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + output, "GITHUB_EVENT_NAME=push"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	outputs = map[string]string{}
	data, _ := os.ReadFile(output)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			outputs[k] = v
		}
	}
	return outputs, string(out), err
}

// The environment comes from OWNSTACK_BRANCHES, first match wins, with *
// matching across /. A manual run may name any of the stack's environments.
// Repos whose stack was not applied since environments existed keep
// deploying main to OWNSTACK_NAMESPACE.
func TestWorkflowResolve(t *testing.T) {
	const branches = "main=prod develop=dev release/*=qa"
	stack := map[string]string{"BRANCHES": branches, "ENVIRONMENTS": "dev qa prod", "PRODUCTION": "prod", "NAMESPACE": "prod"}
	with := func(extra ...string) map[string]string {
		env := map[string]string{}
		for k, v := range stack {
			env[k] = v
		}
		for i := 0; i+1 < len(extra); i += 2 {
			if extra[i+1] == "<unset>" {
				delete(env, extra[i])
				continue
			}
			env[extra[i]] = extra[i+1]
		}
		return env
	}
	for name, tc := range map[string]struct {
		env         map[string]string
		environment string
		production  string
	}{
		"main":                     {with("GITHUB_REF_NAME", "main"), "prod", "prod"},
		"develop":                  {with("GITHUB_REF_NAME", "develop"), "dev", "prod"},
		"release glob":             {with("GITHUB_REF_NAME", "release/1.2"), "qa", "prod"},
		"glob spans slashes":       {with("GITHUB_REF_NAME", "release/1.2/rc"), "qa", "prod"},
		"glob is anchored":         {with("GITHUB_REF_NAME", "pre-release/1.2"), "", "prod"},
		"unmapped":                 {with("GITHUB_REF_NAME", "feature/login"), "", "prod"},
		"name is not a prefix":     {with("GITHUB_REF_NAME", "main-old"), "", "prod"},
		"first match wins":         {with("GITHUB_REF_NAME", "release/1.2", "BRANCHES", "main=prod release/*=qa release/1.*=dev"), "qa", "prod"},
		"malformed pair skipped":   {with("GITHUB_REF_NAME", "develop", "BRANCHES", "main=prod develop"), "", "prod"},
		"production named qa":      {with("GITHUB_REF_NAME", "main", "BRANCHES", "main=qa", "PRODUCTION", "qa", "NAMESPACE", "qa"), "qa", "qa"},
		"not applied since":        {with("GITHUB_REF_NAME", "main", "BRANCHES", "<unset>", "ENVIRONMENTS", "<unset>", "PRODUCTION", "<unset>", "NAMESPACE", "staging"), "staging", "staging"},
		"not applied, other":       {with("GITHUB_REF_NAME", "develop", "BRANCHES", "<unset>", "ENVIRONMENTS", "<unset>", "PRODUCTION", "<unset>"), "", "prod"},
		"nothing set":              {map[string]string{"GITHUB_REF_NAME": "main"}, "prod", "prod"},
		"dispatch picks qa":        {with("GITHUB_REF_NAME", "feature/login", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "qa"), "qa", "prod"},
		"dispatch input wins":      {with("GITHUB_REF_NAME", "main", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "dev"), "dev", "prod"},
		"dispatch by branch":       {with("GITHUB_REF_NAME", "develop", "GITHUB_EVENT_NAME", "workflow_dispatch"), "dev", "prod"},
		"dispatch, not applied":    {with("GITHUB_REF_NAME", "x", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "prod", "ENVIRONMENTS", "<unset>"), "prod", "prod"},
		"production falls back":    {with("GITHUB_REF_NAME", "develop", "PRODUCTION", "<unset>", "NAMESPACE", "live"), "dev", "live"},
		"production variable wins": {with("GITHUB_REF_NAME", "develop", "PRODUCTION", "qa"), "dev", "qa"},
	} {
		t.Run(name, func(t *testing.T) {
			out, log, err := resolveRun(t, tc.env)
			if err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			if out["environment"] != tc.environment || out["production"] != tc.production {
				t.Errorf("outputs = %v, want environment %q production %q\n%s", out, tc.environment, tc.production, log)
			}
			if _, ok := out["environment"]; !ok {
				t.Error("no environment output")
			}
		})
	}

	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"unknown environment": {with("GITHUB_REF_NAME", "main", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "staging"), "must be one of: dev qa prod"},
		// Only the production environment is known until the stack is reapplied.
		"not applied since": {with("GITHUB_REF_NAME", "main", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "dev", "ENVIRONMENTS", "<unset>", "BRANCHES", "<unset>"), "must be one of: prod"},
		"injected input":    {with("GITHUB_REF_NAME", "main", "GITHUB_EVENT_NAME", "workflow_dispatch", "REQUESTED", "prod\n::warning::pwned"), "must be one of"},
		"unmapped dispatch": {with("GITHUB_REF_NAME", "feature/login", "GITHUB_EVENT_NAME", "workflow_dispatch"), "Choose one with the environment input"},
		"bad mapping":       {with("GITHUB_REF_NAME", "develop", "BRANCHES", "main=prod develop=Dev_1"), "invalid environment"},
		"bad production":    {with("GITHUB_REF_NAME", "main", "PRODUCTION", "prod;x"), "OWNSTACK_PRODUCTION_ENVIRONMENT"},
	} {
		t.Run(name, func(t *testing.T) {
			out, log, err := resolveRun(t, tc.env)
			if err == nil || !strings.Contains(log, "::error::") || !strings.Contains(log, tc.want) {
				t.Errorf("err = %v, log:\n%s\nwant an error containing %q", err, log, tc.want)
			}
			if strings.Contains(log, "pwned") {
				t.Error("the environment input was echoed")
			}
			if out["environment"] != "" {
				t.Errorf("outputs = %v", out)
			}
		})
	}
}

// The image tag logic runs for real: branch names with slashes and other
// characters must become a valid tag, and a promotion takes the tag input
// only when it is a valid tag.
func TestWorkflowImageTag(t *testing.T) {
	needUnix(t)
	block := scriptBlock(t, runScript(t), "ref=", "fi")
	for _, tc := range []struct{ promote, want string }{
		{"", "feature-new-ui--0123456"},
		{"main-89abcde", "main-89abcde"},
		{"v1.2.3_rc", "v1.2.3_rc"},
		{"main-1; rm -rf /", ""},
		{"-x", ""},
		{"a/b", ""},
	} {
		cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+block+"\necho \"$tag\"")
		cmd.Env = append(os.Environ(), "GITHUB_REF_NAME=feature/new ui!", "GITHUB_SHA=0123456789abcdef", "PROMOTE_TAG="+tc.promote)
		out, err := cmd.CombinedOutput()
		got := strings.TrimSpace(string(out))
		if tc.want == "" {
			if err == nil || !strings.Contains(got, "::error::The tag input") {
				t.Errorf("tag input %q accepted: %s", tc.promote, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("tag input %q: tag = %q (%v), want %q", tc.promote, got, err, tc.want)
		}
	}
}

// hostRule is the workflow's Python for the deploy summary's host.
func hostRule(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nhost_rule='\n(.*?)\n'\n`).FindStringSubmatch(runScript(t))
	if m == nil {
		t.Fatal("host_rule not found")
	}
	return m[1]
}

// The summary prints the host the Helm library published (internal/helmlib
// holds the library to the same rule, rendering it).
func TestWorkflowHostRule(t *testing.T) {
	needUnix(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	rule := hostRule(t)
	for values, want := range map[string]string{
		`{"id":"app","domain":"example.com"}`:                                                                                                    "app.example.com",
		`{"id":"app","domain":"example.com","url":"shop.example.org"}`:                                                                           "shop.example.org",
		`{"id":"app","domain":"example.com","environment":"prod","productionEnvironment":"prod"}`:                                                "app.example.com",
		`{"id":"app","domain":"example.com","url":"shop.example.org","environment":"prod","productionEnvironment":"prod"}`:                       "shop.example.org",
		`{"id":"app","domain":"example.com","environment":"dev"}`:                                                                                "app.example.com",
		`{"id":"app","domain":"example.com","environment":"","productionEnvironment":"prod"}`:                                                    "app.example.com",
		`{"id":"app","domain":"example.com","environment":"dev","productionEnvironment":"prod"}`:                                                 "app-dev.example.com",
		`{"id":"app","domain":"example.com","url":"shop.example.org","environment":"dev","productionEnvironment":"prod"}`:                        "app-dev.example.com",
		`{"id":"app","environmentUrls":{"dev":"dev.shop.example.org"},"environment":"dev","productionEnvironment":"prod"}`:                       "dev.shop.example.org",
		`{"id":"app","domain":"example.com","environmentUrls":{"dev":"dev.shop.example.org"},"environment":"qa","productionEnvironment":"prod"}`: "app-qa.example.com",
		`{"id":"app","domain":"example.com","environment":{"region":"eu"},"productionEnvironment":"prod"}`:                                       "app.example.com",
		`{"id":"app","domain":"example.com","environment":123,"productionEnvironment":"prod"}`:                                                   "app-123.example.com",
	} {
		cmd := exec.Command("python3", "-c", rule)
		cmd.Stdin = strings.NewReader(values)
		out, err := cmd.CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != want {
			t.Errorf("%s: host = %q (%v), want %q", values, got, err, want)
		}
	}
}

// Hooks get the same environment as with Jenkins: the hook runs for real
// and prints what it received.
func TestWorkflowHookEnvironment(t *testing.T) {
	needUnix(t)
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
	hook := "env | grep -E '^(APPLICATION_ID|RELEASE_NAME|NAMESPACE|IMAGE|IMAGE_REGISTRY|IMAGE_REPOSITORY_PROJECT|IMAGE_TAG|IMAGE_PULL_SECRET|OWNSTACK_DOMAIN|OWNSTACK_ENVIRONMENT)=' | sort\n"
	if err := os.WriteFile(filepath.Join(dir, "before-deployment.sh"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", strings.Join(block, "\n"))
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "dir=" + dir, "app=shop", "image=registry.example.com/apps/shop:develop-0123456",
		"NAMESPACE=dev", "REGISTRY=registry.example.com", "PROJECT=apps", "tag=develop-0123456",
		"PULL_SECRET=registry-credentials", "DOMAIN=example.com",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := strings.Join([]string{
		"APPLICATION_ID=shop", "IMAGE=registry.example.com/apps/shop:develop-0123456", "IMAGE_PULL_SECRET=registry-credentials",
		"IMAGE_REGISTRY=registry.example.com", "IMAGE_REPOSITORY_PROJECT=apps", "IMAGE_TAG=develop-0123456",
		"NAMESPACE=dev", "OWNSTACK_DOMAIN=example.com", "OWNSTACK_ENVIRONMENT=dev", "RELEASE_NAME=shop",
	}, "\n")
	if got := strings.TrimSpace(string(out)); got != want {
		t.Errorf("hook environment:\n%s\nwant:\n%s", got, want)
	}
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
	// A promotion builds nothing, so it needs no emulation.
	if !strings.HasPrefix(qemu.If, "${{ !inputs.tag && ") {
		t.Errorf("QEMU condition = %q, want it skipped for promotions", qemu.If)
	}
}

// deployStubs puts docker and helm stubs on PATH that log their calls to
// $STUB/calls. docker manifest inspect fails for images listed in
// $STUB/missing; helm get values prints $STUB/values.json.
const (
	dockerStub = `#!/bin/sh
echo "docker $*" >> "$STUB/calls"
case "$1 $2" in
  "login "*) cat > /dev/null ;;
  "manifest inspect") if [ -f "$STUB/missing" ] && grep -qxF "$3" "$STUB/missing"; then exit 1; fi ;;
esac
`
	helmStub = `#!/bin/sh
echo "helm $*" >> "$STUB/calls"
if [ "$1" = get ]; then cat "$STUB/values.json"; fi
`
)

type deployRun struct {
	calls   []string
	summary string
	hook    string
	log     string
	err     error
}

// runDeploy runs the deploy step's script in a repo with the apps web (with
// a hook) and api, plus docs, which has no Dockerfile.
func runDeploy(t *testing.T, library string, env map[string]string, missing []string) deployRun {
	t.Helper()
	needUnix(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	stub, repo, home := t.TempDir(), t.TempDir(), t.TempDir()
	files := map[string]string{
		filepath.Join(stub, "docker"):                                               dockerStub,
		filepath.Join(stub, "helm"):                                                 helmStub,
		filepath.Join(stub, "values.json"):                                          `{"id":"web","domain":"example.com","environment":"dev","productionEnvironment":"prod"}`,
		filepath.Join(stub, "missing"):                                              strings.Join(missing, "\n") + "\n",
		filepath.Join(repo, "infrastructure/web/Dockerfile"):                        "FROM scratch\n",
		filepath.Join(repo, "infrastructure/web/helm/Chart.yaml"):                   "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		filepath.Join(repo, "infrastructure/web/before-deployment.sh"):              "echo \"$NAMESPACE $OWNSTACK_ENVIRONMENT $IMAGE_TAG\" > \"$STUB/hook\"\n",
		filepath.Join(repo, "infrastructure/api/Dockerfile"):                        "FROM scratch\n",
		filepath.Join(repo, "infrastructure/api/helm/Chart.yaml"):                   "apiVersion: v2\nname: api\nversion: 0.1.0\n",
		filepath.Join(repo, "infrastructure/docs/README.md"):                        "no Dockerfile\n",
		filepath.Join(repo, ".ownstack/common_helm_library/templates/_helpers.tpl"): library,
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	summary := filepath.Join(stub, "summary")
	cmd := exec.Command("bash", "-c", runScript(t))
	cmd.Dir = repo
	cmd.Env = []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + home, "STUB=" + stub,
		"GITHUB_STEP_SUMMARY=" + summary, "GITHUB_REF_NAME=develop", "GITHUB_SHA=0123456789abcdef",
		"KUBECONFIG_DATA=kubeconfig", "REGISTRY=registry.example.com", "REGISTRY_USERNAME=ci", "REGISTRY_PASSWORD=s3cret-pw",
		"PROJECT=apps", "DOMAIN=example.com", "PULL_SECRET=registry-credentials", "PLATFORM=linux/arm64",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	run := deployRun{log: string(out), err: err}
	if data, _ := os.ReadFile(filepath.Join(stub, "calls")); len(data) > 0 {
		run.calls = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	data, _ := os.ReadFile(summary)
	run.summary = string(data)
	data, _ = os.ReadFile(filepath.Join(stub, "hook"))
	run.hook = strings.TrimSpace(string(data))
	if strings.Contains(run.log+strings.Join(run.calls, "\n")+run.summary, "s3cret-pw") {
		t.Error("the registry password reached the log or a command line")
	}
	return run
}

func liveLibrary(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../../common_helm_library/templates/_helpers.tpl")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func callsWith(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// A push builds and deploys every app into the resolved environment, telling
// Helm and the hooks which environment it is.
func TestWorkflowDeploysBuild(t *testing.T) {
	run := runDeploy(t, liveLibrary(t), map[string]string{"NAMESPACE": "dev", "PRODUCTION_ENVIRONMENT": "prod"}, nil)
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.log)
	}
	builds := callsWith(run.calls, "docker build")
	if len(builds) != 2 || !strings.Contains(builds[0], "--platform linux/arm64") || !strings.Contains(builds[0], "registry.example.com/apps/api:develop-0123456") {
		t.Errorf("builds = %v", builds)
	}
	if pushes := callsWith(run.calls, "docker push"); len(pushes) != 2 {
		t.Errorf("pushes = %v", pushes)
	}
	if inspects := callsWith(run.calls, "docker manifest"); len(inspects) != 0 {
		t.Errorf("a build checked for existing images: %v", inspects)
	}
	upgrades := callsWith(run.calls, "helm upgrade")
	if len(upgrades) != 2 {
		t.Fatalf("helm upgrades = %v", upgrades)
	}
	for _, want := range []string{"--namespace dev", "--set-string environment=dev --set-string productionEnvironment=prod", "--set image=registry.example.com/apps/web:develop-0123456", "--set domain=example.com"} {
		if !strings.Contains(upgrades[1], want) {
			t.Errorf("helm upgrade %q lacks %q", upgrades[1], want)
		}
	}
	if run.hook != "dev dev develop-0123456" {
		t.Errorf("hook got %q", run.hook)
	}
	if !strings.Contains(run.summary, "Deployed web (develop-0123456) to dev: https://web-dev.example.com") {
		t.Errorf("summary:\n%s", run.summary)
	}
}

// A promotion deploys an existing tag everywhere without building, after
// checking every app's image exists.
func TestWorkflowPromotes(t *testing.T) {
	env := map[string]string{"NAMESPACE": "prod", "PRODUCTION_ENVIRONMENT": "prod", "PROMOTE_TAG": "release-1.2-89abcde"}
	run := runDeploy(t, liveLibrary(t), env, nil)
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.log)
	}
	if b := callsWith(run.calls, "docker build"); len(b) != 0 || len(callsWith(run.calls, "docker push")) != 0 {
		t.Errorf("a promotion built or pushed: %v", run.calls)
	}
	inspects := callsWith(run.calls, "docker manifest inspect")
	if len(inspects) != 2 || inspects[0] != "docker manifest inspect registry.example.com/apps/api:release-1.2-89abcde" {
		t.Errorf("manifest checks = %v", inspects)
	}
	upgrades := callsWith(run.calls, "helm upgrade")
	if len(upgrades) != 2 || !strings.Contains(upgrades[1], "--set image=registry.example.com/apps/web:release-1.2-89abcde") || !strings.Contains(upgrades[1], "--namespace prod") {
		t.Errorf("helm upgrades = %v", upgrades)
	}
	if run.hook != "prod prod release-1.2-89abcde" {
		t.Errorf("hook got %q", run.hook)
	}

	// One missing image stops the promotion before anything is deployed.
	run = runDeploy(t, liveLibrary(t), env, []string{"registry.example.com/apps/web:release-1.2-89abcde"})
	if run.err == nil || !strings.Contains(run.log, "::error::registry.example.com/apps/web:release-1.2-89abcde is not in the registry") {
		t.Errorf("err = %v\n%s", run.err, run.log)
	}
	if u := callsWith(run.calls, "helm"); len(u) != 0 || run.hook != "" {
		t.Errorf("deployed despite a missing image: %v", run.calls)
	}
}

// A library from before environments would publish another environment at
// the production host; production deploys still work with it.
func TestWorkflowRefusesOldLibraryOutsideProduction(t *testing.T) {
	old := "{{- define \"public-url-application\" -}}\n{{- end -}}\n"
	run := runDeploy(t, old, map[string]string{"NAMESPACE": "dev", "PRODUCTION_ENVIRONMENT": "prod"}, nil)
	if run.err == nil || !strings.Contains(run.log, "predates environments") || len(run.calls) != 0 {
		t.Errorf("err = %v, calls %v\n%s", run.err, run.calls, run.log)
	}
	run = runDeploy(t, old, map[string]string{"NAMESPACE": "prod", "PRODUCTION_ENVIRONMENT": "prod"}, nil)
	if run.err != nil || len(callsWith(run.calls, "helm upgrade")) != 2 {
		t.Errorf("production deploy with the old library: %v\n%s", run.err, run.log)
	}
}
