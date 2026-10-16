// Package jenkins implements the CI/CD contract with Jenkins: Kubernetes pod
// agents, the shared pipeline library from the cluster repo, and a GitHub
// organization folder that discovers application repos.
package jenkins

import (
	"context"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const (
	namespace = "jenkins"
	release   = "jenkins"
	chartRepo = "https://charts.jenkins.io"

	// Credential IDs referenced by the shared pipeline library.
	vcsCredentialID      = "github-pat"
	registryCredentialID = "harbor-robot"
	alertsCredentialID   = "slack-token"
)

//go:embed values.yaml.tmpl
var valuesTemplate string

//go:embed org-folder.xml.tmpl
var orgFolderTemplate string

var (
	valuesTmpl    = template.Must(template.New("jenkins").Parse(valuesTemplate))
	orgFolderTmpl = template.Must(template.New("org-folder").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(orgFolderTemplate))
)

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.CICD = &stack.CICDOutput{URL: "https://" + rt.Setting("host")}
	_, err := rt.Secret("adminPassword")
	return err
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	var steps []stack.Step
	if !rt.Exec.Has("docker") {
		steps = append(steps, stack.Step{Action: "install", Target: "docker", Detail: "agent pods build images through the host Docker socket"})
	}
	steps = append(steps,
		stack.Step{Action: "ensure", Target: "credentials", Detail: credentialSummary(rt)},
		stack.HelmStep(ctx, rt, release, namespace, rt.Setting("chartVersion")),
		stack.Step{Action: "ensure", Target: "organization folder " + rt.Setting("orgFolder"),
			Detail: fmt.Sprintf("discovers %s/%s with %s", rt.Out.VCS.Owner, rt.Setting("repoFilter"), rt.Setting("jenkinsfilePath"))},
	)
	return steps
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if rt.Out.VCS.Kind != "github" {
		return fmt.Errorf("jenkins supports the github VCS provider only")
	}
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return err
	}

	if !rt.Exec.Has("docker") {
		rt.Logf("Installing Docker (agent pods build through the host Docker socket)")
		if err := stack.Retry(ctx, rt.Console, "docker install", 3, 20*time.Second, func() error {
			return rt.Exec.Shell(ctx, dockerInstallScript)
		}); err != nil {
			return err
		}
	}

	if err := rt.Kube.EnsureNamespace(ctx, namespace); err != nil {
		return err
	}
	// Jenkins deploys Helm charts into every environment, so its service
	// accounts are cluster admins. This is a deliberate single-tenant default.
	if err := rt.Kube.Apply(ctx, clusterAdminBinding()); err != nil {
		return err
	}
	if err := applyCredentials(ctx, rt); err != nil {
		return err
	}

	host := rt.Setting("host")
	values, err := stack.Render(valuesTmpl, valuesData(rt, password))
	if err != nil {
		return err
	}
	rt.Logf("Applying Jenkins %s at https://%s", rt.Setting("chartVersion"), host)
	if err := stack.Retry(ctx, rt.Console, "jenkins helm release", 3, 20*time.Second, func() error {
		return rt.Kube.HelmUpgrade(ctx, stack.Chart{
			Release: release, Namespace: namespace, Repo: chartRepo, Name: "jenkins",
			Version: rt.Setting("chartVersion"), Values: values, Overrides: rt.Values(),
			Timeout: 15 * time.Minute,
		})
	}); err != nil {
		return err
	}

	api, err := newClient(rt, password)
	if err != nil {
		return err
	}
	if err := stack.Retry(ctx, rt.Console, "jenkins ready", 40, 15*time.Second, func() error {
		return api.ready(ctx)
	}); err != nil {
		return err
	}

	folder := rt.Setting("orgFolder")
	exists, err := api.exists(ctx, folder)
	if err != nil {
		return err
	}
	if exists {
		rt.Logf("Jenkins organization folder %s exists", folder)
	} else {
		xmlConfig, err := stack.Render(orgFolderTmpl, map[string]string{
			"Owner":           rt.Out.VCS.Owner,
			"CredentialID":    vcsCredentialID,
			"RepoFilter":      rt.Setting("repoFilter"),
			"JenkinsfilePath": rt.Setting("jenkinsfilePath"),
		})
		if err != nil {
			return err
		}
		if err := stack.Retry(ctx, rt.Console, "create organization folder", 5, 15*time.Second, func() error {
			return api.post(ctx, "/createItem?name="+url.QueryEscape(folder), "application/xml", xmlConfig)
		}); err != nil {
			return err
		}
		rt.Logf("Jenkins organization folder %s created", folder)
	}
	if err := api.post(ctx, "/job/"+url.PathEscape(folder)+"/build", "", ""); err != nil {
		return fmt.Errorf("trigger repository scan: %w", err)
	}
	rt.Logf("Jenkins scanning %s repositories", rt.Out.VCS.Owner)
	return nil
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	checks := []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		stack.CheckHTTPS(ctx, rt.Setting("host"), rt.Out.Compute.PublicIP, "/login", false),
	}
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return append(checks, stack.Check{Name: "organization folder", State: stack.Unknown, Detail: "admin password not available"})
	}
	api, err := newClient(rt, password)
	if err == nil {
		var exists bool
		if exists, err = api.exists(ctx, rt.Setting("orgFolder")); err == nil && exists {
			return append(checks, stack.Check{Name: "organization folder", State: stack.OK, Detail: rt.Setting("orgFolder")})
		}
	}
	detail := "missing"
	if err != nil {
		detail = err.Error()
	}
	return append(checks, stack.Check{Name: "organization folder", State: stack.Fail, Detail: detail})
}

