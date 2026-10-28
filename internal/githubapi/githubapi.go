// Package githubapi is the small GitHub REST client shared by the GitHub VCS
// provider and the GitHub Actions CI provider.
package githubapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/getownstack/ownstack/internal/stack"
)

// Client is a minimal GitHub REST client.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// ErrNotFound is returned for HTTP 404.
var ErrNotFound = errors.New("not found")

// Do sends a request and decodes the JSON response into out.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode == http.StatusNotFound {
		return ErrNotFound
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

// RepoInfo is the subset of repository fields Ownstack uses.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// Login returns the authenticated user.
func (c *Client) Login(ctx context.Context) (string, error) {
	var user struct{ Login string }
	if err := c.Do(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

// Repo returns a repository, or an error when it is missing or inaccessible.
func (c *Client) Repo(ctx context.Context, owner, name string) (RepoInfo, error) {
	var info RepoInfo
	err := c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+name, nil, &info)
	if errors.Is(err, ErrNotFound) {
		return info, fmt.Errorf("%s/%s not found or not accessible", owner, name)
	}
	if info.DefaultBranch == "" {
		info.DefaultBranch = "main"
	}
	return info, err
}

// GenerateRepo creates owner/name from template when it does not exist and
// waits until the generated repo has content.
func (c *Client) GenerateRepo(ctx context.Context, rt *stack.Runtime, owner, name, template, description string) error {
	if _, err := c.Repo(ctx, owner, name); err == nil {
		rt.Logf("GitHub: %s/%s exists", owner, name)
		return nil
	}
	rt.Logf("GitHub: creating private %s/%s from %s", owner, name, template)
	err := c.Do(ctx, http.MethodPost, "/repos/"+template+"/generate", map[string]any{
		"owner": owner, "name": name, "private": true,
		"description": description, "include_all_branches": false,
	}, nil)
	if err != nil {
		return err
	}
	// Generation is asynchronous; wait until the default branch has content.
	return c.waitForBranch(ctx, rt, owner, name)
}

// CreateRepo creates the private repo owner/name with an initial commit.
// login is the authenticated user: their own repos are created through
// /user/repos, any other owner must be an organization the token can create
// repos in.
func (c *Client) CreateRepo(ctx context.Context, rt *stack.Runtime, login, owner, name, description string) error {
	endpoint := "/orgs/" + owner + "/repos"
	if strings.EqualFold(owner, login) {
		endpoint = "/user/repos"
	}
	rt.Logf("GitHub: creating private %s/%s", owner, name)
	err := c.Do(ctx, http.MethodPost, endpoint, map[string]any{
		"name": name, "description": description, "private": true,
		// An initial commit gives the repo its default branch, so files can
		// be committed through the contents API.
		"auto_init": true,
	}, nil)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("cannot create %s/%s: %s is neither the token's user (%s) nor an organization it can see", owner, name, owner, login)
	}
	if err != nil {
		return err
	}
	return c.waitForBranch(ctx, rt, owner, name)
}

// waitForBranch waits until a new repo's default branch exists.
func (c *Client) waitForBranch(ctx context.Context, rt *stack.Runtime, owner, name string) error {
	return stack.Retry(ctx, rt.Console, owner+"/"+name+" contents", 20, 3*time.Second, func() error {
		info, err := c.Repo(ctx, owner, name)
		if err != nil {
			return err
		}
		return c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/branches/"+info.DefaultBranch, nil, nil)
	})
}

// File returns the content of path on branch and its blob sha; sha is ""
// when the file does not exist.
func (c *Client) File(ctx context.Context, owner, repo, branch, path string) (content []byte, sha string, err error) {
	var existing struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	err = c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/contents/"+path+"?ref="+branch, nil, &existing)
	if errors.Is(err, ErrNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	content, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(existing.Content, "\n", ""))
	if err != nil {
		return nil, "", fmt.Errorf("GitHub %s/%s %s: %w", owner, repo, path, err)
	}
	return content, existing.SHA, nil
}

// PutFile commits content to path on branch. sha is the blob it replaces,
// "" for a new file.
func (c *Client) PutFile(ctx context.Context, rt *stack.Runtime, owner, repo, branch, path string, content []byte, sha, message string) error {
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if sha != "" {
		body["sha"] = sha
	}
	return stack.Retry(ctx, rt.Console, "commit "+path, 5, 3*time.Second, func() error {
		return c.Do(ctx, http.MethodPut, "/repos/"+owner+"/"+repo+"/contents/"+path, body, nil)
	})
}

// UpsertFile commits content to path unless it is already identical.
func (c *Client) UpsertFile(ctx context.Context, rt *stack.Runtime, owner, repo, branch, path string, content []byte, message string) (bool, error) {
	current, sha, err := c.File(ctx, owner, repo, branch, path)
	if err != nil {
		return false, err
	}
	if sha != "" && bytes.Equal(current, content) {
		return false, nil
	}
	return true, c.PutFile(ctx, rt, owner, repo, branch, path, content, sha, message)
}
