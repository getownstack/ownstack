package ghactions

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/stack"
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

type workflowDoc struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
	} `yaml:"on"`
	Concurrency any `yaml:"concurrency"`
	Jobs        map[string]struct {
		RunsOn string         `yaml:"runs-on"`
		Steps  []workflowStep `yaml:"steps"`
	} `yaml:"jobs"`
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

// stepRun is the shell of the step with the given id or name.
func stepRun(t *testing.T, name string) string {
	t.Helper()
	for _, step := range workflowSteps(t) {
		if step.ID == name || step.Name == name {
			return step.Run
		}
	}
	t.Fatalf("no step %s", name)
	return ""
}

func resolveScript(t *testing.T) string { return stepRun(t, "resolve") }
func deployScript(t *testing.T) string  { return stepRun(t, "Build, push, and deploy") }

func needUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs the workflow's shell")
	}
}

func TestWorkflowScriptsParse(t *testing.T) {
	for _, step := range workflowSteps(t) {
		if step.Run == "" {
			continue
		}
		path := filepath.Join(t.TempDir(), "step.sh")
		os.WriteFile(path, []byte(step.Run), 0o600)
		if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("%s: bash -n: %v\n%s", step.Name, err, out)
		}
	}
}

// One job: pushes to any branch and manual runs (no inputs: redeploy the
// branch) start it, one run per branch at a time. The first step resolves
// the environment, and every later step runs only when there is one.
func TestWorkflowShape(t *testing.T) {
	wf := parseWorkflow(t)
	if got := wf.On.Push.Branches; len(got) != 1 || got[0] != "**" {
		t.Errorf("push branches = %v, want [**]", got)
	}
	var on struct {
		On map[string]any `yaml:"on"`
	}
	if err := yaml.Unmarshal([]byte(Workflow), &on); err != nil {
		t.Fatal(err)
	}
	if d, ok := on.On["workflow_dispatch"]; !ok || d != nil {
		t.Errorf("workflow_dispatch = %v (present %v), want present without inputs", d, ok)
	}
	if wf.Concurrency != "ownstack-${{ github.ref_name }}" {
		t.Errorf("concurrency = %v", wf.Concurrency)
	}
	if len(wf.Jobs) != 1 {
		t.Errorf("jobs = %v, want deploy only", wf.Jobs)
	}
	steps := workflowSteps(t)
	if len(steps) == 0 || steps[0].ID != "resolve" || steps[0].Uses != "" || steps[0].If != "" {
		t.Fatalf("the first step is not resolve: %+v", steps)
	}
	for name := range steps[0].Env {
		if strings.Contains(steps[0].Env[name], "secrets.") {
			t.Errorf("resolve reads a secret: %s", name)
		}
	}
	for _, step := range steps[1:] {
		if !strings.HasPrefix(step.If, "${{ steps.resolve.outputs.environment != ''") {
			t.Errorf("step %q runs without an environment (if %q)", step.Name+step.Uses, step.If)
		}
	}
	for _, step := range steps {
		if strings.Contains(step.Run, "${{") {
			t.Errorf("step %q interpolates expressions into its script; pass them through env", step.Name)
		}
	}
	// The repo carries one variable and one secret ownstackctl sets, and the
	// customer's optional runner label.
	reads := map[string]bool{}
	for _, m := range regexp.MustCompile(`(vars|secrets)\.[A-Z_]+`).FindAllString(Workflow, -1) {
		reads[m] = true
	}
	want := map[string]bool{"vars.OWNSTACK": true, "vars.OWNSTACK_RUNNER": true, "secrets.OWNSTACK_KUBECONFIG": true}
	if len(reads) != len(want) {
		t.Errorf("the workflow reads %v, want %v", reads, want)
	}
	for name := range want {
		if !reads[name] {
			t.Errorf("the workflow does not read %s", name)
		}
	}
	// Credentials come from the cluster, and no input reaches a script.
	for _, unwanted := range []string{"docker login", "inputs."} {
		if strings.Contains(Workflow, unwanted) {
			t.Errorf("the workflow contains %q", unwanted)
		}
	}
	if !strings.Contains(Workflow, "\n# ownstack-workflow: 5\n") {
		t.Error("the workflow is not marked ownstack-workflow 5")
	}
}

// The job takes common_helm_library from the Ownstack repository at the
// library ref in OWNSTACK.
func TestWorkflowFetchesTheLibraryAtTheLibraryRef(t *testing.T) {
	var library *workflowStep
	steps := workflowSteps(t)
	for i := range steps {
		if steps[i].With["path"] == ".ownstack" {
			library = &steps[i]
		}
	}
	if library == nil {
		t.Fatal("the job does not fetch the Helm library into .ownstack")
	}
	for key, want := range map[string]string{
		"repository":      stack.LibraryRepo,
		"ref":             "${{ steps.resolve.outputs.libraryRef }}",
		"sparse-checkout": "common_helm_library",
	} {
		if library.With[key] != want {
			t.Errorf("library step %s = %q, want %q", key, library.With[key], want)
		}
	}
	if library.Uses != "actions/checkout@v5" {
		t.Errorf("library step uses %s", library.Uses)
	}
}

