package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/getownstack/ownstack/internal/stack"
)

// environments creates one namespace per environment, enforces Pod Security
// on it, and makes its default service account pull from the registry, so
// application charts carry no pull secret.
type environments struct {
	rt *stack.Runtime
}

// registryCredentials is the registry's image pull secret in every environment.
const registryCredentials = "registry-credentials"

// Pod Security Admission labels. Environment namespaces enforce the
// baseline level: no privileged containers, host namespaces, or hostPath
// volumes, so a CI deploy credential limited to these namespaces cannot take
// over the node. warn reports violations to whoever deploys.
const (
	podSecurityEnforce = "pod-security.kubernetes.io/enforce"
	podSecurityWarn    = "pod-security.kubernetes.io/warn"
	podSecurityLevel   = "baseline"
)

// objects are every environment's namespace with its Pod Security labels,
// its pull secret for the registry, and its default service account, which
// pulls with that secret: Kubernetes adds a service account's pull secrets
// to the pods that run as it.
func (e environments) objects() ([]map[string]any, error) {
	images := e.rt.Out.Images
	var config string
	if images != nil {
		if images.PushUsername == "" {
			return nil, fmt.Errorf("the registry at %s has no credential yet; apply its stage first", images.Host)
		}
		var err error
		if config, err = stack.DockerConfigJSON(images.Host, images.PushUsername, images.PushPassword); err != nil {
			return nil, err
		}
	}
	var objects []map[string]any
	for _, environment := range e.rt.Environments() {
		env := environment.Name
		objects = append(objects, map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"name": env,
				"labels": map[string]string{
					podSecurityEnforce: podSecurityLevel,
					podSecurityWarn:    podSecurityLevel,
				},
			},
		})
		if images == nil {
			continue
		}
		objects = append(objects,
			stack.SecretObject(env, registryCredentials, "kubernetes.io/dockerconfigjson", map[string]string{".dockerconfigjson": config}, nil),
			map[string]any{
				"apiVersion":       "v1",
				"kind":             "ServiceAccount",
				"metadata":         map[string]any{"name": "default", "namespace": env},
				"imagePullSecrets": []map[string]string{{"name": registryCredentials}},
			})
	}
	return objects, nil
}

// apply applies every environment's objects in one call.
func (e environments) apply(ctx context.Context) error {
	objects, err := e.objects()
	if err != nil {
		return err
	}
	if err := e.rt.Kube.Apply(ctx, objects...); err != nil {
		return err
	}
	var names []string
	for _, environment := range e.rt.Environments() {
		names = append(names, environment.Name)
	}
	e.rt.Logf("Environments ready: %s", strings.Join(names, ", "))
	return nil
}

func (e environments) status(ctx context.Context) []stack.Check {
	var checks []stack.Check
	images := e.rt.Out.Images
	for _, environment := range e.rt.Environments() {
		env := environment.Name
		if _, err := e.rt.Exec.Output(ctx, "kubectl", "get", "namespace", env, "-o", "name"); err != nil {
			checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "namespace missing"})
			continue
		}
		if images != nil {
			raw, ok := e.rt.Kube.SecretValue(ctx, env, registryCredentials, ".dockerconfigjson")
			if !ok {
				checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "pull secret missing"})
				continue
			}
			// The reason never contains a credential: status goes to health reports.
			if images.PushUsername != "" {
				if reason := stack.PullSecretMismatch(raw, images.Host, images.PushUsername, images.PushPassword); reason != "" {
					checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "stale pull secret (" + reason + "); rerun apply"})
					continue
				}
			}
		}
		checks = append(checks, stack.Check{Name: env, State: stack.OK, Detail: "ready"})
		checks = append(checks, e.appChecks(ctx, env)...)
	}
	return checks
}

// routeResources are the kinds applications publish hosts with: Traefik
// IngressRoutes (common_helm_library), standard Ingresses, and Gateway API
// HTTPRoutes.
var routeResources = []string{"ingressroutes.traefik.io", "ingresses.networking.k8s.io", "httproutes.gateway.networking.k8s.io"}

// appChecks reports every application published in an environment: one
// check per host, with its HTTPS and certificate state.
func (e environments) appChecks(ctx context.Context, env string) []stack.Check {
	var routes []route
	for _, resource := range routeResources {
		out, err := e.rt.Exec.Output(ctx, "kubectl", "get", resource, "-n", env, "-o", "json")
		if err != nil {
			continue // that kind is not installed: no routes of it
		}
		routes = append(routes, parseRoutes(out)...)
	}
	var checks []stack.Check
	for _, r := range uniqueHosts(routes) {
		c := e.rt.CheckHTTPS(ctx, r.host, "/")
		c.Name = "app " + env + "/" + r.name
		c.Detail = "https://" + r.host + " " + c.Detail
		if c.State == stack.Fail {
			// An app being down is reported, but is not a platform failure.
			c.State = stack.Warn
		}
		checks = append(checks, c)
	}
	return checks
}

// route is one host an application object publishes.
type route struct{ name, host string }

var hostRule = regexp.MustCompile("Host\\(`([^`]+)`\\)")

// parseRoutes reads the hosts from `kubectl get -o json` of IngressRoutes
// (Host() rules), Ingresses (spec.rules[].host), or HTTPRoutes
// (spec.hostnames). Empty and wildcard hosts are skipped.
func parseRoutes(data string) []route {
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Routes    []struct{ Match string } `json:"routes"`
				Rules     []struct{ Host string }  `json:"rules"`
				Hostnames []string                 `json:"hostnames"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(data), &list) != nil {
		return nil
	}
	var out []route
	for _, item := range list.Items {
		var hosts []string
		for _, r := range item.Spec.Routes {
			for _, m := range hostRule.FindAllStringSubmatch(r.Match, -1) {
				hosts = append(hosts, m[1])
			}
		}
		for _, r := range item.Spec.Rules {
			hosts = append(hosts, r.Host)
		}
		hosts = append(hosts, item.Spec.Hostnames...)
		for _, h := range hosts {
			if h != "" && !strings.HasPrefix(h, "*") {
				out = append(out, route{item.Metadata.Name, h})
			}
		}
	}
	return out
}

// uniqueHosts keeps the first route for each host, so an app published by
// more than one kind of object is checked once.
func uniqueHosts(routes []route) []route {
	seen := map[string]bool{}
	var out []route
	for _, r := range routes {
		if !seen[r.host] {
			seen[r.host] = true
			out = append(out, r)
		}
	}
	return out
}
