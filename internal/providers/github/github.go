// Package github implements the VCS contract with GitHub. It also keeps the
// customer's cluster repo: a private repo that mirrors the applied
// ownstack.yaml, with a short README.md.
package github

import (
	"context"
	"errors"
	"fmt"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/githubapi"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the github provider's catalog entry.
var Spec = catalog.Provider{
	ID: "github", Aspect: "vcs", Name: "GitHub", Mono: "GH", Hosting: catalog.External,
	Summary: "Repositories on GitHub. ownstackctl keeps a private ownstack-cluster repo in your account with the applied ownstack.yaml.",
	Params: []catalog.Param{
		{Key: "owner", Label: "Owner", Kind: catalog.KindString, Required: true, Placeholder: "your-user-or-org",
			Help: "GitHub user or organization that owns the cluster and application repos."},
		{Key: "token", Label: "Access token", Kind: catalog.KindSecret, Required: true, Env: "GITHUB_TOKEN",
			Help:    "Classic token with the repo and workflow scopes (pre-selected by the link).",
			HelpURL: "https://github.com/settings/tokens/new?scopes=repo,workflow&description=Ownstack"},
		{Key: "demoApp", Label: "Create demo app repo", Kind: catalog.KindBool, Default: "true", Advanced: true,
			Help: "Generate the Lumen Yard demo app so the first pipeline run has something to deploy."},
	},
}

const (
	defaultAPI   = "https://api.github.com"
	demoTemplate = "getownstack/lumen-yard"
	demoRepo     = "lumen-yard"

	// clusterRepo mirrors the applied ownstack.yaml.
	clusterRepo        = "ownstack-cluster"
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
	owner := rt.Setting("owner")
	out := &stack.VCSOutput{
		Kind:           "github",
		Owner:          owner,
		APIURL:         p.api(),
		ClusterRepoURL: fmt.Sprintf("https://github.com/%s/%s", owner, clusterRepo),
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

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	c := p.client(rt)
	login, err := c.Login(ctx)
	if err != nil {
		return fmt.Errorf("GitHub token: %w", err)
	}
	rt.Out.VCS.Username = login
	rt.Logf("GitHub: authenticated as %s", login)
	if err := mirror(ctx, rt, c, login); err != nil {
		return err
	}
	if demo := rt.Out.VCS.DemoRepo; demo != "" {
		return c.GenerateRepo(ctx, rt, rt.Out.VCS.Owner, demo, demoTemplate, "Ownstack demo application")
	}
	return nil
}

// mirror commits the applied ownstack.yaml to the cluster repo, creating the
// repo (private, with a README.md) when it is missing. The server holds the
// stack; the repo keeps its history. A README the repo has stays.
func mirror(ctx context.Context, rt *stack.Runtime, c *githubapi.Client, login string) error {
	owner := rt.Out.VCS.Owner
	_, err := c.Repo(ctx, owner, clusterRepo)
	created := errors.Is(err, githubapi.ErrNotFound)
	if created {
		err = c.CreateRepo(ctx, rt, login, owner, clusterRepo, clusterDescription)
	}
	if err != nil {
		return err
	}
	// A new repo is empty: this first commit creates its default branch.
	sha := ""
	if !created {
		if _, sha, err = c.File(ctx, owner, clusterRepo, "", readmePath); err != nil {
			return err
		}
	}
	if sha == "" {
		if err := c.PutFile(ctx, rt, owner, clusterRepo, "", readmePath, clusterReadme(), "", "Add README.md"); err != nil {
			return err
		}
	}
	changed, err := c.UpsertFile(ctx, rt, owner, clusterRepo, "", manifest.FileName, rt.ManifestFile(), "Update ownstack.yaml")
	if err != nil {
		return err
	}
	if changed {
		rt.Logf("GitHub: committed ownstack.yaml to %s/%s", owner, clusterRepo)
	} else {
		rt.Logf("GitHub: ownstack.yaml in %s/%s is up to date", owner, clusterRepo)
	}
	return nil
}

// clusterReadme is the README.md of a new cluster repo.
func clusterReadme() []byte {
	return []byte("# " + clusterRepo + "\n\n" +
		"The stack definition of an Ownstack server. `ownstack.yaml` describes the stack: its domain, registry, CI, and environments. " +
		"Secrets are not stored here; they stay on the server.\n\n" +
		"The server keeps the applied manifest at `/etc/ownstack/ownstack.yaml`, and every `ownstackctl apply` commits it here, " +
		"so this history is the stack's history. To change the stack, edit the file on the server (or copy your edit there) and run " +
		"`ownstackctl apply`; an edit made only here is replaced by the next apply.\n\n" +
		"Documentation: https://github.com/getownstack/ownstack\n")
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
	if info, err := c.Repo(ctx, rt.Out.VCS.Owner, clusterRepo); err != nil {
		checks = append(checks, stack.Check{Name: "cluster repo", State: stack.Fail, Detail: err.Error()})
	} else {
		checks = append(checks, stack.Check{Name: "cluster repo", State: stack.OK, Detail: info.FullName + visibility(info.Private)})
	}
	return checks
}

func visibility(private bool) string {
	if private {
		return " (private)"
	}
	return " (public)"
}
