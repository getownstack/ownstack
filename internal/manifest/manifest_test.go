package manifest_test

import (
	"flag"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
)

var update = flag.Bool("update", false, "rewrite testdata/marshal.golden.yaml")

// starter is a complete Starter stack: everything else is a default.
const starter = `apiVersion: ownstack.io/v2
domain: example.com
compute: {host: 203.0.113.10}
ingress: {acmeEmail: ops@example.com}
vcs: {owner: acme}
`

func parse(t *testing.T, src string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func paths(issues []manifest.Issue) []string {
	var out []string
	for _, issue := range issues {
		out = append(out, issue.Path)
	}
	return out
}

func TestStarterDefaults(t *testing.T) {
	cat := providers.Catalog()
	m := parse(t, starter)
	if issues := m.Validate(cat); len(issues) > 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	for aspect, want := range map[string]string{"compute": "own-vps", "dns": "cloudflare", "ingress": "traefik", "vcs": "github", "images": "zot", "cicd": "gh-actions"} {
		if got := m.Provider(cat, aspect); got != want {
			t.Errorf("%s provider = %s, want %s", aspect, got, want)
		}
	}
	if got := m.Settings(cat, "compute"); got["host"] != "203.0.113.10" || got["swapGiB"] != "2" {
		t.Errorf("compute settings = %v", got)
	}
	if m.AdminDomain() != "admin.example.com" || len(m.EnvironmentList()) != 3 {
		t.Errorf("admin domain %s, environments %v", m.AdminDomain(), m.EnvironmentList())
	}
	if string(m.Source) != starter {
		t.Errorf("source = %q", m.Source)
	}
}

func TestAdminSubdomainEmpty(t *testing.T) {
	m := parse(t, starter+"adminSubdomain: \"\"\n")
	if m.AdminDomain() != "example.com" {
		t.Errorf("admin domain = %s", m.AdminDomain())
	}
}

func TestExampleManifestsAreValid(t *testing.T) {
	for _, name := range []string{"starter", "standard"} {
		data, err := os.ReadFile("../../examples/" + name + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		if issues := parse(t, string(data)).Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml: %v", name, issues)
		}
		// The commented-out environments are the defaults, and valid.
		uncommented := strings.Replace(string(data), "# environments:", "environments:", 1)
		if uncommented == string(data) {
			t.Errorf("examples/%s.yaml does not show the environments", name)
			continue
		}
		m := parse(t, uncommented)
		if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
			t.Errorf("examples/%s.yaml uncommented: %v", name, issues)
		}
		if !slices.Equal(m.Environments, manifest.DefaultEnvironments) {
			t.Errorf("examples/%s.yaml uncommented: environments = %v", name, m.Environments)
		}
	}
}

// The designer sends JSON; an empty stack names what it still needs.
func TestJSONInput(t *testing.T) {
	m := parse(t, `{"apiVersion":"ownstack.io/v2","domain":"example.com"}`)
	issues := m.Validate(providers.Catalog())
	if got := paths(issues); !slices.Equal(got, []string{"compute.host", "ingress.acmeEmail", "vcs.owner"}) {
		t.Fatalf("issues = %v", issues)
	}
	if issues[0].Message != "Own VPS public IP is required" {
		t.Errorf("message = %q", issues[0].Message)
	}
	m = parse(t, `{"apiVersion":"ownstack.io/v2","domain":"example.com","compute":{"host":"203.0.113.10","swapGiB":0},`+
		`"ingress":{"acmeEmail":"ops@example.com"},"vcs":{"owner":"acme","demoApp":false},"images":{"provider":"harbor","values":{"a":1}}}`)
	if issues := m.Validate(providers.Catalog()); len(issues) > 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if s := m.Settings(providers.Catalog(), "vcs"); s["demoApp"] != "false" || m.Values("images")["a"] != 1 {
		t.Errorf("vcs settings %v, images values %v", s, m.Values("images"))
	}
}

