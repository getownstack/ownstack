// Package ghactions implements the CI/CD contract with GitHub Actions. Builds
// run on GitHub's runners, so the server only runs applications. Each app
// repo carries .github/workflows/ownstack.yml; ownstackctl gives those repos
// a deploy credential that can only touch the environment namespaces.
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
	"path"
	"regexp"
	"slices"
	"sort"
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
		{Key: "repoFilter", Label: "Repository filter", Kind: catalog.KindString, Default: "*", Advanced: true,
			Help: "Space-separated wildcards, such as app-* api-*. Matching repos that contain .github/workflows/ownstack.yml get the deploy credential."},
		{Key: "libraryRef", Label: "Ownstack library ref", Kind: catalog.KindString, Advanced: true,
			Help: "Branch or tag of getownstack/ownstack the workflow takes the Helm library from. " +
				"Empty: the tag of the ownstackctl release running on the server, or main for development builds."},
		{Key: "additionalOwners", Label: "Additional owners", Kind: catalog.KindString, Advanced: true,
			Help: "Comma-separated GitHub users or organizations whose repos also deploy here. The token must be able to administer them."},
	},
}

const (
	systemNamespace = "ownstack-system"
	deployer        = "ownstack-deployer"
	tokenSecret     = "ownstack-deployer-token"
	// targetsConfigMap records the repos the last apply configured, so status
	// does not have to scan every repo of every owner.
	targetsConfigMap = "ownstack-ci-targets"
	workflowFile     = "ownstack.yml"
	workflowPath     = ".github/workflows/" + workflowFile
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
	_, err := parseRepoFilter(rt.Setting("repoFilter"))
	return err
}

func (p Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if rt.Out.VCS.Kind != "github" {
		return fmt.Errorf("GitHub Actions needs the github VCS provider")
	}
	// App repos would otherwise get an empty registry credential.
	if images := rt.Out.Images; images == nil || images.PushUsername == "" || images.PushPassword == "" {
		return fmt.Errorf("the registry did not provide a push credential; apply the images aspect first")
	}
	kubeconfig, err := p.ensureDeployer(ctx, rt)
	if err != nil {
		return err
	}
	if err := p.verifyKubeconfig(ctx, rt, kubeconfig); err != nil {
		return fmt.Errorf("the deploy credential does not work from outside the server: %w", err)
	}
	rt.Logf("Deploy credential verified against %s", rt.Out.Compute.APIServer)

	gh := p.client(rt)
	// Repos that could not be checked fail the apply at the end, after every
	// reachable repo is configured.
	repos, failed, err := p.targetRepos(ctx, rt)
	if err != nil {
		return err
	}
	demoRepo := rt.Out.VCS.DemoRepo
	demo := demoRepo != ""
	if demo {
		repos, failed = withDemo(repos, failed, target{rt.Out.VCS.Owner, demoRepo})
	}
	ref := rt.LibraryRef()
	secrets := map[string]string{
		"OWNSTACK_KUBECONFIG":        kubeconfig,
		"OWNSTACK_REGISTRY_PASSWORD": rt.Out.Images.PushPassword,
	}
	variables := repoVariables(rt.Out.Images, rt.Domain(), rt.Environments(), ref, nodeArch(rt))
	for _, t := range repos {
		if err := configureRepo(ctx, gh, t, secrets, variables); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		rt.Logf("GitHub Actions configured for %s", t)
	}
	// Recorded as soon as the repos hold the credentials, so status finds
	// them even when committing the demo workflow fails.
	if err := recordTargets(ctx, rt, repos, failed); err != nil {
		return err
	}
	rt.Logf("App pipelines take the Helm library from %s@%s, build images for %s, and deploy %s", stack.LibraryRepo, ref, platform(nodeArch(rt)), deploysDetail(rt.Environments()))
	if demo {
		info, err := gh.Repo(ctx, rt.Out.VCS.Owner, demoRepo)
		if err != nil {
			return err
		}
		// Committing the workflow after the secrets exist triggers the first build.
		changed, err := gh.UpsertFile(ctx, rt, rt.Out.VCS.Owner, demoRepo, info.DefaultBranch, workflowPath, []byte(Workflow), "Add Ownstack pipeline")
		if err != nil {
			return err
		}
		if changed {
			rt.Logf("Committed %s to %s/%s; its first build deploys https://%s.%s", workflowPath, rt.Out.VCS.Owner, demoRepo, demoRepo, rt.Domain())
		}
	}
	if len(repos) == 0 {
		rt.Logf("No app repos yet. Add %s to a repo (ownstackctl prints it with `ownstackctl workflow`) and rerun apply.", workflowPath)
	}
	return discoveryError(failed)
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
	// The repos the last apply configured: two requests each, however many
	// repos the owners have. Stacks applied by an older ownstackctl have no
	// record until the next apply, so they are still discovered.
	repos, recorded := recordedTargets(ctx, rt)
	var failed []repoError
	if !recorded {
		var err error
		if repos, failed, err = p.targetRepos(ctx, rt); err != nil {
			return append(checks, stack.Check{Name: "pipelines", State: stack.Unknown, Detail: err.Error()})
		}
	}
	branch := rt.Production().Branch
	if strings.Contains(branch, "*") {
		branch = ""
	}
	for _, t := range repos {
		checks = append(checks, pipelineCheck(ctx, gh, t, branch))
	}
	for _, f := range failed {
		checks = append(checks, stack.Check{Name: "pipeline " + f.String(), State: stack.Unknown, Detail: f.err.Error()})
	}
	return checks
}

