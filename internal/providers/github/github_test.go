package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

func TestUpsertFileSkipsIdenticalContent(t *testing.T) {
	stored := []byte("apiVersion: ownstack.io/v2\n")
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gh-token-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]string{"sha": "abc", "content": base64.StdEncoding.EncodeToString(stored)})
		case http.MethodPut:
			puts++
			var body struct{ Content, SHA string }
			json.NewDecoder(r.Body).Decode(&body)
			if body.SHA != "abc" {
				t.Errorf("update must send the existing sha, got %q", body.SHA)
			}
			stored, _ = base64.StdEncoding.DecodeString(body.Content)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	rt, _ := providertest.Runtime(t, Spec)
	p := Provider{APIURL: srv.URL}
	rt = rt.For("vcs")
	if err := p.Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	c := p.client(rt)
	changed, err := c.UpsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("apiVersion: ownstack.io/v2\n"), "msg")
	if err != nil || changed || puts != 0 {
		t.Fatalf("identical content: changed=%v puts=%d err=%v", changed, puts, err)
	}
	changed, err = c.UpsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("changed\n"), "msg")
	if err != nil || !changed || puts != 1 || string(stored) != "changed\n" {
		t.Fatalf("changed content: changed=%v puts=%d err=%v stored=%q", changed, puts, err, stored)
	}
}

func TestResolveOutputs(t *testing.T) {
	rt, _ := providertest.Runtime(t, Spec)
	if err := (Provider{}).Resolve(context.Background(), rt.For("vcs")); err != nil {
		t.Fatal(err)
	}
	vcs := rt.Out.VCS
	if vcs.ClusterRepoURL != "https://github.com/acme/ownstack-cluster" || vcs.Token != "gh-token-value" || vcs.DemoRepo != "lumen-yard" {
		t.Errorf("outputs = %+v", vcs)
	}
}

// fakeRepo is a repository of fakeGitHub.
type fakeRepo struct {
	private bool
	files   map[string]string // path -> content on main
}

// fakeGitHub serves the endpoints the provider uses, keeping repos and
// files, and records every request.
type fakeGitHub struct {
	mu       sync.Mutex
	login    string
	orgs     map[string]bool      // organizations the token can create repos in
	repos    map[string]*fakeRepo // lowercase owner/name, as GitHub ignores case
	requests []string
	bodies   map[string]map[string]any // "METHOD path" -> last JSON body
}

func newFakeGitHub(login string) *fakeGitHub {
	return &fakeGitHub{login: login, orgs: map[string]bool{}, repos: map[string]*fakeRepo{}, bodies: map[string]map[string]any{}}
}