type envVar struct{ Key, Value string }

func valuesData(rt *stack.Runtime, password string) map[string]any {
	images, vcs := rt.Out.Images, rt.Out.VCS
	env := []envVar{
		{"IMAGE_REGISTRY", images.Host},
		{"IMAGE_REPOSITORY_PROJECT", images.Project},
		{"IMAGE_PULL_SECRET", images.PullSecretName},
		{"COMMON_HELM_LIBRARY_GITHUB_REPO", vcs.ClusterRepoURL},
		{"OWNSTACK_DOMAIN", rt.Domain()},
	}
	var slack *stack.AlertsOutput
	if a := rt.Out.Alerts; a != nil && a.Kind == "slack" {
		slack = a
		env = append(env, envVar{"OWNSTACK_SLACK_CHANNEL", a.Channel})
	}
	return map[string]any{
		"Host":               rt.Setting("host"),
		"Password":           password,
		"Annotations":        rt.Out.Ingress.Annotations,
		"GlobalEnv":          env,
		"VCSCredentialID":    vcsCredentialID,
		"ClusterRepoURL":     vcs.ClusterRepoURL,
		"Slack":              slack,
		"AlertsCredentialID": alertsCredentialID,
	}
}

// applyCredentials turns the VCS, registry, and alerts contract outputs into
// Jenkins credentials through the kubernetes-credentials-provider plugin.
func applyCredentials(ctx context.Context, rt *stack.Runtime) error {
	userPass := map[string]string{"jenkins.io/credentials-type": "usernamePassword"}
	images := rt.Out.Images
	if images.PushUsername == "" {
		return fmt.Errorf("the registry did not provide a push credential; apply the images aspect first")
	}
	creds := []struct {
		name   string
		data   map[string]string
		labels map[string]string
	}{
		{vcsCredentialID, map[string]string{"username": rt.Out.VCS.Username, "password": rt.Out.VCS.Token}, userPass},
		{registryCredentialID, map[string]string{"username": images.PushUsername, "password": images.PushPassword}, userPass},
	}
	if a := rt.Out.Alerts; a != nil && a.Kind == "slack" {
		creds = append(creds, struct {
			name   string
			data   map[string]string
			labels map[string]string
		}{alertsCredentialID, map[string]string{"text": a.Token}, map[string]string{"jenkins.io/credentials-type": "secretText"}})
	}
	for _, c := range creds {
		if err := rt.Kube.ApplySecret(ctx, namespace, c.name, "", c.data, c.labels); err != nil {
			return fmt.Errorf("credential %s: %w", c.name, err)
		}
	}
	rt.Logf("Jenkins credentials ready: %s", credentialSummary(rt))
	return nil
}

