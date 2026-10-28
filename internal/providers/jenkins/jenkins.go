// Package jenkins implements the CI/CD contract with Jenkins: Kubernetes pod
// agents, the shared pipeline library from the Ownstack repository
// (stack.LibraryRepo) at the running release's tag, and a GitHub
// organization folder that builds the listed app repos.
package jenkins

import (
	"context"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
	"github.com/getownstack/ownstack/internal/stack"
)

// Jenkins plugin pins: space-separated <name>:<version> pairs.
const pluginsPattern = `^[a-z0-9][a-z0-9_.-]*:[A-Za-z0-9_.+-]+( +[a-z0-9][a-z0-9_.-]*:[A-Za-z0-9_.+-]+)*$`

// Spec is the jenkins provider's catalog entry.
var Spec = catalog.Provider{
	ID: "jenkins", Aspect: "cicd", Name: "Jenkins", Mono: "JK", Hosting: catalog.SelfHosted,
	Summary:   "Jenkins with Kubernetes pod agents and the shared Ownstack pipeline library.",
	Resources: catalog.Resources{MemoryMiB: 1500, DiskGiB: 10},
	Params: []catalog.Param{
		{Key: "adminPassword", Label: "Admin password", Kind: catalog.KindSecret, Required: true, Generate: true, Env: "JENKINS_ADMIN_PASSWORD",
			Help: "Generated on the server when not provided."},
		{Key: "repos", Label: "App repositories", Kind: catalog.KindString, Placeholder: "api web",
			Help:        "Space-separated repos under the VCS owner that Jenkins builds from their " + jenkinsfilePath + ". The demo repo is always included.",
			Pattern:     `^ *[A-Za-z0-9._-]+(?: +[A-Za-z0-9._-]+)* *$`,
			PatternHelp: "must be space-separated repo names under the VCS owner, such as api web"},
		{Key: "extraPlugins", Label: "Extra plugins", Kind: catalog.KindString, Advanced: true, Placeholder: "build-timeout:1.41 ansicolor:542.v03d235fee02d",
			Help:    "Plugins to add to the locked set, as space-separated name:version pairs; versions are required so installs stay reproducible. A locked plugin listed here gets this version instead. Dependencies missing from the locked set are installed at the oldest version the plugin accepts, so list them too.",
			Pattern: pluginsPattern, PatternHelp: "must be space-separated name:version pairs, such as build-timeout:1.41"},
	},
}

const (
	namespace = "jenkins"
	release   = "jenkins"
	chartRepo = "https://charts.jenkins.io"

	// orgFolder is the organization folder that builds the app repos.
	orgFolder       = "Repositories"
	jenkinsfilePath = "infrastructure/Jenkinsfile"

	// Credential IDs referenced by the organization folder and the shared
	// pipeline library.
	vcsCredentialID      = "github-pat"
	registryCredentialID = "registry"

	defaultGitHubAPI = "https://api.github.com"
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
	rt.Out.CICD = &stack.CICDOutput{URL: "https://" + rt.AdminHost("jenkins"), Login: "user admin, password JENKINS_ADMIN_PASSWORD"}
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
	// Jenkins deploys Helm charts into every environment, so its service
	// accounts are cluster admins. This is a deliberate single-tenant default.
	if err := rt.Kube.Apply(ctx, clusterObjects(rt)...); err != nil {
		return err
	}
	rt.Logf("Jenkins credentials ready: %s, %s", vcsCredentialID, registryCredentialID)

	host := rt.AdminHost("jenkins")
	values, err := stack.Render(valuesTmpl, valuesData(rt, password))
	if err != nil {
		return err
	}
	rt.Logf("Applying Jenkins %s (chart %s, %d plugins) at https://%s", pluginLock.Jenkins, ChartVersion, len(installPlugins(rt)), host)
	rt.Logf("Pipelines take jenkins_pipeline_library and common_helm_library from %s@%s", stack.LibraryRepo, stack.LibraryRef(stack.EngineVersion))
	// Agent pods build through the host Docker socket. Docker is installed
	// while the release starts; neither needs the other. A failed release
	// does not cancel the install: apt killed halfway leaves dpkg broken.
	docker := make(chan error, 1)
	go func() { docker <- installDocker(ctx, rt) }()
	helmErr := rt.Kube.HelmUpgrade(ctx, stack.Chart{
		Release: release, Namespace: namespace, Repo: chartRepo, Name: "jenkins",
		Version: ChartVersion, Values: values, Overrides: rt.Values(),
		Timeout: 15 * time.Minute,
	})
	if err := errors.Join(helmErr, <-docker); err != nil {
		return err
	}

	api, err := newClient(rt, password)
	if err != nil {
		return err
	}
	if err := stack.WaitFor(ctx, rt, "Jenkins API", 5*time.Minute, func(ctx context.Context) (bool, string) {
		_, ok, err := api.get(ctx, "/api/json")
		switch {
		case err != nil:
			return false, err.Error()
		case !ok:
			return false, "HTTP 404"
		}
		return true, ""
	}); err != nil {
		return err
	}
	if err := applyFolder(ctx, rt, api); err != nil {
		return err
	}
	if err := api.post(ctx, "/job/"+url.PathEscape(orgFolder)+"/build", "", ""); err != nil {
		return fmt.Errorf("trigger repository scan: %w", err)
	}
	if names := repos(rt); len(names) > 0 {
		rt.Logf("Jenkins scanning %s/{%s}", rt.Out.VCS.Owner, strings.Join(names, ","))
	} else {
		rt.Logf("No app repos yet: list them in the cicd repos setting and rerun apply.")
	}
	return nil
}

