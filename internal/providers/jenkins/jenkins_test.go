package jenkins

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

func TestValuesRender(t *testing.T) {
	values, err := stack.Render(valuesTmpl, valuesData(jenkinsRuntime(t, ""), "pw"))
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
	for _, want := range []string{"IMAGE_REGISTRY", "harbor.admin.example.com", stack.LibraryRepoURL} {
		if !strings.Contains(casc, want) {
			t.Errorf("JCasC missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(casc), "slack") {
		t.Error("JCasC configures Slack")
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
// given as indented YAML, and spec.environments when any are given.
func jenkinsRuntime(t *testing.T, settings string, environments ...string) *stack.Runtime {
	t.Helper()
	rt, _ := providertest.Runtime(t, Spec)
	if settings != "" {
		m, err := manifest.Parse([]byte(strings.Replace(providertest.Manifest, "    provider: jenkins\n", "    provider: jenkins\n    settings:\n"+settings, 1)))
		if err != nil {
			t.Fatal(err)
		}
		rt.Manifest = m
	}
	rt.Manifest.Spec.Environments = environments
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: "https://api.github.com", ClusterRepoURL: "https://github.com/acme/ownstack-cluster.git"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "apps", PullSecretName: "registry-credentials"}
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

// Jenkins global env carries the production environment, its branch, and
// the mapping, the first match winning; the earlier variables keep their
// meaning for older libraries.
func TestValuesExportBranchMappings(t *testing.T) {
	for _, c := range []struct {
		environments                                 []string
		branches, deployBranch, production, triggers string
	}{
		{nil, "main=prod develop=dev release/*=qa", "main", "prod", `main|develop|release/.*`},
		{[]string{"prod=main"}, "main=prod", "main", "prod", "main"},
		{[]string{"qa=trunk", "dev=develop", "hotfix=hotfix/*"}, "trunk=qa develop=dev hotfix/*=hotfix", "trunk", "qa", `trunk|develop|hotfix/.*`},
		// NoTrigger takes a regular expression: release/1.0 must not also
		// match release/1x0.
		{[]string{"prod=release/1.0", "preview"}, "release/1.0=prod", "release/1.0", "prod", `release/1\.0`},
	} {
		rt := jenkinsRuntime(t, "", c.environments...)
		env := map[string]string{}
		for _, v := range valuesData(rt, "pw")["GlobalEnv"].([]envVar) {
			env[v.Key] = v.Value
		}
		if env["OWNSTACK_BRANCHES"] != c.branches || env["OWNSTACK_DEPLOY_BRANCH"] != c.deployBranch ||
			env["OWNSTACK_PRODUCTION_ENVIRONMENT"] != c.production || env["OWNSTACK_NAMESPACE"] != c.production {
			t.Errorf("environments %q: env %v", c.environments, env)
		}
		out, err := stack.Render(orgFolderTmpl, orgFolderData(rt))
		if err != nil {
			t.Fatal(err)
		}
		if got := folderTriggerBranches(t, out); got != c.triggers {
			t.Errorf("environments %q: org folder triggers %q, want %q", c.environments, got, c.triggers)
		}
	}

	// The values reach JCasC intact.
	values, err := stack.Render(valuesTmpl, valuesData(jenkinsRuntime(t, ""), "pw"))
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
	out, err := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "", "prod=main", "dev=develop", "qa=release/*", "rc=v1.*-rc")))
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
	for _, v := range valuesData(jenkinsRuntime(t, ""), "pw")["GlobalEnv"].([]envVar) {
		if !strings.Contains(lib.String(), "env."+v.Key) {
			t.Errorf("Jenkins global env sets %s, which the library never reads", v.Key)
		}
	}
}

// Jenkins loads the pipeline library from the Ownstack repository at the
// stack's library ref, and global env names the same repository and ref, so
// helmDeploy fetches common_helm_library from where the library came from.
// Cluster repos are not involved.
func TestLibrariesComeFromTheOwnstackRepoAtOneRef(t *testing.T) {
	saved := stack.EngineVersion
	t.Cleanup(func() { stack.EngineVersion = saved })
	for _, c := range []struct{ version, settings, want string }{
		{"dev", "", "main"},
		{"ownstackctl-v0.4.0", "", "ownstackctl-v0.4.0"},
		{"ownstackctl-v0.4.0", "      libraryRef: my-branch\n", "my-branch"},
	} {
		stack.EngineVersion = c.version
		rt := jenkinsRuntime(t, c.settings)
		data := valuesData(rt, "pw")
		env := map[string]string{}
		for _, v := range data["GlobalEnv"].([]envVar) {
			env[v.Key] = v.Value
		}
		if env["OWNSTACK_LIBRARY_REPO"] != "https://github.com/getownstack/ownstack.git" || env["OWNSTACK_LIBRARY_REF"] != c.want {
			t.Errorf("%s %q: library env = %q @ %q, want %s", c.version, c.settings, env["OWNSTACK_LIBRARY_REPO"], env["OWNSTACK_LIBRARY_REF"], c.want)
		}
		// Older pipeline libraries (a libraryRef naming an earlier release)
		// read the Helm library from here.
		if env["COMMON_HELM_LIBRARY_GITHUB_REPO"] != stack.LibraryRepoURL {
			t.Errorf("COMMON_HELM_LIBRARY_GITHUB_REPO = %q", env["COMMON_HELM_LIBRARY_GITHUB_REPO"])
		}
		values, err := stack.Render(valuesTmpl, data)
		if err != nil {
			t.Fatal(err)
		}
		library := globalLibrary(t, values)
		if library.DefaultVersion != c.want || library.Name != "jenkins_pipeline_library" || !library.Implicit {
			t.Errorf("%s %q: global library = %+v", c.version, c.settings, library)
		}
		git := library.Retriever.ModernSCM.SCM.Git
		if library.Retriever.ModernSCM.LibraryPath != "jenkins_pipeline_library/" || git.Remote != stack.LibraryRepoURL {
			t.Errorf("library retriever = %+v", library.Retriever)
		}
		// A release ref is a tag; without tag discovery it would not resolve.
		if !slices.Contains(git.Traits, "gitTagDiscovery") || !slices.Contains(git.Traits, "gitBranchDiscovery") {
			t.Errorf("library traits = %v", git.Traits)
		}
		// The repository is public; the VCS token is for the customer's repos.
		if git.CredentialsID != "" {
			t.Errorf("library uses credential %q", git.CredentialsID)
		}
		if !strings.Contains(libraryDetail(rt), "getownstack/ownstack@"+c.want) {
			t.Errorf("library detail = %q", libraryDetail(rt))
		}
	}

	deploy, err := os.ReadFile("../../../jenkins_pipeline_library/vars/helmDeploy.groovy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"if (env.OWNSTACK_LIBRARY_REPO) {",
		"def ref = env.OWNSTACK_LIBRARY_REF ?: 'main'",
		// Release refs are tags; stack.ReleaseTag accepts only ownstackctl-v*.
		"ref.startsWith('ownstackctl-v') ? 'refs/tags/' + ref : '*/' + ref",
		"remote = [url: env.OWNSTACK_LIBRARY_REPO]",
		"branches: [[name: branch]]",
	} {
		if !strings.Contains(string(deploy), want) {
			t.Errorf("helmDeploy.groovy does not contain %q", want)
		}
	}
}

type jcascLibrary struct {
	DefaultVersion string `yaml:"defaultVersion"`
	Implicit       bool   `yaml:"implicit"`
	Name           string `yaml:"name"`
	Retriever      struct {
		ModernSCM struct {
			LibraryPath string `yaml:"libraryPath"`
			SCM         struct {
				Git struct {
					CredentialsID string   `yaml:"credentialsId"`
					Remote        string   `yaml:"remote"`
					Traits        []string `yaml:"traits"`
				} `yaml:"git"`
			} `yaml:"scm"`
		} `yaml:"modernSCM"`
	} `yaml:"retriever"`
}

// globalLibrary is the one global library of rendered values' JCasC.
func globalLibrary(t *testing.T, values string) jcascLibrary {
	t.Helper()
	var parsed struct {
		Controller struct {
			JCasC struct {
				ConfigScripts map[string]string `yaml:"configScripts"`
			} `yaml:"JCasC"`
		}
	}
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatalf("invalid values YAML: %v", err)
	}
	var casc struct {
		Unclassified struct {
			GlobalLibraries struct {
				Libraries []jcascLibrary `yaml:"libraries"`
			} `yaml:"globalLibraries"`
		} `yaml:"unclassified"`
	}
	if err := yaml.Unmarshal([]byte(parsed.Controller.JCasC.ConfigScripts["main"]), &casc); err != nil {
		t.Fatalf("invalid JCasC YAML: %v", err)
	}
	if n := len(casc.Unclassified.GlobalLibraries.Libraries); n != 1 {
		t.Fatalf("JCasC has %d global libraries", n)
	}
	return casc.Unclassified.GlobalLibraries.Libraries[0]
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
	desired := func(settings string, environments ...string) string {
		out, err := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, settings, environments...)))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	same := desired("      repoFilter: app-* api-*\n", "prod=main")
	if changes, ok, err := folderChanges(same, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("rendered folder against itself: %v %v %v", changes, ok, err)
	}
	if changes, ok, err := folderChanges(jenkinsStored, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("stored folder with the same managed fields: %v %v %v", changes, ok, err)
	}
	changes, ok, err := folderChanges(jenkinsStored, desired("      repoFilter: \"*\"\n", "prod=trunk"))
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
	rt, out := providertest.Runtime(t, Spec)
	ctx := context.Background()

	unchanged, _ := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "      repoFilter: app-* api-*\n", "prod=main")))
	if err := convergeFolder(ctx, rt, api, "Repositories", jenkins.config, unchanged); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 0 {
		t.Errorf("posted without drift: %v", jenkins.posts)
	}

	changed, _ := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, "", "prod=main")))
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

func TestRegistryCredentialEscapesDollar(t *testing.T) {
	if got := shellEscapeDollar("robot$ownstack-ci"); got != `robot\$ownstack-ci` {
		t.Errorf("escaped = %q", got)
	}
}
