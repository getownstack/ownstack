package traefik

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/stack"
)

func render(t *testing.T, staging bool) map[string]any {
	t.Helper()
	data := map[string]any{
		"DashboardHost": "traefik.admin.example.com", "Email": "ops@example.com",
		"Certificate": certificateNames("example.com", "admin.example.com"),
		"Staging":     staging, "StagingURL": stagingACMEURL,
		"DNSProvider": "cloudflare", "SecretName": dnsSecretName, "EnvNames": []string{"CF_DNS_API_TOKEN"},
		"EntryPoint": entryPoint, "CertResolver": certResolver,
	}
	out, err := stack.Render(tmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, out)
	}
	return parsed
}

func acme(parsed map[string]any) map[string]any {
	return parsed["certificatesResolvers"].(map[string]any)["main"].(map[string]any)["acme"].(map[string]any)
}

func TestDNSChallenge(t *testing.T) {
	parsed := render(t, false)
	a := acme(parsed)
	if a["dnsChallenge"].(map[string]any)["provider"] != "cloudflare" {
		t.Errorf("acme = %v", a)
	}
	if _, ok := a["caServer"]; ok {
		t.Error("production run must not use the staging CA")
	}
	env := parsed["env"].([]any)[0].(map[string]any)
	if env["name"] != "CF_DNS_API_TOKEN" {
		t.Errorf("env = %v", env)
	}
}

func TestStaging(t *testing.T) {
	a := acme(render(t, true))
	if !strings.Contains(a["caServer"].(string), "staging") {
		t.Errorf("caServer = %v", a["caServer"])
	}
}

// The dashboard route orders the stack's one certificate: the domain, its
// wildcard and the platform hosts' wildcard.
func TestOneWildcardCertificate(t *testing.T) {
	dashboard := render(t, false)["ingressRoute"].(map[string]any)["dashboard"].(map[string]any)
	tls := dashboard["tls"].(map[string]any)
	want := []any{map[string]any{"main": "example.com", "sans": []any{"*.example.com", "*.admin.example.com"}}}
	if tls["certResolver"] != "main" || !reflect.DeepEqual(tls["domains"], want) {
		t.Errorf("tls = %v", tls)
	}
	if dashboard["matchRule"] != "Host(`traefik.admin.example.com`)" {
		t.Errorf("matchRule = %v", dashboard["matchRule"])
	}
	// Platform hosts directly under the domain need no second wildcard.
	if got := certificateNames("example.com", "example.com"); !reflect.DeepEqual(got, []string{"example.com", "*.example.com"}) {
		t.Errorf("names = %v", got)
	}
}
