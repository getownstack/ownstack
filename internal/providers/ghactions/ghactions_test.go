package ghactions

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/nacl/box"
	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// ghRuntime is the test stack with gh-actions as its CI provider and the
// given repos setting.
func ghRuntime(t *testing.T, repos string) (*stack.Runtime, *bytes.Buffer) {
	t.Helper()
	rt, out := providertest.Runtime(t, Spec)
	providertest.Set(rt, "cicd", "gh-actions", map[string]string{"repos": repos})
	return rt.For("cicd"), out
}

// kubectlStub answers like a cluster that issues the deploy token. Applied
// objects are appended to $STUB/applied and every call is logged to
// $STUB/calls.
const kubectlStub = `#!/bin/sh
echo "$*" >> "$STUB/calls"
case "$*" in
  apply*) cat >> "$STUB/applied"; echo >> "$STUB/applied" ;;
  "get secret ownstack-deployer-token -n ownstack-system -o json") echo '{"data":{"token":"ZGVwbG95LXRva2VuLTE="}}' ;;
  *) exit 1 ;;
esac
`

func stubKubectl(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB", dir)
	return dir
}

// fakeGitHub serves the endpoints the provider uses, keeps the repos'
// settings and workflow files, and records requests.
type fakeGitHub struct {
	mu        sync.Mutex
	requests  []string
	files     map[string]string            // owner/repo -> workflow content
	missing   map[string]bool              // owner/repo answering 404 to everything
	variables map[string]map[string]string // owner/repo -> name -> value
	secrets   map[string]map[string]string // owner/repo -> name -> decrypted value
	commits   []string                     // owner/repo@branch
	pub, priv *[32]byte
}

func newFakeGitHub(t *testing.T, files map[string]string, missing ...string) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{files: files, missing: map[string]bool{}, variables: map[string]map[string]string{},
		secrets: map[string]map[string]string{}, pub: pub, priv: priv}
	if f.files == nil {
		f.files = map[string]string{}
	}
	for _, m := range missing {
		f.missing[m] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || f.missing[parts[1]+"/"+parts[2]] {
		http.NotFound(w, r)
		return
	}
	repo, rest := parts[1]+"/"+parts[2], "/"+strings.Join(parts[3:], "/")
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodPatch && strings.HasPrefix(rest, "/actions/variables/"):
		name := strings.TrimPrefix(rest, "/actions/variables/")
		if _, ok := f.variables[repo][name]; !ok {
			http.NotFound(w, r)
			return
		}
		f.variables[repo][name] = body["value"]
	case r.Method == http.MethodPost && rest == "/actions/variables":
		if f.variables[repo] == nil {
			f.variables[repo] = map[string]string{}
		}
		f.variables[repo][body["name"]] = body["value"]
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && rest == "/actions/secrets/public-key":
		json.NewEncoder(w).Encode(map[string]string{"key_id": "k1", "key": base64.StdEncoding.EncodeToString(f.pub[:])})
	case r.Method == http.MethodPut && strings.HasPrefix(rest, "/actions/secrets/"):
		sealed, _ := base64.StdEncoding.DecodeString(body["encrypted_value"])
		opened, ok := box.OpenAnonymous(nil, sealed, f.pub, f.priv)
		if !ok || body["key_id"] != "k1" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if f.secrets[repo] == nil {
			f.secrets[repo] = map[string]string{}
		}
		f.secrets[repo][strings.TrimPrefix(rest, "/actions/secrets/")] = string(opened)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && rest == "/contents/"+workflowPath:
		content, ok := f.files[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
	case r.Method == http.MethodPut && rest == "/contents/"+workflowPath:
		content, _ := base64.StdEncoding.DecodeString(body["content"])
		f.files[repo] = string(content)
		// Without a branch, GitHub commits to the default branch.
		branch := body["branch"]
		if branch == "" {
			branch = "trunk"
		}
		f.commits = append(f.commits, repo+"@"+branch)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && rest == "/actions/workflows/"+workflowFile+"/runs":
		// Only main's runs succeeded; status must not read other branches'.
		conclusion := "failure"
		if r.URL.Query().Get("branch") == "main" {
			conclusion = "success"
		}
		json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"status": "completed", "conclusion": conclusion, "head_sha": "0123456789"}}})
	default:
		http.NotFound(w, r)
	}
}

// touched are the repos requests went to, sorted.
func (f *fakeGitHub) touched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		parts := strings.Split(strings.SplitN(r, " ", 2)[1], "/")
		if len(parts) >= 4 && parts[1] == "repos" && !slices.Contains(out, parts[2]+"/"+parts[3]) {
			out = append(out, parts[2]+"/"+parts[3])
		}
	}
	sort.Strings(out)
	return out
}

