package stack

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
)

// Render executes a values template.
func Render(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render %s: %w", t.Name(), err)
	}
	return buf.String(), nil
}

// ReleaseCheck reports a Helm release's status.
func ReleaseCheck(ctx context.Context, rt *Runtime, release, namespace string) Check {
	name := "helm " + namespace + "/" + release
	r, ok := rt.Kube.Release(ctx, release, namespace)
	if !ok {
		return Check{Name: name, State: Fail, Detail: "not installed"}
	}
	state := OK
	if r.Status != "deployed" {
		state = Fail
	}
	return Check{Name: name, State: state, Detail: r.Chart + " " + r.Status}
}

// WorkloadCheck reports whether a namespace's Deployments and StatefulSets are ready.
func WorkloadCheck(ctx context.Context, rt *Runtime, namespace string) Check {
	ready, total, err := rt.Kube.WorkloadsReady(ctx, namespace)
	if err != nil {
		return Check{Name: "pods", State: Unknown, Detail: err.Error()}
	}
	state := OK
	if total == 0 || ready < total {
		state = Fail
	}
	return Check{Name: "pods", State: state, Detail: fmt.Sprintf("%d/%d workloads ready", ready, total)}
}