func TestValidationIssues(t *testing.T) {
	cases := map[string]struct {
		add     string // appended to starter, or a replacement "from=>to"
		path    string
		message string
	}{
		"unknown provider":  {add: "cicd: {provider: woodpecker}", path: "cicd.provider", message: `unknown cicd provider "woodpecker"; use one of gh-actions, jenkins`},
		"bad ip":            {add: "compute: {host: 203.0.113.10}=>compute: {host: not-an-ip}", path: "compute.host", message: "Own VPS public IP must be an IPv4 address"},
		"unknown setting":   {add: "vcs: {owner: acme}=>vcs: {owner: acme, colour: blue}", path: "vcs.colour", message: `GitHub has no setting "colour"`},
		"secret":            {add: "vcs: {owner: acme}=>vcs: {owner: acme, token: ghp_x}", path: "vcs.token", message: "GitHub access token is a secret and never goes in ownstack.yaml; set GITHUB_TOKEN on the server instead"},
		"unknown key":       {add: "kind: Stack", path: "kind"},
		"bad domain":        {add: "domain: example.com=>domain: Example", path: "domain"},
		"v1":                {add: "ownstack.io/v2=>ownstack.io/v1", path: "apiVersion", message: "ownstack.io/v1 manifests are no longer read; download the manifest again from the designer"},
		"no api version":    {add: "apiVersion: ownstack.io/v2\n=>", path: "apiVersion"},
		"not a value":       {add: "compute: {host: 203.0.113.10}=>compute: {host: [203.0.113.10]}", path: "compute.host", message: "Own VPS host must be a single value"},
		"not a mapping":     {add: "dns: cloudflare", path: "dns", message: "dns must be a mapping of settings, such as {provider: ...}"},
		"values":            {add: "images: {values: [a]}", path: "images.values"},
		"listed twice":      {add: "compute: {host: 203.0.113.10}=>compute: {host: 203.0.113.10, host: 203.0.113.11}", path: "compute.host"},
		"bool":              {add: "vcs: {owner: acme}=>vcs: {owner: acme, demoApp: maybe}", path: "vcs.demoApp", message: "GitHub create demo app repo must be true or false"},
		"int":               {add: "compute: {host: 203.0.113.10}=>compute: {host: 203.0.113.10, swapGiB: two}", path: "compute.swapGiB"},
		"pattern":           {add: "images: {keepTags: -1}", path: "images.keepTags"},
		"email":             {add: "ops@example.com=>Ops <ops@example.com>", path: "ingress.acmeEmail"},
		"admin subdomain":   {add: "adminSubdomain: Admin", path: "adminSubdomain"},
		"duplicate env":     {add: "environments: [dev, prod, dev]", path: "environments[2]"},
		"reserved env":      {add: "environments: [dev, kube-system]", path: "environments[1]"},
		"provider settings": {add: "cicd: {provider: jenkins, repos: other-org/site}", path: "cicd.repos"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			src := starter + tc.add + "\n"
			if from, to, ok := strings.Cut(tc.add, "=>"); ok {
				src = strings.Replace(starter, from, to, 1)
			}
			issues := parse(t, src).Validate(providers.Catalog())
			for _, issue := range issues {
				if issue.Path == tc.path && (tc.message == "" || issue.Message == tc.message) {
					return
				}
			}
			t.Fatalf("expected issue at %s (%q), got %v", tc.path, tc.message, issues)
		})
	}
}

// One problem is one issue: a v1 file is only told it is v1, and a wrongly
// shaped setting is not also reported as missing.
func TestOneIssuePerProblem(t *testing.T) {
	for src, want := range map[string][]string{
		"apiVersion: ownstack.io/v1\nkind: Stack\nspec: {domain: example.com}\n":                           {"apiVersion"},
		strings.Replace(starter, "{host: 203.0.113.10}", "{host: [203.0.113.10]}", 1):                      {"compute.host"},
		strings.Replace(starter, "{acmeEmail: ops@example.com}", "{acmeEmail: {a: b}, acmeStaging: x}", 1): {"ingress.acmeEmail", "ingress.acmeStaging"},
	} {
		if got := paths(parse(t, src).Validate(providers.Catalog())); !slices.Equal(got, want) {
			t.Errorf("issues at %v, want %v, for:\n%s", got, want, src)
		}
	}
}

func TestReservedEnvironments(t *testing.T) {
	// Namespaces Kubernetes and the platform providers use. A provider that
	// installs into a new namespace must be covered here too.
	for _, ns := range []string{"default", "kube-system", "kube-public", "kube-node-lease", "ownstack-system", "traefik", "zot", "harbor", "jenkins"} {
		if !manifest.ReservedEnvironment(ns) {
			t.Errorf("%s is not reserved", ns)
		}
		issues := parse(t, starter+"environments: [prod, "+ns+"]\n").Validate(providers.Catalog())
		if len(issues) != 1 || issues[0].Path != "environments[1]" || !strings.Contains(issues[0].Message, "reserved") {
			t.Errorf("%s: issues = %v", ns, issues)
		}
	}
	for _, env := range []string{"dev", "qa", "prod", "staging", "kube", "ownstack", "jenkins-ci", "my-harbor"} {
		if manifest.ReservedEnvironment(env) {
			t.Errorf("%s is reserved", env)
		}
	}
}

