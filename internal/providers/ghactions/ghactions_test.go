package ghactions

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

// ghRuntime is the test stack with gh-actions as its CI provider; settings
// is indented YAML under the cicd component.
func ghRuntime(t *testing.T, settings string) (*stack.Runtime, *bytes.Buffer) {
	t.Helper()
	rt, out := providertest.Runtime(t, Spec)
	src := strings.Replace(providertest.Manifest, "    provider: jenkins\n", "    provider: gh-actions\n"+settings, 1)
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	rt.Manifest = m
	return rt.For("cicd"), out
}

// kubectlStub answers like a cluster with the deploy token. The recorded
// targets come from $STUB/configmap.json when the test writes one; applied
// objects are written to $STUB/applied.json and every call is logged to
// $STUB/calls.
const kubectlStub = `#!/bin/sh
echo "$*" >> "$STUB/calls"
case "$*" in
  "apply -f -") cat > "$STUB/applied.json" ;;
  "get configmap ownstack-ci-targets -n ownstack-system -o json") [ -f "$STUB/configmap.json" ] && cat "$STUB/configmap.json" || exit 1 ;;
  "get secret ownstack-deployer-token -n ownstack-system -o json") echo '{"data":{"token":"dG9rZW4="}}' ;;
  *) exit 1 ;;
esac
`

