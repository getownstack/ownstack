package stack

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
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

// HelmStep describes what apply will do to a Helm release.
func HelmStep(ctx context.Context, rt *Runtime, release, namespace, version string) Step {
	target := fmt.Sprintf("helm release %s/%s", namespace, release)
	if r, ok := rt.Kube.Release(ctx, release, namespace); ok {
		installed := chartVersion(r.Chart, version)
		if installed == version && r.Status == "deployed" {
			return Step{Action: "upgrade", Target: target, Detail: "chart " + version + " (reapply values)"}
		}
		return Step{Action: "upgrade", Target: target, Detail: fmt.Sprintf("chart %s -> %s (%s)", installed, version, r.Status)}
	}
	return Step{Action: "install", Target: target, Detail: "chart " + version}
}

// chartSemver matches the version at the end of helm's "<name>-<version>"
// chart column, including pre-release and build suffixes.
var chartSemver = regexp.MustCompile(`-(v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.+-]*)?)$`)

// chartVersion extracts the installed version from helm's chart column
// ("harbor-1.15.0", "app-1.2.3-rc.1"). want is the expected version, which
// settles names that themselves contain a version-like part.
func chartVersion(chart, want string) string {
	if want != "" && strings.HasSuffix(chart, "-"+want) {
		return want
	}
	if m := chartSemver.FindStringSubmatch(chart); m != nil {
		return m[1]
	}
	return chart[strings.LastIndex(chart, "-")+1:]
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
