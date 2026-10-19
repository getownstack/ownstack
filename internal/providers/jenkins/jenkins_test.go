package jenkins

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/slack"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

func TestValuesRenderWithAlerts(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "product", PullSecretName: "harbor-robot"}
	if err := (slack.Provider{}).Resolve(context.Background(), rt.For("alerts")); err != nil {
		t.Fatal(err)
	}
	jrt := rt.For("cicd")
	values, err := stack.Render(valuesTmpl, valuesData(jrt, "pw"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Controller struct {
			JCasC struct {
				ConfigScripts map[string]string `yaml:"configScripts"`
			} `yaml:"JCasC"`
			Ingress struct {
				HostName string `yaml:"hostName"`
			}
		}
	}
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatalf("invalid values YAML: %v\n%s", err, values)
	}
	if parsed.Controller.Ingress.HostName != "jenkins.admin.example.com" {
		t.Errorf("host = %q", parsed.Controller.Ingress.HostName)
	}
	casc := parsed.Controller.JCasC.ConfigScripts["main"]
	var cascDoc map[string]any
	if err := yaml.Unmarshal([]byte(casc), &cascDoc); err != nil {
		t.Fatalf("invalid JCasC YAML: %v\n%s", err, casc)
	}
	for _, want := range []string{"IMAGE_REGISTRY", "harbor.admin.example.com", "OWNSTACK_SLACK_CHANNEL", "slackNotifier", "ownstack-cluster.git"} {
		if !strings.Contains(casc, want) {
			t.Errorf("JCasC missing %q", want)
		}
	}
	if strings.Contains(values, "xoxb-slack-token") {
		t.Error("Slack token must be a credential, not a value")
	}
}

func TestOrgFolderXML(t *testing.T) {
	out, err := stack.Render(orgFolderTmpl, map[string]string{
		"Owner": "acme", "APIURL": "https://api.github.com", "CredentialID": "github-pat", "RepoFilter": "app-* <&>",
		"JenkinsfilePath": "infrastructure/Jenkinsfile", "TriggerBranches": "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Jenkins writes XML 1.1, which encoding/xml refuses; the body is 1.0-compatible.
	body := out[strings.Index(out, "?>")+2:]
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("invalid XML: %v", err)
		}
	}
	if !strings.Contains(out, "<repoOwner>acme</repoOwner>") || !strings.Contains(out, "app-* &lt;&amp;&gt;") {
		t.Errorf("unexpected XML:\n%s", out)
	}
	// Fork pull requests run outside code on agents with the host Docker
	// socket and a cluster-admin service account; they must not be discovered.
	if strings.Contains(out, "ForkPullRequestDiscoveryTrait") {
		t.Error("org folder must not discover fork pull requests")
	}
}

// jenkinsRuntime is the test stack (Jenkins CI) with extra cicd settings,
// given as indented YAML.
func jenkinsRuntime(t *testing.T, settings string) *stack.Runtime {
	t.Helper()
	rt, _ := providertest.Runtime(t)
	if settings != "" {
		m, err := manifest.Parse([]byte(strings.Replace(providertest.Manifest, "    provider: jenkins\n", "    provider: jenkins\n    settings:\n"+settings, 1)))
		if err != nil {
			t.Fatal(err)
		}
		rt.Manifest = m
	}
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: "https://api.github.com", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "product", PullSecretName: "harbor-robot"}
	return rt.For("cicd")
}

