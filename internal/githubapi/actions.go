package githubapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/crypto/nacl/box"
)

// Repos lists the repositories owned by owner. login is the authenticated
// user: for them the private repos are listed too; any other owner is
// treated as an organization.
func (c *Client) Repos(ctx context.Context, owner, login string) ([]string, error) {
	var names []string
	for page := 1; page <= 20; page++ {
		path := fmt.Sprintf("/orgs/%s/repos?per_page=100&page=%d", url.PathEscape(owner), page)
		if strings.EqualFold(owner, login) {
			path = fmt.Sprintf("/user/repos?affiliation=owner&per_page=100&page=%d", page)
		}
		var repos []struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			Archived bool `json:"archived"`
		}
		if err := c.Do(ctx, http.MethodGet, path, nil, &repos); err != nil {
			return nil, err
		}
		for _, r := range repos {
			if !r.Archived && strings.EqualFold(r.Owner.Login, owner) {
				names = append(names, r.Name)
			}
		}
		if len(repos) < 100 {
			break
		}
	}
	return names, nil
}

// FileExists reports whether path exists on the repo's default branch.
func (c *Client) FileExists(ctx context.Context, owner, repo, path string) (bool, error) {
	err := c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/contents/"+path, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// FileContent returns a file on the repo's default branch; ErrNotFound when
// it does not exist.
func (c *Client) FileContent(ctx context.Context, owner, repo, path string) ([]byte, error) {
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.Do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/contents/"+path, nil, &file); err != nil {
		return nil, err
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("%s/%s: %s: unexpected encoding %q", owner, repo, path, file.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
}

// SetSecret creates or updates an Actions secret, sealed with the repo's
// public key so only GitHub can decrypt it.
func (c *Client) SetSecret(ctx context.Context, owner, repo, name, value string) error {
	var key struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	base := "/repos/" + owner + "/" + repo + "/actions/secrets"
	if err := c.Do(ctx, http.MethodGet, base+"/public-key", nil, &key); err != nil {
		return err
	}
	sealed, err := Seal(key.Key, value)
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodPut, base+"/"+name, map[string]string{"encrypted_value": sealed, "key_id": key.KeyID}, nil)
}

// Seal encrypts value for a base64 Curve25519 public key as a libsodium
// sealed box, the format GitHub expects for Actions secrets.
func Seal(publicKey, value string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("invalid repository public key")
	}
	var pk [32]byte
	copy(pk[:], raw)
	out, err := box.SealAnonymous(nil, []byte(value), &pk, rand.Reader)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(out), nil
}

// SetVariable creates or updates a plain Actions variable.
func (c *Client) SetVariable(ctx context.Context, owner, repo, name, value string) error {
	base := "/repos/" + owner + "/" + repo + "/actions/variables"
	body := map[string]string{"name": name, "value": value}
	err := c.Do(ctx, http.MethodPatch, base+"/"+name, body, nil)
	if errors.Is(err, ErrNotFound) {
		return c.Do(ctx, http.MethodPost, base, body, nil)
	}
	return err
}

// DeleteSecret removes an Actions secret; a missing one is not an error.
func (c *Client) DeleteSecret(ctx context.Context, owner, repo, name string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/repos/"+owner+"/"+repo+"/actions/secrets/"+name, nil, nil))
}

// DeleteVariable removes an Actions variable; a missing one is not an error.
func (c *Client) DeleteVariable(ctx context.Context, owner, repo, name string) error {
	return ignoreNotFound(c.Do(ctx, http.MethodDelete, "/repos/"+owner+"/"+repo+"/actions/variables/"+name, nil, nil))
}

func ignoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Run is the latest run of a workflow.
type Run struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
}

// LatestRun returns the most recent run of a workflow file on branch (any
// branch when empty), or nil.
func (c *Client) LatestRun(ctx context.Context, owner, repo, workflow, branch string) (*Run, error) {
	var body struct {
		Runs []Run `json:"workflow_runs"`
	}
	path := "/repos/" + owner + "/" + repo + "/actions/workflows/" + workflow + "/runs?per_page=1"
	if branch != "" {
		path += "&branch=" + url.QueryEscape(branch)
	}
	err := c.Do(ctx, http.MethodGet, path, nil, &body)
	if errors.Is(err, ErrNotFound) || (err == nil && len(body.Runs) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &body.Runs[0], nil
}

// Dispatch starts a workflow_dispatch run on ref.
func (c *Client) Dispatch(ctx context.Context, owner, repo, workflow, ref string) error {
	return c.Do(ctx, http.MethodPost, "/repos/"+owner+"/"+repo+"/actions/workflows/"+workflow+"/dispatches", map[string]string{"ref": ref}, nil)
}