const ownstackVar = `{"domain":"example.com","registry":"registry.admin.example.com/apps","platform":"linux/arm64",` +
	`"environments":["prod=main","dev=develop","qa=release/*"],"libraryRef":"ownstackctl-v0.4.0"}`

// resolveRun runs the resolve step for a branch and returns its outputs.
func resolveRun(t *testing.T, ownstack, branch string) (outputs map[string]string, log string, err error) {
	t.Helper()
	needUnix(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	output := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command("bash", "-c", resolveScript(t))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_OUTPUT=" + output, "GITHUB_REF_NAME=" + branch, "OWNSTACK=" + ownstack}
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

func withEnvironments(items string) string {
	return strings.Replace(ownstackVar, `["prod=main","dev=develop","qa=release/*"]`, items, 1)
}

// The environment is the first item whose glob matches the branch, with *
// matching across /; the first item is production. Unmapped branches
// resolve to no environment, so nothing else runs.
func TestWorkflowResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		ownstack, branch, environment, production string
	}{
		"main":                  {ownstackVar, "main", "prod", "prod"},
		"develop":               {ownstackVar, "develop", "dev", "prod"},
		"release glob":          {ownstackVar, "release/1.2", "qa", "prod"},
		"glob spans slashes":    {ownstackVar, "release/1.2/rc", "qa", "prod"},
		"glob is anchored":      {ownstackVar, "pre-release/1.2", "", "prod"},
		"unmapped":              {ownstackVar, "feature/login", "", "prod"},
		"name is not a prefix":  {ownstackVar, "main-old", "", "prod"},
		"first match wins":      {withEnvironments(`["prod=main","qa=release/*","dev=release/1.*"]`), "release/1.2", "qa", "prod"},
		"no glob":               {withEnvironments(`["prod=main","staging","dev=develop"]`), "develop", "dev", "prod"},
		"name is not a glob":    {withEnvironments(`["prod=main","staging","dev=develop"]`), "staging", "", "prod"},
		"production named live": {withEnvironments(`["live=trunk","dev=main"]`), "main", "dev", "live"},
		"catch-all":             {withEnvironments(`["prod=main","preview=*"]`), "feature/x", "preview", "prod"},
	} {
		t.Run(name, func(t *testing.T) {
			out, log, err := resolveRun(t, tc.ownstack, tc.branch)
			if err != nil {
				t.Fatalf("%v\n%s", err, log)
			}
			if _, ok := out["environment"]; !ok || out["environment"] != tc.environment || out["production"] != tc.production {
				t.Errorf("outputs = %v, want environment %q production %q\n%s", out, tc.environment, tc.production, log)
			}
		})
	}

	out, log, err := resolveRun(t, ownstackVar, "main")
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	for key, want := range map[string]string{
		"domain": "example.com", "registry": "registry.admin.example.com/apps", "platform": "linux/arm64", "libraryRef": "ownstackctl-v0.4.0",
	} {
		if out[key] != want {
			t.Errorf("output %s = %q, want %q", key, out[key], want)
		}
	}
	out, _, _ = resolveRun(t, strings.Replace(ownstackVar, `"platform":"linux/arm64",`, "", 1), "main")
	if out["platform"] != "linux/amd64" {
		t.Errorf("platform without one in OWNSTACK = %q, want linux/amd64", out["platform"])
	}

	for name, ownstack := range map[string]string{"unset": "", "not JSON": "prod=main"} {
		t.Run(name, func(t *testing.T) {
			out, log, err := resolveRun(t, ownstack, "main")
			if err == nil || out["environment"] != "" {
				t.Errorf("err = %v, outputs %v\n%s", err, out, log)
			}
			if ownstack == "" && !strings.Contains(log, "::error::The OWNSTACK variable is not set") {
				t.Errorf("log:\n%s", log)
			}
		})
	}
}

