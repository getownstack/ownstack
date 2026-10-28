// Package ghactions implements the CI/CD contract with GitHub Actions. Builds
// run on GitHub's runners, so the server only runs applications. Each listed
// app repo carries .github/workflows/ownstack.yml; ownstackctl gives those
// repos a deploy credential that can only touch the environment namespaces.
package ghactions

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/githubapi"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the gh-actions provider's catalog entry.
var Spec = catalog.Provider{
	ID: "gh-actions", Aspect: "cicd", Name: "GitHub Actions", Mono: "GA", Hosting: catalog.External,
	Summary: "Builds run on GitHub's runners, so the server only runs your apps. Each app repo gets a small ownstack.yml workflow that builds, pushes to your registry, and deploys with a namespace-scoped credential.",
	Params: []catalog.Param{
		{Key: "repos", Label: "App repositories", Kind: catalog.KindString, Placeholder: "api web other-org/site",
			Help: "Space-separated repos that deploy here: a name under the VCS owner, or owner/name. Each gets the OWNSTACK variable, " +
				"the OWNSTACK_KUBECONFIG secret, and " + workflowPath + " when it has none. The demo repo is always included.",
			Pattern:     `^ *(?:[A-Za-z0-9-]+/)?[A-Za-z0-9._-]+(?: +(?:[A-Za-z0-9-]+/)?[A-Za-z0-9._-]+)* *$`,
			PatternHelp: "must be space-separated repo names, such as api web other-org/site"},
	},
}

const (
	systemNamespace = "ownstack-system"
	deployer        = "ownstack-deployer"
	tokenSecret     = "ownstack-deployer-token"
	workflowFile    = "ownstack.yml"
	workflowPath    = ".github/workflows/" + workflowFile
)

// Workflow is the pipeline committed to app repos; docs link to it too.
//
//go:embed ownstack.yml
var Workflow string

// workflowVersion is the embedded workflow's "# ownstack-workflow: N"
// marker. Bump it when app repos should replace their copy: status warns
// about repos whose marker is missing or older.
var workflowVersion = workflowMarker([]byte(Workflow))

var markerPattern = regexp.MustCompile(`(?m)^#\s*ownstack-workflow:\s*(\d+)\s*$`)

