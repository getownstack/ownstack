package manifest_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers"
)

const valid = `
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
    secrets:
      apiToken: env:CF_TOKEN
  ingress:
    provider: traefik
    settings:
      acmeEmail: ops@example.com
  vcs:
    provider: github
    settings:
      owner: acme
      demoApp: false
  images:
    provider: harbor
  cicd:
    provider: jenkins
    settings:
      repoFilter: "*"
`

func parse(t *testing.T, src string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidManifest(t *testing.T) {
	cat := providers.Catalog()
	m := parse(t, valid)
	if issues := m.Validate(cat); len(issues) > 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	settings := m.Settings(cat, "images")
	if settings["host"] != "harbor.admin.example.com" || settings["project"] != "product" {
		t.Errorf("defaults not applied: %v", settings)
	}
	if got := m.SecretRef(cat, "dns", "apiToken"); got != "env:CF_TOKEN" {
		t.Errorf("explicit ref = %q", got)
	}
	if got := m.SecretRef(cat, "vcs", "token"); got != "env:GITHUB_TOKEN" {
		t.Errorf("conventional ref = %q", got)
	}
	if m.Settings(cat, "vcs")["demoApp"] != "false" {
		t.Errorf("bool setting not read as string")
	}
}

func TestExampleManifestIsValid(t *testing.T) {
	data, err := os.ReadFile("../../examples/ownstack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, string(data))
	if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
		t.Fatalf("examples/ownstack.yaml: %v", issues)
	}
}

func TestAdminSubdomainEmpty(t *testing.T) {
	m := parse(t, strings.Replace(valid, "  domain: example.com", "  domain: example.com\n  adminSubdomain: \"\"", 1))
	if got := m.Settings(providers.Catalog(), "cicd")["host"]; got != "jenkins.example.com" {
		t.Errorf("host = %q", got)
	}
}

func TestValidationIssues(t *testing.T) {
	cases := map[string]struct {
		from, to string
		path     string
	}{
		"planned provider":   {"provider: jenkins", "provider: woodpecker", "spec.cicd.provider"},
		"unknown provider":   {"provider: harbor", "provider: nope", "spec.images.provider"},
		"bad ip":             {"host: 203.0.113.10", "host: not-an-ip", "spec.compute.settings.host"},
		"unknown setting":    {"owner: acme", "owner: acme\n      colour: blue", "spec.vcs.settings.colour"},
		"secret in settings": {"owner: acme", "owner: acme\n      token: ghp_x", "spec.vcs.settings.token"},
		"bad secret ref":     {"apiToken: env:CF_TOKEN", "apiToken: literal-token", "spec.dns.secrets.apiToken"},
		"missing required":   {"acmeEmail: ops@example.com", "dashboardHost: t.example.com", "spec.ingress.settings.acmeEmail"},
		"unknown aspect":     {"  images:", "  storage:\n    provider: x\n  images:", "spec.storage"},
		"bad domain":         {"domain: example.com", "domain: Example", "spec.domain"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := parse(t, strings.Replace(valid, tc.from, tc.to, 1))
			issues := m.Validate(providers.Catalog())
			for _, issue := range issues {
				if issue.Path == tc.path {
					return
				}
			}
			t.Fatalf("expected issue at %s, got %v", tc.path, issues)
		})
	}
}

func TestProviderRequirements(t *testing.T) {
	src := strings.Replace(valid, "  vcs:\n    provider: github\n    settings:\n      owner: acme\n      demoApp: false\n", "", 1)
	issues := parse(t, src).Validate(providers.Catalog())
	found := false
	for _, issue := range issues {
		if issue.Path == "spec.cicd" && strings.Contains(issue.Message, "vcs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected jenkins to require vcs, got %v", issues)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	cat := providers.Catalog()
	m := parse(t, valid)
	out, err := m.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	back := parse(t, string(out))
	if !reflect.DeepEqual(m, back) {
		t.Fatalf("round trip changed the manifest:\n%s", out)
	}
	text := string(out)
	if strings.Index(text, "compute:") > strings.Index(text, "cicd:") {
		t.Errorf("aspects not in catalog order:\n%s", text)
	}
	if !strings.Contains(text, `repoFilter: '*'`) && !strings.Contains(text, `repoFilter: "*"`) {
		t.Errorf("wildcard not quoted:\n%s", text)
	}
}

func TestOldDesignerFileIsNotV1(t *testing.T) {
	_, err := manifest.Parse([]byte("version: 1\nname: My stack\naspects:\n  compute:\n    providers: [own-vps]\n"))
	if err != manifest.ErrNotV1 {
		t.Fatalf("err = %v", err)
	}
}

func TestFromLegacyEnv(t *testing.T) {
	env := map[string]string{
		"vps":                      "203.0.113.10",
		"base_domain":              "example.com",
		"git_username":             "acme",
		"github_pat":               "ghp_secret",
		"system_root_app_repo":     "https://github.com/acme/my-cluster",
		"cloudflare_token":         "cf_secret",
		"harbor_hostname":          "harbor.admin.example.com",
		"harbor_initial_password":  "harbor_pw",
		"traefik_dashboard":        "traefik.admin.example.com",
		"traefik_email":            "ops@example.com",
		"jenkins_hostname":         "jenkins.admin.example.com",
		"jenkins_initial_password": "jenkins_pw",
		"jenkins_chart_version":    "5.8.79",
	}
	m, secrets, ok := manifest.FromLegacyEnv(func(k string) string { return env[k] })
	if !ok {
		t.Fatal("legacy env not detected")
	}
	if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
		t.Fatalf("legacy manifest invalid: %v", issues)
	}
	if got := m.Spec.Components["vcs"].Settings["clusterRepo"]; got != "my-cluster" {
		t.Errorf("clusterRepo = %q", got)
	}
	if secrets["GITHUB_TOKEN"] != "ghp_secret" || secrets["JENKINS_ADMIN_PASSWORD"] != "jenkins_pw" {
		t.Errorf("secrets not mapped: %v", secrets)
	}
	if _, _, ok := manifest.FromLegacyEnv(func(string) string { return "" }); ok {
		t.Error("empty env detected as legacy")
	}
}
