// Package github implements the VCS contract with GitHub. It also bootstraps
// the customer-owned cluster repo: a private repo that holds ownstack.yaml
// (and a short README.md), created when missing.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/getownstack/ownstack/internal/githubapi"
	"github.com/getownstack/ownstack/internal/stack"
)

const (
	defaultAPI   = "https://api.github.com"
	demoTemplate = "getownstack/lumen-yard"
	demoRepo     = "lumen-yard"

	clusterDescription = "Ownstack stack definition (ownstack.yaml)"
	readmePath         = "README.md"
)

type Provider struct {
	// APIURL overrides the API endpoint in tests.
	APIURL string
}

func (p Provider) api() string {
	if p.APIURL != "" {
		return p.APIURL
	}
	return defaultAPI
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	owner, repo := rt.Setting("owner"), rt.Setting("clusterRepo")
	out := &stack.VCSOutput{
		Kind:           "github",
		Owner:          owner,
		APIURL:         p.api(),
		ClusterRepo:    repo,
		ClusterRepoURL: fmt.Sprintf("https://github.com/%s/%s.git", owner, repo),
		Username:       owner,
	}
	if rt.SettingBool("demoApp") {
		out.DemoRepo = demoRepo
	}
	rt.Out.VCS = out
	token, err := rt.Secret("token")
	if err != nil {
		return err
	}
	out.Token = token
	return nil
}

func (p Provider) client(rt *stack.Runtime) *githubapi.Client {
	return &githubapi.Client{Base: p.api(), Token: rt.Out.VCS.Token, HTTP: rt.HTTP}
}

func (p Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	c := p.client(rt)
	var steps []stack.Step
	for _, r := range p.repos(rt) {
		target := "repo " + rt.Out.VCS.Owner + "/" + r.name
		if rt.Out.VCS.Token == "" {
			steps = append(steps, stack.Step{Action: "ensure", Target: target, Detail: r.origin()})
			continue
		}
		if _, err := c.Repo(ctx, rt.Out.VCS.Owner, r.name); err == nil {
			steps = append(steps, stack.Step{Action: "verify", Target: target, Detail: "exists"})
		} else {
			steps = append(steps, stack.Step{Action: "create", Target: target, Detail: "private, " + r.origin()})
		}
	}
	return steps
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	c := p.client(rt)
	login, err := c.Login(ctx)
	if err != nil {
		return fmt.Errorf("GitHub token: %w", err)
	}
	rt.Out.VCS.Username = login
	rt.Logf("GitHub: authenticated as %s", login)
	if err := ensureClusterRepo(ctx, rt, c, login); err != nil {
		return err
	}
	if demo := rt.Out.VCS.DemoRepo; demo != "" {
		return c.GenerateRepo(ctx, rt, rt.Out.VCS.Owner, demo, demoTemplate, "Ownstack demo application")
	}
	return nil
}

// ensureClusterRepo makes the cluster repo exist: a private plain repo with
// README.md, created when missing. An existing repo is reused, including
// ones generated from the former cluster template.
func ensureClusterRepo(ctx context.Context, rt *stack.Runtime, c *githubapi.Client, login string) error {
	owner, name := rt.Out.VCS.Owner, rt.Out.VCS.ClusterRepo
	created := false
	info, err := c.Repo(ctx, owner, name)
	if err == nil {
		rt.Logf("GitHub: %s/%s exists", owner, name)
	} else {
		if err := c.CreateRepo(ctx, rt, login, owner, name, clusterDescription); err != nil {
			return err
		}
		created = true
		if info, err = c.Repo(ctx, owner, name); err != nil {
			return err
		}
	}
	current, sha, err := c.File(ctx, owner, name, info.DefaultBranch, readmePath)
	if err != nil {
		return err
	}
	// A README the repo already has (the former template's, or the
	// customer's own) stays. GitHub's placeholder from creation is replaced,
	// also when an earlier run stopped before replacing it.
	if sha != "" && !created && !placeholderReadme(current, name) {
		return nil
	}
	if err := c.PutFile(ctx, rt, owner, name, info.DefaultBranch, readmePath, clusterReadme(name), sha, "Add README.md"); err != nil {
		return err
	}
	rt.Logf("GitHub: committed %s to %s/%s", readmePath, owner, name)
	return nil
}

// clusterReadme is the README.md of a new cluster repo.
func clusterReadme(name string) []byte {
	return []byte("# " + name + "\n\n" +
		"The stack definition of an Ownstack server. `ownstack.yaml` describes the stack: its domain, registry, CI, and environments. " +
		"Secrets are not stored here; they stay on the server.\n\n" +
		"To change the stack, edit `ownstack.yaml`, commit it, and run `ownstackctl apply` in `/root/ownstack-cluster` on the server " +
		"(`git pull` there first when you committed elsewhere).\n\n" +
		"Documentation: https://github.com/getownstack/ownstack\n")
}

