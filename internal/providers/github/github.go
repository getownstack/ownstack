// Package github implements the VCS contract with GitHub. It also bootstraps
// the customer-owned cluster repo from the Ownstack template.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/githubapi"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const (
	defaultAPI      = "https://api.github.com"
	defaultTemplate = "getownstack/ownstack-cluster-template"
	demoTemplate    = "getownstack/lumen-yard"
	demoRepo        = "lumen-yard"
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

// Template is the cluster template repo new cluster repos are generated from.
// OWNSTACK_TEMPLATE_REPO overrides it for development forks.
func Template() string {
	if t := os.Getenv("OWNSTACK_TEMPLATE_REPO"); t != "" {
		return t
	}
	return defaultTemplate
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	owner, repo := rt.Setting("owner"), rt.Setting("clusterRepo")
	out := &stack.VCSOutput{
		Kind:           "github",
		Owner:          owner,
		APIURL:         p.api(),
		ClusterRepoURL: fmt.Sprintf("https://github.com/%s/%s.git", owner, repo),
		Username:       owner,
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
			steps = append(steps, stack.Step{Action: "ensure", Target: target, Detail: "from template " + r.template})
			continue
		}
		if _, err := c.Repo(ctx, rt.Out.VCS.Owner, r.name); err == nil {
			steps = append(steps, stack.Step{Action: "verify", Target: target, Detail: "exists"})
		} else {
			steps = append(steps, stack.Step{Action: "create", Target: target, Detail: "private, from template " + r.template})
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
	for _, r := range p.repos(rt) {
		if err := c.EnsureRepo(ctx, rt, rt.Out.VCS.Owner, r.name, r.template, r.description); err != nil {
			return err
		}
	}
	return nil
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

type repoSpec struct{ name, template, description string }

func (p Provider) repos(rt *stack.Runtime) []repoSpec {
	repos := []repoSpec{{rt.Setting("clusterRepo"), Template(), "Ownstack cluster configuration"}}
	if rt.SettingBool("demoApp") {
		repos = append(repos, repoSpec{demoRepo, demoTemplate, "Ownstack demo application"})
	}
	return repos
}

func visibility(private bool) string {
	if private {
		return " (private)"
	}
	return " (public)"
}