func (g *fakeGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer gh-token-value" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		call := r.Method + " " + r.URL.Path
		g.requests = append(g.requests, call)
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		if body != nil {
			g.bodies[call] = body
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		create := func(owner string) {
			name := body["name"].(string)
			key := strings.ToLower(owner + "/" + name)
			if g.repos[key] != nil {
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]string{"message": "name already exists on this account"})
				return
			}
			repo := &fakeRepo{private: body["private"] == true, files: map[string]string{}}
			g.repos[key] = repo
			w.WriteHeader(http.StatusCreated)
		}
		switch {
		case call == "GET /user":
			json.NewEncoder(w).Encode(map[string]string{"login": g.login})
		case call == "POST /user/repos":
			create(g.login)
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "orgs" && parts[2] == "repos":
			if !g.orgs[parts[1]] {
				http.NotFound(w, r)
				return
			}
			create(parts[1])
		case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "generate":
			g.repos[strings.ToLower(body["owner"].(string)+"/"+body["name"].(string))] = &fakeRepo{private: true, files: map[string]string{"README.md": "template"}}
			w.WriteHeader(http.StatusCreated)
		case len(parts) >= 3 && parts[0] == "repos":
			full := parts[1] + "/" + parts[2]
			repo := g.repos[strings.ToLower(full)]
			if repo == nil {
				http.NotFound(w, r)
				return
			}
			switch {
			case len(parts) == 3 && r.Method == http.MethodGet:
				json.NewEncoder(w).Encode(map[string]any{"full_name": full, "private": repo.private, "default_branch": "main"})
			case len(parts) == 5 && parts[3] == "branches" && parts[4] == "main" && len(repo.files) > 0:
				json.NewEncoder(w).Encode(map[string]string{"name": "main"})
			case len(parts) == 5 && parts[3] == "contents":
				path := parts[4]
				content, ok := repo.files[path]
				if r.Method == http.MethodGet {
					if ref := r.URL.Query().Get("ref"); !ok || (ref != "" && ref != "main") {
						http.NotFound(w, r)
						return
					}
					json.NewEncoder(w).Encode(map[string]string{"sha": "sha-" + content, "content": base64.StdEncoding.EncodeToString([]byte(content))})
					return
				}
				// PUT replaces a file only with its current sha.
				branch, _ := body["branch"].(string)
				if sha, _ := body["sha"].(string); (ok && sha != "sha-"+content) || (!ok && sha != "") || (branch != "" && branch != "main") {
					w.WriteHeader(http.StatusConflict)
					return
				}
				data, _ := base64.StdEncoding.DecodeString(body["content"].(string))
				repo.files[path] = string(data)
				w.WriteHeader(http.StatusOK)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *fakeGitHub) got() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// apply runs the vcs stage against gh for the test stack (owner acme),
// whose applied manifest is manifestYAML.
func apply(t *testing.T, gh *fakeGitHub, manifestYAML string) (*stack.Runtime, error) {
	t.Helper()
	srv := gh.serve(t)
	rt, _ := providertest.Runtime(t, Spec)
	rt.Manifest.Source = []byte(manifestYAML)
	rt = rt.For("vcs")
	p := Provider{APIURL: srv.URL}
	if err := p.Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	return rt, p.Apply(context.Background(), rt)
}

const testManifest = "apiVersion: ownstack.io/v2\ndomain: example.com\n"

// A missing cluster repo is created empty and private for the authenticated
// user; its first commit is README.md, then the applied ownstack.yaml is
// committed. Nothing is generated from a template except the demo app.
func TestApplyCreatesUserClusterRepo(t *testing.T) {
	gh := newFakeGitHub("Acme") // logins are case-insensitive
	if _, err := apply(t, gh, testManifest); err != nil {
		t.Fatal(err)
	}
	requests := gh.got()
	if !slices.Contains(requests, "POST /user/repos") || slices.Contains(requests, "POST /orgs/acme/repos") {
		t.Fatalf("requests: %v", requests)
	}
	body := gh.bodies["POST /user/repos"]
	if body["name"] != "ownstack-cluster" || body["private"] != true || body["auto_init"] != false {
		t.Errorf("create body = %v", body)
	}
	if put := gh.bodies["PUT /repos/acme/ownstack-cluster/contents/README.md"]; put["branch"] != nil {
		t.Errorf("the first commit to an empty repo names a branch: %v", put)
	}
	for _, r := range requests {
		if strings.HasPrefix(r, "GET /repos/acme/ownstack-cluster/contents/README.md") {
			t.Errorf("read README.md of a repo it just created: %v", requests)
		}
	}
	repo := gh.repos["acme/ownstack-cluster"]
	if repo == nil || !repo.private {
		t.Fatalf("cluster repo not created privately: %v", gh.repos)
	}
	if repo.files["ownstack.yaml"] != testManifest {
		t.Errorf("ownstack.yaml = %q", repo.files["ownstack.yaml"])
	}
	readme := repo.files["README.md"]
	for _, want := range []string{"# ownstack-cluster\n", "`ownstack.yaml`", "`ownstackctl apply`", "/etc/ownstack/ownstack.yaml", "https://github.com/getownstack/ownstack"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not contain %q:\n%s", want, readme)
		}
	}
	// Only the demo app comes from a template.
	var generated []string
	for _, r := range requests {
		if strings.HasSuffix(r, "/generate") {
			generated = append(generated, r)
		}
	}
	if !slices.Equal(generated, []string{"POST /repos/getownstack/lumen-yard/generate"}) {
		t.Errorf("generated: %v", generated)
	}
	if demo := gh.bodies["POST /repos/getownstack/lumen-yard/generate"]; demo["owner"] != "acme" || demo["name"] != "lumen-yard" || demo["private"] != true {
		t.Errorf("demo generate body = %v", demo)
	}
}