func folderTriggerBranches(t *testing.T, xmlConfig string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<jenkins\.branch\.NoTriggerOrganizationFolderProperty>\s*<branches>([^<]*)</branches>`).FindStringSubmatch(xmlConfig)
	if m == nil {
		t.Fatal("org folder has no NoTriggerOrganizationFolderProperty branches")
	}
	return m[1]
}

// The org folder builds only the deploy branch automatically, and the shared
// library deploys only from it. Both default to the same branch, and the
// folder follows the deployBranch setting.
func TestOrgFolderTriggerMatchesLibraryDeployBranch(t *testing.T) {
	lib, err := os.ReadFile("../../../jenkins_pipeline_library/vars/values.groovy")
	if err != nil {
		t.Fatal(err)
	}
	deploy := regexp.MustCompile(`env\.OWNSTACK_DEPLOY_BRANCH \?: '([^']*)'`).FindSubmatch(lib)
	if deploy == nil {
		t.Fatal("values.groovy has no OWNSTACK_DEPLOY_BRANCH default")
	}
	rt := jenkinsRuntime(t, "")
	if got := deployBranch(rt); got != string(deploy[1]) || defaultDeployBranch != got {
		t.Errorf("default deploy branch %q (fallback %q), but the library deploys %q", got, defaultDeployBranch, deploy[1])
	}
	out, err := stack.Render(orgFolderTmpl, orgFolderData(rt))
	if err != nil {
		t.Fatal(err)
	}
	if got := folderTriggerBranches(t, out); got != string(deploy[1]) {
		t.Errorf("org folder triggers %q but the library deploys %q", got, deploy[1])
	}
	namespace := regexp.MustCompile(`env\.OWNSTACK_NAMESPACE \?: '([^']*)'`).FindSubmatch(lib)
	if namespace == nil || deployNamespace(rt) != string(namespace[1]) || defaultNamespace != string(namespace[1]) {
		t.Errorf("default namespace %q, library %q", deployNamespace(rt), namespace)
	}

	// NoTrigger takes a regular expression: release/1.0 must not also match release/1x0.
	out, err = stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "      deployBranch: release/1.0\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := folderTriggerBranches(t, out); got != `release/1\.0` {
		t.Errorf("branches = %q", got)
	}
}

// Jenkins global env carries the deploy branch and namespace the library reads.
func TestValuesExportDeploySettings(t *testing.T) {
	for _, c := range []struct{ settings, branch, namespace string }{
		{"", "main", "prod"},
		{"      deployBranch: trunk\n      environment: qa\n", "trunk", "qa"},
	} {
		env := map[string]string{}
		for _, v := range valuesData(jenkinsRuntime(t, c.settings), "pw")["GlobalEnv"].([]envVar) {
			env[v.Key] = v.Value
		}
		if env["OWNSTACK_DEPLOY_BRANCH"] != c.branch || env["OWNSTACK_NAMESPACE"] != c.namespace {
			t.Errorf("settings %q: env %v", c.settings, env)
		}
	}
}

func globalEnv(t *testing.T, settings string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, v := range valuesData(jenkinsRuntime(t, settings), "pw")["GlobalEnv"].([]envVar) {
		env[v.Key] = v.Value
	}
	return env
}

// OWNSTACK_BRANCHES is the effective mapping, deploy branch first, and
// OWNSTACK_PRODUCTION_ENVIRONMENT the environment the deploy branch deploys
// into. The earlier variables keep their meaning for older libraries.
func TestValuesExportBranchMappings(t *testing.T) {
	for _, c := range []struct{ settings, branches, production, namespace string }{
		{"", "main=prod", "prod", "prod"},
		{"      branches: develop=dev release/*=qa\n", "main=prod develop=dev release/*=qa", "prod", "prod"},
		{"      deployBranch: trunk\n      environment: qa\n      branches: \"  develop=dev\n        hotfix/*=qa \"\n", "trunk=qa develop=dev hotfix/*=qa", "qa", "qa"},
		// Validation rejects these; the first match wins regardless, so a
		// repeated or default branch (and a malformed pair) is dropped.
		{"      branches: main=dev develop=dev develop=qa broken =x y= a=b=c\n", "main=prod develop=dev", "prod", "prod"},
	} {
		env := globalEnv(t, c.settings)
		if env["OWNSTACK_BRANCHES"] != c.branches || env["OWNSTACK_PRODUCTION_ENVIRONMENT"] != c.production || env["OWNSTACK_NAMESPACE"] != c.namespace {
			t.Errorf("settings %q: env %v", c.settings, env)
		}
	}

	// The values reach JCasC intact.
	values, err := stack.Render(valuesTmpl, valuesData(jenkinsRuntime(t, "      branches: develop=dev release/*=qa\n"), "pw"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Controller struct {
			JCasC struct {
				ConfigScripts map[string]string `yaml:"configScripts"`
			} `yaml:"JCasC"`
		}
	}
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatal(err)
	}
	var casc struct {
		Jenkins struct {
			GlobalNodeProperties []struct {
				EnvVars struct {
					Env []struct{ Key, Value string }
				} `yaml:"envVars"`
			} `yaml:"globalNodeProperties"`
		}
	}
	if err := yaml.Unmarshal([]byte(parsed.Controller.JCasC.ConfigScripts["main"]), &casc); err != nil || len(casc.Jenkins.GlobalNodeProperties) != 1 {
		t.Fatalf("JCasC: %v", err)
	}
	found := map[string]string{}
	for _, e := range casc.Jenkins.GlobalNodeProperties[0].EnvVars.Env {
		found[e.Key] = e.Value
	}
	if found["OWNSTACK_BRANCHES"] != "main=prod develop=dev release/*=qa" || found["OWNSTACK_PRODUCTION_ENVIRONMENT"] != "prod" {
		t.Errorf("JCasC env = %v", found)
	}
}

