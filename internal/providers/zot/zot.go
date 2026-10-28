// Package zot implements the image registry contract with Zot, a single
// small OCI registry process with htpasswd authentication.
package zot

import (
	"context"
	_ "embed"
	"strings"
	"text/template"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the zot provider's catalog entry.
var Spec = catalog.Provider{
	ID: "zot", Aspect: "images", Name: "Zot", Mono: "ZT", Hosting: catalog.SelfHosted,
	Summary:   "A minimal OCI registry in your cluster: one small process, password-protected, on a persistent volume. The light alternative to Harbor.",
	Resources: catalog.Resources{MemoryMiB: 100, DiskGiB: 10},
	Params: []catalog.Param{
		{Key: "password", Label: "Password", Kind: catalog.KindSecret, Required: true, Generate: true, Env: "ZOT_PASSWORD",
			Help: "Generated on the server when not provided. Use it to docker login as " + username + "."},
		{Key: "keepTags", Label: "Tags to keep", Kind: catalog.KindInt, Default: "0", Advanced: true,
			Help:    "How many of the most recently pushed tags to keep in each repository; older tags are deleted unless pulled in the last 30 days. 0 keeps everything.",
			Pattern: `^[0-9]+$`, PatternHelp: "must be 0 or more"},
	},
}

const (
	namespace    = "zot"
	release      = "zot"
	chartRepo    = "https://zotregistry.dev/helm-charts"
	chartVersion = "0.1.128"
	// username is the one registry user: CI pushes and environments pull
	// with it.
	username = "ownstack"
	// project is the repository namespace images are pushed under.
	project = "apps"
)

//go:embed values.yaml.tmpl
var valuesTemplate string

var tmpl = template.Must(template.New("zot").Parse(valuesTemplate))

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	out := &stack.ImagesOutput{Host: rt.AdminHost("registry"), Project: project, PushUsername: username}
	rt.Out.Images = out
	password, err := rt.Secret("password")
	if err != nil {
		return err
	}
	out.PushPassword = password
	return nil
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
		"Htpasswd":    htpasswd,
		"KeepTags":    rt.SettingInt("keepTags"),
	})
	if err != nil {
		return err
	}
	rt.Logf("Applying Zot %s at https://%s", chartVersion, out.Host)
	if err := rt.Kube.HelmUpgrade(ctx, stack.Chart{
		Release: release, Namespace: namespace, Repo: chartRepo, Name: "zot",
		Version: chartVersion, Values: values, Overrides: rt.Values(),
	}); err != nil {
		return err
	}
	// CI pushes from outside the server, so the registry is ready once it
	// accepts the credential behind a trusted certificate.
	var ready stack.Check
	if err := stack.WaitFor(ctx, rt, "Zot to accept "+out.PushUsername+" behind a trusted certificate", 5*time.Minute, func(ctx context.Context) (bool, string) {
		ready = rt.CheckLogin(ctx, out.Host, "/v2/", out.PushUsername, out.PushPassword)
		return ready.State == stack.OK, ready.Detail
	}); err != nil {
		return err
	}
	rt.Logf("Zot ready (%s); push as %s to %s/%s", ready.Detail, out.PushUsername, out.Host, out.Project)
	return nil
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	out := rt.Out.Images
	checks := []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
	}
	if out.PushPassword == "" {
		return append(checks, rt.CheckHTTPS(ctx, out.Host, "/v2/"),
			stack.Check{Name: "credential", State: stack.Unknown, Detail: "password not available on this node"})
	}
	return append(checks, rt.CheckLogin(ctx, out.Host, "/v2/", out.PushUsername, out.PushPassword))
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
