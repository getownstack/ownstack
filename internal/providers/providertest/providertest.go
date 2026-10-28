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

const Manifest = `
apiVersion: ownstack.io/v1
kind: Stack
metadata:
  name: acme
spec:
  domain: example.com
  compute:
    provider: own-vps
    settings:
      host: 203.0.113.10
  dns:
    provider: cloudflare
  ingress:
    provider: traefik
    settings:
      acmeEmail: ops@example.com
  vcs:
    provider: github
    settings:
      owner: acme
  images:
    provider: harbor
  alerts:
    provider: slack
  cicd:
    provider: jenkins
`

// Env holds the secret values every test runtime resolves.
var Env = map[string]string{
	"CLOUDFLARE_API_TOKEN":   "cf-token-value",
	"GITHUB_TOKEN":           "gh-token-value",
	"HARBOR_ADMIN_PASSWORD":  "harbor-admin-pw",
	"JENKINS_ADMIN_PASSWORD": "jenkins-admin-pw",
	"SLACK_BOT_TOKEN":        "xoxb-slack-token",
}

// Runtime returns a runtime for the test manifest and the console buffer.
func Runtime(t *testing.T) (*stack.Runtime, *bytes.Buffer) {
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
	// The real catalog marks availability from the provider registry, which
	// would import the providers under test; availability is irrelevant here.
	cat := catalog.Default()
	rt := stack.NewRuntime(m, cat, store, console)
	rt.Out.Compute = &stack.ComputeOutput{PublicIP: "203.0.113.10"}
	rt.Out.DNS = &stack.DNSOutput{Challenge: stack.ChallengeDNS, ACMEProvider: "cloudflare", ACMEEnv: map[string]string{"CF_DNS_API_TOKEN": Env["CLOUDFLARE_API_TOKEN"]}}
	rt.Out.Ingress = &stack.IngressOutput{EntryPoint: "websecure", CertResolver: "main", Annotations: map[string]string{
		"traefik.ingress.kubernetes.io/router.entrypoints":      "websecure",
		"traefik.ingress.kubernetes.io/router.tls.certresolver": "main",
	}}
	return rt, &out
}
