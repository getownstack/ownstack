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

// Pod Security Admission labels. Environment namespaces enforce at least the
// baseline level: no privileged containers, host namespaces, or hostPath
// volumes, so a CI deploy credential limited to these namespaces cannot take
// over the node. warn reports violations to whoever deploys.
const (
	podSecurityEnforce = "pod-security.kubernetes.io/enforce"
	podSecurityWarn    = "pod-security.kubernetes.io/warn"
	podSecurityLevel   = "baseline"
)

// atLeastBaseline is the level to set when a namespace has current: a
// stricter level (restricted) is kept, anything else becomes baseline.
func atLeastBaseline(current string) string {
	if current == "restricted" {
		return current
	}
	return podSecurityLevel
}

// enforcesPodSecurity reports whether a namespace's labels enforce at least
// baseline.
func enforcesPodSecurity(labels map[string]string) bool {
	level := labels[podSecurityEnforce]
	return level == "baseline" || level == "restricted"
}

// namespaceObject is an environment namespace with its Pod Security labels,
// given the labels it has now. The environments stage is the only code that
// applies these namespaces: an apply without the labels would remove them.
func namespaceObject(env string, current map[string]string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name": env,
			"labels": map[string]string{
				podSecurityEnforce: atLeastBaseline(current[podSecurityEnforce]),
				podSecurityWarn:    atLeastBaseline(current[podSecurityWarn]),
			},
		},
	}
}

// namespace reads an environment namespace. exists is false when kubectl
// cannot get it; labels is nil when its JSON could not be read.
func (e environments) namespace(ctx context.Context, env string) (exists bool, labels map[string]string) {
	out, err := e.rt.Exec.Output(ctx, "kubectl", "get", "namespace", env, "-o", "json")
	if err != nil {
		return false, nil
	}
	var ns struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if json.Unmarshal([]byte(out), &ns) != nil {
		return true, nil
	}
	if ns.Metadata.Labels == nil {
		ns.Metadata.Labels = map[string]string{}
	}
	return true, ns.Metadata.Labels
}

func (e environments) plan(ctx context.Context) []stack.Step {
	var steps []stack.Step
	for _, env := range e.rt.Manifest.Environments() {
		detail := "namespace, Pod Security " + podSecurityLevel
		if images := e.rt.Out.Images; images != nil {
			detail += " + pull secret " + images.PullSecretName
		}
		steps = append(steps, stack.Step{Action: "ensure", Target: env, Detail: detail})
	}
	return steps
}

func (e environments) apply(ctx context.Context) error {
	images := e.rt.Out.Images
	if images != nil && images.PushUsername == "" {
		e.rt.Logf("warning: the registry has no credential yet, so the environments get no pull secret %s; apply the images aspect", images.PullSecretName)
	}
	for _, env := range e.rt.Manifest.Environments() {
		exists, labels := e.namespace(ctx, env)
		if exists && labels != nil && !enforcesPodSecurity(labels) {
			e.reportPodSecurityViolations(ctx, env)
		}
		if err := e.rt.Kube.Apply(ctx, namespaceObject(env, labels)); err != nil {
			return err
		}
		if images != nil && images.PushUsername != "" {
			if err := e.rt.Kube.ApplyPullSecret(ctx, env, images.PullSecretName, images.PullSecretServer(), images.PushUsername, images.PushPassword); err != nil {
				return err
			}
			e.defaultServiceAccountPulls(ctx, env, images.PullSecretName)
		}
		e.rt.Logf("Environment %s ready", env)
	}
	return nil
}

// reportPodSecurityViolations asks the API server, in a dry run, which
// existing pods the baseline level would reject. They keep running; their
// replacements (a restart, a reschedule, a rollout) are refused.
func (e environments) reportPodSecurityViolations(ctx context.Context, env string) {
	warnings, err := e.rt.Kube.NamespaceLabelWarnings(ctx, env, map[string]string{podSecurityEnforce: podSecurityLevel})
	if err != nil {
		e.rt.Logf("warning: could not check the pods in %s against Pod Security %s: %v", env, podSecurityLevel, err)
		return
	}
	if len(warnings) == 0 {
		return
	}
	e.rt.Logf("warning: %s now enforces Pod Security %s. These pods keep running, but replacements for them will be rejected:", env, podSecurityLevel)
	for _, w := range warnings {
		e.rt.Logf("  %s", w)
	}
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
	for _, env := range e.rt.Manifest.Environments() {
		exists, labels := e.namespace(ctx, env)
		if !exists {
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
				if reason := stack.PullSecretMismatch(raw, images.PullSecretServer(), images.PushUsername, images.PushPassword); reason != "" {
					checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "stale pull secret (" + reason + "); rerun apply --only environments"})
					continue
				}
			}
		}
		if labels != nil && !enforcesPodSecurity(labels) {
			checks = append(checks, stack.Check{Name: env, State: stack.Warn, Detail: "ready; Pod Security is not enforced, rerun apply --only environments"})
		} else {
			checks = append(checks, stack.Check{Name: env, State: stack.OK, Detail: "ready"})
		}
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
