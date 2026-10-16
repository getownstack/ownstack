package engine

import (
	"context"
	"encoding/json"
	"regexp"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// environments creates one namespace per environment and gives each the
// registry's pull secret, so application charts can pull private images.
type environments struct {
	rt *stack.Runtime
}

func (e environments) plan(ctx context.Context) []stack.Step {
	var steps []stack.Step
	for _, env := range e.rt.Manifest.Environments() {
		detail := "namespace"
		if images := e.rt.Out.Images; images != nil {
			detail += " + pull secret " + images.PullSecretName
		}
		steps = append(steps, stack.Step{Action: "ensure", Target: env, Detail: detail})
	}
	return steps
}

func (e environments) apply(ctx context.Context) error {
	images := e.rt.Out.Images
	for _, env := range e.rt.Manifest.Environments() {
		if err := e.rt.Kube.EnsureNamespace(ctx, env); err != nil {
			return err
		}
		if images != nil && images.PushUsername != "" {
			if err := e.rt.Kube.ApplyPullSecret(ctx, env, images.PullSecretName, images.PullSecretServer(), images.PushUsername, images.PushPassword); err != nil {
				return err
			}
		}
		e.rt.Logf("Environment %s ready", env)
	}
	return nil
}

func (e environments) status(ctx context.Context) []stack.Check {
	var checks []stack.Check
	images := e.rt.Out.Images
	for _, env := range e.rt.Manifest.Environments() {
		if _, err := e.rt.Exec.Output(ctx, "kubectl", "get", "namespace", env); err != nil {
			checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "namespace missing"})
			continue
		}
		if images != nil {
			if _, ok := e.rt.Kube.SecretValue(ctx, env, images.PullSecretName, ".dockerconfigjson"); !ok {
				checks = append(checks, stack.Check{Name: env, State: stack.Fail, Detail: "pull secret missing"})
				continue
			}
		}
		checks = append(checks, stack.Check{Name: env, State: stack.OK, Detail: "ready"})
		checks = append(checks, e.appChecks(ctx, env)...)
	}
	return checks
}

var hostRule = regexp.MustCompile("Host\\(`([^`]+)`\\)")

// appChecks reports every application published in an environment: one
// check per IngressRoute host, with its HTTPS and certificate state.
func (e environments) appChecks(ctx context.Context, env string) []stack.Check {
	out, err := e.rt.Exec.Output(ctx, "kubectl", "get", "ingressroutes.traefik.io", "-n", env, "-o", "json")
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Spec     struct {
				Routes []struct{ Match string } `json:"routes"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil
	}
	var checks []stack.Check
	for _, item := range list.Items {
		for _, route := range item.Spec.Routes {
			for _, m := range hostRule.FindAllStringSubmatch(route.Match, -1) {
				c := stack.CheckHTTPS(ctx, m[1], e.rt.Out.Compute.PublicIP, "/", false)
				c.Name = "app " + env + "/" + item.Metadata.Name
				c.Detail = "https://" + m[1] + " " + c.Detail
				if c.State == stack.Fail {
					// An app being down is reported, but is not a platform failure.
					c.State = stack.Warn
				}
				checks = append(checks, c)
			}
		}
	}
	return checks
}