func credentialSummary(rt *stack.Runtime) string {
	ids := []string{vcsCredentialID, registryCredentialID}
	if a := rt.Out.Alerts; a != nil && a.Kind == "slack" {
		ids = append(ids, alertsCredentialID)
	}
	return strings.Join(ids, ", ")
}

func clusterAdminBinding() map[string]any {
	return map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]any{"name": "jenkins-cluster-admin"},
		"subjects": []map[string]any{
			{"kind": "ServiceAccount", "name": "default", "namespace": namespace},
			{"kind": "ServiceAccount", "name": "jenkins", "namespace": namespace},
		},
		"roleRef": map[string]any{"kind": "ClusterRole", "name": "cluster-admin", "apiGroup": "rbac.authorization.k8s.io"},
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

const dockerInstallScript = `export DEBIAN_FRONTEND=noninteractive
for pkg in docker.io docker-doc docker-compose docker-compose-v2 podman-docker runc; do
  apt-get remove -y -qq "$pkg" >/dev/null 2>&1 || true
done
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
. /etc/os-release
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${UBUNTU_CODENAME:-$VERSION_CODENAME} stable" > /etc/apt/sources.list.d/docker.list
apt-get update -qq
apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin
`

// ── API client ───────────────────────────────────────────────────────────────

type client struct {
	base, password string
	http           *http.Client
}

func newClient(rt *stack.Runtime, password string) (*client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	httpClient := *rt.ServiceClient()
	httpClient.Jar = jar
	return &client{base: "https://" + rt.Setting("host"), password: password, http: &httpClient}, nil
}

func (c *client) request(ctx context.Context, method, path, contentType, body string, crumb *crumb) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth("admin", c.password)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if crumb != nil {
		req.Header.Set(crumb.Field, crumb.Value)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res.StatusCode, data, nil
}

type crumb struct {
	Value string `json:"crumb"`
	Field string `json:"crumbRequestField"`
}

func (c *client) ready(ctx context.Context) error {
	code, _, err := c.request(ctx, http.MethodGet, "/api/json", "", "", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
}

func (c *client) exists(ctx context.Context, job string) (bool, error) {
	code, _, err := c.request(ctx, http.MethodGet, "/job/"+url.PathEscape(job)+"/api/json", "", "", nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("HTTP %d", code)
}

func (c *client) post(ctx context.Context, path, contentType, body string) error {
	code, data, err := c.request(ctx, http.MethodGet, "/crumbIssuer/api/json", "", "", nil)
	if err != nil {
		return err
	}
	var cr *crumb
	if code == http.StatusOK {
		cr = &crumb{}
		if err := json.Unmarshal(data, cr); err != nil {
			return err
		}
	}
	code, data, err = c.request(ctx, http.MethodPost, path, contentType, body, cr)
	if err != nil {
		return err
	}
	if code >= 400 {
		return fmt.Errorf("POST %s: HTTP %d: %s", path, code, firstLine(string(data)))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (Provider) Installed(ctx context.Context, rt *stack.Runtime) bool {
	_, ok := rt.Kube.Release(ctx, release, namespace)
	return ok
}

// Remove uninstalls the release and its namespace, including stored data.
func (Provider) Remove(ctx context.Context, rt *stack.Runtime) error {
	if err := rt.Kube.HelmUninstall(ctx, release, namespace); err != nil {
		return err
	}
	if err := rt.Kube.Delete(ctx, "clusterrolebinding", "jenkins-cluster-admin"); err != nil {
		return err
	}
	return rt.Kube.Delete(ctx, "namespace", namespace)
}
