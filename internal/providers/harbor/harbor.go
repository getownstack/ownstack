// Package harbor implements the image registry contract with Harbor running
// in the cluster.
package harbor

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the harbor provider's catalog entry.
var Spec = catalog.Provider{
	ID: "harbor", Aspect: "images", Name: "Harbor", Mono: "HB", Hosting: catalog.SelfHosted,
	Summary:   "A full private registry in your cluster, with projects and robot accounts.",
	Resources: catalog.Resources{MemoryMiB: 1500, DiskGiB: 20},
	Params: []catalog.Param{
		{Key: "adminPassword", Label: "Admin password", Kind: catalog.KindSecret, Required: true, Generate: true, Env: "HARBOR_ADMIN_PASSWORD",
			Help: "Generated on the server when not provided."},
	},
}

const (
	namespace    = "harbor"
	release      = "harbor"
	chartRepo    = "https://helm.goharbor.io"
	chartVersion = "1.17.2"
	// project is the Harbor project CI pushes images to.
	project = "apps"
	// robotName is the CI robot account; Harbor reports it as robot$ownstack-ci.
	robotName = "ownstack-ci"
	// robotSecret stores the robot credential in the registry's own namespace
	// so reruns reuse it instead of rotating it.
	robotSecret = "ownstack-registry-robot"
)

// robotCheckTimeout is how long apply waits for Harbor to accept or reject
// the stored robot credential, so a Harbor restart or a proxy error never
// rotates it.
var robotCheckTimeout = time.Minute

//go:embed values.yaml.tmpl
var valuesTemplate string

var tmpl = template.Must(template.New("harbor").Parse(valuesTemplate))

type Provider struct{}

func (Provider) Resolve(ctx context.Context, rt *stack.Runtime) error {
	out := &stack.ImagesOutput{
		Host:    rt.AdminHost("harbor"),
		Project: project,
		Login:   "user admin, password HARBOR_ADMIN_PASSWORD",
	}
	rt.Out.Images = out
	if user, ok := rt.Kube.SecretValue(ctx, namespace, robotSecret, "username"); ok {
		out.PushUsername = user
		out.PushPassword, _ = rt.Kube.SecretValue(ctx, namespace, robotSecret, "password")
		rt.Console.Redact(out.PushPassword)
	}
	_, err := rt.Secret("adminPassword")
	return err
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return err
	}
	host := rt.Out.Images.Host
	values, err := stack.Render(tmpl, map[string]any{
		"Host":        host,
		"Password":    password,
		"Annotations": rt.Out.Ingress.Annotations,
	})
	if err != nil {
		return err
	}
	rt.Logf("Applying Harbor %s at https://%s", chartVersion, host)
	if err := rt.Kube.HelmUpgrade(ctx, stack.Chart{
		Release: release, Namespace: namespace, Repo: chartRepo, Name: "harbor",
		Version: chartVersion, Values: values, Overrides: rt.Values(),
		Timeout: 20 * time.Minute,
	}); err != nil {
		return err
	}

	// Docker on the node and in CI refuses untrusted registry certificates,
	// so Harbor is ready once it is healthy behind a trusted one.
	api := &client{base: "https://" + host + "/api/v2.0", user: "admin", password: password, http: rt.ServiceClient()}
	if err := stack.WaitFor(ctx, rt, "Harbor healthy behind a trusted certificate", 15*time.Minute, func(ctx context.Context) (bool, string) {
		if err := api.healthy(ctx); err != nil {
			return false, err.Error()
		}
		if check := rt.CheckHTTPS(ctx, host, "/api/v2.0/ping"); check.State != stack.OK {
			return false, check.Detail
		}
		return true, ""
	}); err != nil {
		return err
	}
	rt.Logf("Harbor is healthy at https://%s", host)

	if err := api.ensureProject(ctx, project); err != nil {
		return err
	}
	rt.Logf("Harbor project %s ready", project)

	out := rt.Out.Images
	if out.PushUsername != "" {
		robotAPI := &client{base: api.base, user: out.PushUsername, password: out.PushPassword, http: api.http}
		valid, err := checkRobot(ctx, rt, robotAPI)
		if err != nil {
			return err
		}
		if valid {
			rt.Logf("Harbor robot %s is valid", out.PushUsername)
			return nil
		}
		rt.Logf("Harbor rejects the stored robot credential; recreating it")
	}
	name, secret, err := api.recreateRobot(ctx)
	if err != nil {
		return err
	}
	rt.Console.Redact(secret)
	out.PushUsername, out.PushPassword = name, secret
	if err := rt.Kube.ApplySecret(ctx, namespace, robotSecret, "", map[string]string{"username": name, "password": secret}, nil); err != nil {
		return err
	}
	rt.Logf("Harbor robot %s created", name)
	return nil
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	host := rt.Out.Images.Host
	checks := []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		rt.CheckHTTPS(ctx, host, "/api/v2.0/ping"),
	}
	api := &client{base: "https://" + host + "/api/v2.0", http: rt.ServiceClient()}
	if err := api.healthy(ctx); err != nil {
		checks = append(checks, stack.Check{Name: "health", State: stack.Fail, Detail: err.Error()})
	} else {
		checks = append(checks, stack.Check{Name: "health", State: stack.OK, Detail: "all components healthy"})
	}
	if rt.Out.Images.PushUsername == "" {
		checks = append(checks, stack.Check{Name: "robot", State: stack.Fail, Detail: "no CI robot credential stored"})
	} else {
		robotAPI := &client{base: api.base, user: rt.Out.Images.PushUsername, password: rt.Out.Images.PushPassword, http: api.http}
		switch valid, err := robotAPI.robotAccess(ctx, project); {
		case err != nil:
			checks = append(checks, stack.Check{Name: "robot", State: stack.Unknown, Detail: err.Error()})
		case valid:
			checks = append(checks, stack.Check{Name: "robot", State: stack.OK, Detail: rt.Out.Images.PushUsername})
		default:
			checks = append(checks, stack.Check{Name: "robot", State: stack.Fail, Detail: "credential rejected; rerun apply"})
		}
	}
	return checks
}

