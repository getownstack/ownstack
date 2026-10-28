// Package helmlib tests common_helm_library by rendering it the way both CI
// providers deploy it: copied into an app chart's charts/ directory and
// templated with the pipeline's --set flags. Both CI providers fetch the
// library from this repository at deploy time, so these renders are a
// compatibility contract for every existing app chart.
package helmlib

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/ghactions"
)

const (
	libraryDir = "../../common_helm_library"
	// baselineDir is a frozen copy of the library from before environments.
	// Production deploys must render byte-identically to it: ownstack.io and
	// every existing app chart take the live library from main.
	baselineDir = "testdata/baseline-library"
	image       = "registry.example.com/product/app:main-abc1234"
)

// githubActionsFlags mirrors the helm upgrade in ghactions/ownstack.yml, which
// always passes domain and imagePullSecret (empty when the variable is unset).
// ownstack-workflow 3 adds environment and productionEnvironment
// (withEnvironment); older workflows do not.
func githubActionsFlags(domain, pullSecret string) []string {
	return []string{"--set", "image=" + image, "--set", "id=app", "--set", "domain=" + domain, "--set", "imagePullSecret=" + pullSecret}
}

// withEnvironment adds the environment being deployed to and the production
// environment, as ownstack-workflow 3 always does and the Jenkins library
// does when it knows them.
func withEnvironment(flags []string, environment, production string) []string {
	return append(slices.Clone(flags), "--set-string", "environment="+environment, "--set-string", "productionEnvironment="+production)
}

// jenkinsFlags mirrors a production deploy by helmDeploy.groovy, which passes
// domain and imagePullSecret only when Jenkins has them in its global env, and
// always passes the environment (here prod, the production environment).
func jenkinsFlags(domain, pullSecret string) []string {
	flags := []string{"--set", "image=" + image, "--set", "id=app"}
	if domain != "" {
		flags = append(flags, "--set", "domain="+domain)
	}
	if pullSecret != "" {
		flags = append(flags, "--set", "imagePullSecret="+pullSecret)
	}
	return withEnvironment(flags, "prod", "prod")
}

type deployment struct {
	Metadata objectMeta
	Spec     struct {
		Replicas int
		Selector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		}
		Template struct {
			Metadata struct{ Labels map[string]string }
			Spec     struct {
				ImagePullSecrets []struct{ Name string } `yaml:"imagePullSecrets"`
				Containers       []struct{ Name, Image string }
			}
		}
	}
}

type service struct {
	Metadata objectMeta
	Spec     struct {
		Type     string
		Selector map[string]string
		Ports    []struct {
			Port       int
			TargetPort int `yaml:"targetPort"`
		}
	}
}

type ingressRoute struct {
	Metadata objectMeta
	Spec     struct {
		EntryPoints []string `yaml:"entryPoints"`
		Routes      []struct {
			Kind     string
			Match    string
			Services []struct {
				Name string
				Port int
			}
		}
		TLS struct {
			CertResolver string `yaml:"certResolver"`
		}
	}
}

type objectMeta struct{ Name, Namespace string }

// rendered holds one chart render, keyed by "<apiVersion> <kind>".
type rendered map[string]*yaml.Node

func (r rendered) decode(t *testing.T, key string, into any) {
	t.Helper()
	node, ok := r[key]
	if !ok {
		t.Fatalf("render has no %s", key)
	}
	if err := node.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
}

func helmPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI to render common_helm_library")
		}
		t.Skip("SKIPPING common_helm_library render tests: helm is not on PATH. Install helm 3 to run them.")
	}
	return path
}