// installDocker installs Docker Engine unless the host has it.
func installDocker(ctx context.Context, rt *stack.Runtime) error {
	if rt.Exec.Has("docker") {
		return nil
	}
	rt.Logf("Installing Docker (agent pods build through the host Docker socket)")
	// apt fails while another apt run (unattended upgrades on a fresh
	// server) holds its lock.
	return stack.Retry(ctx, rt.Console, "docker install", 3, 20*time.Second, func() error {
		return rt.Exec.Shell(ctx, dockerInstallScript)
	})
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	checks := []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		rt.CheckHTTPS(ctx, rt.AdminHost("jenkins"), "/login"),
	}
	password, err := rt.Secret("adminPassword")
	if err != nil {
		return append(checks, stack.Check{Name: "organization folder", State: stack.Unknown, Detail: "admin password not available"})
	}
	api, err := newClient(rt, password)
	if err == nil {
		var exists bool
		if _, exists, err = api.get(ctx, "/job/"+url.PathEscape(orgFolder)+"/config.xml"); err == nil && exists {
			return append(checks, stack.Check{Name: "organization folder", State: stack.OK, Detail: orgFolder})
		}
	}
	detail := "missing"
	if err != nil {
		detail = err.Error()
	}
	return append(checks, stack.Check{Name: "organization folder", State: stack.Fail, Detail: detail})
}

// applyFolder creates the organization folder, or replaces its config.xml
// when a field ownstackctl manages differs. Admin edits to the folder
// survive only while the managed fields match. Jenkins may refuse items for
// a moment after its API answers, so posts are retried.
func applyFolder(ctx context.Context, rt *stack.Runtime, api *client) error {
	desired, err := stack.Render(orgFolderTmpl, orgFolderData(rt))
	if err != nil {
		return err
	}
	current, exists, err := api.get(ctx, "/job/"+url.PathEscape(orgFolder)+"/config.xml")
	if err != nil {
		return err
	}
	path, done := "/createItem?name="+url.QueryEscape(orgFolder), "created"
	if exists {
		changes, ok, err := folderChanges(current, desired)
		switch {
		case err != nil:
			rt.Logf("warning: cannot read Jenkins folder %s (%v); leaving it unchanged", orgFolder, err)
			return nil
		case !ok:
			rt.Logf("warning: Jenkins item %s is not an organization folder; leaving it unchanged", orgFolder)
			return nil
		case len(changes) == 0:
			rt.Logf("Jenkins organization folder %s is up to date", orgFolder)
			return nil
		}
		path, done = "/job/"+url.PathEscape(orgFolder)+"/config.xml", "updated: "+strings.Join(changes, ", ")
	}
	if err := stack.Retry(ctx, rt.Console, "organization folder", 5, 10*time.Second, func() error {
		return api.post(ctx, path, "application/xml", desired)
	}); err != nil {
		return err
	}
	rt.Logf("Jenkins organization folder %s %s", orgFolder, done)
	return nil
}