// apiServer is the cluster's API server as a runner reaches it: it lists
// prod's pods for the deploy token only.
func apiServer(t *testing.T) (url, caPEM string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/prod/pods" || r.Header.Get("Authorization") != "Bearer deploy-token-1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, `{"items":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// applyRuntime is a runtime ready to apply: the deploy identity, the API
// server, a registry, and GitHub with the demo repo.
func applyRuntime(t *testing.T, repos, github string) (*stack.Runtime, *bytes.Buffer, string) {
	t.Helper()
	dir := stubKubectl(t)
	server, ca := apiServer(t)
	rt, out := ghRuntime(t, repos)
	rt.Out.Compute = &stack.ComputeOutput{PublicIP: "203.0.113.10", APIServer: server, CACert: ca, Arch: "arm64"}
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: github, Token: "t", Username: "acme", DemoRepo: "lumen-yard"}
	rt.Out.Images = &stack.ImagesOutput{Host: "registry.admin.example.com", Project: "apps", PushUsername: "ownstack", PushPassword: "pw"}
	return rt, out, dir
}

// Apply configures the demo repo and the listed repos, and nothing else:
// one variable and one secret each, and the workflow only where it is
// missing. Existing workflows are never replaced.
func TestApplyConfiguresListedRepos(t *testing.T) {
	saved := stack.EngineVersion
	t.Cleanup(func() { stack.EngineVersion = saved })
	stack.EngineVersion = "ownstackctl-v0.4.0-rc.1"

	old := "# ownstack-workflow: 3\nname: ownstack\n"
	gh, srv := newFakeGitHub(t, map[string]string{"other-org/site": old})
	rt, out, dir := applyRuntime(t, "api other-org/site Lumen-Yard api", srv.URL)
	if err := (Provider{}).Apply(context.Background(), rt); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}

	repos := []string{"acme/api", "acme/lumen-yard", "other-org/site"}
	if got := gh.touched(); !slices.Equal(got, repos) {
		t.Errorf("touched %v, want %v", got, repos)
	}
	for _, r := range gh.requests {
		if !strings.Contains(r, " /repos/") {
			t.Errorf("request outside the listed repos (no discovery): %s", r)
		}
	}
	want := `{"domain":"example.com","registry":"registry.admin.example.com/apps","platform":"linux/arm64",` +
		`"environments":["prod=main","dev=develop","qa=release/*"],"libraryRef":"ownstackctl-v0.4.0-rc.1"}`
	for _, repo := range repos {
		if vars := gh.variables[repo]; len(vars) != 1 || vars["OWNSTACK"] != want {
			t.Errorf("%s variables = %v", repo, vars)
		}
		secrets := gh.secrets[repo]
		if len(secrets) != 1 {
			t.Errorf("%s has %d secrets, want only OWNSTACK_KUBECONFIG", repo, len(secrets))
			continue
		}
		var kubeconfig struct {
			Clusters []struct {
				Cluster struct{ Server string } `yaml:"cluster"`
			} `yaml:"clusters"`
			Users []struct {
				User struct{ Token string } `yaml:"user"`
			} `yaml:"users"`
		}
		if err := yaml.Unmarshal([]byte(secrets["OWNSTACK_KUBECONFIG"]), &kubeconfig); err != nil ||
			kubeconfig.Clusters[0].Cluster.Server != rt.Out.Compute.APIServer || kubeconfig.Users[0].User.Token != "deploy-token-1" {
			t.Errorf("%s OWNSTACK_KUBECONFIG is not the deploy kubeconfig: %v", repo, err)
		}
	}
	// Committed on the default branch, after the settings it needs exist.
	if !slices.Equal(gh.commits, []string{"acme/lumen-yard@trunk", "acme/api@trunk"}) {
		t.Errorf("commits = %v", gh.commits)
	}
	if gh.files["acme/api"] != Workflow || gh.files["other-org/site"] != old {
		t.Error("the workflow must be committed where missing and left alone where present")
	}
	for i, r := range gh.requests {
		if strings.HasSuffix(r, "/contents/"+workflowPath) && strings.HasPrefix(r, "PUT ") {
			repo := strings.Split(r, "/")[2] + "/" + strings.Split(r, "/")[3]
			if !slices.ContainsFunc(gh.requests[:i], func(prev string) bool {
				return strings.HasPrefix(prev, "PUT /repos/"+repo+"/actions/secrets/")
			}) {
				t.Errorf("%s: workflow committed before its secret", repo)
			}
		}
	}

	// The deploy identity is one kubectl apply, bound in every environment.
	applied, err := os.ReadFile(filepath.Join(dir, "applied"))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Kind  string
		Items []struct {
			Kind     string
			Metadata struct{ Name, Namespace string }
		}
	}
	if err := json.Unmarshal(bytes.TrimSpace(applied), &list); err != nil || list.Kind != "List" {
		t.Fatalf("applied %s: %v", applied, err)
	}
	var bound []string
	for _, item := range list.Items {
		if item.Kind == "RoleBinding" {
			bound = append(bound, item.Metadata.Namespace)
		}
	}
	if !slices.Equal(bound, []string{"prod", "dev", "qa"}) || list.Items[0].Kind != "Namespace" {
		t.Errorf("deploy identity %+v", list.Items)
	}

	// A rerun sets the settings again and commits nothing.
	before := len(gh.requests)
	if err := (Provider{}).Apply(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if len(gh.commits) != 2 {
		t.Errorf("rerun committed: %v", gh.commits)
	}
	// Per repo: the variable, the key and the secret, the workflow check.
	if n := len(gh.requests) - before; n != 4*len(repos) {
		t.Errorf("rerun made %d requests, want %d: %v", n, 4*len(repos), gh.requests[before:])
	}
}

// A listed repo that does not exist, or the token cannot administer, fails
// apply with its name.
func TestApplyNamesInaccessibleRepo(t *testing.T) {
	_, srv := newFakeGitHub(t, nil, "acme/typo")
	rt, _, _ := applyRuntime(t, "typo", srv.URL)
	err := Provider{}.Apply(context.Background(), rt)
	if err == nil || !strings.Contains(err.Error(), "acme/typo") || !strings.Contains(err.Error(), "cannot administer") {
		t.Errorf("err = %v", err)
	}
}

// Without the demo repo and repos, apply configures the deploy identity only.
func TestApplyWithoutRepos(t *testing.T) {
	gh, srv := newFakeGitHub(t, nil)
	rt, out, _ := applyRuntime(t, "", srv.URL)
	rt.Out.VCS.DemoRepo = ""
	if err := (Provider{}).Apply(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	if len(gh.requests) != 0 || !strings.Contains(out.String(), "No app repos yet") {
		t.Errorf("requests %v, log:\n%s", gh.requests, out)
	}
}

func TestTargets(t *testing.T) {
	rt, _ := ghRuntime(t, "  web  Other/Site acme/WEB other/site api ")
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", DemoRepo: "lumen-yard"}
	var got []string
	for _, tg := range targets(rt) {
		got = append(got, tg.String())
	}
	if strings.Join(got, " ") != "acme/lumen-yard acme/web Other/Site acme/api" {
		t.Errorf("targets = %v", got)
	}
	param, _ := Spec.Param("repos")
	for value, ok := range map[string]bool{
		"api": true, "api web other-org/site": true, " lumen-yard ": true, "my.app_x": true,
		"a//b": false, "owner/": false, "/x": false, "a,b": false, "org/name/x": false,
	} {
		if param.MatchesPattern(value) != ok {
			t.Errorf("repos %q accepted: %v", value, !ok)
		}
	}
}

// OWNSTACK carries every environment item, production first, and the
// server's platform (linux/amd64 when the compute provider reports none).
func TestRepoSettings(t *testing.T) {
	rt, _ := ghRuntime(t, "")
	rt.Manifest.Environments = []string{"prod=trunk", "staging", "dev=*"}
	rt.Out.Images = &stack.ImagesOutput{Host: "harbor.admin.example.com", Project: "apps"}
	got := repoSettings(rt)
	want := settings{Domain: "example.com", Registry: "harbor.admin.example.com/apps", Platform: "linux/amd64",
		Environments: []string{"prod=trunk", "staging", "dev=*"}, LibraryRef: stack.LibraryRef(stack.EngineVersion)}
	if !slices.Equal(got.Environments, want.Environments) || got.Domain != want.Domain || got.Registry != want.Registry ||
		got.Platform != want.Platform || got.LibraryRef != want.LibraryRef {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
}

// The workflow reads the two repo settings ownstackctl sets, every field it
// takes from OWNSTACK is one ownstackctl writes, and OWNSTACK_RUNNER stays
// the customer's.
func TestWorkflowReadsManagedSettings(t *testing.T) {
	read := map[string]bool{}
	for _, m := range regexp.MustCompile(`\b(vars|secrets)\.([A-Z_]+)`).FindAllStringSubmatch(Workflow, -1) {
		read[m[1]+"."+m[2]] = true
	}
	if len(read) != 3 || !read["vars.OWNSTACK"] || !read["secrets.OWNSTACK_KUBECONFIG"] || !read["vars.OWNSTACK_RUNNER"] {
		t.Errorf("workflow reads %v; ownstackctl sets vars.OWNSTACK and secrets.OWNSTACK_KUBECONFIG, the customer OWNSTACK_RUNNER", read)
	}

	data, err := json.Marshal(settings{})
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	if err := json.Unmarshal(data, &written); err != nil {
		t.Fatal(err)
	}
	// jq reads fields as \(.field) and .environments[].
	fields := regexp.MustCompile(`\\\(\.([a-zA-Z]+)|jq -r '\.([a-zA-Z]+)\[\]'`).FindAllStringSubmatch(Workflow, -1)
	seen := map[string]bool{}
	for _, m := range fields {
		field := m[1] + m[2]
		seen[field] = true
		if _, ok := written[field]; !ok {
			t.Errorf("workflow reads OWNSTACK .%s, which ownstackctl does not write", field)
		}
	}
	for field := range written {
		if !seen[field] {
			t.Errorf("ownstackctl writes OWNSTACK .%s, which the workflow never reads", field)
		}
	}
}

// Status checks only the demo and listed repos: the workflow file and the
// latest production run, two requests each, and warns about outdated
// workflows.
func TestStatusChecksListedRepos(t *testing.T) {
	stubKubectl(t)
	gh, srv := newFakeGitHub(t, map[string]string{
		"acme/app-web": Workflow,
		"shop/old":     "name: ownstack\non: push\n",
	})
	rt, _ := ghRuntime(t, "app-web shop/old")
	rt.Out.VCS = &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: srv.URL, Token: "t"}

	states := map[string]stack.Check{}
	for _, c := range (Provider{}).Status(context.Background(), rt) {
		states[c.Name] = c
	}
	if c := states["deploy identity"]; c.State != stack.OK {
		t.Errorf("deploy identity = %+v", c)
	}
	if c := states["pipeline acme/app-web"]; c.State != stack.OK {
		t.Errorf("current workflow = %+v", c)
	}
	if c := states["pipeline shop/old"]; c.State != stack.Warn || !strings.Contains(c.Detail, "ownstackctl workflow") {
		t.Errorf("outdated workflow = %+v", c)
	}
	if len(gh.requests) != 4 || !slices.Equal(gh.touched(), []string{"acme/app-web", "shop/old"}) {
		t.Errorf("requests = %v, want 2 per listed repo", gh.requests)
	}
}

func TestWorkflowMarker(t *testing.T) {
	// 5 reads the single OWNSTACK variable; status must warn about repos
	// with older copies.
	if workflowVersion < 5 {
		t.Fatalf("embedded workflow version = %d", workflowVersion)
	}
	for content, want := range map[string]int{
		"# ownstack-workflow: 3\nname: x\n":   3,
		"name: x\n#ownstack-workflow:7  \n":   7,
		"name: x\n":                           0,
		"  # ownstack-workflow: 3 (edited)\n": 0,
	} {
		if got := workflowMarker([]byte(content)); got != want {
			t.Errorf("marker(%q) = %d, want %d", content, got, want)
		}
	}
}

// The deploy token manages workloads in its namespaces but cannot change the
// namespace object: deleting it, or relabeling its Pod Security level.
func TestDeployerRole(t *testing.T) {
	rules := deployerRole()["rules"].([]map[string]any)
	var nsVerbs []string
	core := map[string]bool{}
	for _, rule := range rules {
		groups, resources, verbs := rule["apiGroups"].([]string), rule["resources"].([]string), rule["verbs"].([]string)
		if !slices.Contains(groups, "") {
			if slices.ContainsFunc(groups, func(g string) bool { return g == "rbac.authorization.k8s.io" || g == "*" }) {
				t.Errorf("rule grants %v", groups)
			}
			continue
		}
		if len(groups) != 1 {
			t.Errorf("core rule shares groups %v", groups)
		}
		for _, r := range resources {
			if r == "*" {
				t.Error("core resources must be listed, not a wildcard")
			}
			if r == "namespaces" {
				nsVerbs = append(nsVerbs, verbs...)
				continue
			}
			core[r] = slices.Contains(verbs, "*")
		}
	}
	if !slices.Equal(nsVerbs, []string{"get"}) {
		t.Errorf("namespace verbs = %v, want only get", nsVerbs)
	}
	for _, r := range []string{"pods", "pods/log", "services", "configmaps", "secrets", "serviceaccounts", "persistentvolumeclaims", "events"} {
		if !core[r] {
			t.Errorf("%s not fully manageable", r)
		}
	}
	for _, r := range []string{"nodes", "persistentvolumes"} {
		if _, ok := core[r]; ok {
			t.Errorf("%s is cluster-scoped and must not be listed", r)
		}
	}
}
