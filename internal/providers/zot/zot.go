// Package zot implements the image registry contract with Zot, a single
// small OCI registry process with htpasswd authentication.
package zot

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const (
	namespace = "zot"
	release   = "zot"
	chartRepo = "https://zotregistry.dev/helm-charts"
)

//go:embed values.yaml.tmpl
var valuesTemplate string

var tmpl = template.Must(template.New("zot").Parse(valuesTemplate))

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	out := &stack.ImagesOutput{
		Host:           rt.Setting("host"),
		Project:        rt.Setting("project"),
		PushUsername:   rt.Setting("username"),
		PullSecretName: rt.Setting("pullSecretName"),
	}
	rt.Out.Images = out
	password, err := rt.Secret("password")
	if err != nil {
		return err
	}
	out.PushPassword = password
	return nil
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	steps := []stack.Step{
		stack.HelmStep(ctx, rt, release, namespace, rt.Setting("chartVersion")),
		{Action: "ensure", Target: "user " + rt.Setting("username"), Detail: "push and pull for CI and the environments"},
	}
	if keep := rt.SettingInt("keepTags"); keep > 0 {
		steps = append(steps, stack.Step{Action: "ensure", Target: "tag retention", Detail: fmt.Sprintf("keep the %d most recently pushed tags per repository, and tags pulled in the last 30 days", keep)})
	}
	return append(steps, stack.Step{Action: "verify", Target: "https://" + rt.Setting("host") + "/v2/", Detail: "authenticated registry API"})
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	out := rt.Out.Images
	htpasswd, err := htpasswdLine(ctx, rt, out.PushUsername, out.PushPassword)
	if err != nil {
		return err
	}
	values, err := stack.Render(tmpl, map[string]any{
		"Host":        out.Host,
		"Annotations": rt.Out.Ingress.Annotations,
		"StorageGiB":  rt.SettingInt("storageGiB"),
		"Htpasswd":    htpasswd,
		// 0 renders the configuration without retention byte for byte, so
		// existing registries are not restarted.
		"KeepTags": rt.SettingInt("keepTags"),
	})
	if err != nil {
		return err
	}
	rt.Logf("Applying Zot %s at https://%s", rt.Setting("chartVersion"), out.Host)
	if err := stack.Retry(ctx, rt.Console, "zot helm release", 3, 20*time.Second, func() error {
		return rt.Kube.HelmUpgrade(ctx, stack.Chart{
			Release: release, Namespace: namespace, Repo: chartRepo, Name: "zot",
			Version: rt.Setting("chartVersion"), Values: values, Overrides: rt.Values(),
			Timeout: 10 * time.Minute,
		})
	}); err != nil {
		return err
	}
	if err := stack.Retry(ctx, rt.Console, "zot accepts the push credential", 20, 10*time.Second, func() error {
		return checkLogin(ctx, rt.ServiceClient(), out.Host, out.PushUsername, out.PushPassword)
	}); err != nil {
		return err
	}
	// CI pushes from outside the server; that needs a trusted certificate.
	if err := stack.WaitForCertificate(ctx, rt, out.Host, "/v2/", 5*time.Minute); err != nil {
		return err
	}
	rt.Logf("Zot ready; push as %s to %s/%s", out.PushUsername, out.Host, out.Project)
	return nil
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	out := rt.Out.Images
	checks := []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		rt.CheckHTTPS(ctx, out.Host, "/v2/"),
	}
	if out.PushPassword == "" {
		return append(checks, stack.Check{Name: "credential", State: stack.Unknown, Detail: "password not available on this node"})
	}
	if err := checkLogin(ctx, rt.ServiceClient(), out.Host, out.PushUsername, out.PushPassword); err != nil {
		return append(checks, stack.Check{Name: "credential", State: stack.Fail, Detail: err.Error()})
	}
	return append(checks, stack.Check{Name: "credential", State: stack.OK, Detail: out.PushUsername + " can log in"})
}

// htpasswdLine reuses the stored bcrypt hash while the password still matches,
// so reruns do not change the secret and restart the registry.
func htpasswdLine(ctx context.Context, rt *stack.Runtime, user, password string) (string, error) {
	if existing, ok := rt.Kube.SecretValue(ctx, namespace, release+"-secret", "htpasswd"); ok {
		if line, ok := reuseHtpasswd(existing, user, password); ok {
			return line, nil
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return user + ":" + string(hash), nil
}

// reuseHtpasswd returns user's line from an existing htpasswd file when its
// hash still matches password.
func reuseHtpasswd(existing, user, password string) (string, bool) {
	for _, line := range strings.Split(existing, "\n") {
		name, hash, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && name == user && bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil {
			return name + ":" + hash, true
		}
	}
	return "", false
}

func checkLogin(ctx context.Context, client *http.Client, host, user, password string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/v2/", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("registry returned HTTP %d for %s", res.StatusCode, user)
	}
	return nil
}

func (Provider) Installed(ctx context.Context, rt *stack.Runtime) bool {
	_, ok := rt.Kube.Release(ctx, release, namespace)
	return ok
}

// Remove uninstalls the release and its namespace, including stored data.
func (Provider) Remove(ctx context.Context, rt *stack.Runtime) error {
	if err := rt.Kube.HelmUninstall(ctx, release, namespace); err != nil {
		return err
	}
	return rt.Kube.Delete(ctx, "namespace", namespace)
}
