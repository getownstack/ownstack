// Package traefik implements the ingress contract with Traefik and Let's
// Encrypt, validating certificates with DNS-01 through the DNS contract.
package traefik

import (
	"context"
	_ "embed"
	"sort"
	"strings"
	"text/template"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the traefik provider's catalog entry.
var Spec = catalog.Provider{
	ID: "traefik", Aspect: "ingress", Name: "Traefik", Mono: "TR", Hosting: catalog.SelfHosted,
	Summary:   "Traefik ingress with one Let's Encrypt wildcard certificate and a dashboard.",
	Resources: catalog.Resources{MemoryMiB: 128, DiskGiB: 1},
	Params: []catalog.Param{
		{Key: "acmeEmail", Label: "ACME email", Kind: catalog.KindEmail, Required: true, Placeholder: "ops@example.com",
			Help: "Let's Encrypt account email for expiry notices."},
		{Key: "acmeStaging", Label: "Use Let's Encrypt staging", Kind: catalog.KindBool, Default: "false", Advanced: true,
			Help: "Issue untrusted staging certificates. Useful for test runs that would hit rate limits."},
	},
}

const (
	namespace      = "traefik"
	release        = "traefik"
	chartRepo      = "https://traefik.github.io/charts"
	chartVersion   = "36.3.0"
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
		Annotations: map[string]string{
			"traefik.ingress.kubernetes.io/router.entrypoints":      entryPoint,
			"traefik.ingress.kubernetes.io/router.tls.certresolver": certResolver,
		},
		UntrustedCerts: rt.SettingBool("acmeStaging"),
		DashboardURL:   "https://" + rt.AdminHost("traefik"),
	}
	return nil
}

// certificateNames are the names of the stack's one certificate: the
// domain, every host under it, and every platform host. Traefik orders
// nothing for a host that a stored or pending certificate already covers, so
// the registry, CI and every app with a host one label under the domain use
// this certificate.
func certificateNames(domain, adminDomain string) []string {
	names := []string{domain, "*." + domain}
	if adminDomain != domain {
		names = append(names, "*."+adminDomain)
	}
	return names
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if err := rt.Kube.Apply(ctx,
		map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace}},
		stack.SecretObject(namespace, dnsSecretName, "", rt.Out.DNS.ACMEEnv, nil),
	); err != nil {
		return err
	}
	var envNames []string
	for name := range rt.Out.DNS.ACMEEnv {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)

	names := certificateNames(rt.Domain(), rt.AdminDomain())
	values, err := stack.Render(tmpl, map[string]any{
		"DashboardHost": rt.AdminHost("traefik"),
		"Certificate":   names,
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
	rt.Logf("Applying Traefik %s; it orders one certificate for %s", chartVersion, strings.Join(names, ", "))
	return rt.Kube.HelmUpgrade(ctx, stack.Chart{
		Release: release, Namespace: namespace, Repo: chartRepo, Name: "traefik",
		Version: chartVersion, Values: values, Overrides: rt.Values(),
	})
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	return []stack.Check{
		stack.ReleaseCheck(ctx, rt, release, namespace),
		stack.WorkloadCheck(ctx, rt, namespace),
		rt.CheckHTTPS(ctx, rt.AdminHost("traefik"), "/"),
	}
}