// An owner other than the token's user is an organization.
func TestApplyCreatesOrgClusterRepo(t *testing.T) {
	gh := newFakeGitHub("someone")
	gh.orgs["acme"] = true
	if _, err := apply(t, gh, testManifest); err != nil {
		t.Fatal(err)
	}
	if requests := gh.got(); !slices.Contains(requests, "POST /orgs/acme/repos") || slices.Contains(requests, "POST /user/repos") {
		t.Fatalf("requests: %v", requests)
	}
	repo := gh.repos["acme/ownstack-cluster"]
	if repo == nil || !repo.private || repo.files["ownstack.yaml"] != testManifest || !strings.Contains(repo.files["README.md"], "ownstackctl apply") {
		t.Fatalf("cluster repo = %+v", repo)
	}
}

func TestApplyExplainsAnOwnerItCannotCreateIn(t *testing.T) {
	gh := newFakeGitHub("someone")
	_, err := apply(t, gh, testManifest)
	if err == nil || !strings.Contains(err.Error(), "neither the token's user (someone) nor an organization") {
		t.Fatalf("err = %v", err)
	}
	if len(gh.repos) != 0 {
		t.Errorf("created %v", gh.repos)
	}
}

// An existing cluster repo is reused: its README stays, an unchanged
// ownstack.yaml is not committed again, and a changed one is.
func TestApplyReusesExistingClusterRepo(t *testing.T) {
	gh := newFakeGitHub("acme")
	gh.repos["acme/ownstack-cluster"] = &fakeRepo{private: true, files: map[string]string{
		"README.md":     "# Our stack\n\nThe customer's own notes.\n",
		"ownstack.yaml": testManifest,
	}}
	gh.repos["acme/lumen-yard"] = &fakeRepo{private: true, files: map[string]string{"README.md": "demo"}}
	if _, err := apply(t, gh, testManifest); err != nil {
		t.Fatal(err)
	}
	for _, r := range gh.got() {
		if r != "GET /user" && !strings.HasPrefix(r, "GET /repos/") {
			t.Errorf("unexpected request %s", r)
		}
	}
	if got := gh.repos["acme/ownstack-cluster"].files["README.md"]; !strings.Contains(got, "customer's own notes") {
		t.Errorf("README.md replaced: %q", got)
	}

	changed := testManifest + "vcs: {owner: acme}\n"
	if _, err := apply(t, gh, changed); err != nil {
		t.Fatal(err)
	}
	if got := gh.repos["acme/ownstack-cluster"].files["ownstack.yaml"]; got != changed {
		t.Errorf("ownstack.yaml = %q", got)
	}
}

// A repo without a README gets one, such as one an earlier run created
// before it could commit.
func TestApplyAddsMissingReadme(t *testing.T) {
	gh := newFakeGitHub("acme")
	gh.repos["acme/ownstack-cluster"] = &fakeRepo{private: true, files: map[string]string{}}
	if _, err := apply(t, gh, testManifest); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(gh.got(), "POST /user/repos") {
		t.Error("recreated an existing repo")
	}
	files := gh.repos["acme/ownstack-cluster"].files
	if files["README.md"] != string(clusterReadme()) || files["ownstack.yaml"] != testManifest {
		t.Errorf("files = %v", files)
	}
}
