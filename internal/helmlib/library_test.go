// Package helmlib tests common_helm_library by rendering it the way both CI
// providers deploy it: copied into an app chart's charts/ directory and
// templated with the pipeline's --set flags. GitHub Actions stacks fetch the
// library from this repository at deploy time, so these renders are a
// compatibility contract for every existing app chart.
package helmlib

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	libraryDir = "../../common_helm_library"
	image      = "registry.example.com/product/app:main-abc1234"
)

// githubActionsFlags mirrors the helm upgrade in ghactions/ownstack.yml, which
// always passes domain and imagePullSecret (empty when the variable is unset).
func githubActionsFlags(domain, pullSecret string) []string {
	return []string{"--set", "image=" + image, "--set", "id=app", "--set", "domain=" + domain, "--set", "imagePullSecret=" + pullSecret}
}

// jenkinsFlags mirrors helmDeploy.groovy, which passes domain and
// imagePullSecret only when Jenkins has them in its global env.
func jenkinsFlags(domain, pullSecret string) []string {
	flags := []string{"--set", "image=" + image, "--set", "id=app"}
	if domain != "" {
		flags = append(flags, "--set", "domain="+domain)
	}
	if pullSecret != "" {
		flags = append(flags, "--set", "imagePullSecret="+pullSecret)
	}
	return flags
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

// render builds an app chart from testdata with the given values.yaml, copies
// the library into charts/common_helm_library as both pipelines do, and runs
// helm template for release "app" in namespace prod.
func render(t *testing.T, values string, flags ...string) (rendered, error) {
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
		return nil, errors.New(strings.TrimSpace(stderr.String()))
	}

	out := rendered{}
	dec := yaml.NewDecoder(&stdout)
	for {
		var node yaml.Node
		if err := dec.Decode(&node); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("invalid YAML from helm template: %v\n%s", err, stdout.String())
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
