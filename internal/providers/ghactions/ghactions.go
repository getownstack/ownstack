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
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/githubapi"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const (
	systemNamespace = "ownstack-system"
	deployer        = "ownstack-deployer"
	tokenSecret     = "ownstack-deployer-token"
	workflowFile    = "ownstack.yml"
	workflowPath    = ".github/workflows/" + workflowFile
	demoRepo        = "lumen-yard"
)

// Workflow is the pipeline committed to app repos; docs link to it too.
//
//go:embed ownstack.yml
var Workflow string

type Provider struct {
	// APIURL overrides the GitHub API in tests.
	APIURL string
}

func (p Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.CICD = &stack.CICDOutput{URL: "https://github.com/" + rt.Out.VCS.Owner}
	return nil
}

func (p Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	steps := []stack.Step{
		{Action: "ensure", Target: "deploy identity", Detail: fmt.Sprintf("service account %s/%s, may only change %s", systemNamespace, deployer, strings.Join(rt.Manifest.Environments(), ", "))},
	}
	if rt.Out.VCS.Token == "" {
		return append(steps, stack.Step{Action: "configure", Target: "app repos", Detail: "repos with " + workflowPath})
	}
	repos, err := p.targetRepos(ctx, rt)
	if err != nil {
		return append(steps, stack.Step{Action: "configure", Target: "app repos", Detail: err.Error()})
	}
	for _, r := range repos {
		steps = append(steps, stack.Step{Action: "configure", Target: rt.Out.VCS.Owner + "/" + r, Detail: "OWNSTACK_* secrets and variables"})
	}
	if rt.Manifest.Settings(rt.Catalog, "vcs")["demoApp"] == "true" {
		steps = append(steps, stack.Step{Action: "ensure", Target: rt.Out.VCS.Owner + "/" + demoRepo, Detail: "commit " + workflowPath + " (first build deploys the demo)"})
	}
	return steps
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

	gh := p.client(rt)
	repos, err := p.targetRepos(ctx, rt)
	if err != nil {
		return err
	}
	demo := rt.Manifest.Settings(rt.Catalog, "vcs")["demoApp"] == "true"
	if demo && !contains(repos, demoRepo) {
		repos = append(repos, demoRepo)
	}
	for _, repo := range repos {
		if err := p.configureRepo(ctx, rt, gh, repo, kubeconfig); err != nil {
			return fmt.Errorf("%s/%s: %w", rt.Out.VCS.Owner, repo, err)
		}
		rt.Logf("GitHub Actions configured for %s/%s", rt.Out.VCS.Owner, repo)
	}
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
	repos, err := p.targetRepos(ctx, rt)
	if err != nil {
		return append(checks, stack.Check{Name: "pipelines", State: stack.Unknown, Detail: err.Error()})
	}
	for _, repo := range repos {
		run, err := gh.LatestRun(ctx, rt.Out.VCS.Owner, repo, workflowFile)
		name := "pipeline " + repo
		switch {
		case err != nil:
			checks = append(checks, stack.Check{Name: name, State: stack.Unknown, Detail: err.Error()})
		case run == nil:
			checks = append(checks, stack.Check{Name: name, State: stack.Warn, Detail: "no runs yet"})
		case run.Status != "completed":
			checks = append(checks, stack.Check{Name: name, State: stack.OK, Detail: run.Status + " " + short(run.HeadSHA)})
		case run.Conclusion == "success":
			checks = append(checks, stack.Check{Name: name, State: stack.OK, Detail: "last build succeeded " + short(run.HeadSHA)})
		default:
			checks = append(checks, stack.Check{Name: name, State: stack.Warn, Detail: "last build " + run.Conclusion + ": " + run.HTMLURL})
		}
	}
	return checks
}

func (p Provider) client(rt *stack.Runtime) *githubapi.Client {
	base := p.APIURL
	if base == "" {
		base = rt.Out.VCS.APIURL
	}
	return &githubapi.Client{Base: base, Token: rt.Out.VCS.Token, HTTP: rt.HTTP}
}

// targetRepos are the owner's repos matching the filter that carry the
// Ownstack workflow.
func (p Provider) targetRepos(ctx context.Context, rt *stack.Runtime) ([]string, error) {
	gh := p.client(rt)
	login := rt.Out.VCS.Username
	if l, err := gh.Login(ctx); err == nil {
		login = l
	}
	all, err := gh.Repos(ctx, rt.Out.VCS.Owner, login)
	if err != nil {
		return nil, err
	}
	filter := rt.Setting("repoFilter")
	clusterRepo := rt.Manifest.Settings(rt.Catalog, "vcs")["clusterRepo"]
	var out []string
	for _, name := range all {
		if name == clusterRepo {
			continue
		}
		if ok, _ := path.Match(filter, name); !ok {
			continue
		}
		if has, err := gh.FileExists(ctx, rt.Out.VCS.Owner, name, workflowPath); err == nil && has {
			out = append(out, name)
		}
	}
	return out, nil
}

func (p Provider) configureRepo(ctx context.Context, rt *stack.Runtime, gh *githubapi.Client, repo, kubeconfig string) error {
	owner, images := rt.Out.VCS.Owner, rt.Out.Images
	secrets := map[string]string{
		"OWNSTACK_KUBECONFIG":        kubeconfig,
		"OWNSTACK_REGISTRY_PASSWORD": images.PushPassword,
	}
	variables := map[string]string{
		"OWNSTACK_REGISTRY":          images.Host,
		"OWNSTACK_REGISTRY_USERNAME": images.PushUsername,
		"OWNSTACK_IMAGE_PROJECT":     images.Project,
		"OWNSTACK_PULL_SECRET":       images.PullSecretName,
		"OWNSTACK_DOMAIN":            rt.Domain(),
		"OWNSTACK_NAMESPACE":         rt.Setting("environment"),
		"OWNSTACK_LIBRARY_REF":       rt.Setting("libraryRef"),
	}
	for name, value := range secrets {
		if err := gh.SetSecret(ctx, owner, repo, name, value); err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
	}
	for name, value := range variables {
		if err := gh.SetVariable(ctx, owner, repo, name, value); err != nil {
			return fmt.Errorf("variable %s: %w", name, err)
		}
	}
	return nil
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
		{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
			"metadata": map[string]any{"name": deployer},
			"rules": []map[string]any{{
				"apiGroups": []string{"", "apps", "batch", "autoscaling", "policy", "networking.k8s.io", "traefik.io", "gateway.networking.k8s.io"},
				"resources": []string{"*"},
				"verbs":     []string{"*"},
			}},
		},
	}
	for _, env := range rt.Manifest.Environments() {
		objects = append(objects, map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
			"metadata": map[string]any{"name": deployer, "namespace": env},
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
	return buildKubeconfig(rt.Out.Compute.APIServer, rt.Out.Compute.CACert, token, rt.Setting("environment"))
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
	ns := rt.Setting("environment")
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

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
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

func (p Provider) Installed(ctx context.Context, rt *stack.Runtime) bool {
	_, ok := rt.Kube.SecretValue(ctx, systemNamespace, tokenSecret, "token")
	return ok
}

// Remove deletes the deploy identity; repos keep their workflow but can no
// longer deploy.
func (p Provider) Remove(ctx context.Context, rt *stack.Runtime) error {
	for _, env := range rt.Manifest.Environments() {
		if err := rt.Kube.Delete(ctx, "rolebinding", deployer, "-n", env); err != nil {
			return err
		}
	}
	if err := rt.Kube.Delete(ctx, "clusterrole", deployer); err != nil {
		return err
	}
	return rt.Kube.Delete(ctx, "namespace", systemNamespace)
}