// renderChart builds an app chart from testdata with the given values.yaml
// and extra templates (name -> content), copies library into
// charts/common_helm_library as both pipelines do, and runs helm template for
// release "app" in namespace prod (flags may name another namespace). It
// returns helm's output exactly.
func renderChart(t *testing.T, library, values string, extra map[string]string, flags ...string) (string, error) {
	t.Helper()
	helm := helmPath(t)
	chart := filepath.Join(t.TempDir(), "app")
	copyDir(t, "testdata/app-chart", chart)
	copyDir(t, library, filepath.Join(chart, "charts", "common_helm_library"))
	if values != "" {
		if err := os.WriteFile(filepath.Join(chart, "values.yaml"), []byte(values), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range extra {
		if err := os.WriteFile(filepath.Join(chart, "templates", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	home := t.TempDir()
	cmd := exec.Command(helm, append([]string{"template", "app", chart, "--namespace", "prod"}, flags...)...)
	cmd.Env = append(os.Environ(),
		"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
		"HELM_DATA_HOME="+filepath.Join(home, "data"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", errors.New(strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// render renders the live library with the given values.yaml and flags.
func render(t *testing.T, values string, flags ...string) (rendered, error) {
	t.Helper()
	return renderWith(t, values, nil, flags...)
}

func renderWith(t *testing.T, values string, extra map[string]string, flags ...string) (rendered, error) {
	t.Helper()
	text, err := renderChart(t, libraryDir, values, extra, flags...)
	if err != nil {
		return nil, err
	}
	out := rendered{}
	dec := yaml.NewDecoder(strings.NewReader(text))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("invalid YAML from helm template: %v\n%s", err, text)
		}
		var head struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string
		}
		if err := node.Decode(&head); err != nil {
			t.Fatal(err)
		}
		if head.Kind == "" {
			continue
		}
		key := head.APIVersion + " " + head.Kind
		if _, dup := out[key]; dup {
			t.Fatalf("render has two %s documents", key)
		}
		out[key] = &node
	}
	return out, nil
}

func mustRender(t *testing.T, values string, flags ...string) rendered {
	t.Helper()
	r, err := render(t, values, flags...)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	return r
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublicURLApplication(t *testing.T) {
	r := mustRender(t, "", githubActionsFlags("example.com", "registry-credentials")...)
	if len(r) != 3 {
		t.Errorf("rendered %d documents, want Deployment, Service, IngressRoute", len(r))
	}

	var d deployment
	r.decode(t, "apps/v1 Deployment", &d)
	if d.Metadata != (objectMeta{"app", "prod"}) || d.Spec.Replicas != 1 {
		t.Errorf("deployment = %+v, replicas %d", d.Metadata, d.Spec.Replicas)
	}
	if d.Spec.Selector.MatchLabels["app"] != "app" || d.Spec.Template.Metadata.Labels["app"] != "app" {
		t.Errorf("deployment labels = %v / %v", d.Spec.Selector.MatchLabels, d.Spec.Template.Metadata.Labels)
	}
	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 || pod.Containers[0].Name != "app" || pod.Containers[0].Image != image {
		t.Errorf("containers = %+v", pod.Containers)
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "registry-credentials" {
		t.Errorf("imagePullSecrets = %+v", pod.ImagePullSecrets)
	}

	var s service
	r.decode(t, "v1 Service", &s)
	if s.Metadata != (objectMeta{"app", "prod"}) || s.Spec.Type != "ClusterIP" || s.Spec.Selector["app"] != "app" {
		t.Errorf("service = %+v", s)
	}
	if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 80 || s.Spec.Ports[0].TargetPort != 80 {
		t.Errorf("service ports = %+v", s.Spec.Ports)
	}

	var ir ingressRoute
	r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
	if ir.Metadata != (objectMeta{"app", "prod"}) {
		t.Errorf("ingress route = %+v", ir.Metadata)
	}
	if len(ir.Spec.EntryPoints) != 1 || ir.Spec.EntryPoints[0] != "websecure" || ir.Spec.TLS.CertResolver != "main" {
		t.Errorf("entryPoints = %v, certResolver = %q", ir.Spec.EntryPoints, ir.Spec.TLS.CertResolver)
	}
	if len(ir.Spec.Routes) != 1 {
		t.Fatalf("routes = %+v", ir.Spec.Routes)
	}
	route := ir.Spec.Routes[0]
	if route.Kind != "Rule" || route.Match != "Host(`app.example.com`)" {
		t.Errorf("route = %q %q", route.Kind, route.Match)
	}
	if len(route.Services) != 1 || route.Services[0].Name != "app" || route.Services[0].Port != 80 {
		t.Errorf("route services = %+v", route.Services)
	}
}

// The pipeline's pull secret is used when set; charts deployed before the
// registry contract existed rely on the harbor-robot default.
func TestPullSecret(t *testing.T) {
	for name, tc := range map[string]struct {
		flags []string
		want  string
	}{
		"github actions":                 {githubActionsFlags("example.com", "registry-credentials"), "registry-credentials"},
		"github actions, unset variable": {githubActionsFlags("example.com", ""), "harbor-robot"},
		"jenkins":                        {jenkinsFlags("example.com", "registry-credentials"), "registry-credentials"},
		"jenkins, no IMAGE_PULL_SECRET":  {jenkinsFlags("example.com", ""), "harbor-robot"},
	} {
		t.Run(name, func(t *testing.T) {
			var d deployment
			mustRender(t, "", tc.flags...).decode(t, "apps/v1 Deployment", &d)
			secrets := d.Spec.Template.Spec.ImagePullSecrets
			if len(secrets) != 1 || secrets[0].Name != tc.want {
				t.Errorf("imagePullSecrets = %+v, want %s", secrets, tc.want)
			}
		})
	}
}

// A chart's own url wins over the stack domain, with or without one.
func TestURLOverridesDomain(t *testing.T) {
	for name, flags := range map[string][]string{
		"github actions":           githubActionsFlags("example.com", "registry-credentials"),
		"jenkins without a domain": jenkinsFlags("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			r := mustRender(t, "url: shop.example.org\nreplicaCount: 3\n", flags...)
			var ir ingressRoute
			r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
			if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != "Host(`shop.example.org`)" {
				t.Errorf("routes = %+v", ir.Spec.Routes)
			}
			var d deployment
			r.decode(t, "apps/v1 Deployment", &d)
			if d.Spec.Replicas != 3 {
				t.Errorf("replicas = %d", d.Spec.Replicas)
			}
		})
	}
}

func TestPathPrefix(t *testing.T) {
	r := mustRender(t, "pathPrefix: /docs\n", jenkinsFlags("example.com", "harbor-robot")...)
	var ir ingressRoute
	r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
	want := "Host(`app.example.com`) && ( Path(`/docs`) || PathPrefix(`/docs/`) )"
	if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != want {
		t.Errorf("routes = %+v, want match %s", ir.Spec.Routes, want)
	}
}

func TestDomainOrURLRequired(t *testing.T) {
	for name, flags := range map[string][]string{
		"github actions, empty domain": githubActionsFlags("", "registry-credentials"),
		"jenkins, no OWNSTACK_DOMAIN":  jenkinsFlags("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := render(t, "", flags...)
			if err == nil || !strings.Contains(err.Error(), "url or domain is required") {
				t.Errorf("err = %v, want 'url or domain is required'", err)
			}
		})
	}
}

// Production deploys, and deploys by pipelines that name no environment (older
// workflows and Jenkins libraries), render byte-identically to the library
// before environments existed, for every value combination charts and
// pipelines used. ownstack.io and every existing app chart take the live
// library, so this must never be relaxed.
func TestProductionOutputUnchanged(t *testing.T) {
	values := map[string]string{
		"defaults":                      "",
		"url":                           "url: shop.example.org\nreplicaCount: 3\n",
		"path prefix":                   "pathPrefix: /docs\n",
		"url, path prefix, pull secret": "url: shop.example.org\npathPrefix: /api\nimagePullSecret: custom\n",
		"environment urls":              "environmentUrls:\n  dev: dev.shop.example.org\n",
		"chart's own environment map":   "environment:\n  region: eu\n",
		"chart's own environment":       "environment: staging\n",
	}
	pipelines := map[string][]string{
		"github actions":                  githubActionsFlags("example.com", "registry-credentials"),
		"github actions, unset variables": githubActionsFlags("", ""),
		"jenkins":                         jenkinsFlags("example.com", "registry-credentials"),
		"jenkins, no domain":              jenkinsFlags("", ""),
	}
	environments := map[string][]string{
		"no environment":        nil,
		"prod":                  {"--set", "environment=prod", "--set", "productionEnvironment=prod"},
		"production named live": {"--set", "environment=live", "--set", "productionEnvironment=live"},
		"no production known":   {"--set", "environment=dev"},
		"empty environment":     {"--set", "environment=", "--set", "productionEnvironment=prod"},
	}
	helmPath(t)
	var mu sync.Mutex
	renders, errs := 0, 0
	t.Cleanup(func() {
		// Both outcomes must be covered: renders, and the missing-domain error.
		if !t.Failed() && (renders == 0 || errs == 0) {
			t.Errorf("%d renders and %d errors compared; want both", renders, errs)
		}
	})
	for vName, v := range values {
		for pName, p := range pipelines {
			for eName, e := range environments {
				flags := append(slices.Clone(p), e...)
				t.Run(vName+"/"+pName+"/"+eName, func(t *testing.T) {
					t.Parallel()
					want, wantErr := renderChart(t, baselineDir, v, nil, flags...)
					got, gotErr := renderChart(t, libraryDir, v, nil, flags...)
					if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
						t.Fatalf("error = %v, want %v", gotErr, wantErr)
					}
					if got != want {
						t.Errorf("output changed:\n%s\nwant:\n%s", got, want)
					}
					mu.Lock()
					defer mu.Unlock()
					if wantErr != nil {
						errs++
					} else {
						renders++
					}
				})
			}
		}
	}
}

// Any other environment publishes at environmentUrls.<environment>, else
// <id>-<environment>.<domain>; url names the production host and is ignored.
// Nothing but the host differs from a production render.
func TestEnvironmentHost(t *testing.T) {
	gh := githubActionsFlags("example.com", "registry-credentials")
	urls := "environmentUrls:\n  dev: dev.shop.example.org\n"
	for name, tc := range map[string]struct {
		values string
		flags  []string
		want   string
	}{
		"dev":                        {"", withEnvironment(gh, "dev", "prod"), "app-dev.example.com"},
		"url is the production host": {"url: shop.example.org\n", withEnvironment(gh, "qa", "prod"), "app-qa.example.com"},
		"environment url":            {urls, withEnvironment(gh, "dev", "prod"), "dev.shop.example.org"},
		"another environment's url":  {urls, withEnvironment(gh, "qa", "prod"), "app-qa.example.com"},
		"environment url, no domain": {urls, withEnvironment(jenkinsFlags("", ""), "dev", "prod"), "dev.shop.example.org"},
		"production named live":      {"url: shop.example.org\n", withEnvironment(gh, "prod", "live"), "app-prod.example.com"},
		"jenkins":                    {"", withEnvironment(jenkinsFlags("example.com", ""), "staging", "prod"), "app-staging.example.com"},
		"numeric environment":        {"", withEnvironment(gh, "2", "prod"), "app-2.example.com"},
		"environment named true":     {"", withEnvironment(gh, "true", "prod"), "app-true.example.com"},
		"environment named null":     {"", withEnvironment(gh, "null", "prod"), "app-null.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			r := mustRender(t, tc.values, append(tc.flags, "--namespace", "dev")...)
			var ir ingressRoute
			r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
			if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != "Host(`"+tc.want+"`)" {
				t.Errorf("routes = %+v, want host %s", ir.Spec.Routes, tc.want)
			}
			if ir.Metadata.Namespace != "dev" {
				t.Errorf("namespace = %q", ir.Metadata.Namespace)
			}
		})
	}

	t.Run("path prefix", func(t *testing.T) {
		r := mustRender(t, "pathPrefix: /docs\n", withEnvironment(jenkinsFlags("example.com", "harbor-robot"), "dev", "prod")...)
		var ir ingressRoute
		r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
		want := "Host(`app-dev.example.com`) && ( Path(`/docs`) || PathPrefix(`/docs/`) )"
		if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != want {
			t.Errorf("routes = %+v, want match %s", ir.Spec.Routes, want)
		}
	})

	t.Run("only the host differs", func(t *testing.T) {
		values := "replicaCount: 2\npathPrefix: /api\n"
		flags := append(withEnvironment(gh, "dev", "prod"), "--namespace", "dev")
		got, err := renderChart(t, libraryDir, values, nil, flags...)
		if err != nil {
			t.Fatal(err)
		}
		production, err := renderChart(t, baselineDir, values, nil, flags...)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(production, "Host(`app.example.com`)", "Host(`app-dev.example.com`)", 1)
		if got != want || want == production {
			t.Errorf("render:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("domain or environment url required", func(t *testing.T) {
		_, err := render(t, "url: shop.example.org\n", withEnvironment(jenkinsFlags("", ""), "dev", "prod")...)
		if err == nil || !strings.Contains(err.Error(), "environmentUrls.dev or domain is required") {
			t.Errorf("err = %v", err)
		}
	})
}

// valuesDump is an extra app-chart template exposing the computed values, as
// helm get values -a returns them after a deploy.
const valuesDump = `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
data:
  values.json: {{ .Values | toJson | quote }}
`

// workflowHostRule is the Python the GitHub Actions workflow prints the
// deployed host with.
func workflowHostRule(t *testing.T) string {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(ghactions.Workflow), &wf); err != nil {
		t.Fatal(err)
	}
	for _, step := range wf.Jobs["deploy"].Steps {
		if m := regexp.MustCompile(`(?s)\nhost_rule='\n(.*?)\n'\n`).FindStringSubmatch(step.Run); m != nil {
			return m[1]
		}
	}
	t.Fatal("the workflow has no host_rule")
	return ""
}

// The workflow's deploy summary reads the host back from the release's values
// with the library's rule; both must agree for every case.
func TestWorkflowHostRuleMatchesLibrary(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	rule := workflowHostRule(t)
	gh := githubActionsFlags("example.com", "registry-credentials")
	urls := "environmentUrls:\n  dev: dev.shop.example.org\n"
	cases := map[string]struct {
		values string
		flags  []string
	}{
		"older workflow":           {"", gh},
		"older workflow, url":      {"url: shop.example.org\n", gh},
		"production":               {"url: shop.example.org\n", withEnvironment(gh, "prod", "prod")},
		"production, no url":       {urls, withEnvironment(gh, "prod", "prod")},
		"dev":                      {"url: shop.example.org\n", withEnvironment(gh, "dev", "prod")},
		"dev url":                  {urls, withEnvironment(gh, "dev", "prod")},
		"qa, dev url":              {urls, withEnvironment(gh, "qa", "prod")},
		"no production known":      {"", append(slices.Clone(gh), "--set", "environment=dev")},
		"empty environment":        {"url: shop.example.org\n", withEnvironment(gh, "", "prod")},
		"chart's environment map":  {"environment:\n  region: eu\n", append(slices.Clone(gh), "--set", "productionEnvironment=prod")},
		"chart's environment":      {"environment: staging\n", gh},
		"numeric environment":      {"", withEnvironment(gh, "2", "prod")},
		"jenkins, environment url": {urls, withEnvironment(jenkinsFlags("", ""), "dev", "prod")},
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	host := regexp.MustCompile("^Host\\(`([^`]+)`\\)")
	for _, name := range names {
		tc := cases[name]
		t.Run(name, func(t *testing.T) {
			r, err := renderWith(t, tc.values, map[string]string{"values.yaml": valuesDump}, tc.flags...)
			if err != nil {
				t.Fatal(err)
			}
			var ir ingressRoute
			r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
			m := host.FindStringSubmatch(ir.Spec.Routes[0].Match)
			if m == nil {
				t.Fatalf("match = %q", ir.Spec.Routes[0].Match)
			}
			var dump struct{ Data map[string]string }
			r.decode(t, "v1 ConfigMap", &dump)
			cmd := exec.Command("python3", "-c", rule)
			cmd.Stdin = strings.NewReader(dump.Data["values.json"])
			out, err := cmd.CombinedOutput()
			if got := strings.TrimSpace(string(out)); err != nil || got != m[1] {
				t.Errorf("workflow host = %q (%v), library host = %q", got, err, m[1])
			}
		})
	}
}