// environments lists <name>[=<branch glob>]: the first is production, and a
// branch deploys into the first environment whose glob matches it.
func TestEnvironments(t *testing.T) {
	m := parse(t, starter)
	want := []manifest.Environment{{Name: "prod", Branch: "main"}, {Name: "dev", Branch: "develop"}, {Name: "qa", Branch: "release/*"}}
	if got := m.EnvironmentList(); !reflect.DeepEqual(got, want) {
		t.Errorf("default environments = %v", got)
	}
	m = parse(t, starter+"environments: [live=trunk, staging, preview=*]\n")
	want = []manifest.Environment{{Name: "live", Branch: "trunk"}, {Name: "staging"}, {Name: "preview", Branch: "*"}}
	if got := m.EnvironmentList(); !reflect.DeepEqual(got, want) {
		t.Errorf("environments = %v", got)
	}

	for name, tc := range map[string]struct {
		environments string
		issues       []string // paths, in order
	}{
		"names only":         {"[prod, dev]", nil},
		"globs":              {"[prod=main, dev=feature/**, qa=release/1.*, x=Team_x-1]", nil},
		"catch-all last":     {"[prod=main, dev=*]", nil},
		"empty branch":       {"[prod=]", []string{"environments[0]"}},
		"two equals":         {"[prod=main=x]", []string{"environments[0]"}},
		"bad name":           {"[Prod=main]", []string{"environments[0]"}},
		"no name":            {"[=main]", []string{"environments[0]"}},
		"bad branch":         {`[prod=main, dev="feature;rm"]`, []string{"environments[1]"}},
		"question mark":      {`[prod=main, "qa=release/?"]`, []string{"environments[1]"}},
		"leading dash":       {"[prod=-x]", []string{"environments[0]"}},
		"duplicate glob":     {"[prod=main, dev=main]", []string{"environments[1]"}},
		"duplicate name":     {"[prod=main, prod=develop]", []string{"environments[1]"}},
		"reserved with glob": {"[jenkins=main]", []string{"environments[0]"}},
		"several problems":   {"[prod=main, Dev=main, qa=a;b]", []string{"environments[1]", "environments[1]", "environments[2]"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := paths(parse(t, starter+"environments: "+tc.environments+"\n").Validate(providers.Catalog()))
			if !slices.Equal(got, tc.issues) {
				t.Fatalf("issues at %v, want %v", got, tc.issues)
			}
		})
	}
}

// Marshal writes only what differs from the defaults, in catalog order, and
// reading it back gives the same stack.
func TestMarshalGolden(t *testing.T) {
	cat := providers.Catalog()
	src, err := os.ReadFile("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, string(src))
	if issues := m.Validate(cat); len(issues) > 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	out, err := m.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/marshal.golden.yaml"
	if *update {
		if err := os.WriteFile(golden, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(want) {
		t.Errorf("Marshal:\n%s\nwant (go test -update):\n%s", out, want)
	}

	back := parse(t, string(out))
	if issues := back.Validate(cat); len(issues) > 0 {
		t.Fatalf("round trip issues: %v", issues)
	}
	for _, a := range cat.Aspects {
		if back.Provider(cat, a.ID) != m.Provider(cat, a.ID) || !reflect.DeepEqual(back.Settings(cat, a.ID), m.Settings(cat, a.ID)) ||
			!reflect.DeepEqual(back.Values(a.ID), m.Values(a.ID)) {
			t.Errorf("%s changed in the round trip", a.ID)
		}
	}
	if back.AdminDomain() != m.AdminDomain() || !reflect.DeepEqual(back.EnvironmentList(), m.EnvironmentList()) {
		t.Errorf("stack settings changed in the round trip")
	}
	again, _ := back.Marshal(cat)
	if string(again) != string(out) {
		t.Errorf("Marshal is not stable:\n%s", again)
	}
}

func TestMarshalDropsDefaults(t *testing.T) {
	cat := providers.Catalog()
	// A secret is an issue, and never written back.
	verbose := strings.Replace(starter, "vcs: {owner: acme}", "vcs: {owner: acme, token: ghp_example}", 1) +
		"adminSubdomain: admin\nenvironments: [prod=main, dev=develop, qa=release/*]\n" +
		"dns: {provider: cloudflare}\nimages: {provider: zot, keepTags: 0}\ncicd: {repos: \"\"}\n"
	out, err := parse(t, verbose).Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != starter {
		t.Errorf("Marshal:\n%s\nwant:\n%s", out, starter)
	}
}

func TestOtherFilesAreNotManifests(t *testing.T) {
	m := parse(t, "version: 1\nname: My stack\naspects:\n  compute:\n    providers: [own-vps]\n")
	issues := m.Validate(providers.Catalog())
	if len(issues) == 0 || issues[0].Path != "apiVersion" {
		t.Fatalf("issues = %v", issues)
	}
	if _, err := manifest.Parse([]byte("environments: prod\n")); err == nil {
		t.Error("a wrongly typed field parsed")
	}
}