// The org folder builds every mapped branch automatically. Java matches the
// expression against the whole branch name, so it is checked anchored here.
func TestOrgFolderTriggersEveryMappedBranch(t *testing.T) {
	out, err := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "      branches: develop=dev release/*=qa v1.*-rc=qa\n")))
	if err != nil {
		t.Fatal(err)
	}
	got := folderTriggerBranches(t, out)
	if got != `main|develop|release/.*|v1\..*-rc` {
		t.Fatalf("branches = %q", got)
	}
	re := regexp.MustCompile(`^(?:` + got + `)$`)
	for branch, want := range map[string]bool{
		"main": true, "develop": true, "release/1.0": true, "release/a/b": true, "release/": true, "v1.2-rc": true, "v1.2/x-rc": true,
		"mainx": false, "xmain": false, "develop2": false, "release": false, "feature/x": false, "v1x2-rc": false, "PR-1": false,
	} {
		if re.MatchString(branch) != want {
			t.Errorf("trigger expression %q matches %q: %v, want %v", got, branch, !want, want)
		}
	}
	if g := globRegexp("*"); g != ".*" {
		t.Errorf("globRegexp(*) = %q", g)
	}

	// Adding mappings to an existing folder rewrites it once.
	changes, ok, err := folderChanges(jenkinsStored, out)
	if err != nil || !ok || !strings.Contains(strings.Join(changes, ", "), `auto-build branches "main" → "main|develop|release/.*|v1\\..*-rc"`) {
		t.Errorf("changes = %v %v %v", changes, ok, err)
	}
}

// Every variable ownstackctl puts in Jenkins global env is one the shared
// library reads.
func TestLibraryReadsGlobalEnv(t *testing.T) {
	files, err := filepath.Glob("../../../jenkins_pipeline_library/vars/*.groovy")
	if err != nil || len(files) == 0 {
		t.Fatalf("no library sources: %v", err)
	}
	var lib strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lib.Write(data)
	}
	rt, _ := providertest.Runtime(t)
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "product", PullSecretName: "harbor-robot"}
	if err := (slack.Provider{}).Resolve(context.Background(), rt.For("alerts")); err != nil {
		t.Fatal(err)
	}
	for _, v := range valuesData(rt.For("cicd"), "pw")["GlobalEnv"].([]envVar) {
		if !strings.Contains(lib.String(), "env."+v.Key) {
			t.Errorf("Jenkins global env sets %s, which the library never reads", v.Key)
		}
	}
	// The library's fallback production environment is the namespace default.
	values, _ := os.ReadFile("../../../jenkins_pipeline_library/vars/values.groovy")
	if !strings.Contains(string(values), `env.OWNSTACK_PRODUCTION_ENVIRONMENT ?: (env.OWNSTACK_NAMESPACE ?: '`+defaultNamespace+`')`) {
		t.Error("values.groovy productionEnvironment does not fall back to OWNSTACK_NAMESPACE")
	}
}