// hostRule is the workflow's Python for the deploy summary's host.
func hostRule(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nhost_rule='\n(.*?)\n'\n`).FindStringSubmatch(deployScript(t))
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

// Images are built for the server's platform. Emulation is set up by a
// pinned action only when the runner's architecture differs, and the build
// stays a plain docker build and push (no buildx --push, which would push an
// index with attestations).
func TestWorkflowBuildsForTheServerPlatform(t *testing.T) {
	script := deployScript(t)
	if !strings.Contains(script, `docker build --platform "$PLATFORM" `) || !strings.Contains(script, `docker push "$image"`) {
		t.Error("build does not target the server's platform")
	}
	if strings.Contains(Workflow, "buildx") || strings.Contains(Workflow, "--push") {
		t.Error("use docker build and docker push")
	}
	var qemu *workflowStep
	steps := workflowSteps(t)
	for i, step := range steps {
		if step.Env["PLATFORM"] != "" && step.Env["PLATFORM"] != "${{ steps.resolve.outputs.platform }}" {
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
	if !strings.Contains(qemu.If, "steps.resolve.outputs.platform != (runner.arch == 'ARM64' && 'linux/arm64' || 'linux/amd64')") {
		t.Errorf("QEMU condition = %q", qemu.If)
	}
}

// Stubs on PATH log their calls to $STUB/calls. docker manifest inspect
// succeeds for images listed in $STUB/existing, and docker push records the
// Docker config it pushes with; kubectl prints a dockerconfigjson; helm get
// values prints $STUB/values.json. Each records the KUBECONFIG it sees.
const (
	workflowDockerStub = `#!/bin/sh
echo "docker $*" >> "$STUB/calls"
case "$1 $2" in
  "manifest inspect") [ -f "$STUB/existing" ] && grep -qxF "$3" "$STUB/existing" || exit 1 ;;
  "push "*) cat "$DOCKER_CONFIG/config.json" > "$STUB/pushed-with" ;;
esac
`
	workflowKubectlStub = `#!/bin/sh
echo "kubectl $*" >> "$STUB/calls"
cat "$KUBECONFIG" > "$STUB/kubeconfig"
cat "$STUB/dockerconfigjson.b64"
`
	workflowHelmStub = `#!/bin/sh
