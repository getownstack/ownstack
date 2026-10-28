package jenkins

import (
	"bytes"
	"context"
	"flag"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

var update = flag.Bool("update", false, "rewrite testdata/values.golden.yaml")

const goldenPath = "testdata/values.golden.yaml"

// goldenHeader starts the golden values; helm reads them as values.
const goldenHeader = `# Helm values the jenkins provider renders for a representative Standard
# stack (Harbor, default settings), with fake secrets. CI installs the locked
# chart with them. Regenerate after changing the values template or the lock:
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
		if !slices.ContainsFunc(lock.Plugins, func(e pluginlock.Entry) bool { return e.Name == name }) {
			t.Errorf("plugins.in lists %s, which plugins.lock does not; run lockgen", name)
		}
	}
	if len(lock.Plugins) <= len(requested) {
		t.Errorf("plugins.lock has %d plugins for %d requested; dependencies are missing", len(lock.Plugins), len(requested))
	}
}

// Jenkins installs the chart the plugins are locked for.
func TestChartMatchesLock(t *testing.T) {
	if ChartVersion != pluginLock.Chart {
		t.Errorf("ChartVersion %q, plugins.lock chart %s; run lockgen", ChartVersion, pluginLock.Chart)
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
	_, v := renderValues(t, jenkinsRuntime(t, nil))
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
	rt := jenkinsRuntime(t, map[string]string{"extraPlugins": "zz-extra:1.2.3 " + locked.Name + ":999.v1"})
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

	// The manifest's pattern rejects malformed values; resolving rejects
	// them too, before anything is installed.
	if err := (Provider{}).Resolve(context.Background(), jenkinsRuntime(t, map[string]string{"extraPlugins": "git"})); err == nil || !strings.Contains(err.Error(), "extraPlugins") {
		t.Errorf("resolve with extraPlugins git = %v", err)
	}
}

// The golden values are what CI installs the locked chart with.
func TestValuesGolden(t *testing.T) {
	jrt := jenkinsRuntime(t, nil)
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
