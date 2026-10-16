// Package github implements the VCS contract with GitHub. It also bootstraps
// the customer-owned cluster repo from the Ownstack template.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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

func (p Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	c := p.client(rt)
	var steps []stack.Step
	for _, r := range p.repos(rt) {
		target := "repo " + rt.Out.VCS.Owner + "/" + r.name
		if rt.Out.VCS.Token == "" {
			steps = append(steps, stack.Step{Action: "ensure", Target: target, Detail: "from template " + r.template})
			continue
		}
		if _, err := c.repo(ctx, rt.Out.VCS.Owner, r.name); err == nil {
			steps = append(steps, stack.Step{Action: "verify", Target: target, Detail: "exists"})
		} else {
			steps = append(steps, stack.Step{Action: "create", Target: target, Detail: "private, from template " + r.template})
		}
	}
	return steps
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	c := p.client(rt)
	login, err := c.login(ctx)
	if err != nil {
		return fmt.Errorf("GitHub token: %w", err)
	}
	rt.Out.VCS.Username = login
	rt.Logf("GitHub: authenticated as %s", login)
	for _, r := range p.repos(rt) {
		if err := c.ensureRepo(ctx, rt, rt.Out.VCS.Owner, r.name, r.template, r.description); err != nil {
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
	login, err := c.login(ctx)
	if err != nil {
		return []stack.Check{{Name: "token", State: stack.Fail, Detail: err.Error()}}
	}
	checks := []stack.Check{{Name: "token", State: stack.OK, Detail: "authenticated as " + login}}
	repo := rt.Setting("clusterRepo")
	if info, err := c.repo(ctx, rt.Out.VCS.Owner, repo); err != nil {
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
	info, err := c.repo(ctx, owner, repo)
	if err != nil {
		return err
	}
	changed, err := c.upsertFile(ctx, rt, owner, repo, info.DefaultBranch, "ownstack.yaml", manifestYAML, "Update ownstack.yaml")
	if err != nil {
		return err
	}
	if changed {
		rt.Logf("GitHub: committed ownstack.yaml to %s/%s", owner, repo)
	} else {
		rt.Logf("GitHub: ownstack.yaml in %s/%s is up to date", owner, repo)
	}
	return p.checkout(ctx, rt, info.DefaultBranch, dir)
}

func (p Provider) checkout(ctx context.Context, rt *stack.Runtime, branch, dir string) error {
	auth := base64.StdEncoding.EncodeToString([]byte(rt.Out.VCS.Username + ":" + rt.Out.VCS.Token))
	git := &stack.Exec{Console: rt.Console, Env: []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + auth,
	}}
	rt.Console.Redact(auth)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
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

// ── API client ───────────────────────────────────────────────────────────────

type client struct {
	base  string
	token string
	http  *http.Client
}

func (p Provider) client(rt *stack.Runtime) *client {
	return &client{base: p.api(), token: rt.Out.VCS.Token, http: rt.HTTP}
}

var errNotFound = errors.New("not found")

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if res.StatusCode >= 300 {
		var e struct{ Message string }
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("GitHub %s %s: HTTP %d: %s", method, path, res.StatusCode, e.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type repoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

func (c *client) login(ctx context.Context) (string, error) {
	var user struct{ Login string }
	if err := c.do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

func (c *client) repo(ctx context.Context, owner, name string) (repoInfo, error) {
	var info repoInfo
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil, &info)
	if errors.Is(err, errNotFound) {
		return info, fmt.Errorf("%s/%s not found or not accessible", owner, name)
	}
	if info.DefaultBranch == "" {
		info.DefaultBranch = "main"
	}
	return info, err
}

func (c *client) ensureRepo(ctx context.Context, rt *stack.Runtime, owner, name, template, description string) error {
	if _, err := c.repo(ctx, owner, name); err == nil {
		rt.Logf("GitHub: %s/%s exists", owner, name)
		return nil
	}
	rt.Logf("GitHub: creating private %s/%s from %s", owner, name, template)
	err := c.do(ctx, http.MethodPost, "/repos/"+template+"/generate", map[string]any{
		"owner": owner, "name": name, "private": true,
		"description": description, "include_all_branches": false,
	}, nil)
	if err != nil {
		return err
	}
	// Generation is asynchronous; wait until the default branch has content.
	return stack.Retry(ctx, rt.Console, owner+"/"+name+" contents", 20, 3*time.Second, func() error {
		info, err := c.repo(ctx, owner, name)
		if err != nil {
			return err
		}
		return c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/branches/"+info.DefaultBranch, nil, nil)
	})
}

// upsertFile commits content to path unless it is already identical.
func (c *client) upsertFile(ctx context.Context, rt *stack.Runtime, owner, repo, branch, path string, content []byte, message string) (bool, error) {
	endpoint := "/repos/" + owner + "/" + repo + "/contents/" + path
	var existing struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	err := c.do(ctx, http.MethodGet, endpoint+"?ref="+branch, nil, &existing)
	if err != nil && !errors.Is(err, errNotFound) {
		return false, err
	}
	if existing.SHA != "" {
		current, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(existing.Content, "\n", ""))
		if bytes.Equal(current, content) {
			return false, nil
		}
	}
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if existing.SHA != "" {
		body["sha"] = existing.SHA
	}
	return true, stack.Retry(ctx, rt.Console, "commit "+path, 5, 3*time.Second, func() error {
		return c.do(ctx, http.MethodPut, endpoint, body, nil)
	})
}