// workflowMarker returns a workflow's version marker, 0 when it has none.
func workflowMarker(content []byte) int {
	m := markerPattern.FindSubmatch(content)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

type Provider struct {
	// APIURL overrides the GitHub API in tests.
	APIURL string
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.CICD = &stack.CICDOutput{URL: "https://github.com/" + rt.Out.VCS.Owner}
	return nil
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if rt.Out.VCS.Kind != "github" {
		return fmt.Errorf("GitHub Actions needs the github VCS provider")
	}
	kubeconfig, err := p.ensureDeployer(ctx, rt)
	if err != nil {
		return err
	}
	if err := p.verifyKubeconfig(ctx, rt, kubeconfig); err != nil {
		return fmt.Errorf("the deploy credential does not work from outside the server: %w", err)
	}
	rt.Logf("Deploy credential verified against %s", rt.Out.Compute.APIServer)

	settings := repoSettings(rt)
	variable, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	repos := targets(rt)
	if len(repos) == 0 {
		rt.Logf("No app repos yet: list them in the cicd repos setting and rerun apply.")
		return nil
	}
	gh := p.client(rt)
	for _, t := range repos {
		committed, err := configureRepo(ctx, rt, gh, t, string(variable), kubeconfig)
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		if committed {
			rt.Logf("GitHub Actions configured for %s; committed %s, whose first run deploys it", t, workflowPath)
		} else {
			rt.Logf("GitHub Actions configured for %s", t)
		}
	}
	rt.Logf("App pipelines take the Helm library from %s@%s and build images for %s", stack.LibraryRepo, settings.LibraryRef, settings.Platform)
	return nil
}

func (p Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	var checks []stack.Check
	if token, ok := rt.Kube.SecretValue(ctx, systemNamespace, tokenSecret, "token"); ok && token != "" {
		checks = append(checks, stack.Check{Name: "deploy identity", State: stack.OK, Detail: systemNamespace + "/" + deployer})
	} else {
		checks = append(checks, stack.Check{Name: "deploy identity", State: stack.Fail, Detail: "missing; rerun apply"})
	}
	if rt.Out.VCS.Token == "" {
		return append(checks, stack.Check{Name: "pipelines", State: stack.Unknown, Detail: "GitHub token not available on this node"})
	}
	gh := p.client(rt)
	// A glob cannot be filtered on: then any branch's run counts.
	branch := rt.Production().Branch
	if strings.Contains(branch, "*") {
		branch = ""
	}
	for _, t := range targets(rt) {
		checks = append(checks, pipelineCheck(ctx, gh, t, branch))
	}
	return checks
}

// pipelineCheck reports a repo's latest run, and warns when its workflow is
// older than the one this ownstackctl ships.
func pipelineCheck(ctx context.Context, gh *githubapi.Client, t target, branch string) stack.Check {
	name := "pipeline " + t.String()
	content, err := gh.FileContent(ctx, t.owner, t.name, workflowPath)
	if errors.Is(err, githubapi.ErrNotFound) {
		return stack.Check{Name: name, State: stack.Warn, Detail: workflowPath + " not found; rerun apply to commit it"}
	}
	if err != nil {
		return stack.Check{Name: name, State: stack.Unknown, Detail: err.Error()}
	}
	// Runs on the production branch: every push runs the workflow, and
	// pushes to unmapped branches only resolve, so they would hide a failed
	// deploy.
	run, err := gh.LatestRun(ctx, t.owner, t.name, workflowFile, branch)
	var check stack.Check
	switch {
	case err != nil:
		check = stack.Check{Name: name, State: stack.Unknown, Detail: err.Error()}
	case run == nil:
		check = stack.Check{Name: name, State: stack.Warn, Detail: "no runs yet"}
	case run.Status != "completed":
		check = stack.Check{Name: name, State: stack.OK, Detail: run.Status + " " + short(run.HeadSHA)}
	case run.Conclusion == "success":
		check = stack.Check{Name: name, State: stack.OK, Detail: "last build succeeded " + short(run.HeadSHA)}
	default:
		check = stack.Check{Name: name, State: stack.Warn, Detail: "last build " + run.Conclusion + ": " + run.HTMLURL}
	}
	if workflowMarker(content) < workflowVersion {
		if check.State == stack.OK {
			check.State = stack.Warn
		}
		check.Detail += fmt.Sprintf("; %s is older than ownstack-workflow %d, replace it with the output of `ownstackctl workflow`", workflowFile, workflowVersion)
	}
	return check
}

// target is one app repo the pipeline is configured for.
type target struct{ owner, name string }

func (t target) String() string { return t.owner + "/" + t.name }

// targets are the demo repo, when the VCS provider created one, and the
// repos setting: names under the VCS owner, or owner/name. GitHub names are
// case-insensitive, so duplicates are dropped regardless of case.
func targets(rt *stack.Runtime) []target {
	var out []target
	add := func(t target) {
		for _, have := range out {
			if strings.EqualFold(have.owner, t.owner) && strings.EqualFold(have.name, t.name) {
				return
			}
		}
		out = append(out, t)
	}
	if demo := rt.Out.VCS.DemoRepo; demo != "" {
		add(target{rt.Out.VCS.Owner, demo})
	}
	for _, repo := range strings.Fields(rt.Setting("repos")) {
		if owner, name, ok := strings.Cut(repo, "/"); ok {
			add(target{owner, name})
		} else {
			add(target{rt.Out.VCS.Owner, repo})
		}
	}
	return out
}

func (p Provider) client(rt *stack.Runtime) *githubapi.Client {
	base := p.APIURL
	if base == "" {
		base = rt.Out.VCS.APIURL
	}
	return &githubapi.Client{Base: base, Token: rt.Out.VCS.Token, HTTP: rt.HTTP}
}

// settings is the OWNSTACK repo variable the workflow reads (C6).
// Environments are <name>[=<branch glob>] items, production first.
type settings struct {
	Domain       string   `json:"domain"`
	Registry     string   `json:"registry"`
	Platform     string   `json:"platform"`
	Environments []string `json:"environments"`
	LibraryRef   string   `json:"libraryRef"`
}

func repoSettings(rt *stack.Runtime) settings {
	environments := []string{}
	for _, env := range rt.Environments() {
		item := env.Name
		if env.Branch != "" {
			item += "=" + env.Branch
		}
		environments = append(environments, item)
	}
	arch := ""
	if rt.Out.Compute != nil {
		arch = rt.Out.Compute.Arch
	}
	return settings{
		Domain:       rt.Domain(),
		Registry:     rt.Out.Images.Host + "/" + rt.Out.Images.Project,
		Platform:     platform(arch),
		Environments: environments,
		LibraryRef:   stack.LibraryRef(stack.EngineVersion),
	}
}

// platform is the image platform the workflow builds for: the server's.
func platform(arch string) string {
	if arch == "" {
		return "linux/amd64"
	}
	return "linux/" + arch
}

// configureRepo sets the repo's two settings, then commits the workflow to
// its default branch when the repo has none; that commit starts the first
// deploy, so it comes last. An existing workflow is never replaced.
func configureRepo(ctx context.Context, rt *stack.Runtime, gh *githubapi.Client, t target, variable, kubeconfig string) (committed bool, err error) {
	if err := gh.SetVariable(ctx, t.owner, t.name, "OWNSTACK", variable); err != nil {
		return false, repoError("variable OWNSTACK", err)
	}
	if err := gh.SetSecret(ctx, t.owner, t.name, "OWNSTACK_KUBECONFIG", kubeconfig); err != nil {
		return false, repoError("secret OWNSTACK_KUBECONFIG", err)
	}
	_, err = gh.FileContent(ctx, t.owner, t.name, workflowPath)
	if !errors.Is(err, githubapi.ErrNotFound) {
		return false, err
	}
	return true, gh.PutFile(ctx, rt, t.owner, t.name, "", workflowPath, []byte(Workflow), "", "Add Ownstack pipeline")
}

func repoError(what string, err error) error {
	if errors.Is(err, githubapi.ErrNotFound) {
		return fmt.Errorf("%s: the repo does not exist, or the GitHub token cannot administer it", what)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// deployerCoreResources are the core resources app charts manage. They are
// listed because a wildcard would include the namespace object itself:
// bound in prod, the token could then delete prod or relabel it (Pod
// Security).
var deployerCoreResources = []string{
	"pods", "pods/log", "pods/exec", "pods/portforward", "pods/attach",
	"services", "endpoints", "configmaps", "secrets", "serviceaccounts", "persistentvolumeclaims",
	"events", "replicationcontrollers", "resourcequotas", "limitranges",
}

// deployerRole is what the deploy token may do in each environment namespace
// it is bound in: manage workloads, and only read the namespace itself.
func deployerRole() map[string]any {
	return map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
		"metadata": map[string]any{"name": deployer},
		"rules": []map[string]any{
			{"apiGroups": []string{""}, "resources": deployerCoreResources, "verbs": []string{"*"}},
			{"apiGroups": []string{""}, "resources": []string{"namespaces"}, "verbs": []string{"get"}},
			{
				"apiGroups": []string{"apps", "batch", "autoscaling", "policy", "networking.k8s.io", "traefik.io", "gateway.networking.k8s.io"},
				"resources": []string{"*"},
				"verbs":     []string{"*"},
			},
		},
	}
}

// deployerObjects are the deploy identity: its namespace, service account,
// token, role, and a binding in every environment.
func deployerObjects(environments []stack.Environment) []map[string]any {
	items := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": systemNamespace}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": deployer, "namespace": systemNamespace}},
		{
			"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/service-account-token",
			"metadata": map[string]any{"name": tokenSecret, "namespace": systemNamespace,
				"annotations": map[string]string{"kubernetes.io/service-account.name": deployer}},
		},
		deployerRole(),
	}
	for _, env := range environments {
		items = append(items, map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": deployer, "namespace": env.Name},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": deployer},
			"subjects": []map[string]any{{"kind": "ServiceAccount", "name": deployer, "namespace": systemNamespace}},
		})
	}
	return items
}

