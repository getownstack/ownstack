// Package jenkins implements the CI/CD contract with Jenkins: Kubernetes pod
// agents, the shared pipeline library from the Ownstack repository
// (stack.LibraryRepo) at the stack's library ref, and a GitHub organization
// folder that discovers application repos.
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
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
	"github.com/getownstack/ownstack/internal/stack"
)

const (
	namespace = "jenkins"
	release   = "jenkins"
	chartRepo = "https://charts.jenkins.io"

	// Credential IDs referenced by the shared pipeline library.
	vcsCredentialID      = "github-pat"
	registryCredentialID = "harbor-robot"
	alertsCredentialID   = "slack-token"

	// The shared library's defaults when Jenkins global env does not set
	// OWNSTACK_DEPLOY_BRANCH or OWNSTACK_NAMESPACE.
	defaultDeployBranch = "main"
	defaultNamespace    = "prod"
	defaultGitHubAPI    = "https://api.github.com"
)

//go:embed values.yaml.tmpl
var valuesTemplate string

//go:embed org-folder.xml.tmpl
var orgFolderTemplate string

// plugins.lock is every plugin Jenkins installs, at exact versions resolved
// and verified for one chart and Jenkins core. Regenerate it with
// `go run ./internal/providers/jenkins/lockgen`.
//
//go:embed plugins.lock
var lockFile []byte

var pluginLock = mustParseLock(lockFile)

func mustParseLock(data []byte) *pluginlock.Lock {
	l, err := pluginlock.ParseLock(data)
	if err != nil {
		panic(err)
	}
	return l
}

var (
	valuesTmpl    = template.Must(template.New("jenkins").Parse(valuesTemplate))
	orgFolderTmpl = template.Must(template.New("org-folder").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(orgFolderTemplate))
)

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.CICD = &stack.CICDOutput{URL: "https://" + rt.Setting("host"), Login: "user admin, password JENKINS_ADMIN_PASSWORD"}
	if _, err := extraPlugins(rt); err != nil {
		return err
	}
	_, err := rt.Secret("adminPassword")
	return err
}

// extraPlugins parses the extraPlugins setting: name:version pairs added to,
// or replacing versions in, the locked set.
func extraPlugins(rt *stack.Runtime) ([]pluginlock.Entry, error) {
	extra, err := pluginlock.ParseExtra(rt.Setting("extraPlugins"))
	if err != nil {
		return nil, fmt.Errorf("extraPlugins: %w", err)
	}
	return extra, nil
}

// installPlugins is controller.installPlugins: the lock plus extraPlugins,
// as name:version lines. Resolve rejects a malformed extraPlugins first.
func installPlugins(rt *stack.Runtime) []string {
	extra, _ := extraPlugins(rt)
	entries := pluginLock.With(extra)
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.String()
	}
	return out
}

// chartMismatch describes the risk of a chartVersion other than the one the
// plugins are locked for, or is empty.
func chartMismatch(rt *stack.Runtime) string {
	if chart := rt.Setting("chartVersion"); chart != "" && chart != pluginLock.Chart {
		return fmt.Sprintf("plugins are locked for chart %s / Jenkins %s; a different chart may not start", pluginLock.Chart, pluginLock.Jenkins)
	}
	return ""
}

// pluginSummary describes the Jenkins version and plugin set, for plan.
func pluginSummary(rt *stack.Runtime) string {
	summary := fmt.Sprintf("Jenkins %s, %d locked plugins", pluginLock.Jenkins, len(pluginLock.Plugins))
	if extra, err := extraPlugins(rt); err == nil && len(extra) > 0 {
		var overrides []string
		for _, e := range extra {
			if v, ok := pluginLock.Version(e.Name); ok && v != e.Version {
				overrides = append(overrides, fmt.Sprintf("%s %s → %s", e.Name, v, e.Version))
			}
		}
		summary += fmt.Sprintf(" (+%d extra", len(extra))
		if len(overrides) > 0 {
			summary += "; " + strings.Join(overrides, ", ")
		}
		summary += ")"
	}
	if warning := chartMismatch(rt); warning != "" {
		summary += "; warning: " + warning
	}
	return summary
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	var steps []stack.Step
	if !rt.Exec.Has("docker") {
		steps = append(steps, stack.Step{Action: "install", Target: "docker", Detail: "agent pods build images through the host Docker socket"})
	}
	steps = append(steps,
		stack.Step{Action: "ensure", Target: "credentials", Detail: credentialSummary(rt)},
		stack.HelmStep(ctx, rt, release, namespace, rt.Setting("chartVersion")),
		stack.Step{Action: "ensure", Target: "plugins", Detail: pluginSummary(rt)},
		stack.Step{Action: "configure", Target: "pipeline libraries", Detail: libraryDetail(rt)},
		folderStep(ctx, rt),
	)
	return steps
}

