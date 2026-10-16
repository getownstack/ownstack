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

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
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

// EnsureRepo creates owner/name from template when it does not exist and
// waits until the generated repo has content.
func (c *Client) EnsureRepo(ctx context.Context, rt *stack.Runtime, owner, name, template, description string) error {
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
	return stack.Retry(ctx, rt.Console, owner+"/"+name+" contents", 20, 3*time.Second, func() error {
		info, err := c.Repo(ctx, owner, name)
		if err != nil {
			return err
		}
		return c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/branches/"+info.DefaultBranch, nil, nil)
	})
}

// UpsertFile commits content to path unless it is already identical.
func (c *Client) UpsertFile(ctx context.Context, rt *stack.Runtime, owner, repo, branch, path string, content []byte, message string) (bool, error) {
	endpoint := "/repos/" + owner + "/" + repo + "/contents/" + path
	var existing struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	err := c.Do(ctx, http.MethodGet, endpoint+"?ref="+branch, nil, &existing)
	if err != nil && !errors.Is(err, ErrNotFound) {
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
		return c.Do(ctx, http.MethodPut, endpoint, body, nil)
	})
}
