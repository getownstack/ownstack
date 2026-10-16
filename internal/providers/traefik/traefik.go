// Package traefik implements the ingress contract with Traefik and Let's
// Encrypt, using whichever ACME challenge the DNS contract offers.
package traefik

import (
	"context"
	_ "embed"
	"sort"
	"text/template"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

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
	}
	return nil
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	challenge := "TLS-ALPN on the node"
	if rt.Out.DNS.Challenge == stack.ChallengeDNS {
		challenge = "DNS-01 via " + rt.Out.DNS.ACMEProvider
	}
	return []stack.Step{
		stack.HelmStep(ctx, rt, release, namespace, rt.Setting("chartVersion")),
		{Action: "configure", Target: "certificates", Detail: "Let's Encrypt, " + challenge},
		{Action: "verify", Target: "https://" + rt.Setting("dashboardHost"), Detail: "dashboard serves an issued certificate"},
	}
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if err := rt.Kube.EnsureNamespace(ctx, namespace); err != nil {
		return err
	}
	var envNames []string
	if rt.Out.DNS.Challenge == stack.ChallengeDNS {
		if err := rt.Kube.ApplySecret(ctx, namespace, dnsSecretName, "", rt.Out.DNS.ACMEEnv, nil); err != nil {
			return err
		}
		for name := range rt.Out.DNS.ACMEEnv {
			envNames = append(envNames, name)
		}
		sort.Strings(envNames)
	}

	values, err := stack.Render(tmpl, map[string]any{
		"DashboardHost": rt.Setting("dashboardHost"),
		"Email":         rt.Setting("acmeEmail"),
		"Staging":       rt.SettingBool("acmeStaging"),
		"StagingURL":    stagingACMEURL,
		"Challenge":     rt.Out.DNS.Challenge,
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
		check := stack.CheckHTTPS(ctx, host, rt.Out.Compute.PublicIP, "/", rt.SettingBool("acmeStaging"))
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
		stack.CheckHTTPS(ctx, rt.Setting("dashboardHost"), rt.Out.Compute.PublicIP, "/", rt.SettingBool("acmeStaging")),
	}
}
