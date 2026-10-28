package jenkins

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// jenkinsRuntime is the test stack with Jenkins CI, the given cicd settings,
// and the given environments (the defaults when none), on Harbor.
func jenkinsRuntime(t *testing.T, settings map[string]string, environments ...string) *stack.Runtime {
	t.Helper()
	rt, _ := jenkinsRuntimeOut(t, settings, environments...)
	return rt
}

// jenkinsRuntimeOut is jenkinsRuntime with its console output.
func jenkinsRuntimeOut(t *testing.T, settings map[string]string, environments ...string) (*stack.Runtime, *bytes.Buffer) {
	t.Helper()
	rt, out := providertest.Runtime(t, Spec)
	providertest.Set(rt, "cicd", "jenkins", settings)
	rt.Manifest.Environments = environments
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: "https://api.github.com", Username: "acme", Token: "gh-token-value", DemoRepo: "lumen-yard"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "apps", PushUsername: "robot$ownstack-ci", PushPassword: "robot-pw"}
	return rt.For("cicd"), out
}

// jcasc is the main JCasC script of rendered values.
func jcasc(t *testing.T, values string) string {
	t.Helper()
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
	return parsed.Controller.JCasC.ConfigScripts["main"]
}

// globalEnv is the Jenkins global env of rendered values, in order.
func globalEnv(t *testing.T, rt *stack.Runtime) [][2]string {
	t.Helper()
	values, err := stack.Render(valuesTmpl, valuesData(rt, "pw"))
	if err != nil {
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
	if err := yaml.Unmarshal([]byte(jcasc(t, values)), &casc); err != nil || len(casc.Jenkins.GlobalNodeProperties) != 1 {
		t.Fatalf("JCasC: %v", err)
	}
	var out [][2]string
	for _, e := range casc.Jenkins.GlobalNodeProperties[0].EnvVars.Env {
		out = append(out, [2]string{e.Key, e.Value})
	}
	return out
}

// Jenkins global env is exactly the five C7 variables.
func TestGlobalEnv(t *testing.T) {
	for _, c := range []struct {
		environments []string
		want         string
	}{
		{nil, "prod=main dev=develop qa=release/*"},
		{[]string{"qa=trunk", "preview", "hotfix=hotfix/*"}, "qa=trunk preview hotfix=hotfix/*"},
	} {
		got := globalEnv(t, jenkinsRuntime(t, nil, c.environments...))
		want := [][2]string{
			{"OWNSTACK_DOMAIN", "example.com"},
			{"OWNSTACK_REGISTRY", "harbor.admin.example.com/apps"},
			{"OWNSTACK_ENVIRONMENTS", c.want},
			{"OWNSTACK_LIBRARY_REPO", "https://github.com/getownstack/ownstack.git"},
			{"OWNSTACK_LIBRARY_REF", "main"},
		}
		if !slices.Equal(got, want) {
			t.Errorf("environments %q: global env\n%v\nwant\n%v", c.environments, got, want)
		}
	}
	values, err := stack.Render(valuesTmpl, valuesData(jenkinsRuntime(t, nil), "pw"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(jcasc(t, values)), "slack") {
		t.Error("JCasC configures Slack")
	}
}

// librarySource is every step of the shared pipeline library, concatenated.
func librarySource(t *testing.T) string {
	t.Helper()
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
	return lib.String()
}

// Every variable in Jenkins global env is one the shared library reads, as
// env.KEY in Groovy or $KEY in a shell step.
func TestLibraryReadsGlobalEnv(t *testing.T) {
	lib := librarySource(t)
	for _, v := range globalEnv(t, jenkinsRuntime(t, nil)) {
		if !regexp.MustCompile(`(env\.|\$\{?)` + v[0] + `\b`).MatchString(lib) {
			t.Errorf("Jenkins global env sets %s, which the library never reads", v[0])
		}
	}
}

// Every credential the shared library uses is one the provider creates (C7).
func TestLibraryUsesProviderCredentials(t *testing.T) {
	created := map[string]bool{}
	for _, obj := range clusterObjects(jenkinsRuntime(t, nil)) {
		if obj["kind"] == "Secret" {
			created[obj["metadata"].(map[string]any)["name"].(string)] = true
		}
	}
	used := regexp.MustCompile(`credentialsId:\s*['"]([^'"]+)['"]`).FindAllStringSubmatch(librarySource(t), -1)
	if len(used) == 0 {
		t.Fatal("the library uses no credentialsId; the registry push needs one")
	}
	for _, m := range used {
		if !created[m[1]] {
			t.Errorf("the library uses credential %q, which the provider does not create (it creates %v)", m[1], created)
		}
	}
}

// Jenkins loads the pipeline library from the Ownstack repository at the
// running release's tag (main for development builds), and global env names
// the same repository and ref, so the Deploy stage clones common_helm_library
// from where the pipeline library came from.
func TestLibrariesComeFromTheOwnstackRepoAtOneRef(t *testing.T) {
	saved := stack.EngineVersion
	t.Cleanup(func() { stack.EngineVersion = saved })
	for version, want := range map[string]string{"dev": "main", "ownstackctl-v0.4.0-rc.1": "ownstackctl-v0.4.0-rc.1"} {
		stack.EngineVersion = version
		rt := jenkinsRuntime(t, nil)
		env := map[string]string{}
		for _, v := range globalEnv(t, rt) {
			env[v[0]] = v[1]
		}
		if env["OWNSTACK_LIBRARY_REPO"] != stack.LibraryRepoURL || env["OWNSTACK_LIBRARY_REF"] != want {
			t.Errorf("%s: library env = %q @ %q, want %s", version, env["OWNSTACK_LIBRARY_REPO"], env["OWNSTACK_LIBRARY_REF"], want)
		}
		values, err := stack.Render(valuesTmpl, valuesData(rt, "pw"))
		if err != nil {
			t.Fatal(err)
		}
		library := globalLibrary(t, values)
		if library.DefaultVersion != want || library.Name != "jenkins_pipeline_library" || !library.Implicit {
			t.Errorf("%s: global library = %+v", version, library)
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
	}

	pipeline, err := os.ReadFile("../../../jenkins_pipeline_library/vars/runPipeline.groovy")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`git clone( --quiet)? --depth 1 --branch "\$OWNSTACK_LIBRARY_REF" "\$OWNSTACK_LIBRARY_REPO"`).Match(pipeline) {
		t.Error(`runPipeline.groovy does not clone common_helm_library with --branch "$OWNSTACK_LIBRARY_REF" "$OWNSTACK_LIBRARY_REPO"`)
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
	var casc struct {
		Unclassified struct {
			GlobalLibraries struct {
				Libraries []jcascLibrary `yaml:"libraries"`
			} `yaml:"globalLibraries"`
		} `yaml:"unclassified"`
	}
	if err := yaml.Unmarshal([]byte(jcasc(t, values)), &casc); err != nil {
		t.Fatalf("invalid JCasC YAML: %v", err)
	}
	if n := len(casc.Unclassified.GlobalLibraries.Libraries); n != 1 {
		t.Fatalf("JCasC has %d global libraries", n)
	}
	return casc.Unclassified.GlobalLibraries.Libraries[0]
}

// The credentials are the VCS token and the registry push credential,
// stored as they are: the library never passes them through a shell.
func TestClusterObjects(t *testing.T) {
	objects := clusterObjects(jenkinsRuntime(t, nil))
	if objects[0]["kind"] != "Namespace" {
		t.Errorf("first object is %v, want the namespace", objects[0]["kind"])
	}
	credentials := map[string]map[string]string{}
	for _, obj := range objects {
		if obj["kind"] != "Secret" {
			continue
		}
		meta := obj["metadata"].(map[string]any)
		if meta["namespace"] != namespace || meta["labels"].(map[string]string)["jenkins.io/credentials-type"] != "usernamePassword" {
			t.Errorf("credential metadata %v", meta)
		}
		data := map[string]string{}
		if plain, ok := obj["stringData"].(map[string]string); ok {
			data = plain
		}
		if encoded, ok := obj["data"].(map[string]string); ok {
			for k, v := range encoded {
				raw, _ := base64.StdEncoding.DecodeString(v)
				data[k] = string(raw)
			}
		}
		credentials[meta["name"].(string)] = data
	}
	if len(credentials) != 2 ||
		credentials["github-pat"]["username"] != "acme" || credentials["github-pat"]["password"] != "gh-token-value" ||
		credentials["registry"]["username"] != "robot$ownstack-ci" || credentials["registry"]["password"] != "robot-pw" {
		t.Errorf("credentials = %v", credentials)
	}
}

func TestOrgFolderXML(t *testing.T) {
	out, err := stack.Render(orgFolderTmpl, map[string]string{
		"Owner": "acme", "APIURL": "https://api.github.com", "CredentialID": "github-pat", "Repos": "app <&>",
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
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("invalid XML: %v", err)
		}
	}
	if !strings.Contains(out, "<repoOwner>acme</repoOwner>") || !strings.Contains(out, "<includes>app &lt;&amp;&gt;</includes>") {
		t.Errorf("unexpected XML:\n%s", out)
	}
	// Fork pull requests run outside code on agents with the host Docker
	// socket and a cluster-admin service account; they must not be discovered.
	if strings.Contains(out, "ForkPullRequestDiscoveryTrait") {
		t.Error("org folder must not discover fork pull requests")
	}
}

// The organization folder builds the demo repo and the listed repos, and
// builds every environment's branch automatically. Java matches the trigger
// expression against the whole branch name, so it is checked anchored here.
func TestOrgFolderData(t *testing.T) {
	data := orgFolderData(jenkinsRuntime(t, map[string]string{"repos": " api  web Lumen-Yard api "}, "prod=main", "dev=develop", "qa=release/*", "preview", "rc=v1.*-rc"))
	if data["Repos"] != "lumen-yard api web" || data["JenkinsfilePath"] != "infrastructure/Jenkinsfile" || data["CredentialID"] != "github-pat" {
		t.Errorf("folder data = %v", data)
	}
	got := data["TriggerBranches"]
	if got != `main|develop|release/.*|v1\..*-rc` {
		t.Fatalf("branches = %q", got)
	}
	re := regexp.MustCompile(`^(?:` + got + `)$`)
	for branch, want := range map[string]bool{
		"main": true, "develop": true, "release/1.0": true, "release/a/b": true, "release/": true, "v1.2-rc": true, "v1.2/x-rc": true,
		"mainx": false, "xmain": false, "develop2": false, "release": false, "feature/x": false, "v1x2-rc": false, "PR-1": false, "preview": false,
	} {
		if re.MatchString(branch) != want {
			t.Errorf("trigger expression %q matches %q: %v, want %v", got, branch, !want, want)
		}
	}
	if g := globRegexp("*"); g != ".*" {
		t.Errorf("globRegexp(*) = %q", g)
	}
	rt := jenkinsRuntime(t, nil, "prod=release/1.0")
	rt.Out.VCS.DemoRepo = ""
	if data := orgFolderData(rt); data["Repos"] != "" || data["TriggerBranches"] != `release/1\.0` {
		t.Errorf("without repos: %v", data)
	}
	param, _ := Spec.Param("repos")
	for value, ok := range map[string]bool{"api": true, " api web ": true, "my.app_x": true, "acme/api": false, "app-*": false, "a,b": false} {
		if param.MatchesPattern(value) != ok {
			t.Errorf("repos %q accepted: %v", value, !ok)
		}
	}
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
          <includes>lumen-yard   api
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

func renderFolder(t *testing.T, repos string, environments ...string) string {
	t.Helper()
	out, err := stack.Render(orgFolderTmpl, orgFolderData(jenkinsRuntime(t, map[string]string{"repos": repos}, environments...)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFolderChanges(t *testing.T) {
	same := renderFolder(t, "api", "prod=main")
	if changes, ok, err := folderChanges(same, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("rendered folder against itself: %v %v %v", changes, ok, err)
	}
	if changes, ok, err := folderChanges(jenkinsStored, same); err != nil || !ok || len(changes) != 0 {
		t.Errorf("stored folder with the same managed fields: %v %v %v", changes, ok, err)
	}
	changes, ok, err := folderChanges(jenkinsStored, renderFolder(t, "", "prod=trunk"))
	if err != nil || !ok || strings.Join(changes, ", ") != `repositories "lumen-yard api" → "lumen-yard", auto-build branches "main" → "trunk"` {
		t.Errorf("changes = %v %v %v", changes, ok, err)
	}
	if _, ok, err := folderChanges(`<?xml version='1.1' encoding='UTF-8'?><com.cloudbees.hudson.plugins.folder.Folder/>`, same); ok || err != nil {
		t.Errorf("a plain folder is not ownstackctl's to update: %v %v", ok, err)
	}
	if _, _, err := folderChanges("<not xml", same); err == nil {
		t.Error("unreadable config accepted")
	}
}

// fakeJenkins answers the API, the crumb issuer, and one folder's
// config.xml, and records posts.
type fakeJenkins struct {
	mu     sync.Mutex
	config string
	posts  []string
}

func (f *fakeJenkins) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/json":
		w.Write([]byte(`{}`))
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
		f.posts = append(f.posts, r.URL.RequestURI()+" "+string(body))
	default:
		http.NotFound(w, r)
	}
}

func TestApplyFolderPostsOnlyOnDrift(t *testing.T) {
	jenkins := &fakeJenkins{config: jenkinsStored}
	srv := httptest.NewServer(http.HandlerFunc(jenkins.handler))
	defer srv.Close()
	api := &client{base: srv.URL, password: "pw", http: srv.Client()}
	ctx := context.Background()

	rt := jenkinsRuntime(t, map[string]string{"repos": "api"}, "prod=main")
	if err := applyFolder(ctx, rt, api); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 0 {
		t.Errorf("posted without drift: %v", jenkins.posts)
	}

	rt, out := jenkinsRuntimeOut(t, map[string]string{"repos": "api web"}, "prod=main")
	if err := applyFolder(ctx, rt, api); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 1 || !strings.HasPrefix(jenkins.posts[0], "/job/Repositories/config.xml <?xml") || !strings.Contains(jenkins.posts[0], "<includes>lumen-yard api web</includes>") {
		t.Errorf("posts = %v", jenkins.posts)
	}
	if !strings.Contains(out.String(), `Repositories updated: repositories "lumen-yard api" → "lumen-yard api web"`) {
		t.Errorf("log:\n%s", out)
	}

	jenkins.config, jenkins.posts = "", nil
	if err := applyFolder(ctx, rt, api); err != nil {
		t.Fatal(err)
	}
	if len(jenkins.posts) != 1 || !strings.HasPrefix(jenkins.posts[0], "/createItem?name=Repositories <?xml") {
		t.Errorf("posts = %v", jenkins.posts)
	}
}

// The stubs record events in $STUB/events. Docker's install (bash, since
// docker is not on PATH) and the Helm release each start, then wait for the
// other to have started before they finish: run one after the other, the
// first gives up after 3 seconds. With $HELM_FAIL the release fails as soon
// as it starts, and the install takes a moment longer to finish.
const (
	waitForStub = `wait_for() {
  i=0
  while [ ! -f "$STUB/$1" ]; do
    i=$((i + 1)); [ $i -lt 300 ] || exit 1
    /bin/sleep 0.01
  done
}
`
	kubectlStub = `#!/bin/sh
echo "kubectl $*" >> "$STUB/events"
case "$1" in
  apply) while read -r line; do :; done ;;
  *) exit 1 ;;
esac
`
	helmStub = `#!/bin/sh
` + waitForStub + `case "$1" in
  upgrade) echo "helm upgrade started" >> "$STUB/events"; : > "$STUB/helm-started"
    [ -z "$HELM_FAIL" ] || exit 1
    wait_for docker-started; echo "helm upgrade done" >> "$STUB/events" ;;
  pull) while [ $# -gt 0 ]; do if [ "$1" = --destination ]; then : > "$2/jenkins.tgz"; fi; shift; done ;;
  *) exit 1 ;;
esac
`
	bashStub = `#!/bin/sh
` + waitForStub + `while read -r line; do :; done
echo "docker install started" >> "$STUB/events"; : > "$STUB/docker-started"
wait_for helm-started; [ -z "$HELM_FAIL" ] || /bin/sleep 0.5
echo "docker install done" >> "$STUB/events"
`
)

// applyWithStubs runs Apply against the kubectl, helm, and bash stubs and a
// fake Jenkins API, and returns the stubs' events, the API's posts, the
// log, and Apply's error.
func applyWithStubs(t *testing.T, helmFail bool) (events, posts []string, out *bytes.Buffer, err error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	stub := t.TempDir()
	for name, script := range map[string]string{"kubectl": kubectlStub, "helm": helmStub, "bash": bashStub} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the stubs: a docker binary on the test machine must not skip the
	// install.
	t.Setenv("PATH", stub)
	t.Setenv("STUB", stub)
	t.Setenv("OWNSTACK_DIR", t.TempDir())
	t.Setenv("HELM_FAIL", "")
	if helmFail {
		t.Setenv("HELM_FAIL", "1")
	}

	jenkins := &fakeJenkins{}
	srv := httptest.NewTLSServer(http.HandlerFunc(jenkins.handler))
	defer srv.Close()
	rt, out := jenkinsRuntimeOut(t, map[string]string{"repos": "api"})
	// Every Jenkins request reaches the fake, whatever its host.
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	rt.ServiceHTTP = &http.Client{Transport: transport}

	err = (Provider{}).Apply(context.Background(), rt)
	data, readErr := os.ReadFile(filepath.Join(stub, "events"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	jenkins.mu.Lock()
	defer jenkins.mu.Unlock()
	return strings.Split(strings.TrimSpace(string(data)), "\n"), jenkins.posts, out, err
}

func TestApplyInstallsDockerAlongsideTheRelease(t *testing.T) {
	events, posts, out, err := applyWithStubs(t, false)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.HasPrefix(events[0], "kubectl apply") || !slices.Contains(events, "helm upgrade done") || !slices.Contains(events, "docker install done") {
		t.Errorf("events:\n%s", strings.Join(events, "\n"))
	}
	if len(posts) != 2 || !strings.HasPrefix(posts[0], "/createItem?name=Repositories ") || !strings.HasPrefix(posts[1], "/job/Repositories/build ") {
		t.Errorf("posts = %v", posts)
	}
	if !strings.Contains(out.String(), "Jenkins scanning acme/{lumen-yard,api}") {
		t.Errorf("log:\n%s", out)
	}
}

// A failed release fails apply, but only after the Docker install finished:
// killing apt halfway would leave dpkg broken on the server.
func TestFailedReleaseLetsDockerInstallFinish(t *testing.T) {
	events, posts, out, err := applyWithStubs(t, true)
	if err == nil || !strings.Contains(err.Error(), "helm") {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !slices.Contains(events, "docker install done") {
		t.Errorf("the Docker install was cut short:\n%s", strings.Join(events, "\n"))
	}
	if len(posts) != 0 {
		t.Errorf("configured Jenkins after a failed release: %v", posts)
	}
}