echo "helm $*" >> "$STUB/calls"
if [ "$1" = get ]; then cat "$STUB/values.json"; fi
`
	dockerConfig = `{"auths":{"registry.admin.example.com":{"auth":"Y2k6czNjcmV0LXB3"}}}`
)

type deployRun struct {
	calls                           []string
	summary, pushedWith, kubeconfig string
	log                             string
	err                             error
}

// runDeploy runs the deploy step in a repo with the apps web and api, plus
// docs, which has no Dockerfile, as the resolve step's outputs for develop
// would.
func runDeploy(t *testing.T, env map[string]string, existing []string) deployRun {
	t.Helper()
	needUnix(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	stub, repo, temp := t.TempDir(), t.TempDir(), t.TempDir()
	files := map[string]string{
		filepath.Join(stub, "docker"):                                   workflowDockerStub,
		filepath.Join(stub, "kubectl"):                                  workflowKubectlStub,
		filepath.Join(stub, "helm"):                                     workflowHelmStub,
		filepath.Join(stub, "dockerconfigjson.b64"):                     base64.StdEncoding.EncodeToString([]byte(dockerConfig)),
		filepath.Join(stub, "values.json"):                              `{"id":"web","domain":"example.com","environment":"dev","productionEnvironment":"prod"}`,
		filepath.Join(stub, "existing"):                                 strings.Join(existing, "\n") + "\n",
		filepath.Join(repo, "infrastructure/web/Dockerfile"):            "FROM scratch\n",
		filepath.Join(repo, "infrastructure/web/helm/Chart.yaml"):       "apiVersion: v2\nname: web\nversion: 0.1.0\n",
		filepath.Join(repo, "infrastructure/api/Dockerfile"):            "FROM scratch\n",
		filepath.Join(repo, "infrastructure/api/helm/Chart.yaml"):       "apiVersion: v2\nname: api\nversion: 0.1.0\n",
		filepath.Join(repo, "infrastructure/docs/README.md"):            "no Dockerfile\n",
		filepath.Join(repo, ".ownstack/common_helm_library/Chart.yaml"): "apiVersion: v2\nname: common-helm-library\nversion: 0.1.0\ntype: library\n",
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
	cmd := exec.Command("bash", "-c", deployScript(t))
	cmd.Dir = repo
	cmd.Env = []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + t.TempDir(), "STUB=" + stub,
		"RUNNER_TEMP=" + temp, "GITHUB_STEP_SUMMARY=" + summary, "GITHUB_SHA=0123456789abcdef0123",
		"KUBECONFIG_DATA=kubeconfig-data", "ENVIRONMENT=dev", "PRODUCTION=prod", "DOMAIN=example.com",
		"REGISTRY=registry.admin.example.com/apps", "PLATFORM=linux/arm64",
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	run := deployRun{log: string(out), err: err}
	if data, _ := os.ReadFile(filepath.Join(stub, "calls")); len(data) > 0 {
		run.calls = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(stub, name))
		return string(data)
	}
	run.summary, run.pushedWith, run.kubeconfig = read("summary"), read("pushed-with"), read("kubeconfig")
	if strings.Contains(run.log+strings.Join(run.calls, "\n")+run.summary, "Y2k6czNjcmV0LXB3") {
		t.Error("the registry credential reached the log or a command line")
	}
	// The credentials stay in the runner's temporary directory, private.
	for _, name := range []string{"kubeconfig", "docker/config.json"} {
		if info, err := os.Stat(filepath.Join(temp, name)); err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
		}
	}
	return run
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

// A new commit builds every app once, tagged with the commit, logged in to
// the registry with the environment's registry-credentials secret, and
// deploys each into the environment, telling Helm which environment it is.
func TestWorkflowDeploysBuild(t *testing.T) {
	run := runDeploy(t, nil, nil)
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.log)
	}
	if got := callsWith(run.calls, "kubectl"); len(got) != 1 || got[0] != `kubectl -n dev get secret registry-credentials -o jsonpath={.data.\.dockerconfigjson}` {
		t.Errorf("kubectl calls = %v", got)
	}
	if run.kubeconfig != "kubeconfig-data" {
		t.Errorf("kubectl used kubeconfig %q", run.kubeconfig)
	}
	if run.pushedWith != dockerConfig {
		t.Errorf("docker pushed with config %q, want the cluster's dockerconfigjson", run.pushedWith)
	}
	inspects := callsWith(run.calls, "docker manifest inspect")
	if len(inspects) != 2 || inspects[0] != "docker manifest inspect registry.admin.example.com/apps/api:0123456789ab" {
		t.Errorf("manifest checks = %v", inspects)
	}
	builds := callsWith(run.calls, "docker build")
	if len(builds) != 2 || builds[0] != "docker build --platform linux/arm64 -f infrastructure/api/Dockerfile -t registry.admin.example.com/apps/api:0123456789ab ." {
		t.Errorf("builds = %v", builds)
	}
	if pushes := callsWith(run.calls, "docker push"); len(pushes) != 2 || pushes[1] != "docker push registry.admin.example.com/apps/web:0123456789ab" {
		t.Errorf("pushes = %v", pushes)
	}
	if logins := callsWith(run.calls, "docker login"); len(logins) != 0 {
		t.Errorf("docker login = %v", logins)
	}
	upgrades := callsWith(run.calls, "helm upgrade")
	if len(upgrades) != 2 {
		t.Fatalf("helm upgrades = %v", upgrades)
	}
	want := "helm upgrade --install web infrastructure/web/helm --namespace dev" +
		" --set image=registry.admin.example.com/apps/web:0123456789ab --set id=web --set domain=example.com" +
		" --set-string environment=dev --set-string productionEnvironment=prod --wait --timeout 5m"
	if upgrades[1] != want {
		t.Errorf("helm upgrade:\n%s\nwant:\n%s", upgrades[1], want)
	}
	if !strings.Contains(run.summary, "Deployed web (0123456789ab) to dev: https://web-dev.example.com") {
		t.Errorf("summary:\n%s", run.summary)
	}
}

// An image that is already in the registry (a rerun, or a commit another
// branch built) is deployed without building or pushing it again.
func TestWorkflowSkipsExistingImages(t *testing.T) {
	run := runDeploy(t, nil, []string{"registry.admin.example.com/apps/api:0123456789ab", "registry.admin.example.com/apps/web:0123456789ab"})
	if run.err != nil {
		t.Fatalf("%v\n%s", run.err, run.log)
	}
	if b, p := callsWith(run.calls, "docker build"), callsWith(run.calls, "docker push"); len(b)+len(p) != 0 {
		t.Errorf("built or pushed an existing image: %v %v", b, p)
	}
	if !strings.Contains(run.log, "registry.admin.example.com/apps/web:0123456789ab exists, skipping the build.") {
		t.Errorf("log:\n%s", run.log)
	}
	if upgrades := callsWith(run.calls, "helm upgrade"); len(upgrades) != 2 {
		t.Errorf("helm upgrades = %v", upgrades)
	}

	run = runDeploy(t, nil, []string{"registry.admin.example.com/apps/api:0123456789ab"})
	if b := callsWith(run.calls, "docker build"); run.err != nil || len(b) != 1 || !strings.Contains(b[0], "apps/web:") {
		t.Errorf("builds = %v (%v)\n%s", b, run.err, run.log)
	}
}

func TestWorkflowNeedsTheKubeconfig(t *testing.T) {
	run := runDeploy(t, map[string]string{"KUBECONFIG_DATA": ""}, nil)
	if run.err == nil || !strings.Contains(run.log, "::error::OWNSTACK_KUBECONFIG is not set") || len(run.calls) != 0 {
		t.Errorf("err = %v, calls %v\n%s", run.err, run.calls, run.log)
	}
}
