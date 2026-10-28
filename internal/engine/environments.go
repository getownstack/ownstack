package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/getownstack/ownstack/internal/stack"
)

// environments creates one namespace per environment, enforces Pod Security
// on it, and gives it the registry's pull secret, so application charts can
// pull private images.
type environments struct {
	rt *stack.Runtime
}

// Pod Security Admission labels. Environment namespaces enforce the
// baseline level: no privileged containers, host namespaces, or hostPath
// volumes, so a CI deploy credential limited to these namespaces cannot take
// over the node. warn reports violations to whoever deploys.
const (
	podSecurityEnforce = "pod-security.kubernetes.io/enforce"
	podSecurityWarn    = "pod-security.kubernetes.io/warn"
	podSecurityLevel   = "baseline"
)

// namespaceObject is an environment namespace with its Pod Security labels.
// The environments stage is the only code that applies these namespaces: an
// apply without the labels would remove them.
func namespaceObject(env string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": env,
			"labels": map[string]string{
				podSecurityEnforce: podSecurityLevel,
				podSecurityWarn:    podSecurityLevel,
			},
		},
	}
}

func (e environments) apply(ctx context.Context) error {
	images := e.rt.Out.Images
	if images != nil && images.PushUsername == "" {
		e.rt.Logf("warning: the registry has no credential yet, so the environments get no pull secret %s; apply the images aspect", images.PullSecretName)
	}
	for _, environment := range e.rt.Environments() {
		env := environment.Name
		if err := e.rt.Kube.Apply(ctx, namespaceObject(env)); err != nil {
			return err
		}
		if images != nil && images.PushUsername != "" {
			if err := e.rt.Kube.ApplyPullSecret(ctx, env, images.PullSecretName, images.Host, images.PushUsername, images.PushPassword); err != nil {
				return err
			}
			e.defaultServiceAccountPulls(ctx, env, images.PullSecretName)
		}
		e.rt.Logf("Environment %s ready", env)
	}
	return nil
}

// The default service account appears shortly after its namespace.
var (
	serviceAccountAttempts = 10
	serviceAccountDelay    = 2 * time.Second
)

// defaultServiceAccountPulls adds the pull secret to the namespace's default
// service account, so pods that name no imagePullSecrets (hooks, CronJobs,
// third-party charts) pull from the registry too. Pods that name one, as
// public-url-application does, are unaffected, so a failure is a warning.
func (e environments) defaultServiceAccountPulls(ctx context.Context, env, secret string) {
	var failed error
	err := stack.Retry(ctx, e.rt.Console, "default service account in "+env, serviceAccountAttempts, serviceAccountDelay, func() error {
		_, err := e.rt.Kube.AddImagePullSecret(ctx, env, "default", secret)
		if err != nil && !strings.Contains(err.Error(), "(NotFound)") && !strings.Contains(err.Error(), "(Conflict)") {
			failed = err
			return nil
		}
		return err
	})
	if err == nil {
		err = failed
	}
	if err != nil {
		e.rt.Logf("warning: the default service account in %s does not pull with %s: %v", env, secret, err)
	}
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
			raw, ok := e.rt.Kube.SecretValue(ctx, env, images.PullSecretName, ".dockerconfigjson")
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
