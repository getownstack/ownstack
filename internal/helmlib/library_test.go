// Package helmlib tests common_helm_library by rendering it the way both CI
// providers deploy it: copied into an app chart's charts/ directory and
// templated with the pipeline's --set flags.
package helmlib

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/ghactions"
)

const (
	libraryDir = "../../common_helm_library"
	image      = "registry.example.com/apps/app:0123456789ab"
)

// deployFlags mirrors the helm upgrade of both pipelines (ghactions/ownstack.yml
// and helmDeploy.groovy), which pass the same values: the image, the app id,
// the stack's domain, the environment being deployed to, and the production
// environment.
func deployFlags(domain, environment, production string) []string {
	return []string{
		"--set", "image=" + image, "--set", "id=app", "--set", "domain=" + domain,
		"--set-string", "environment=" + environment, "--set-string", "productionEnvironment=" + production,
	}
}

// prodFlags is a production deploy.
func prodFlags(domain string) []string { return deployFlags(domain, "prod", "prod") }

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
// and extra templates (name -> content), copies the library into
// charts/common_helm_library as both pipelines do, and runs helm template for
// release "app" in namespace prod (flags may name another namespace). It
// returns helm's output exactly.
func renderChart(t *testing.T, values string, extra map[string]string, flags ...string) (string, error) {
	t.Helper()
	helm := helmPath(t)
	chart := filepath.Join(t.TempDir(), "app")
	copyDir(t, "testdata/app-chart", chart)
	copyDir(t, libraryDir, filepath.Join(chart, "charts", "common_helm_library"))
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
	text, err := renderChart(t, values, extra, flags...)
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
	r := mustRender(t, "", prodFlags("example.com")...)
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
	// Pods pull with the namespace's default service account.
	if len(pod.ImagePullSecrets) != 0 {
		t.Errorf("imagePullSecrets = %+v, want none", pod.ImagePullSecrets)
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

// In production a chart's own url wins over the stack domain, with or
// without one.
func TestURLOverridesDomain(t *testing.T) {
	for name, flags := range map[string][]string{
		"domain":    prodFlags("example.com"),
		"no domain": prodFlags(""),
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
	r := mustRender(t, "pathPrefix: /docs\n", prodFlags("example.com")...)
	var ir ingressRoute
	r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
	want := "Host(`app.example.com`) && ( Path(`/docs`) || PathPrefix(`/docs/`) )"
	if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != want {
		t.Errorf("routes = %+v, want match %s", ir.Spec.Routes, want)
	}
}

func TestDomainOrURLRequired(t *testing.T) {
	_, err := render(t, "", prodFlags("")...)
	if err == nil || !strings.Contains(err.Error(), "url or domain is required") {
		t.Errorf("err = %v, want 'url or domain is required'", err)
	}
}

// Any other environment publishes at environmentUrls.<environment>, else
// <id>-<environment>.<domain>; url names the production host and is ignored.
// Nothing but the host differs from a production render.
func TestEnvironmentHost(t *testing.T) {
	urls := "environmentUrls:\n  dev: dev.shop.example.org\n"
	for name, tc := range map[string]struct {
		values string
		flags  []string
		want   string
	}{
		"dev":                        {"", deployFlags("example.com", "dev", "prod"), "app-dev.example.com"},
		"url is the production host": {"url: shop.example.org\n", deployFlags("example.com", "qa", "prod"), "app-qa.example.com"},
		"environment url":            {urls, deployFlags("example.com", "dev", "prod"), "dev.shop.example.org"},
		"another environment's url":  {urls, deployFlags("example.com", "qa", "prod"), "app-qa.example.com"},
		"environment url, no domain": {urls, deployFlags("", "dev", "prod"), "dev.shop.example.org"},
		"production named live":      {"url: shop.example.org\n", deployFlags("example.com", "prod", "live"), "app-prod.example.com"},
		"production is live":         {"url: shop.example.org\n", deployFlags("example.com", "live", "live"), "shop.example.org"},
		"numeric environment":        {"", deployFlags("example.com", "2", "prod"), "app-2.example.com"},
		"environment named true":     {"", deployFlags("example.com", "true", "prod"), "app-true.example.com"},
		"environment named null":     {"", deployFlags("example.com", "null", "prod"), "app-null.example.com"},
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
		r := mustRender(t, "pathPrefix: /docs\n", deployFlags("example.com", "dev", "prod")...)
		var ir ingressRoute
		r.decode(t, "traefik.io/v1alpha1 IngressRoute", &ir)
		want := "Host(`app-dev.example.com`) && ( Path(`/docs`) || PathPrefix(`/docs/`) )"
		if len(ir.Spec.Routes) != 1 || ir.Spec.Routes[0].Match != want {
			t.Errorf("routes = %+v, want match %s", ir.Spec.Routes, want)
		}
	})

	t.Run("only the host differs", func(t *testing.T) {
		values := "replicaCount: 2\npathPrefix: /api\n"
		got, err := renderChart(t, values, nil, append(deployFlags("example.com", "dev", "prod"), "--namespace", "dev")...)
		if err != nil {
			t.Fatal(err)
		}
		production, err := renderChart(t, values, nil, append(prodFlags("example.com"), "--namespace", "dev")...)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(production, "Host(`app.example.com`)", "Host(`app-dev.example.com`)", 1)
		if got != want || want == production {
			t.Errorf("render:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("domain or environment url required", func(t *testing.T) {
		_, err := render(t, "url: shop.example.org\n", deployFlags("", "dev", "prod")...)
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
	// noEnvironment renders without the environment flags, as a chart
	// rendered outside the pipelines would be.
	noEnvironment := []string{"--set", "image=" + image, "--set", "id=app", "--set", "domain=example.com"}
	urls := "environmentUrls:\n  dev: dev.shop.example.org\n"
	cases := map[string]struct {
		values string
		flags  []string
	}{
		"no environment":             {"", noEnvironment},
		"no environment, url":        {"url: shop.example.org\n", noEnvironment},
		"production":                 {"url: shop.example.org\n", prodFlags("example.com")},
		"production, no url":         {urls, prodFlags("example.com")},
		"dev":                        {"url: shop.example.org\n", deployFlags("example.com", "dev", "prod")},
		"dev url":                    {urls, deployFlags("example.com", "dev", "prod")},
		"qa, dev url":                {urls, deployFlags("example.com", "qa", "prod")},
		"no production known":        {"", append(slices.Clone(noEnvironment), "--set", "environment=dev")},
		"empty environment":          {"url: shop.example.org\n", deployFlags("example.com", "", "prod")},
		"chart's environment map":    {"environment:\n  region: eu\n", append(slices.Clone(noEnvironment), "--set", "productionEnvironment=prod")},
		"chart's environment":        {"environment: staging\n", noEnvironment},
		"numeric environment":        {"", deployFlags("example.com", "2", "prod")},
		"environment url, no domain": {urls, deployFlags("", "dev", "prod")},
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