func TestPlanDescribesBranchMappings(t *testing.T) {
	if got := deploySummary(branchMappings(jenkinsRuntime(t, ""))); got != "builds main automatically and deploys it to prod" {
		t.Errorf("default summary = %q", got)
	}
	if got := deploySummary(branchMappings(jenkinsRuntime(t, "      branches: develop=dev release/*=qa\n"))); got != "builds and deploys automatically main to prod, develop to dev, release/* to qa" {
		t.Errorf("summary = %q", got)
	}
}

// helmDeploy refuses a non-production deploy when the cluster repo's Helm
// library predates environments, by looking for a word in its templates. The
// live library must contain that word, or every such deploy would fail.
func TestHelmDeployRecognizesCurrentHelmLibrary(t *testing.T) {
	deploy, err := os.ReadFile("../../../jenkins_pipeline_library/vars/helmDeploy.groovy")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`grep -q (\w+) \./tmp-infrastructure/common_helm_library/templates/\*\.tpl`).FindSubmatch(deploy)
	if m == nil {
		t.Fatal("helmDeploy.groovy does not check the Helm library")
	}
	if !strings.Contains(string(deploy), "--set-string "+string(m[1])+"=") {
		t.Errorf("helmDeploy.groovy checks for %s but does not set it", m[1])
	}
	templates, err := filepath.Glob("../../../common_helm_library/templates/*.tpl")
	if err != nil || len(templates) == 0 {
		t.Fatalf("no library templates: %v", err)
	}
	for _, f := range templates {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), string(m[1])) {
			return
		}
	}
	t.Errorf("common_helm_library never mentions %s, so helmDeploy would refuse every non-production deploy", m[1])
}

// jenkinsStored is how Jenkins keeps a folder ownstackctl created: XML 1.1,
// plugin versions, normalized whitespace, an admin's extra trait, and no
// apiUri (older folders).
const jenkinsStored = `<?xml version='1.1' encoding='UTF-8'?>
<jenkins.branch.OrganizationFolder plugin="branch-api@2.1217">
  <properties>
    <jenkins.branch.NoTriggerOrganizationFolderProperty>
      <branches>main</branches>
      <strategy>NONE</strategy>
    </jenkins.branch.NoTriggerOrganizationFolderProperty>
  </properties>
  <navigators>
    <org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator plugin="github-branch-source@1810.v913311241fa_9">
      <repoOwner>acme</repoOwner>
      <credentialsId>github-pat</credentialsId>
      <traits>
        <jenkins.scm.impl.trait.WildcardSCMSourceFilterTrait plugin="scm-api@704.v3ce5c542825a_">
          <includes>app-*   api-*
          </includes>
          <excludes></excludes>
        </jenkins.scm.impl.trait.WildcardSCMSourceFilterTrait>
        <org.jenkinsci.plugins.github__branch__source.ForkPullRequestDiscoveryTrait>
          <strategyId>2</strategyId>
        </org.jenkinsci.plugins.github__branch__source.ForkPullRequestDiscoveryTrait>
      </traits>
    </org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator>
  </navigators>
  <projectFactories>
    <org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory plugin="workflow-multibranch@795.ve0cb_1f45ca_9a_">
      <scriptPath>infrastructure/Jenkinsfile</scriptPath>
    </org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory>
  </projectFactories>
</jenkins.branch.OrganizationFolder>`

func TestFolderChanges(t *testing.T) {
	desired := func(settings string) string {
		out, err := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, settings)))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	same := desired("      repoFilter: app-* api-*\n")
	if changes, ok, err := folderChanges(same, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("rendered folder against itself: %v %v %v", changes, ok, err)
	}
	if changes, ok, err := folderChanges(jenkinsStored, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("stored folder with the same managed fields: %v %v %v", changes, ok, err)
	}
	changes, ok, err := folderChanges(jenkinsStored, desired("      repoFilter: \"*\"\n      deployBranch: trunk\n"))
	if err != nil || !ok || strings.Join(changes, ", ") != `repository filter "app-* api-*" → "*", auto-build branches "main" → "trunk"` {
		t.Errorf("changes = %v %v %v", changes, ok, err)
	}
	if _, ok, err := folderChanges(`<?xml version='1.1' encoding='UTF-8'?><com.cloudbees.hudson.plugins.folder.Folder/>`, same); ok || err != nil {
		t.Errorf("a plain folder is not ownstackctl's to update: %v %v", ok, err)
	}
	if _, _, err := folderChanges("<not xml", same); err == nil {
		t.Error("unreadable config accepted")
	}
}

