package jenkins

import (
	"bytes"
	"context"
	"flag"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/providers/slack"
	"github.com/getownstack/ownstack/internal/stack"
)

var update = flag.Bool("update", false, "rewrite testdata/values.golden.yaml")

const goldenPath = "testdata/values.golden.yaml"

// goldenHeader starts the golden values; helm reads them as values.
const goldenHeader = `# Helm values the jenkins provider renders for a representative Standard
# stack (Harbor, Slack alerts, default settings), with fake secrets. CI
# installs the locked chart with them. Regenerate after changing the values
# template or the lock:
#   go test ./internal/providers/jenkins -run TestValuesGolden -update
`

// The lock is well formed (ParseLock checks the header, sorting, and
// uniqueness) and covers every top-level plugin.
func TestLockCoversPluginsIn(t *testing.T) {
	if !bytes.HasPrefix(lockFile, []byte(pluginlock.Header+"\n")) {
		t.Fatal("plugins.lock does not start with the generated header")
	}
	lock, err := pluginlock.ParseLock(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Image != "jenkins/jenkins:"+lock.Jenkins+"-jdk21" || lock.ImageTag() != lock.Jenkins+"-jdk21" {
		t.Errorf("image %q for Jenkins %s", lock.Image, lock.Jenkins)
	}
	data, err := os.ReadFile("plugins.in")
	if err != nil {
		t.Fatal(err)
	}
	requested, err := pluginlock.ParseInput(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range requested {
		if _, ok := lock.Version(name); !ok {
			t.Errorf("plugins.in lists %s, which plugins.lock does not; run lockgen", name)
		}
	}
	if len(lock.Plugins) <= len(requested) {
		t.Errorf("plugins.lock has %d plugins for %d requested; dependencies are missing", len(lock.Plugins), len(requested))
	}
}

// The catalog's default chart is the one the plugins are locked for.
func TestCatalogChartMatchesLock(t *testing.T) {
	p, ok := catalog.Default().Provider("cicd", "jenkins")
	if !ok {
		t.Fatal("no jenkins provider")
	}
	param, ok := p.Param("chartVersion")
	if !ok || param.Default != pluginLock.Chart || catalog.JenkinsChartVersion != pluginLock.Chart {
		t.Errorf("catalog chartVersion %q (JenkinsChartVersion %q), plugins.lock chart %s; run lockgen", param.Default, catalog.JenkinsChartVersion, pluginLock.Chart)
	}
	if got := jenkinsRuntime(t, "").Setting("chartVersion"); got != pluginLock.Chart {
		t.Errorf("default chartVersion setting = %q", got)
	}
}

type renderedValues struct {
	Controller struct {
		Image struct {
			Tag string `yaml:"tag"`
		} `yaml:"image"`
		InstallPlugins                []string `yaml:"installPlugins"`
		InstallLatestPlugins          *bool    `yaml:"installLatestPlugins"`
		InstallLatestSpecifiedPlugins *bool    `yaml:"installLatestSpecifiedPlugins"`
		OverwritePlugins              *bool    `yaml:"overwritePlugins"`
	} `yaml:"controller"`
}

func renderValues(t *testing.T, rt *stack.Runtime) (string, renderedValues) {
	t.Helper()
	values, err := stack.Render(valuesTmpl, valuesData(rt, "pw"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed renderedValues
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatalf("invalid values YAML: %v\n%s", err, values)
	}
	return values, parsed
}

// Jenkins installs exactly the locked plugins on the locked core: no latest
// versions, every dependency pinned, and the image tag from the lock.
func TestValuesInstallLockedPlugins(t *testing.T) {
	_, v := renderValues(t, jenkinsRuntime(t, ""))
	c := v.Controller
	if c.InstallLatestPlugins == nil || *c.InstallLatestPlugins || c.InstallLatestSpecifiedPlugins == nil || *c.InstallLatestSpecifiedPlugins {
		t.Errorf("installLatestPlugins %v, installLatestSpecifiedPlugins %v; both must be false", c.InstallLatestPlugins, c.InstallLatestSpecifiedPlugins)
	}
	if c.OverwritePlugins == nil || !*c.OverwritePlugins {
		t.Error("overwritePlugins must be true, so the installed set is the locked set")
	}
	if c.Image.Tag != pluginLock.Jenkins+"-jdk21" {
		t.Errorf("image tag = %q, lock has Jenkins %s", c.Image.Tag, pluginLock.Jenkins)
	}
	var want []string
	for _, e := range pluginLock.Plugins {
		want = append(want, e.Name+":"+e.Version)
	}
	if !reflect.DeepEqual(c.InstallPlugins, want) {
		t.Errorf("installPlugins differ from plugins.lock:\n%v\n%v", c.InstallPlugins, want)
	}
}

// extraPlugins adds plugins, and replaces a locked plugin's version.
func TestValuesExtraPlugins(t *testing.T) {
	locked := pluginLock.Plugins[0]
	rt := jenkinsRuntime(t, "      extraPlugins: zz-extra:1.2.3 "+locked.Name+":999.v1\n")
	if err := (Provider{}).Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	_, v := renderValues(t, rt)
	plugins := v.Controller.InstallPlugins
	if len(plugins) != len(pluginLock.Plugins)+1 {
		t.Fatalf("%d plugins, want the %d locked plus one", len(plugins), len(pluginLock.Plugins))
	}
	got := strings.Join(plugins, " ")
	if !strings.Contains(got, " zz-extra:1.2.3") || !strings.HasPrefix(got, locked.Name+":999.v1 ") || strings.Contains(got, locked.String()) {
		t.Errorf("installPlugins = %v", plugins)
	}
	summary := pluginSummary(rt)
	if want := "Jenkins " + pluginLock.Jenkins + ", " + strconv.Itoa(len(pluginLock.Plugins)) + " locked plugins (+2 extra; " + locked.Name + " " + locked.Version + " → 999.v1)"; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}

	// The manifest's pattern rejects malformed values; resolving rejects
	// them too, before anything is installed.
	if err := (Provider{}).Resolve(context.Background(), jenkinsRuntime(t, "      extraPlugins: git\n")); err == nil || !strings.Contains(err.Error(), "extraPlugins") {
		t.Errorf("resolve with extraPlugins git = %v", err)
	}
}

// Plan names the Jenkins version and plugin count, and warns when the chart
// is not the one the plugins are locked for.
func TestPlanPlugins(t *testing.T) {
	rt := jenkinsRuntime(t, "")
	summary := pluginSummary(rt)
	if !strings.HasPrefix(summary, "Jenkins "+pluginLock.Jenkins+", ") || !strings.HasSuffix(summary, " locked plugins") {
		t.Errorf("summary = %q", summary)
	}
	var found bool
	for _, step := range (Provider{}).Plan(context.Background(), rt) {
		if step.Target == "plugins" {
			found = step.Detail == summary
		}
	}
	if !found {
		t.Error("plan has no plugins step")
	}
	if strings.Contains(summary, "warning") {
		t.Errorf("default chart warns: %q", summary)
	}
	// An explicit chartVersion keeps working, with a warning.
	other := jenkinsRuntime(t, "      chartVersion: 5.0.0\n")
	if err := (Provider{}).Resolve(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	want := "warning: plugins are locked for chart " + pluginLock.Chart + " / Jenkins " + pluginLock.Jenkins + "; a different chart may not start"
	if got := pluginSummary(other); got != summary+"; "+want {
		t.Errorf("summary with another chart = %q", got)
	}
}

// The golden values are what CI installs the locked chart with.
func TestValuesGolden(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: "https://api.github.com", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "product", PullSecretName: "harbor-robot"}
	if err := (slack.Provider{}).Resolve(context.Background(), rt.For("alerts")); err != nil {
		t.Fatal(err)
	}
	jrt := rt.For("cicd")
	password, err := jrt.Secret("adminPassword")
	if err != nil {
		t.Fatal(err)
	}
	values, err := stack.Render(valuesTmpl, valuesData(jrt, password))
	if err != nil {
		t.Fatal(err)
	}
	got := goldenHeader + values
	if !strings.HasSuffix(got, "\n") {
		got += "\n"
	}
	for _, secret := range providertest.Env {
		if secret != providertest.Env["JENKINS_ADMIN_PASSWORD"] && strings.Contains(got, secret) {
			t.Fatalf("golden values contain a secret other than the fake admin password")
		}
	}
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/providers/jenkins -run TestValuesGolden -update", err)
	}
	if string(want) != got {
		t.Errorf("%s is out of date; run go test ./internal/providers/jenkins -run TestValuesGolden -update", goldenPath)
	}
}