// repos are the repos the organization folder builds: the demo repo, when
// the VCS provider created one, and the repos setting.
func repos(rt *stack.Runtime) []string {
	var out []string
	if demo := rt.Out.VCS.DemoRepo; demo != "" {
		out = append(out, demo)
	}
	for _, name := range strings.Fields(rt.Setting("repos")) {
		if !slices.ContainsFunc(out, func(have string) bool { return strings.EqualFold(have, name) }) {
			out = append(out, name)
		}
	}
	return out
}

// environments is OWNSTACK_ENVIRONMENTS: the <name>[=<branch glob>] items,
// production first, space-separated.
func environments(rt *stack.Runtime) string {
	items := make([]string, 0, len(rt.Environments()))
	for _, env := range rt.Environments() {
		item := env.Name
		if env.Branch != "" {
			item += "=" + env.Branch
		}
		items = append(items, item)
	}
	return strings.Join(items, " ")
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
// every environment's branch glob, so each one builds and deploys on push.
// Jenkins matches it against the whole branch name.
func triggerBranches(rt *stack.Runtime) string {
	var alternatives []string
	for _, env := range rt.Environments() {
		if env.Branch != "" {
			alternatives = append(alternatives, globRegexp(env.Branch))
		}
	}
	return strings.Join(alternatives, "|")
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
		"Repos":           strings.Join(repos(rt), " "),
		"JenkinsfilePath": jenkinsfilePath,
		"TriggerBranches": triggerBranches(rt),
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
		{"repositories", have.Includes, want.Includes},
		{"Jenkinsfile path", have.ScriptPath, want.ScriptPath},
		{"auto-build branches", have.TriggerBranches, want.TriggerBranches},
	} {
		if f.have != f.want {
			changes = append(changes, fmt.Sprintf("%s %q → %q", f.name, f.have, f.want))
		}
	}
	return changes, true, nil
}

type envVar struct{ Key, Value string }

func valuesData(rt *stack.Runtime, password string) map[string]any {
	images := rt.Out.Images
	ref := stack.LibraryRef(stack.EngineVersion)
	// The shared library reads exactly these.
	env := []envVar{
		{"OWNSTACK_DOMAIN", rt.Domain()},
		{"OWNSTACK_REGISTRY", images.Host + "/" + images.Project},
		{"OWNSTACK_ENVIRONMENTS", environments(rt)},
		// The Deploy stage clones common_helm_library from the repository and
		// ref the pipeline library itself is loaded from.
		{"OWNSTACK_LIBRARY_REPO", stack.LibraryRepoURL},
		{"OWNSTACK_LIBRARY_REF", ref},
	}
	return map[string]any{
		"Host":           rt.AdminHost("jenkins"),
		"ImageTag":       pluginLock.ImageTag(),
		"Plugins":        installPlugins(rt),
		"Password":       password,
		"Annotations":    rt.Out.Ingress.Annotations,
		"GlobalEnv":      env,
		"LibraryRepoURL": stack.LibraryRepoURL,
		"LibraryRef":     ref,
	}
}

// clusterObjects are what Jenkins needs before its release: its namespace,
// the cluster-admin binding of its service accounts, and the VCS and
// registry contract outputs as Jenkins credentials (through the
// kubernetes-credentials-provider plugin).
func clusterObjects(rt *stack.Runtime) []map[string]any {
	userPass := map[string]string{"jenkins.io/credentials-type": "usernamePassword"}
	return []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}},
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
			"metadata": map[string]any{"name": "jenkins-cluster-admin"},
			"subjects": []map[string]any{
				{"kind": "ServiceAccount", "name": "default", "namespace": namespace},
				{"kind": "ServiceAccount", "name": "jenkins", "namespace": namespace},
			},
			"roleRef": map[string]any{"kind": "ClusterRole", "name": "cluster-admin", "apiGroup": "rbac.authorization.k8s.io"},
		},
		stack.SecretObject(namespace, vcsCredentialID, "", map[string]string{"username": rt.Out.VCS.Username, "password": rt.Out.VCS.Token}, userPass),
		// dockerPush hands the credential to the shell only as environment
		// variables, so Harbor's robot$name needs no escaping.
		stack.SecretObject(namespace, registryCredentialID, "", map[string]string{"username": rt.Out.Images.PushUsername, "password": rt.Out.Images.PushPassword}, userPass),
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
	return &client{base: "https://" + rt.AdminHost("jenkins"), password: password, http: &httpClient}, nil
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
