// Package traefik implements the ingress contract with Traefik and Let's
// Encrypt, validating certificates with DNS-01 through the DNS contract.
package traefik

import (
	"context"
	_ "embed"
	"sort"
	"text/template"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the traefik provider's catalog entry.
var Spec = catalog.Provider{
	ID: "traefik", Aspect: "ingress", Name: "Traefik", Mono: "TR", Hosting: catalog.SelfHosted,
	Summary:   "Traefik ingress with Let's Encrypt certificates and a dashboard.",
	Resources: catalog.Resources{MemoryMiB: 128, DiskGiB: 1},
	Params: []catalog.Param{
		{Key: "acmeEmail", Label: "ACME email", Kind: catalog.KindEmail, Required: true, Placeholder: "ops@example.com",
			Help: "Let's Encrypt account email for expiry notices."},
		{Key: "dashboardHost", Label: "Dashboard host", Kind: catalog.KindHostname, Default: "traefik.{adminDomain}"},
		{Key: "acmeStaging", Label: "Use Let's Encrypt staging", Kind: catalog.KindBool, Default: "false", Advanced: true,
			Help: "Issue untrusted staging certificates. Useful for test runs that would hit rate limits."},
		{Key: "chartVersion", Label: "Chart version", Kind: catalog.KindString, Default: "36.3.0", Advanced: true},
	},
}

const (
	namespace      = "traefik"
	release        = "traefik"
	chartRepo      = "https://traefik.github.io/charts"
	dnsSecretName  = "ownstack-acme-dns"
	entryPoint     = "websecure"
	certResolver   = "main"
	stagingACMEURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

//go:embed values.yaml.tmpl
var valuesTemplate string

var tmpl = template.Must(template.New("traefik").Parse(valuesTemplate))

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.Ingress = &stack.IngressOutput{
		EntryPoint:   entryPoint,
		CertResolver: certResolver,
		Annotations: map[string]string{
			"traefik.ingress.kubernetes.io/router.entrypoints":      entryPoint,
			"traefik.ingress.kubernetes.io/router.tls.certresolver": certResolver,
		},
		UntrustedCerts: rt.SettingBool("acmeStaging"),
		DashboardURL:   "https://" + rt.Setting("dashboardHost"),
	}
	return nil
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if err := rt.Kube.EnsureNamespace(ctx, namespace); err != nil {
		return err
	}
	if err := rt.Kube.ApplySecret(ctx, namespace, dnsSecretName, "", rt.Out.DNS.ACMEEnv, nil); err != nil {
		return err
	}
	var envNames []string
	for name := range rt.Out.DNS.ACMEEnv {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)

	values, err := stack.Render(tmpl, map[string]any{
		"DashboardHost": rt.Setting("dashboardHost"),
		"Email":         rt.Setting("acmeEmail"),
		"Staging":       rt.SettingBool("acmeStaging"),
		"StagingURL":    stagingACMEURL,
		"DNSProvider":   rt.Out.DNS.ACMEProvider,
		"EnvNames":      envNames,
		"SecretName":    dnsSecretName,
		"EntryPoint":    entryPoint,
		"CertResolver":  certResolver,
	})
	if err != nil {
		return err
	}
	rt.Logf("Applying Traefik %s", rt.Setting("chartVersion"))
	if err := stack.Retry(ctx, rt.Console, "traefik helm release", 3, 20*time.Second, func() error {
		return rt.Kube.HelmUpgrade(ctx, stack.Chart{
			Release: release, Namespace: namespace, Repo: chartRepo, Name: "traefik",
			Version: rt.Setting("chartVersion"), Values: values, Overrides: rt.Values(),
			Timeout: 10 * time.Minute,
		})
	}); err != nil {
		return err
	}

	// Certificate issuance proves DNS, ports, and ACME end to end. It can lag,
	// so a slow certificate is reported but does not fail the run.
	host := rt.Setting("dashboardHost")
	deadline := time.Now().Add(5 * time.Minute)
	for {
		check := rt.CheckHTTPS(ctx, host, "/")
		if check.State == stack.OK {
			rt.Logf("Traefik: %s", check.Detail)
			return nil
		}
		if time.Now().After(deadline) {
			rt.Logf("warning: %s: %s. Certificates may still be issuing; `ownstackctl status` will show when they are.", host, check.Detail)
			return nil
		}
		rt.Logf("  waiting: certificate for %s (%s)", host, check.Detail)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	return []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		rt.CheckHTTPS(ctx, rt.Setting("dashboardHost"), "/"),
	}
}