// placeholderReadme reports whether content is the README GitHub writes for
// a repo it initializes: a "# <name>" heading, perhaps with the description.
func placeholderReadme(content []byte, name string) bool {
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if strings.TrimSpace(lines[0]) != "# "+name || len(lines) > 2 {
		return false
	}
	return len(lines) == 1 || strings.TrimSpace(lines[1]) == clusterDescription
}

func (p Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	if rt.Out.VCS.Token == "" {
		return []stack.Check{{Name: "token", State: stack.Unknown, Detail: "token not available on this node"}}
	}
	c := p.client(rt)
	login, err := c.Login(ctx)
	if err != nil {
		return []stack.Check{{Name: "token", State: stack.Fail, Detail: err.Error()}}
	}
	checks := []stack.Check{{Name: "token", State: stack.OK, Detail: "authenticated as " + login}}
	repo := rt.Setting("clusterRepo")
	if info, err := c.Repo(ctx, rt.Out.VCS.Owner, repo); err != nil {
		checks = append(checks, stack.Check{Name: "cluster repo", State: stack.Fail, Detail: err.Error()})
	} else {
		checks = append(checks, stack.Check{Name: "cluster repo", State: stack.OK, Detail: info.FullName + visibility(info.Private)})
	}
	return checks
}

// Bootstrap makes the cluster repo exist, commits the manifest to it, and
// checks it out in dir. It is the first half of `ownstackctl up`.
func (p Provider) Bootstrap(ctx context.Context, rt *stack.Runtime, manifestYAML []byte, dir string) error {
	if err := p.Apply(ctx, rt); err != nil {
		return err
	}
	c := p.client(rt)
	owner, repo := rt.Out.VCS.Owner, rt.Setting("clusterRepo")
	info, err := c.Repo(ctx, owner, repo)
	if err != nil {
		return err
	}
	changed, err := c.UpsertFile(ctx, rt, owner, repo, info.DefaultBranch, "ownstack.yaml", manifestYAML, "Update ownstack.yaml")
	if err != nil {
		return err
	}
	if changed {
		rt.Logf("GitHub: committed ownstack.yaml to %s/%s", owner, repo)
	} else {
		rt.Logf("GitHub: ownstack.yaml in %s/%s is up to date", owner, repo)
	}
	return p.checkout(ctx, rt, info.DefaultBranch, dir, manifestYAML)
}

func (p Provider) checkout(ctx context.Context, rt *stack.Runtime, branch, dir string, manifestYAML []byte) error {
	auth := base64.StdEncoding.EncodeToString([]byte(rt.Out.VCS.Username + ":" + rt.Out.VCS.Token))
	git := &stack.Exec{Console: rt.Console, Env: []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
	}}
	rt.Console.Redact(auth)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// `up -f` on the checkout's own ownstack.yaml has just committed that
		// edit; the identical local copy would otherwise block the pull.
		local := filepath.Join(dir, "ownstack.yaml")
		if data, err := os.ReadFile(local); err == nil && bytes.Equal(data, manifestYAML) {
			_ = git.Run(ctx, "git", "-C", dir, "checkout", "--", "ownstack.yaml")
		}
		rt.Logf("Updating %s", dir)
		return stack.Retry(ctx, rt.Console, "git pull", 3, 10*time.Second, func() error {
			return git.Run(ctx, "git", "-C", dir, "pull", "--ff-only", "origin", branch)
		})
	}
	rt.Logf("Cloning %s into %s", rt.Out.VCS.ClusterRepoURL, dir)
	return stack.Retry(ctx, rt.Console, "git clone", 5, 10*time.Second, func() error {
		_ = os.RemoveAll(dir)
		return git.Run(ctx, "git", "clone", "--branch", branch, rt.Out.VCS.ClusterRepoURL, dir)
	})
}

// repoSpec is a repo the provider makes exist: a plain repo, or one
// generated from template.
type repoSpec struct{ name, template string }

func (r repoSpec) origin() string {
	if r.template == "" {
		return "with " + readmePath
	}
	return "from template " + r.template
}

func (p Provider) repos(rt *stack.Runtime) []repoSpec {
	repos := []repoSpec{{name: rt.Out.VCS.ClusterRepo}}
	if rt.Out.VCS.DemoRepo != "" {
		repos = append(repos, repoSpec{rt.Out.VCS.DemoRepo, demoTemplate})
	}
	return repos
}

func visibility(private bool) string {
	if private {
		return " (private)"
	}
	return " (public)"
}