// pipelineCheck reports a repo's latest run, and warns when its workflow is
// older than the one this ownstackctl ships.
func pipelineCheck(ctx context.Context, gh *githubapi.Client, t target, branch string) stack.Check {
	name := "pipeline " + t.String()
	content, err := gh.FileContent(ctx, t.owner, t.name, workflowPath)
	if errors.Is(err, githubapi.ErrNotFound) {
		return stack.Check{Name: name, State: stack.Warn, Detail: workflowPath + " not found; rerun apply if the repo no longer deploys here"}
	}
	if err != nil {
		return stack.Check{Name: name, State: stack.Unknown, Detail: err.Error()}
	}
	// Runs on the production branch: since ownstack-workflow 3 every push runs
	// the workflow, and pushes to unmapped branches only resolve, so they would
	// hide a failed deploy. A glob cannot be filtered on: then any run counts.
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

// owners are the VCS owner plus any additional owners whose repos deploy here.
func owners(rt *stack.Runtime) []string {
	out := []string{rt.Out.VCS.Owner}
	for _, o := range strings.Split(rt.Setting("additionalOwners"), ",") {
		if o = strings.TrimSpace(o); o != "" && !strings.EqualFold(o, rt.Out.VCS.Owner) {
			out = append(out, o)
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

// repoFilter is the repoFilter setting: space-separated wildcards, as the
// Jenkins organization folder takes them. A repo matches when any does.
type repoFilter []string

func parseRepoFilter(s string) (repoFilter, error) {
	patterns := strings.Fields(s)
	if len(patterns) == 0 {
		return nil, fmt.Errorf("repoFilter is empty; use * for every repo")
	}
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("repoFilter: bad pattern %q", p)
		}
	}
	return patterns, nil
}

func (f repoFilter) match(name string) bool {
	for _, p := range f {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// repoError is a repo whose workflow could not be checked.
type repoError struct {
	target
	err error
}

// targetRepos are the owners' repos matching the filter that carry the
// Ownstack workflow. Repos that could not be checked (rate limits, SSO, an
// outage) are returned in failed, so one of them does not hide the others.
func (p Provider) targetRepos(ctx context.Context, rt *stack.Runtime) (targets []target, failed []repoError, err error) {
	filter, err := parseRepoFilter(rt.Setting("repoFilter"))
	if err != nil {
		return nil, nil, err
	}
	gh := p.client(rt)
	login := rt.Out.VCS.Username
	if l, err := gh.Login(ctx); err == nil {
		login = l
	}
	clusterRepo := rt.Out.VCS.ClusterRepo
	for _, owner := range owners(rt) {
		all, err := gh.Repos(ctx, owner, login)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", owner, err)
		}
		for _, name := range all {
			if owner == rt.Out.VCS.Owner && name == clusterRepo {
				continue
			}
			if !filter.match(name) {
				continue
			}
			switch has, err := gh.FileExists(ctx, owner, name, workflowPath); {
			case err != nil:
				failed = append(failed, repoError{target{owner, name}, err})
			case has:
				targets = append(targets, target{owner, name})
			}
		}
	}
	return targets, failed, nil
}

// withDemo adds the demo repo, which is configured before its workflow is
// committed. Once configured it is no longer a repo that could not be checked.
func withDemo(repos []target, failed []repoError, demo target) ([]target, []repoError) {
	if !containsTarget(repos, demo) {
		repos = append(repos, demo)
	}
	return repos, slices.DeleteFunc(failed, func(f repoError) bool { return containsTarget(repos, f.target) })
}

// discoveryError fails an apply that could not check some repos.
func discoveryError(failed []repoError) error {
	if len(failed) == 0 {
		return nil
	}
	reasons := make([]string, len(failed))
	for i, f := range failed {
		reasons[i] = f.String() + ": " + f.err.Error()
	}
	return fmt.Errorf("could not check %d repos for %s (%s); the other repos are configured", len(failed), workflowPath, strings.Join(reasons, "; "))
}

// nodeArch is the server's architecture from the compute output, "" when
// the compute provider does not report one.
func nodeArch(rt *stack.Runtime) string {
	if rt.Out.Compute == nil {
		return ""
	}
	return rt.Out.Compute.Arch
}

// platform is the image platform the workflow builds for.
func platform(arch string) string {
	if arch == "" {
		return "linux/amd64"
	}
	return "linux/" + arch
}

// branchMapping sends pushes to branches matching pattern (a name, or a glob
// whose * matches any characters, / included) to an environment.
type branchMapping struct{ pattern, environment string }

// branchMap is the mapping the workflow reads, first match wins: every
// environment that has a branch, in order.
func branchMap(environments []stack.Environment) []branchMapping {
	var out []branchMapping
	for _, env := range environments {
		if env.Branch != "" {
			out = append(out, branchMapping{env.Branch, env.Name})
		}
	}
	return out
}

// formatBranches is OWNSTACK_BRANCHES: the mapping as space-separated pairs,
// such as "main=prod develop=dev release/*=qa".
func formatBranches(mapping []branchMapping) string {
	pairs := make([]string, len(mapping))
	for i, m := range mapping {
		pairs[i] = m.pattern + "=" + m.environment
	}
	return strings.Join(pairs, " ")
}

// deploysDetail says where pushes deploy.
func deploysDetail(environments []stack.Environment) string {
	mapping := branchMap(environments)
	if len(mapping) == 0 {
		return "nothing on push"
	}
	parts := make([]string, len(mapping))
	for i, m := range mapping {
		parts[i] = m.pattern + " to " + m.environment
	}
	return strings.Join(parts, ", ")
}

// repoVariables are the Actions variables the workflow reads; the registry
// password and the deploy kubeconfig are secrets instead. environments[0]
// is production.
func repoVariables(images *stack.ImagesOutput, domain string, environments []stack.Environment, libraryRef, arch string) map[string]string {
	names := make([]string, len(environments))
	for i, env := range environments {
		names[i] = env.Name
	}
	production := environments[0].Name
	variables := map[string]string{
		"OWNSTACK_REGISTRY":          images.Host,
		"OWNSTACK_REGISTRY_USERNAME": images.PushUsername,
		"OWNSTACK_IMAGE_PROJECT":     images.Project,
		"OWNSTACK_PULL_SECRET":       images.PullSecretName,
		"OWNSTACK_DOMAIN":            domain,
		// Workflows before ownstack-workflow 3 deploy main here.
		"OWNSTACK_NAMESPACE":              production,
		"OWNSTACK_PRODUCTION_ENVIRONMENT": production,
		"OWNSTACK_BRANCHES":               formatBranches(branchMap(environments)),
		"OWNSTACK_ENVIRONMENTS":           strings.Join(names, " "),
		"OWNSTACK_LIBRARY_REF":            libraryRef,
	}
	// Without it the workflow builds linux/amd64.
	if arch != "" {
		variables["OWNSTACK_PLATFORM"] = platform(arch)
	}
	return variables
}

func configureRepo(ctx context.Context, gh *githubapi.Client, t target, secrets, variables map[string]string) error {
	for name, value := range secrets {
		if err := gh.SetSecret(ctx, t.owner, t.name, name, value); err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
	}
	for name, value := range variables {
		if err := gh.SetVariable(ctx, t.owner, t.name, name, value); err != nil {
			return fmt.Errorf("variable %s: %w", name, err)
		}
	}
	return nil
}

// recordedTargets returns the repos the last apply configured; ok is false
// when there is no record (not applied yet, applied by an older
// ownstackctl, or the cluster is unreachable).
func recordedTargets(ctx context.Context, rt *stack.Runtime) (targets []target, ok bool) {
	out, err := rt.Kube.Exec.Output(ctx, "kubectl", "get", "configmap", targetsConfigMap, "-n", systemNamespace, "-o", "json")
	if err != nil {
		return nil, false
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &cm) != nil {
		return nil, false
	}
	raw, ok := cm.Data["targets"]
	if !ok {
		return nil, false
	}
	return parseTargets(raw), true
}

func parseTargets(raw string) []target {
	var out []target
	for _, line := range strings.Split(raw, "\n") {
		owner, name, ok := strings.Cut(strings.TrimSpace(line), "/")
		if ok && owner != "" && name != "" {
			out = append(out, target{owner, name})
		}
	}
	return out
}

func targetsObject(targets []target) map[string]any {
	lines := make([]string, len(targets))
	for i, t := range targets {
		lines[i] = t.String()
	}
	sort.Strings(lines)
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": targetsConfigMap, "namespace": systemNamespace},
		"data":     map[string]string{"targets": strings.Join(lines, "\n")},
	}
}

// recordTargets saves the configured repos for status. Repos
// that could not be checked this time stay recorded; repos that dropped out
// are reported, because they still hold the deploy credential.
func recordTargets(ctx context.Context, rt *stack.Runtime, configured []target, failed []repoError) error {
	previous, _ := recordedTargets(ctx, rt)
	record := append([]target(nil), configured...)
	for _, t := range previous {
		switch {
		case containsTarget(configured, t):
		case containsFailed(failed, t):
			record = append(record, t)
		default:
			rt.Logf("warning: %s no longer has %s or matches the repository filter, but still holds its OWNSTACK_* secrets and variables; delete them in the repo's settings if it should not deploy here", t, workflowPath)
		}
	}
	return rt.Kube.Apply(ctx, targetsObject(record))
}

func containsFailed(list []repoError, v target) bool {
	for _, f := range list {
		if strings.EqualFold(f.owner, v.owner) && f.name == v.name {
			return true
		}
	}
	return false
}

// deployerCoreResources are the core resources app charts and
// before-deployment hooks manage. They are listed because a wildcard would
// include the namespace object itself: bound in prod, the token could then
// delete prod or relabel it (Pod Security).
var deployerCoreResources = []string{
	"pods", "pods/log", "pods/exec", "pods/portforward", "pods/attach",
	"services", "endpoints", "configmaps", "secrets", "serviceaccounts", "persistentvolumeclaims",
	"events", "replicationcontrollers", "resourcequotas", "limitranges",
}

// deployerRole is what the deploy token may do in each environment namespace
// it is bound in: manage workloads, and only read the namespace itself
// (hooks such as ownstack.io's check it exists).
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

// ensureDeployer creates a service account that can manage workloads only in
// the environment namespaces, and returns a kubeconfig for it that works from
// outside the server.
func (p Provider) ensureDeployer(ctx context.Context, rt *stack.Runtime) (string, error) {
	k := rt.Kube
	if err := k.EnsureNamespace(ctx, systemNamespace); err != nil {
		return "", err
	}
	objects := []map[string]any{
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": deployer, "namespace": systemNamespace}},
		{
			"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/service-account-token",
			"metadata": map[string]any{"name": tokenSecret, "namespace": systemNamespace,
				"annotations": map[string]string{"kubernetes.io/service-account.name": deployer}},
		},
		deployerRole(),
	}
	for _, env := range rt.Environments() {
		objects = append(objects, map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": deployer, "namespace": env.Name},
			"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": deployer},
			"subjects": []map[string]any{{"kind": "ServiceAccount", "name": deployer, "namespace": systemNamespace}},
		})
	}
	for _, obj := range objects {
		if err := k.Apply(ctx, obj); err != nil {
			return "", err
		}
	}
	var token string
	if err := stack.Retry(ctx, rt.Console, "deploy token", 20, 2*time.Second, func() error {
		t, ok := k.SecretValue(ctx, systemNamespace, tokenSecret, "token")
		if !ok || t == "" {
			return fmt.Errorf("token not issued yet")
		}
		token = t
		return nil
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
	return stack.Retry(ctx, rt.Console, "deploy credential", 12, 5*time.Second, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.Clusters[0].Cluster.Server+"/api/v1/namespaces/"+ns+"/pods?limit=1", nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+config.Users[0].User.Token)
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d listing pods in %s", res.StatusCode, ns)
		}
		return nil
	})
}

func containsTarget(list []target, v target) bool {
	for _, item := range list {
		if strings.EqualFold(item.owner, v.owner) && item.name == v.name {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