// fakeJenkins answers the crumb issuer and one folder's config.xml, and
// records posts.
type fakeJenkins struct {
	mu     sync.Mutex
	config string
	posts  []string
}

func (f *fakeJenkins) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/crumbIssuer/api/json":
		w.Write([]byte(`{"crumb":"c","crumbRequestField":"Jenkins-Crumb"}`))
	case r.URL.Path == "/job/Repositories/config.xml" && r.Method == http.MethodGet:
		if f.config == "" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(f.config))
	case r.Method == http.MethodPost:
		if r.Header.Get("Jenkins-Crumb") != "c" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.posts = append(f.posts, r.URL.Path+" "+string(body))
	default:
		http.NotFound(w, r)
	}
}

func TestConvergeFolderPostsOnlyOnDrift(t *testing.T) {
	jenkins := &fakeJenkins{config: jenkinsStored}
	srv := httptest.NewServer(http.HandlerFunc(jenkins.handler))
	defer srv.Close()
	api := &client{base: srv.URL, password: "pw", http: srv.Client()}
	rt, out := providertest.Runtime(t)
	ctx := context.Background()

	unchanged, _ := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "      repoFilter: app-* api-*\n")))
	if err := convergeFolder(ctx, rt, api, "Repositories", jenkins.config, unchanged); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 0 {
		t.Errorf("posted without drift: %v", jenkins.posts)
	}

	changed, _ := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "")))
	if err := convergeFolder(ctx, rt, api, "Repositories", jenkins.config, changed); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 1 || !strings.HasPrefix(jenkins.posts[0], "/job/Repositories/config.xml <?xml") || !strings.Contains(jenkins.posts[0], "<includes>*</includes>") {
		t.Errorf("posts = %v", jenkins.posts)
	}
	if !strings.Contains(out.String(), `Repositories updated: repository filter "app-* api-*" → "*"`) {
		t.Errorf("log:\n%s", out.String())
	}
}

// Plan says when apply will rewrite an existing folder, and what that costs.
func TestPlanReportsFolderUpdate(t *testing.T) {
	jenkins := &fakeJenkins{config: jenkinsStored}
	srv := httptest.NewTLSServer(http.HandlerFunc(jenkins.handler))
	defer srv.Close()
	folder := func(settings string) stack.Step {
		rt := jenkinsRuntime(t, settings)
		rt.ServiceHTTP = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			},
		}}
		return folderStep(context.Background(), rt)
	}
	if step := folder("      repoFilter: app-* api-*\n"); step.Action != "ensure" || !strings.Contains(step.Detail, "builds main automatically and deploys it to prod") {
		t.Errorf("unchanged folder: %+v", step)
	}
	step := folder("      jenkinsfilePath: ci/Jenkinsfile\n      repoFilter: app-* api-*\n")
	if step.Action != "update" || !strings.Contains(step.Detail, `Jenkinsfile path "infrastructure/Jenkinsfile" → "ci/Jenkinsfile"`) ||
		!strings.Contains(step.Detail, "deletes the jobs") {
		t.Errorf("drifted folder: %+v", step)
	}
	if len(jenkins.posts) != 0 {
		t.Errorf("plan changed Jenkins: %v", jenkins.posts)
	}
}

func TestRegistryCredentialEscapesDollar(t *testing.T) {
	if got := shellEscapeDollar("robot$ownstack-ci"); got != `robot\$ownstack-ci` {
		t.Errorf("escaped = %q", got)
	}
}
