package engine

import (
	"context"

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
			if err := e.rt.Kube.ApplyPullSecret(ctx, env, images.PullSecretName, images.Host, images.PushUsername, images.PushPassword); err != nil {
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
	}
	return checks
}