// ensureDeployer creates a service account that can manage workloads only in
// the environment namespaces, and returns a kubeconfig for it that works from
// outside the server.
func (p Provider) ensureDeployer(ctx context.Context, rt *stack.Runtime) (string, error) {
	if err := rt.Kube.Apply(ctx, deployerObjects(rt.Environments())...); err != nil {
		return "", err
	}
	var token string
	if err := stack.WaitFor(ctx, rt, "deploy token", time.Minute, func(ctx context.Context) (bool, string) {
		t, ok := rt.Kube.SecretValue(ctx, systemNamespace, tokenSecret, "token")
		token = t
		return ok && t != "", "not issued yet"
	}); err != nil {
		return "", err
	}
	rt.Console.Redact(token)
	return buildKubeconfig(rt.Out.Compute.APIServer, rt.Out.Compute.CACert, token, rt.Production().Name)
}

func buildKubeconfig(server, ca, token, namespace string) (string, error) {
	if ca == "" {
		return "", fmt.Errorf("cluster CA not available")
	}
	config := map[string]any{
		"apiVersion": "v1", "kind": "Config",
		"clusters": []map[string]any{{"name": "ownstack", "cluster": map[string]any{
			"server": server, "certificate-authority-data": base64.StdEncoding.EncodeToString([]byte(ca)),
		}}},
		"users": []map[string]any{{"name": deployer, "user": map[string]any{"token": token}}},
		"contexts": []map[string]any{{"name": "ownstack", "context": map[string]any{
			"cluster": "ownstack", "user": deployer, "namespace": namespace,
		}}},
		"current-context": "ownstack",
	}
	out, err := yaml.Marshal(config)
	return string(out), err
}

// verifyKubeconfig calls the API server at its public address with the
// deploy credential, proving the certificate, the port, and RBAC all work
// the way a GitHub runner will see them.
func (p Provider) verifyKubeconfig(ctx context.Context, rt *stack.Runtime, kubeconfig string) error {
	var config struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
				CA     string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				Token string `yaml:"token"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal([]byte(kubeconfig), &config); err != nil {
		return err
	}
	ca, _ := base64.StdEncoding.DecodeString(config.Clusters[0].Cluster.CA)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	ns := rt.Production().Name
	// A new token takes a moment to be accepted.
	return stack.WaitFor(ctx, rt, "deploy credential", time.Minute, func(ctx context.Context) (bool, string) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.Clusters[0].Cluster.Server+"/api/v1/namespaces/"+ns+"/pods?limit=1", nil)
		if err != nil {
			return false, err.Error()
		}
		req.Header.Set("Authorization", "Bearer "+config.Users[0].User.Token)
		res, err := client.Do(req)
		if err != nil {
			return false, err.Error()
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return false, fmt.Sprintf("HTTP %d listing pods in %s", res.StatusCode, ns)
		}
		return true, ""
	})
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
