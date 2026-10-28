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

func TestExampleManifestsAreValid(t *testing.T) {
	for _, name := range []string{"starter", "standard", "byo-registry"} {
		data, err := os.ReadFile("../../examples/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		m := parse(t, string(data))
		if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml: %v", name, issues)
		}
		// Commented-out settings must be valid once uncommented.
		uncommented := strings.NewReplacer("    # settings:", "    settings:", "    #   ", "      ").Replace(string(data))
		if uncommented == string(data) {
			continue
		}
		m = parse(t, uncommented)
		if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml uncommented: %v", name, issues)
		}
		if m.Spec.Components["cicd"].Settings["branches"] != "develop=dev release/*=qa" {
			t.Errorf("examples/%s.yaml uncommented: cicd settings = %v", name, m.Spec.Components["cicd"].Settings)
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
		"planned provider":    {"provider: jenkins", "provider: woodpecker", "spec.cicd.provider"},
		"unknown provider":    {"provider: harbor", "provider: nope", "spec.images.provider"},
		"bad ip":              {"host: 203.0.113.10", "host: not-an-ip", "spec.compute.settings.host"},
		"unknown setting":     {"owner: acme", "owner: acme\n      colour: blue", "spec.vcs.settings.colour"},
		"secret in settings":  {"owner: acme", "owner: acme\n      token: ghp_x", "spec.vcs.settings.token"},
		"bad secret ref":      {"apiToken: env:CF_TOKEN", "apiToken: literal-token", "spec.dns.secrets.apiToken"},
		"missing required":    {"acmeEmail: ops@example.com", "dashboardHost: t.example.com", "spec.ingress.settings.acmeEmail"},
		"unknown aspect":      {"  images:", "  storage:\n    provider: x\n  images:", "spec.storage"},
		"bad domain":          {"domain: example.com", "domain: Example", "spec.domain"},
		"empty pull secret":   {"provider: harbor", "provider: harbor\n    settings:\n      pullSecretName: \"\"", "spec.images.settings.pullSecretName"},
		"bad pull secret":     {"provider: harbor", "provider: harbor\n    settings:\n      pullSecretName: Harbor_Robot", "spec.images.settings.pullSecretName"},
		"uppercase project":   {"provider: harbor", "provider: harbor\n    settings:\n      project: Product", "spec.images.settings.project"},
		"bad k3s version":     {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: latest", "spec.compute.settings.k3sVersion"},
		"bad deploy branch":   {`repoFilter: "*"`, "deployBranch: main;rm", "spec.cicd.settings.deployBranch"},
		"duplicate env":       {"domain: example.com", "domain: example.com\n  environments: [dev, prod, dev]", "spec.environments[2]"},
		"reserved env":        {"domain: example.com", "domain: example.com\n  environments: [dev, kube-system]", "spec.environments[1]"},
		"cicd env not listed": {"domain: example.com", "domain: example.com\n  environments: [dev, staging]", "spec.cicd.settings.environment"},
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

func TestPatternsAndEnvironmentsAccept(t *testing.T) {
	for name, tc := range map[string]struct{ from, to string }{
		"k3s version without v": {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: 1.33.4+k3s1"},
		"k3s release candidate": {"host: 203.0.113.10", "host: 203.0.113.10\n      k3sVersion: v1.34.0-rc1+k3s1"},
		"deploy branch":         {`repoFilter: "*"`, "deployBranch: release/1.2"},
		"project path":          {"provider: harbor", "provider: harbor\n    settings:\n      project: team/my--app\n      pullSecretName: harbor-robot"},
		"custom environments":   {"domain: example.com", "domain: example.com\n  environments: [dev, staging]"},
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(valid, tc.from, tc.to, 1)
			if name == "custom environments" {
				src = strings.Replace(src, `repoFilter: "*"`, "environment: staging", 1)
			}
			if issues := parse(t, src).Validate(providers.Catalog()); len(issues) > 0 {
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

func TestCICDEnvironmentMustBeListed(t *testing.T) {
	ghActions := strings.Replace(valid, "provider: jenkins\n    settings:\n      repoFilter: \"*\"", "provider: gh-actions", 1)
	ghActions = strings.Replace(ghActions, "provider: harbor", "provider: zot", 1)
	for name, tc := range map[string]struct {
		src   string
		valid bool
	}{
		"default prod":          {ghActions, true},
		"listed":                {strings.Replace(ghActions, "provider: gh-actions", "provider: gh-actions\n    settings:\n      environment: qa", 1), true},
		"not listed":            {strings.Replace(ghActions, "provider: gh-actions", "provider: gh-actions\n    settings:\n      environment: staging", 1), false},
		"default not in custom": {strings.Replace(ghActions, "domain: example.com", "domain: example.com\n  environments: [dev, staging]", 1), false},
		"explicitly empty":      {strings.Replace(ghActions, "provider: gh-actions", "provider: gh-actions\n    settings:\n      environment: \"\"", 1), false},
		"jenkins default prod":  {valid, true},
		"jenkins not in custom": {strings.Replace(valid, "domain: example.com", "domain: example.com\n  environments: [dev, staging]", 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			issues := parse(t, tc.src).Validate(providers.Catalog())
			if tc.valid {
				if len(issues) > 0 {
					t.Fatalf("unexpected issues: %v", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Path != "spec.cicd.settings.environment" {
				t.Fatalf("issues = %v", issues)
			}
		})
	}
}

func TestBranches(t *testing.T) {
	ghActions := strings.Replace(valid, "provider: jenkins\n    settings:\n      repoFilter: \"*\"", "provider: gh-actions\n    settings:\n      SETTINGS", 1)
	ghActions = strings.Replace(ghActions, "provider: harbor", "provider: zot", 1)
	jenkins := strings.Replace(valid, `repoFilter: "*"`, "SETTINGS", 1)
	custom := func(src string) string {
		return strings.Replace(src, "domain: example.com", "domain: example.com\n  environments: [dev, staging, prod]", 1)
	}
	for name, tc := range map[string]struct {
		src, settings string
		issues        int // at spec.cicd.settings.branches; no other issues
	}{
		"empty":                  {ghActions, `branches: ""`, 0},
		"gitflow":                {ghActions, `branches: develop=dev release/*=qa`, 0},
		"glob everything":        {ghActions, `branches: "develop=dev *=qa"`, 0},
		"tabs and newlines":      {ghActions, `branches: "develop=dev\trelease/*=qa\n hotfix/*=prod "`, 0},
		"globs and dots":         {ghActions, `branches: "release/1.*=qa feature/**=dev Team_x-1=dev"`, 0},
		"custom environment":     {custom(ghActions), `branches: develop=staging`, 0},
		"jenkins gitflow":        {jenkins, `branches: develop=dev release/*=qa`, 0},
		"jenkins main elsewhere": {jenkins, "deployBranch: release\n      branches: main=dev", 0},
		"no equals":              {ghActions, `branches: develop`, 1},
		"two equals":             {ghActions, `branches: develop=dev=qa`, 1},
		"empty branch":           {ghActions, `branches: =dev`, 1},
		"empty environment":      {ghActions, `branches: develop=`, 1},
		"unknown environment":    {ghActions, `branches: develop=staging`, 1},
		"not in custom":          {custom(ghActions), `branches: develop=qa`, 1},
		"bad branch":             {ghActions, `branches: "feature;rm=dev"`, 1},
		"leading dash":           {ghActions, `branches: -x=dev`, 1},
		"question mark":          {ghActions, `branches: release/?=qa`, 1},
		"carriage return":        {ghActions, `branches: "develop=dev\r"`, 1},
		"listed twice":           {ghActions, `branches: develop=dev release/*=qa develop=qa`, 1},
		"default branch":         {ghActions, `branches: main=dev`, 1},
		"jenkins default":        {jenkins, `branches: develop=dev main=qa`, 1},
		"jenkins deploy branch":  {jenkins, "deployBranch: release\n      branches: release=qa", 1},
		"several problems":       {ghActions, `branches: main=dev develop=x develop=dev feature`, 4},
	} {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(tc.src, "SETTINGS", tc.settings, 1)
			issues := parse(t, src).Validate(providers.Catalog())
			n := 0
			for _, issue := range issues {
				if issue.Path != "spec.cicd.settings.branches" {
					t.Fatalf("unexpected issue %v", issue)
				}
				n++
			}
			if n != tc.issues {
				t.Fatalf("%d issues, want %d: %v", n, tc.issues, issues)
			}
		})
	}
}

// A branches value may start with '*', a YAML alias indicator: writing the
// manifest must quote it.
func TestBranchesRoundTrip(t *testing.T) {
	cat := providers.Catalog()
	m := parse(t, strings.Replace(valid, `repoFilter: "*"`, `branches: "*=qa develop=dev"`, 1))
	out, err := m.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	back := parse(t, string(out))
	if got := back.Spec.Components["cicd"].Settings["branches"]; got != "*=qa develop=dev" {
		t.Fatalf("branches = %q after a round trip:\n%s", got, out)
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