func stubKubectl(t *testing.T, recorded *string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	if recorded != nil {
		cm, _ := json.Marshal(map[string]any{"data": map[string]string{"targets": *recorded}})
		if err := os.WriteFile(filepath.Join(dir, "configmap.json"), cm, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB", dir)
	return dir
}

// fakeGitHub serves the endpoints the provider uses and records requests.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []string
	repos    map[string][]string // owner -> repos
	files    map[string]string   // owner/repo -> workflow content
	denied   map[string]bool     // owner/repo whose contents answer 403
}

func (f *fakeGitHub) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		repo := ""
		if len(parts) >= 3 && parts[0] == "repos" {
			repo = parts[1] + "/" + parts[2]
		}
		switch {
		case r.URL.Path == "/user":
			json.NewEncoder(w).Encode(map[string]string{"login": "acme"})
		case r.URL.Path == "/user/repos" || (len(parts) == 3 && parts[0] == "orgs" && parts[2] == "repos"):
			owner := "acme"
			if parts[0] == "orgs" {
				owner = parts[1]
			}
			var list []map[string]any
			for _, name := range f.repos[owner] {
				list = append(list, map[string]any{"name": name, "owner": map[string]string{"login": owner}})
			}
			json.NewEncoder(w).Encode(list)
		case f.denied[repo]:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource protected by organization SAML enforcement"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/contents/"+workflowPath):
			content, ok := f.files[repo]
			if !ok {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))})
		case strings.HasSuffix(r.URL.Path, "/runs"):
			// Only main's runs succeeded; status must not read other branches'.
			conclusion := "failure"
			if r.URL.Query().Get("branch") == "main" {
				conclusion = "success"
			}
			json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]string{{"status": "completed", "conclusion": conclusion, "head_sha": "0123456789"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHub) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func vcs(api string) *stack.VCSOutput {
	return &stack.VCSOutput{Kind: "github", Owner: "acme", APIURL: api, Token: "t", Username: "acme", ClusterRepo: "ownstack-cluster"}
}

// App repos must never get an empty registry credential.
func TestApplyRefusesWithoutPushCredential(t *testing.T) {
	for _, images := range []*stack.ImagesOutput{
		nil,
		{Host: "registry.admin.example.com", Project: "apps", PushUsername: "ownstack"},
		{Host: "registry.admin.example.com", Project: "apps", PushPassword: "pw"},
	} {
		rt, _ := ghRuntime(t, "")
		rt.Out.VCS = vcs("http://127.0.0.1:1")
		rt.Out.Images = images
		err := Provider{}.Apply(context.Background(), rt)
		if err == nil || !strings.Contains(err.Error(), "push credential") {
			t.Errorf("images %+v: err = %v", images, err)
		}
	}
}

// defaultEnvironments are spec.environments' defaults.
var defaultEnvironments = []stack.Environment{{Name: "prod", Branch: "main"}, {Name: "dev", Branch: "develop"}, {Name: "qa", Branch: "release/*"}}

func TestRepoVariables(t *testing.T) {
	images := &stack.ImagesOutput{Host: "registry.admin.example.com", Project: "apps", PushUsername: "ownstack", PushPassword: "pw", PullSecretName: "registry-credentials"}
	vars := repoVariables(images, "example.com", defaultEnvironments, "ownstackctl-v0.3.0", "arm64")
	want := map[string]string{
		"OWNSTACK_REGISTRY":          "registry.admin.example.com",
		"OWNSTACK_REGISTRY_USERNAME": "ownstack",
		"OWNSTACK_IMAGE_PROJECT":     "apps",
		"OWNSTACK_PULL_SECRET":       "registry-credentials",
		"OWNSTACK_DOMAIN":            "example.com",
		// Kept for workflows committed before ownstack-workflow 3.
		"OWNSTACK_NAMESPACE":              "prod",
		"OWNSTACK_PRODUCTION_ENVIRONMENT": "prod",
		"OWNSTACK_BRANCHES":               "main=prod develop=dev release/*=qa",
		"OWNSTACK_ENVIRONMENTS":           "prod dev qa",
		"OWNSTACK_LIBRARY_REF":            "ownstackctl-v0.3.0",
		"OWNSTACK_PLATFORM":               "linux/arm64",
	}
	if len(vars) != len(want) {
		t.Errorf("variables = %v", vars)
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
	}
	for _, v := range vars {
		if v == "pw" {
			t.Error("the registry password must be a secret, not a variable")
		}
	}
	// Without a known architecture the workflow keeps its linux/amd64 default.
	if _, ok := repoVariables(images, "example.com", defaultEnvironments, "main", "")["OWNSTACK_PLATFORM"]; ok {
		t.Error("OWNSTACK_PLATFORM set without an architecture")
	}
}

// Everything the workflow reads is set by ownstackctl, except
// OWNSTACK_RUNNER, which belongs to the customer.
func TestWorkflowReadsManagedSettings(t *testing.T) {
	set := repoVariables(&stack.ImagesOutput{}, "", defaultEnvironments, "", "amd64")
	for _, m := range regexp.MustCompile(`vars\.(OWNSTACK_[A-Z_]+)`).FindAllStringSubmatch(Workflow, -1) {
		_, ok := set[m[1]]
		if m[1] == "OWNSTACK_RUNNER" && ok {
			t.Error("OWNSTACK_RUNNER is the customer's; ownstackctl must not manage it")
		} else if m[1] != "OWNSTACK_RUNNER" && !ok {
			t.Errorf("workflow reads %s, which ownstackctl does not set", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`secrets\.(OWNSTACK_[A-Z_]+)`).FindAllStringSubmatch(Workflow, -1) {
		if m[1] != "OWNSTACK_KUBECONFIG" && m[1] != "OWNSTACK_REGISTRY_PASSWORD" {
			t.Errorf("workflow reads secret %s, which ownstackctl does not set", m[1])
		}
	}
}

// Every environment with a branch deploys, in order; the first match wins.
func TestBranchMap(t *testing.T) {
	for _, tc := range []struct {
		environments []stack.Environment
		branches     string
		deploys      string
	}{
		{defaultEnvironments, "main=prod develop=dev release/*=qa", "main to prod, develop to dev, release/* to qa"},
		{[]stack.Environment{{Name: "prod", Branch: "trunk"}, {Name: "staging"}, {Name: "dev", Branch: "*"}}, "trunk=prod *=dev", "trunk to prod, * to dev"},
		{[]stack.Environment{{Name: "prod"}}, "", "nothing on push"},
	} {
		if got := formatBranches(branchMap(tc.environments)); got != tc.branches {
			t.Errorf("%v: OWNSTACK_BRANCHES = %q, want %q", tc.environments, got, tc.branches)
		}
		if got := deploysDetail(tc.environments); got != tc.deploys {
			t.Errorf("%v: deploys %q, want %q", tc.environments, got, tc.deploys)
		}
	}
}

func TestRepoFilter(t *testing.T) {
	f, err := parseRepoFilter("  app-*   api-* ")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"app-web": true, "api-core": true, "web": false, "my-app-x": false} {
		if got := f.match(name); got != want {
			t.Errorf("match(%q) = %v", name, got)
		}
	}
	if f, _ := parseRepoFilter("*"); !f.match("anything") {
		t.Error("* must match every repo")
	}
	for _, bad := range []string{"", "  ", "[", "app-* [x", `app-\`} {
		if _, err := parseRepoFilter(bad); err == nil {
			t.Errorf("filter %q accepted", bad)
		}
	}
	rt, _ := ghRuntime(t, "    settings:\n      repoFilter: \"app-[\"\n")
	rt.Out.VCS = vcs("")
	if err := (Provider{}).Resolve(context.Background(), rt); err == nil || !strings.Contains(err.Error(), "repoFilter") {
		t.Errorf("Resolve accepted a bad filter: %v", err)
	}
}

func TestTargetRepos(t *testing.T) {
	gh := &fakeGitHub{
		repos: map[string][]string{
			"acme": {"ownstack-cluster", "app-web", "app-nowf", "api-core", "app-sso", "docs"},
			"shop": {"app-store"},
		},
		files: map[string]string{
			"acme/ownstack-cluster": Workflow, "acme/app-web": Workflow, "acme/api-core": Workflow,
			"acme/docs": Workflow, "shop/app-store": Workflow,
		},
		denied: map[string]bool{"acme/app-sso": true},
	}
	srv := gh.serve(t)
	rt, _ := ghRuntime(t, "    settings:\n      repoFilter: \"app-* api-* ownstack-*\"\n      additionalOwners: shop, acme\n")
	rt.Out.VCS = vcs(srv.URL)

	targets, failed, err := Provider{}.targetRepos(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tg := range targets {
		names = append(names, tg.String())
	}
	if strings.Join(names, " ") != "acme/app-web acme/api-core shop/app-store" {
		t.Errorf("targets = %v", names)
	}
	if len(failed) != 1 || failed[0].String() != "acme/app-sso" || !strings.Contains(failed[0].err.Error(), "403") {
		t.Errorf("failed = %+v", failed)
	}
	if !slices.Contains(gh.got(), "GET /orgs/shop/repos") {
		t.Errorf("additional owners are listed as organizations: %v", gh.got())
	}
	err = discoveryError(failed)
	if err == nil || !strings.Contains(err.Error(), "acme/app-sso") {
		t.Errorf("discovery error = %v", err)
	}
	if discoveryError(nil) != nil {
		t.Error("no failures, no error")
	}

	// The demo repo is configured even when its workflow could not be
	// checked, so it does not fail the apply.
	demo := target{"acme", "app-sso"}
	repos, rest := withDemo(targets, failed, demo)
	if !containsTarget(repos, demo) || len(repos) != 4 || len(rest) != 0 || discoveryError(rest) != nil {
		t.Errorf("with demo: %v %v", repos, rest)
	}
	if repos, rest := withDemo([]target{demo}, nil, demo); len(repos) != 1 || len(rest) != 0 {
		t.Errorf("demo with the workflow already: %v %v", repos, rest)
	}
}

// Status checks only the recorded repos: the workflow file and the latest
// run, two requests each, and warns about outdated workflows.
func TestStatusUsesRecordedTargets(t *testing.T) {
	recorded := "acme/app-web\nshop/old\n"
	stubKubectl(t, &recorded)
	gh := &fakeGitHub{files: map[string]string{
		"acme/app-web": Workflow,
		"shop/old":     "name: ownstack\non: push\n",
	}}
	srv := gh.serve(t)
	rt, _ := ghRuntime(t, "")
	rt.Out.VCS = vcs(srv.URL)

	checks := Provider{}.Status(context.Background(), rt)
	states := map[string]stack.Check{}
	for _, c := range checks {
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
	requests := gh.got()
	if len(requests) != 4 {
		t.Errorf("requests = %v, want 2 per recorded repo", requests)
	}
	for _, r := range requests {
		if strings.Contains(r, "/user") || strings.Contains(r, "/orgs/") {
			t.Errorf("status scanned repos: %s", r)
		}
	}
}

func TestStatusScansWithoutRecord(t *testing.T) {
	stubKubectl(t, nil)
	gh := &fakeGitHub{repos: map[string][]string{"acme": {"app"}}, files: map[string]string{"acme/app": Workflow}}
	srv := gh.serve(t)
	rt, _ := ghRuntime(t, "")
	rt.Out.VCS = vcs(srv.URL)

	var found bool
	for _, c := range (Provider{}).Status(context.Background(), rt) {
		found = found || (c.Name == "pipeline acme/app" && c.State == stack.OK)
	}
	if !found || !slices.Contains(gh.got(), "GET /user/repos") {
		t.Errorf("no scan fallback: %v", gh.got())
	}
}

func TestWorkflowMarker(t *testing.T) {
	// 3 added environments, 4 takes the Helm library from getownstack/ownstack;
	// status must warn about repos with older copies.
	if workflowVersion < 4 {
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

func TestRecordTargets(t *testing.T) {
	previous := "acme/app\nacme/flaky\nacme/old\n"
	dir := stubKubectl(t, &previous)
	rt, out := ghRuntime(t, "")
	configured := []target{{"acme", "new"}, {"acme", "app"}}
	failed := []repoError{{target{"acme", "flaky"}, os.ErrDeadlineExceeded}}
	if err := recordTargets(context.Background(), rt, configured, failed); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Kind     string
		Metadata struct{ Name, Namespace string }
		Data     map[string]string
	}
	if err := json.Unmarshal(data, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Kind != "ConfigMap" || cm.Metadata.Name != targetsConfigMap || cm.Metadata.Namespace != systemNamespace {
		t.Errorf("applied %s", data)
	}
	// A repo that could not be checked stays recorded, so status still checks it.
	if got := cm.Data["targets"]; got != "acme/app\nacme/flaky\nacme/new" {
		t.Errorf("targets = %q", got)
	}
	log := out.String()
	if !strings.Contains(log, "acme/old no longer has") || strings.Contains(log, "acme/flaky no longer") {
		t.Errorf("log:\n%s", log)
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
		t.Errorf("namespace verbs = %v, want only get (ownstack.io's hook checks the namespace)", nsVerbs)
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
