// Package providertest builds runtimes for provider unit tests.
package providertest

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// Manifest is the test stack: the Standard preset.
const Manifest = `apiVersion: ownstack.io/v2
domain: example.com
compute: {host: 203.0.113.10}
ingress: {acmeEmail: ops@example.com}
vcs: {owner: acme}
images: {provider: harbor}
cicd: {provider: jenkins}
`

// Env holds the secret values every test runtime resolves.
var Env = map[string]string{
	"CLOUDFLARE_API_TOKEN":   "cf-token-value",
	"GITHUB_TOKEN":           "gh-token-value",
	"HARBOR_ADMIN_PASSWORD":  "harbor-admin-pw",
	"JENKINS_ADMIN_PASSWORD": "jenkins-admin-pw",
}

// Runtime returns a runtime for the test manifest and the console buffer.
// Its catalog holds specs, the providers under test: the full catalog
// (providers.Catalog) would import the packages that use this one. Change
// the stack with Set, or set rt.Manifest.Environments.
func Runtime(t *testing.T, specs ...catalog.Provider) (*stack.Runtime, *bytes.Buffer) {
	t.Helper()
	m, err := manifest.Parse([]byte(Manifest))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	console := stack.NewConsole(&out)
	store := secrets.NewStore(filepath.Join(t.TempDir(), "secrets.env"))
	store.Getenv = func(k string) string { return Env[k] }
	store.OnValue(console.Redact)
	cat := catalog.Default()
	cat.Providers = specs
	rt := stack.NewRuntime(m, cat, store, console)
	rt.Out.Compute = &stack.ComputeOutput{PublicIP: "203.0.113.10"}
	rt.Out.DNS = &stack.DNSOutput{ACMEProvider: "cloudflare", ACMEEnv: map[string]string{"CF_DNS_API_TOKEN": Env["CLOUDFLARE_API_TOKEN"]}}
	rt.Out.Ingress = &stack.IngressOutput{EntryPoint: "websecure", CertResolver: "main", Annotations: map[string]string{
		"traefik.ingress.kubernetes.io/router.entrypoints":      "websecure",
		"traefik.ingress.kubernetes.io/router.tls.certresolver": "main",
	}}
	return rt, &out
}

// Set replaces an aspect's entry in rt's manifest: its provider ("" for the
// aspect's default) and its settings, as ownstack.yaml would give them.
func Set(rt *stack.Runtime, aspect, provider string, settings map[string]string) {
	if settings == nil {
		settings = map[string]string{}
	}
	rt.Manifest.Components[aspect] = &manifest.Component{Provider: provider, Settings: settings}
}
