package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
)

func TestUpsertFileSkipsIdenticalContent(t *testing.T) {
	stored := []byte("apiVersion: ownstack.io/v1\n")
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

	rt, _ := providertest.Runtime(t)
	p := Provider{APIURL: srv.URL}
	rt = rt.For("vcs")
	if err := p.Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	c := p.client(rt)
	changed, err := c.UpsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("apiVersion: ownstack.io/v1\n"), "msg")
	if err != nil || changed || puts != 0 {
		t.Fatalf("identical content: changed=%v puts=%d err=%v", changed, puts, err)
	}
	changed, err = c.UpsertFile(context.Background(), rt, "acme", "ownstack-cluster", "main", "ownstack.yaml", []byte("changed\n"), "msg")
	if err != nil || !changed || puts != 1 || string(stored) != "changed\n" {
		t.Fatalf("changed content: changed=%v puts=%d err=%v stored=%q", changed, puts, err, stored)
	}
}

func TestResolveOutputs(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	if err := (Provider{}).Resolve(context.Background(), rt.For("vcs")); err != nil {
		t.Fatal(err)
	}
	vcs := rt.Out.VCS
	if vcs.ClusterRepoURL != "https://github.com/acme/ownstack-cluster.git" || vcs.Token != "gh-token-value" {
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
			if body["auto_init"] == true {
				// GitHub's own README for an initialized repo.
				repo.files["README.md"] = "# " + name + "\n" + body["description"].(string)
			}
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
					if !ok || r.URL.Query().Get("ref") != "main" {
						http.NotFound(w, r)
						return
					}
					json.NewEncoder(w).Encode(map[string]string{"sha": "sha-" + content, "content": base64.StdEncoding.EncodeToString([]byte(content))})
					return
				}
				// PUT replaces a file only with its current sha.
				if sha, _ := body["sha"].(string); (ok && sha != "sha-"+content) || (!ok && sha != "") || body["branch"] != "main" {
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

// stubGit puts a git on PATH that records its arguments, so Bootstrap's
// checkout runs without the network.
func stubGit(t *testing.T) (calls func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	stub := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// bootstrap runs Bootstrap against gh for the test stack (owner acme).
func bootstrap(t *testing.T, gh *fakeGitHub, manifestYAML string) (*stack.Runtime, error) {
	t.Helper()
	srv := gh.serve(t)
	rt, _ := providertest.Runtime(t)
	rt = rt.For("vcs")
	p := Provider{APIURL: srv.URL}
	if err := p.Resolve(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	return rt, p.Bootstrap(context.Background(), rt, []byte(manifestYAML), filepath.Join(t.TempDir(), "ownstack-cluster"))
}

const testManifest = "apiVersion: ownstack.io/v1\nkind: Stack\n"

// A missing cluster repo is created as a private plain repo of the
// authenticated user, then gets README.md and ownstack.yaml, and is cloned.
// Nothing is generated from a template except the demo app.
func TestBootstrapCreatesUserClusterRepo(t *testing.T) {
	calls := stubGit(t)
	gh := newFakeGitHub("Acme") // logins are case-insensitive
	if _, err := bootstrap(t, gh, testManifest); err != nil {
		t.Fatal(err)
	}
	requests := gh.got()
	if !slices.Contains(requests, "POST /user/repos") || slices.Contains(requests, "POST /orgs/acme/repos") {
		t.Fatalf("requests: %v", requests)
	}
	body := gh.bodies["POST /user/repos"]
	if body["name"] != "ownstack-cluster" || body["private"] != true || body["auto_init"] != true {
		t.Errorf("create body = %v", body)
	}
	repo := gh.repos["acme/ownstack-cluster"]
	if repo == nil || !repo.private {
		t.Fatalf("cluster repo not created privately: %v", gh.repos)
	}
	if repo.files["ownstack.yaml"] != testManifest {
		t.Errorf("ownstack.yaml = %q", repo.files["ownstack.yaml"])
	}
	readme := repo.files["README.md"]
	for _, want := range []string{"# ownstack-cluster\n", "`ownstack.yaml`", "`ownstackctl apply`", "/root/ownstack-cluster", "https://github.com/getownstack/ownstack"} {
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
	git := calls()
	if len(git) != 1 || !strings.HasPrefix(git[0], "clone --branch main https://github.com/acme/ownstack-cluster.git ") {
		t.Errorf("git calls: %v", git)
	}
}

// An owner other than the token's user is an organization.
func TestBootstrapCreatesOrgClusterRepo(t *testing.T) {
	stubGit(t)
	gh := newFakeGitHub("someone")
	gh.orgs["acme"] = true
	if _, err := bootstrap(t, gh, testManifest); err != nil {
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

func TestBootstrapExplainsAnOwnerItCannotCreateIn(t *testing.T) {
	stubGit(t)
	gh := newFakeGitHub("someone")
	_, err := bootstrap(t, gh, testManifest)
	if err == nil || !strings.Contains(err.Error(), "neither the token's user (someone) nor an organization") {
		t.Fatalf("err = %v", err)
	}
	if len(gh.repos) != 0 {
		t.Errorf("created %v", gh.repos)
	}
}

// An existing cluster repo, such as one generated from the former cluster
// template, is reused: its README stays, and an unchanged ownstack.yaml is
// not committed again.
func TestBootstrapReusesExistingClusterRepo(t *testing.T) {
	stubGit(t)
	gh := newFakeGitHub("acme")
	gh.repos["acme/ownstack-cluster"] = &fakeRepo{private: true, files: map[string]string{
		"README.md":     "# Ownstack cluster template\n\nThe customer's own notes.\n",
		"ownstack.yaml": testManifest,
	}}
	gh.repos["acme/lumen-yard"] = &fakeRepo{private: true, files: map[string]string{"README.md": "demo"}}
	if _, err := bootstrap(t, gh, testManifest); err != nil {
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

	// A changed manifest is committed.
	if _, err := bootstrap(t, gh, testManifest+"metadata: {name: acme}\n"); err != nil {
		t.Fatal(err)
	}
	if got := gh.repos["acme/ownstack-cluster"].files["ownstack.yaml"]; got != testManifest+"metadata: {name: acme}\n" {
		t.Errorf("ownstack.yaml = %q", got)
	}
}

// A run that stopped after creating the repo left GitHub's placeholder
// README; the next run replaces it. A repo without a README gets one.
func TestBootstrapCompletesClusterRepoReadme(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"placeholder": {"README.md": "# ownstack-cluster\n" + clusterDescription},
		"missing":     {".gitignore": "*.env\n"},
	} {
		t.Run(name, func(t *testing.T) {
			stubGit(t)
			gh := newFakeGitHub("acme")
			gh.repos["acme/ownstack-cluster"] = &fakeRepo{private: true, files: files}
			if _, err := bootstrap(t, gh, testManifest); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(gh.got(), "POST /user/repos") {
				t.Error("recreated an existing repo")
			}
			if got := gh.repos["acme/ownstack-cluster"].files["README.md"]; got != string(clusterReadme("ownstack-cluster")) {
				t.Errorf("README.md = %q", got)
			}
		})
	}
}

func TestPlaceholderReadme(t *testing.T) {
	for content, want := range map[string]bool{
		"# ownstack-cluster":   true,
		"# ownstack-cluster\n": true,
		"# ownstack-cluster\r\n" + clusterDescription + "\r\n": true,
		"# ownstack-cluster\n" + clusterDescription:            true,
		"# ownstack-cluster\nOther description":                false,
		"# ownstack-cluster\n\nMore text.\n":                   false,
		"# Ownstack cluster template\n":                        false,
		"":                                                     false,
		string(clusterReadme("ownstack-cluster")):              false,
		"# ownstack-cluster\n" + clusterDescription + "\nmore": false,
	} {
		if got := placeholderReadme([]byte(content), "ownstack-cluster"); got != want {
			t.Errorf("placeholderReadme(%q) = %v, want %v", content, got, want)
		}
	}
}
