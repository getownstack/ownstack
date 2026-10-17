// Package registry implements the image registry contract with a registry
// the customer already has: their own Harbor, GitHub Container Registry,
// Docker Hub, or any OCI registry. Nothing is installed; the credential is
// verified and handed to CI and the environments.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const PullSecretName = "registry-credentials"

type Provider struct {
	// Client overrides the HTTP client in tests.
	Client *http.Client
	// Scheme overrides https in tests.
	Scheme string
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	host := rt.Setting("host")
	out := &stack.ImagesOutput{
		Host:           host,
		Project:        rt.Setting("project"),
		PushUsername:   rt.Setting("username"),
		PullSecretName: rt.Setting("pullSecretName"),
	}
	if isDockerHub(host) {
		out.PullServer = "https://index.docker.io/v1/"
	}
	rt.Out.Images = out
	password, err := rt.Secret("password")
	if err != nil {
		return err
	}
	out.PushPassword = password
	return nil
}

func (p Provider) Plan(_ context.Context, rt *stack.Runtime) []stack.Step {
	out := rt.Out.Images
	return []stack.Step{{Action: "verify", Target: out.Host, Detail: fmt.Sprintf("%s can push and pull %s/*", out.PushUsername, out.Project)}}
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	out := rt.Out.Images
	if err := p.verify(ctx, rt); err != nil {
		return fmt.Errorf("%s rejected the credential for %s: %w", out.Host, out.PushUsername, err)
	}
	rt.Logf("Registry %s accepts %s for %s/*", out.Host, out.PushUsername, out.Project)
	return nil
}

func (p Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	out := rt.Out.Images
	if out.PushPassword == "" {
		return []stack.Check{{Name: "credential", State: stack.Unknown, Detail: "password not available on this node"}}
	}
	if err := p.verify(ctx, rt); err != nil {
		return []stack.Check{{Name: "credential", State: stack.Fail, Detail: err.Error()}}
	}
	return []stack.Check{{Name: "credential", State: stack.OK, Detail: fmt.Sprintf("%s can push to %s/%s", out.PushUsername, out.Host, out.Project)}}
}

func isDockerHub(host string) bool {
	return host == "docker.io" || host == "index.docker.io" || host == "registry-1.docker.io"
}

func (p Provider) client(rt *stack.Runtime) *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return rt.HTTP
}

// verify performs the Docker registry v2 authentication handshake: either the
// registry accepts basic auth directly, or it points at a token service that
// must issue a push+pull token for the project.
func (p Provider) verify(ctx context.Context, rt *stack.Runtime) error {
	out := rt.Out.Images
	host := out.Host
	if isDockerHub(host) {
		host = "registry-1.docker.io"
	}
	scheme := p.Scheme
	if scheme == "" {
		scheme = "https"
	}
	client := p.client(rt)
	base := scheme + "://" + host + "/v2/"

	res, err := get(ctx, client, base, func(r *http.Request) { r.SetBasicAuth(out.PushUsername, out.PushPassword) })
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	challenge := res.Header.Get("WWW-Authenticate")
	if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return fmt.Errorf("HTTP %d from %s", res.StatusCode, base)
	}
	params := parseChallenge(challenge[len("bearer "):])
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("token challenge without realm")
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", fmt.Sprintf("repository:%s/ownstack-check:push,pull", out.Project))
	tokenRes, err := get(ctx, client, realm+"?"+q.Encode(), func(r *http.Request) { r.SetBasicAuth(out.PushUsername, out.PushPassword) })
	if err != nil {
		return err
	}
	defer tokenRes.Body.Close()
	if tokenRes.StatusCode != http.StatusOK {
		return fmt.Errorf("token service returned HTTP %d", tokenRes.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(tokenRes.Body).Decode(&body); err != nil {
		return err
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return fmt.Errorf("token service returned no token")
	}
	rt.Console.Redact(token)
	check, err := get(ctx, client, base, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) })
	if err != nil {
		return err
	}
	check.Body.Close()
	if check.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d with an issued token", check.StatusCode)
	}
	return nil
}

func get(ctx context.Context, client *http.Client, u string, auth func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	auth(req)
	return client.Do(req)
}

// parseChallenge parses key="value" pairs of a WWW-Authenticate header.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			out[strings.ToLower(k)] = strings.Trim(v, `"`)
		}
	}
	return out
}