// ── API client ───────────────────────────────────────────────────────────────

type client struct {
	base, user, password string
	http                 *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data, nil
}

func (c *client) healthy(ctx context.Context) error {
	code, data, err := c.do(ctx, http.MethodGet, "/health", nil)
	if err != nil {
		return err
	}
	var health struct {
		Status     string
		Components []struct{ Name, Status string }
	}
	if code != http.StatusOK || json.Unmarshal(data, &health) != nil {
		return fmt.Errorf("health endpoint returned HTTP %d", code)
	}
	if health.Status != "healthy" {
		var unhealthy []string
		for _, comp := range health.Components {
			if comp.Status != "healthy" {
				unhealthy = append(unhealthy, comp.Name)
			}
		}
		return fmt.Errorf("unhealthy components: %s", strings.Join(unhealthy, ", "))
	}
	return nil
}

func (c *client) ensureProject(ctx context.Context, name string) error {
	code, _, err := c.do(ctx, http.MethodHead, "/projects?project_name="+url.QueryEscape(name), nil)
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return nil
	}
	code, data, err := c.do(ctx, http.MethodPost, "/projects", map[string]any{
		"project_name": name,
		"metadata":     map[string]string{"public": "false"},
	})
	if err != nil {
		return err
	}
	if code != http.StatusCreated && code != http.StatusConflict {
		return fmt.Errorf("create project %s: HTTP %d: %s", name, code, strings.TrimSpace(string(data)))
	}
	return nil
}

// robotAccess reports whether the client's credential can list the
// project's repositories: true on HTTP 200, false when Harbor rejects the
// credential (401 or 403). Anything else (a timeout, a proxy error, Harbor
// restarting) is an error, because it proves nothing about the credential.
func (c *client) robotAccess(ctx context.Context, project string) (bool, error) {
	code, _, err := c.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(project)+"/repositories?page_size=1", nil)
	switch {
	case err != nil:
		return false, err
	case code == http.StatusOK:
		return true, nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return false, nil
	}
	return false, fmt.Errorf("checking the robot credential: HTTP %d", code)
}

// checkRobot waits until Harbor gives a definite answer about the robot
// credential, so the robot is only recreated when Harbor actually rejects it.
func checkRobot(ctx context.Context, rt *stack.Runtime, robotAPI *client) (bool, error) {
	var valid bool
	err := stack.WaitFor(ctx, rt, "Harbor to check the robot credential", robotCheckTimeout, func(ctx context.Context) (bool, string) {
		var err error
		if valid, err = robotAPI.robotAccess(ctx, project); err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	return valid, err
}

// recreateRobot deletes the CI robot if it exists and creates it again,
// returning its full name and secret. Harbor only reveals a robot's secret
// at creation time.
func (c *client) recreateRobot(ctx context.Context) (string, string, error) {
	code, data, err := c.do(ctx, http.MethodGet, "/robots?page_size=100", nil)
	if err != nil {
		return "", "", err
	}
	if code != http.StatusOK {
		return "", "", fmt.Errorf("list robots: HTTP %d", code)
	}
	var robots []struct {
		ID   int64
		Name string
	}
	if err := json.Unmarshal(data, &robots); err != nil {
		return "", "", err
	}
	for _, r := range robots {
		if r.Name == robotName || strings.HasSuffix(r.Name, "$"+robotName) {
			if code, _, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/robots/%d", r.ID), nil); err != nil || code >= 300 {
				return "", "", fmt.Errorf("delete old robot: HTTP %d %v", code, err)
			}
		}
	}

	access := []map[string]string{
		{"action": "push", "resource": "repository"},
		{"action": "pull", "resource": "repository"},
		{"action": "create", "resource": "tag"},
		{"action": "delete", "resource": "tag"},
		{"action": "list", "resource": "repository"},
	}
	code, data, err = c.do(ctx, http.MethodPost, "/robots", map[string]any{
		"name":        robotName,
		"description": "Ownstack CI push and environment pull credential",
		"level":       "system",
		"disable":     false,
		"duration":    -1,
		"permissions": []map[string]any{{"kind": "project", "namespace": "*", "access": access}},
	})
	if err != nil {
		return "", "", err
	}
	if code != http.StatusCreated {
		return "", "", fmt.Errorf("create robot: HTTP %d: %s", code, strings.TrimSpace(string(data)))
	}
	var created struct{ Name, Secret string }
	if err := json.Unmarshal(data, &created); err != nil || created.Secret == "" {
		return "", "", fmt.Errorf("create robot: unexpected response")
	}
	return created.Name, created.Secret, nil
}
