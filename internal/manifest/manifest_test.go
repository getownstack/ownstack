package manifest_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
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
	settings := m.Settings(cat, "cicd")
	if settings["host"] != "jenkins.admin.example.com" || settings["orgFolder"] != "Repositories" || settings["repoFilter"] != "*" {
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

func TestExampleManifestsAreValid(t *testing.T) {
	for _, name := range []string{"starter", "standard"} {
		data, err := os.ReadFile("../../examples/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		m := parse(t, string(data))
		if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml: %v", name, issues)
		}
		// The commented-out environments are the defaults, and valid.
		uncommented := strings.Replace(string(data), "  # environments:", "  environments:", 1)
		if uncommented == string(data) {
			t.Errorf("examples/%s.yaml does not show the environments", name)
			continue
		}
		m = parse(t, uncommented)
		if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml uncommented: %v", name, issues)
		}
		if !reflect.DeepEqual(m.Spec.Environments, manifest.DefaultEnvironments) {
			t.Errorf("examples/%s.yaml uncommented: environments = %v", name, m.Spec.Environments)
		}
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
		"unknown provider":   {"provider: harbor", "provider: nope", "spec.images.provider"},
		"bad ip":             {"host: 203.0.113.10", "host: not-an-ip", "spec.compute.settings.host"},
		"unknown setting":    {"owner: acme", "owner: acme\n      colour: blue", "spec.vcs.settings.colour"},
		"secret in settings": {"owner: acme", "owner: acme\n      token: ghp_x", "spec.vcs.settings.token"},
		"bad secret ref":     {"apiToken: env:CF_TOKEN", "apiToken: literal-token", "spec.dns.secrets.apiToken"},
		"missing required":   {"acmeEmail: ops@example.com", "dashboardHost: t.example.com", "spec.ingress.settings.acmeEmail"},
		"unknown aspect":     {"  images:", "  storage:\n    provider: x\n  images:", "spec.storage"},
		"bad domain":         {"domain: example.com", "domain: Example", "spec.domain"},
		"empty pull secret":  {"provider: harbor", "provider: zot\n    settings:\n      pullSecretName: \"\"", "spec.images.settings.pullSecretName"},
		"bad pull secret":    {"provider: harbor", "provider: zot\n    settings:\n      pullSecretName: Harbor_Robot", "spec.images.settings.pullSecretName"},
		"uppercase project":  {"provider: harbor", "provider: zot\n    settings:\n      project: Product", "spec.images.settings.project"},
		"harbor project":     {"provider: harbor", "provider: harbor\n    settings:\n      project: apps", "spec.images.settings.project"},
		"bad k3s version":    {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: latest", "spec.compute.settings.k3sVersion"},
		"removed setting":    {`repoFilter: "*"`, "branches: develop=dev", "spec.cicd.settings.branches"},
		"duplicate env":      {"domain: example.com", "domain: example.com\n  environments: [dev, prod, dev]", "spec.environments[2]"},
		"reserved env":       {"domain: example.com", "domain: example.com\n  environments: [dev, kube-system]", "spec.environments[1]"},
		"missing aspect":     {"  cicd:\n    provider: jenkins\n", "", "spec.cicd"},
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

func TestPatternsAccept(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"k3s version without v": {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: 1.33.4+k3s1"},
		"k3s release candidate": {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: v1.34.0-rc1+k3s1"},
		"project path":          {"provider: harbor", "provider: zot\n    settings:\n      project: team/my--app\n      pullSecretName: harbor-robot"},
	} {
		t.Run(name, func(t *testing.T) {
			if issues := parse(t, strings.Replace(valid, tc.from, tc.to, 1)).Validate(providers.Catalog()); len(issues) > 0 {
				t.Fatalf("unexpected issues: %v", issues)
			}
		})
	}
}

func TestReservedEnvironments(t *testing.T) {
	// Namespaces Kubernetes and the platform providers use. A provider that
	// installs into a new namespace must be covered here too.
	for _, ns := range []string{"default", "kube-system", "kube-public", "kube-node-lease", "ownstack-system", "traefik", "zot", "harbor", "jenkins"} {
		if !manifest.ReservedEnvironment(ns) {
			t.Errorf("%s is not reserved", ns)
		}
		src := strings.Replace(valid, "domain: example.com", "domain: example.com\n  environments: [prod, "+ns+"]", 1)
		issues := parse(t, src).Validate(providers.Catalog())
		if len(issues) != 1 || issues[0].Path != "spec.environments[1]" || !strings.Contains(issues[0].Message, "reserved") {
			t.Errorf("%s: issues = %v", ns, issues)
		}
	}
	for _, env := range []string{"dev", "qa", "prod", "staging", "kube", "ownstack", "jenkins-ci", "my-harbor"} {
		if manifest.ReservedEnvironment(env) {
			t.Errorf("%s is reserved", env)
		}
	}
}

// spec.environments lists <name>[=<branch glob>]: the first is production,
// and a branch deploys into the first environment whose glob matches it.
func TestEnvironments(t *testing.T) {
	m := parse(t, valid)
	want := []manifest.Environment{{Name: "prod", Branch: "main"}, {Name: "dev", Branch: "develop"}, {Name: "qa", Branch: "release/*"}}
	if got := m.Environments(); !reflect.DeepEqual(got, want) {
		t.Errorf("default environments = %v", got)
	}
	m = parse(t, strings.Replace(valid, "domain: example.com", "domain: example.com\n  environments: [live=trunk, staging, preview=*]", 1))
	want = []manifest.Environment{{Name: "live", Branch: "trunk"}, {Name: "staging"}, {Name: "preview", Branch: "*"}}
	if got := m.Environments(); !reflect.DeepEqual(got, want) {
		t.Errorf("environments = %v", got)
	}

	for name, tc := range map[string]struct {
		environments string
		issues       []string // paths, in order
	}{
		"defaults":           {"", nil},
		"names only":         {"[prod, dev]", nil},
		"globs":              {"[prod=main, dev=feature/**, qa=release/1.*, x=Team_x-1]", nil},
		"catch-all last":     {"[prod=main, dev=*]", nil},
		"empty branch":       {"[prod=]", []string{"spec.environments[0]"}},
		"two equals":         {"[prod=main=x]", []string{"spec.environments[0]"}},
		"bad name":           {"[Prod=main]", []string{"spec.environments[0]"}},
		"no name":            {"[=main]", []string{"spec.environments[0]"}},
		"bad branch":         {`[prod=main, dev="feature;rm"]`, []string{"spec.environments[1]"}},
		"question mark":      {`[prod=main, "qa=release/?"]`, []string{"spec.environments[1]"}},
		"leading dash":       {"[prod=-x]", []string{"spec.environments[0]"}},
		"duplicate glob":     {"[prod=main, dev=main]", []string{"spec.environments[1]"}},
		"duplicate name":     {"[prod=main, prod=develop]", []string{"spec.environments[1]"}},
		"reserved with glob": {"[jenkins=main]", []string{"spec.environments[0]"}},
		"several problems":   {"[prod=main, Dev=main, qa=a;b]", []string{"spec.environments[1]", "spec.environments[1]", "spec.environments[2]"}},
	} {
		t.Run(name, func(t *testing.T) {
			src := valid
			if tc.environments != "" {
				src = strings.Replace(valid, "domain: example.com", "domain: example.com\n  environments: "+tc.environments, 1)
			}
			var got []string
			for _, issue := range parse(t, src).Validate(providers.Catalog()) {
				got = append(got, issue.Path)
			}
			if !reflect.DeepEqual(got, tc.issues) {
				t.Fatalf("issues at %v, want %v", got, tc.issues)
			}
		})
	}
}

// Environments are written back the way they were read.
func TestEnvironmentsRoundTrip(t *testing.T) {
	cat := providers.Catalog()
	m := parse(t, strings.Replace(valid, "domain: example.com", "domain: example.com\n  environments: [prod=main, dev=*, preview]", 1))
	out, err := m.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	back := parse(t, string(out))
	if got := back.Spec.Environments; !reflect.DeepEqual(got, []string{"prod=main", "dev=*", "preview"}) {
		t.Fatalf("environments = %q after a round trip:\n%s", got, out)
	}
	if issues := back.Validate(cat); len(issues) > 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
}

func TestZotUsername(t *testing.T) {
	zot := strings.Replace(valid, "provider: harbor", "provider: zot\n    settings:\n      username: USER", 1)
	for user, ok := range map[string]bool{"ownstack": true, "me@example.com": true, `"a:b"`: false, `"a b"`: false} {
		issues := parse(t, strings.Replace(zot, "USER", user, 1)).Validate(providers.Catalog())
		if ok != (len(issues) == 0) {
			t.Errorf("username %s: issues = %v", user, issues)
		}
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

func TestOtherFilesAreNotManifests(t *testing.T) {
	m := parse(t, "version: 1\nname: My stack\naspects:\n  compute:\n    providers: [own-vps]\n")
	issues := m.Validate(providers.Catalog())
	if len(issues) == 0 || issues[0].Path != "apiVersion" {
		t.Fatalf("issues = %v", issues)
	}
}