// folderStep plans the organization folder, and says what changes when an
// existing folder's managed fields differ from the manifest.
func folderStep(ctx context.Context, rt *stack.Runtime) stack.Step {
	folder := rt.Setting("orgFolder")
	step := stack.Step{Action: "ensure", Target: "organization folder " + folder,
		Detail: fmt.Sprintf("discovers %s/%s with %s; %s",
			rt.Out.VCS.Owner, rt.Setting("repoFilter"), rt.Setting("jenkinsfilePath"), deploySummary(branchMappings(rt)))}
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return step
	}
	desired, err := stack.Render(orgFolderTmpl, orgFolderData(rt))
	if err != nil {
		return step
	}
	api, err := newClient(rt, password)
	if err != nil {
		return step
	}
	current, exists, err := api.get(ctx, "/job/"+url.PathEscape(folder)+"/config.xml")
	if err != nil || !exists {
		return step
	}
	changes, ok, err := folderChanges(current, desired)
	if err != nil || !ok || len(changes) == 0 {
		return step
	}
	return stack.Step{Action: "update", Target: "organization folder " + folder,
		Detail: strings.Join(changes, ", ") + "; Jenkins then deletes the jobs and build history of repos that no longer match"}
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if rt.Out.VCS.Kind != "github" {
		return fmt.Errorf("jenkins supports the github VCS provider only")
	}
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return err
	}
	if _, err := extraPlugins(rt); err != nil {
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
	if warning := chartMismatch(rt); warning != "" {
		rt.Logf("warning: %s", warning)
	}
	rt.Logf("Applying Jenkins %s (chart %s, %d plugins) at https://%s", pluginLock.Jenkins, rt.Setting("chartVersion"), len(installPlugins(rt)), host)
	rt.Logf("Pipelines take %s", libraryDetail(rt))
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
	xmlConfig, err := stack.Render(orgFolderTmpl, orgFolderData(rt))
	if err != nil {
		return err
	}
	current, exists, err := api.get(ctx, "/job/"+url.PathEscape(folder)+"/config.xml")
	if err != nil {
		return err
	}
	if exists {
		if err := convergeFolder(ctx, rt, api, folder, current, xmlConfig); err != nil {
			return err
		}
	} else {
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
		rt.CheckHTTPS(ctx, rt.Setting("host"), "/login"),
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

// convergeFolder replaces an existing organization folder's config.xml when
// a field ownstackctl manages differs from the manifest. Admin edits to the
// folder survive only while the managed fields match.
func convergeFolder(ctx context.Context, rt *stack.Runtime, api *client, folder, current, desired string) error {
	changes, ok, err := folderChanges(current, desired)
	switch {
	case err != nil:
		rt.Logf("warning: cannot read Jenkins folder %s (%v); leaving it unchanged", folder, err)
		return nil
	case !ok:
		rt.Logf("warning: Jenkins item %s is not an organization folder; leaving it unchanged", folder)
		return nil
	case len(changes) == 0:
		rt.Logf("Jenkins organization folder %s is up to date", folder)
		return nil
	}
	if err := stack.Retry(ctx, rt.Console, "update organization folder", 5, 15*time.Second, func() error {
		return api.post(ctx, "/job/"+url.PathEscape(folder)+"/config.xml", "application/xml", desired)
	}); err != nil {
		return err
	}
	rt.Logf("Jenkins organization folder %s updated: %s", folder, strings.Join(changes, ", "))
	return nil
}

// deployBranch is the branch Jenkins builds automatically and deploys from.
func deployBranch(rt *stack.Runtime) string {
	if b := rt.Setting("deployBranch"); b != "" {
		return b
	}
	return defaultDeployBranch
}

// deployNamespace is the environment the deploy branch deploys into: the
// production environment.
func deployNamespace(rt *stack.Runtime) string {
	if ns := rt.Setting("environment"); ns != "" {
		return ns
	}
	return defaultNamespace
}

// branchMapping maps a branch, a name or a glob where '*' matches any run of
// characters including '/', to the environment it deploys into.
type branchMapping struct{ Branch, Environment string }

// branchMappings is the effective mapping: the deploy branch to the
// production environment, then the branches setting's <branch>=<environment>
// pairs in order. The first match wins, so a pair whose branch is already
// mapped is dropped; malformed pairs (which validation rejects) are skipped.
func branchMappings(rt *stack.Runtime) []branchMapping {
	mappings := []branchMapping{{deployBranch(rt), deployNamespace(rt)}}
	seen := map[string]bool{mappings[0].Branch: true}
	for _, pair := range strings.Fields(rt.Setting("branches")) {
		branch, environment, ok := strings.Cut(pair, "=")
		if !ok || branch == "" || environment == "" || strings.Contains(environment, "=") || seen[branch] {
			continue
		}
		seen[branch] = true
		mappings = append(mappings, branchMapping{branch, environment})
	}
	return mappings
}

// branchesEnv formats mappings for OWNSTACK_BRANCHES, the shared library's
// input: space-separated <branch>=<environment> pairs, deploy branch first.
func branchesEnv(mappings []branchMapping) string {
	pairs := make([]string, len(mappings))
	for i, m := range mappings {
		pairs[i] = m.Branch + "=" + m.Environment
	}
	return strings.Join(pairs, " ")
}

// globRegexp turns a branch glob into a regular expression: '*' matches any
// run of characters, everything else is literal. A name without '*' gives
// regexp.QuoteMeta(name), which Java's regex syntax reads the same way.
func globRegexp(glob string) string {
	parts := strings.Split(glob, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return strings.Join(parts, ".*")
}

// triggerBranches is the organization folder's NoTrigger regular expression:
// every mapped branch, so each one builds and deploys on push. With only the
// deploy branch it is the quoted deploy branch, as before branch mappings.
func triggerBranches(mappings []branchMapping) string {
	alternatives := make([]string, len(mappings))
	for i, m := range mappings {
		alternatives[i] = globRegexp(m.Branch)
	}
	return strings.Join(alternatives, "|")
}

// deploySummary describes which branches deploy where, for plan.
func deploySummary(mappings []branchMapping) string {
	if len(mappings) == 1 {
		return fmt.Sprintf("builds %s automatically and deploys it to %s", mappings[0].Branch, mappings[0].Environment)
	}
	pairs := make([]string, len(mappings))
	for i, m := range mappings {
		pairs[i] = m.Branch + " to " + m.Environment
	}
	return "builds and deploys automatically " + strings.Join(pairs, ", ")
}

func orgFolderData(rt *stack.Runtime) map[string]string {
	api := rt.Out.VCS.APIURL
	if api == "" {
		api = defaultGitHubAPI
	}
	return map[string]string{
		"Owner":           rt.Out.VCS.Owner,
		"APIURL":          api,
		"CredentialID":    vcsCredentialID,
		"RepoFilter":      rt.Setting("repoFilter"),
		"JenkinsfilePath": rt.Setting("jenkinsfilePath"),
		// NoTriggerOrganizationFolderProperty takes a regular expression,
		// which Jenkins matches against the whole branch name.
		"TriggerBranches": triggerBranches(branchMappings(rt)),
	}
}

// folderConfig is what ownstackctl manages in an organization folder's
// config.xml.
type folderConfig struct {
	XMLName         xml.Name
	Owner           string `xml:"navigators>org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator>repoOwner"`
	APIURL          string `xml:"navigators>org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator>apiUri"`
	CredentialID    string `xml:"navigators>org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator>credentialsId"`
	Includes        string `xml:"navigators>org.jenkinsci.plugins.github__branch__source.GitHubSCMNavigator>traits>jenkins.scm.impl.trait.WildcardSCMSourceFilterTrait>includes"`
	ScriptPath      string `xml:"projectFactories>org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProjectFactory>scriptPath"`
	TriggerBranches string `xml:"properties>jenkins.branch.NoTriggerOrganizationFolderProperty>branches"`
}

func parseFolderConfig(raw string) (folderConfig, error) {
	// Jenkins writes XML 1.1, which encoding/xml refuses; the body is
	// 1.0-compatible.
	if strings.HasPrefix(strings.TrimSpace(raw), "<?xml") {
		if i := strings.Index(raw, "?>"); i >= 0 {
			raw = raw[i+2:]
		}
	}
	var c folderConfig
	if err := xml.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	// Jenkins may normalize whitespace; an unset API URL means github.com.
	c.Owner, c.CredentialID = strings.TrimSpace(c.Owner), strings.TrimSpace(c.CredentialID)
	c.ScriptPath, c.TriggerBranches = strings.TrimSpace(c.ScriptPath), strings.TrimSpace(c.TriggerBranches)
	c.Includes = strings.Join(strings.Fields(c.Includes), " ")
	if c.APIURL = strings.TrimSpace(c.APIURL); c.APIURL == "" {
		c.APIURL = defaultGitHubAPI
	}
	return c, nil
}

// folderChanges lists the managed fields of an existing folder that differ
// from the desired config, as "field old → new". ok is false when the
// existing item is not an organization folder.
func folderChanges(current, desired string) (changes []string, ok bool, err error) {
	have, err := parseFolderConfig(current)
	if err != nil {
		return nil, false, err
	}
	want, err := parseFolderConfig(desired)
	if err != nil {
		return nil, false, err
	}
	if have.XMLName.Local != want.XMLName.Local {
		return nil, false, nil
	}
	for _, f := range []struct{ name, have, want string }{
		{"owner", have.Owner, want.Owner},
		{"API", have.APIURL, want.APIURL},
		{"credential", have.CredentialID, want.CredentialID},
		{"repository filter", have.Includes, want.Includes},
		{"Jenkinsfile path", have.ScriptPath, want.ScriptPath},
		{"auto-build branches", have.TriggerBranches, want.TriggerBranches},
	} {
		if f.have != f.want {
			changes = append(changes, fmt.Sprintf("%s %q → %q", f.name, f.have, f.want))
		}
	}
	return changes, true, nil
}

// libraryDetail says where pipelines take the Ownstack libraries from, for
// plan and apply.
func libraryDetail(rt *stack.Runtime) string {
	return fmt.Sprintf("jenkins_pipeline_library and common_helm_library from %s@%s", stack.LibraryRepo, rt.LibraryRef())
}

type envVar struct{ Key, Value string }

func valuesData(rt *stack.Runtime, password string) map[string]any {
	images := rt.Out.Images
	ref := rt.LibraryRef()
	env := []envVar{
		{"IMAGE_REGISTRY", images.Host},
		{"IMAGE_REPOSITORY_PROJECT", images.Project},
		{"IMAGE_PULL_SECRET", images.PullSecretName},
		// helmDeploy fetches common_helm_library from the repository and ref
		// the pipeline library itself is loaded from.
		{"OWNSTACK_LIBRARY_REPO", stack.LibraryRepoURL},
		{"OWNSTACK_LIBRARY_REF", ref},
		// Pipeline libraries from before OWNSTACK_LIBRARY_REPO (a libraryRef
		// that names an older release) fetch common_helm_library from here at
		// main; cluster repos no longer carry it.
		{"COMMON_HELM_LIBRARY_GITHUB_REPO", stack.LibraryRepoURL},
		{"OWNSTACK_DOMAIN", rt.Domain()},
		// Libraries without branch mappings deploy only this branch, into
		// this namespace.
		{"OWNSTACK_DEPLOY_BRANCH", deployBranch(rt)},
		{"OWNSTACK_NAMESPACE", deployNamespace(rt)},
		// Newer libraries deploy each mapped branch into its environment
		// and tell the Helm library which environment is production.
		{"OWNSTACK_BRANCHES", branchesEnv(branchMappings(rt))},
		{"OWNSTACK_PRODUCTION_ENVIRONMENT", deployNamespace(rt)},
	}
	var slack *stack.AlertsOutput
	if a := rt.Out.Alerts; a != nil && a.Kind == "slack" {
		slack = a
		env = append(env, envVar{"OWNSTACK_SLACK_CHANNEL", a.Channel})
	}
	return map[string]any{
		"Host":               rt.Setting("host"),
		"ImageTag":           pluginLock.ImageTag(),
		"Plugins":            installPlugins(rt),
		"Password":           password,
		"Annotations":        rt.Out.Ingress.Annotations,
		"GlobalEnv":          env,
		"LibraryRepoURL":     stack.LibraryRepoURL,
		"LibraryRef":         ref,
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
		{registryCredentialID, map[string]string{"username": shellEscapeDollar(images.PushUsername), "password": shellEscapeDollar(images.PushPassword)}, userPass},
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

// shellEscapeDollar escapes $ for credentials used inside the Kubernetes
// plugin's container() steps, which run commands through a shell: Harbor's
// robot$name username would otherwise reach docker login as "robot".
func shellEscapeDollar(s string) string { return strings.ReplaceAll(s, "$", `\$`) }

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

// get returns a page; exists is false on 404.
func (c *client) get(ctx context.Context, path string) (body string, exists bool, err error) {
	code, data, err := c.request(ctx, http.MethodGet, path, "", "", nil)
	if err != nil {
		return "", false, err
	}
	switch code {
	case http.StatusOK:
		return string(data), true, nil
	case http.StatusNotFound:
		return "", false, nil
	}
	return "", false, fmt.Errorf("GET %s: HTTP %d", path, code)
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
