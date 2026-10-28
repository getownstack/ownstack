package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// The environments stage and orphan detection call kubectl and helm. These
// stubs answer like an empty, reachable cluster; a pull secret is returned
// when the test writes one to $OWNSTACK_STUB_DIR/secret.json.
const kubectlStub = `#!/bin/sh
case "$*" in
  "apply -f -") cat >/dev/null ;;
  "get --raw /readyz"|"get namespace "*) ;;
  "get secret "*) [ -f "$OWNSTACK_STUB_DIR/secret.json" ] && cat "$OWNSTACK_STUB_DIR/secret.json" || exit 1 ;;
  "get ingressroutes.traefik.io "*|"get ingresses.networking.k8s.io "*|"get httproutes.gateway.networking.k8s.io "*) echo '{"items":[]}' ;;
  *) exit 1 ;;
esac
`

func stubTools(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{"kubectl": kubectlStub, "helm": "#!/bin/sh\necho '[]'\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OWNSTACK_STUB_DIR", dir)
	return dir
}

// fake is a recording provider.
type fake struct {
	aspect   string
	log      *[]string
	resolve  func(rt *stack.Runtime)
	apply    func(rt *stack.Runtime) error
	checks   []stack.Check
	booted   bool
	bootable bool
}

func (f *fake) Resolve(_ context.Context, rt *stack.Runtime) error {
	if f.resolve != nil {
		f.resolve(rt)
	}
	return nil
}

func (f *fake) Plan(context.Context, *stack.Runtime) []stack.Step { return nil }

func (f *fake) Apply(_ context.Context, rt *stack.Runtime) error {
	*f.log = append(*f.log, "apply "+f.aspect)
	if f.apply != nil {
		return f.apply(rt)
	}
	return nil
}

func (f *fake) Status(context.Context, *stack.Runtime) []stack.Check {
	if f.checks == nil {
		return []stack.Check{{Name: f.aspect, State: stack.OK}}
	}
	return f.checks
}

// removable is an installed provider the manifest no longer selects.
type removable struct{ *fake }

func (removable) Installed(context.Context, *stack.Runtime) bool { return true }

func (r removable) Remove(context.Context, *stack.Runtime) error {
	*r.log = append(*r.log, "remove "+r.aspect)
	return nil
}

type bootstrapper struct{ *fake }

func (b bootstrapper) Bootstrap(context.Context, *stack.Runtime, []byte, string) error {
	b.booted = true
	return nil
}

type harness struct {
	e      *Engine
	out    bytes.Buffer
	log    []string
	fakes  map[string]*fake
	extra  map[string]stack.Provider // installed providers outside the manifest
	stubs  string
	store  *secrets.Store
	secret string // secrets file path
}

// newHarness builds an engine over src whose providers are all fakes. env
// holds the secret environment.
func newHarness(t *testing.T, src string, env map[string]string) *harness {
	t.Helper()
	h := &harness{fakes: map[string]*fake{}, extra: map[string]stack.Provider{}, stubs: stubTools(t)}
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	h.secret = filepath.Join(t.TempDir(), "secrets.env")
	h.store = secrets.NewStore(h.secret)
	h.store.Getenv = func(k string) string { return env[k] }
	h.store.Out = io.Discard
	lookup := func(aspect, id string) (stack.Provider, bool) {
		if c, ok := m.Component(aspect); ok && c.Provider == id {
			f := h.fake(aspect)
			if f.bootable {
				return bootstrapper{f}, true
			}
			return f, true
		}
		p, ok := h.extra[catalog.Key(aspect, id)]
		return p, ok
	}
	h.e = NewWith(m, h.store, stack.NewConsole(&h.out), providers.Catalog(), lookup)
	return h
}

func (h *harness) fake(aspect string) *fake {
	if h.fakes[aspect] == nil {
		h.fakes[aspect] = &fake{aspect: aspect, log: &h.log}
	}
	return h.fakes[aspect]
}

var eventField = regexp.MustCompile(`id="([^"]*)"`)

// events lists "name id" for every structured event line.
func (h *harness) events() []string {
	var out []string
	for _, line := range strings.Split(h.out.String(), "\n") {
		name, rest, ok := strings.Cut(strings.TrimPrefix(line, "ownstack.event="), " ")
		if !strings.HasPrefix(line, "ownstack.event=") || !ok {
			continue
		}
		if m := eventField.FindStringSubmatch(rest); m != nil {
			name += " " + m[1]
		}
		out = append(out, name)
	}
	return out
}

func stageEvents(ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, "stage_started "+id, "stage_completed "+id)
	}
	return out
}

func withoutEnv(keys ...string) map[string]string {
	env := map[string]string{}
	for k, v := range providertest.Env {
		if !slices.Contains(keys, k) {
			env[k] = v
		}
	}
	return env
}

var fits = ApplyOptions{MemTotalMiB: 16384}

func TestApplyLifecycle(t *testing.T) {
	h := newHarness(t, providertest.Manifest, withoutEnv("JENKINS_ADMIN_PASSWORD"))
	// Secrets are saved before the first stage, including a generated one
	// that no fake Resolve asks for.
	h.fake("compute").apply = func(*stack.Runtime) error {
		data, _ := os.ReadFile(h.secret)
		if !strings.Contains(string(data), "JENKINS_ADMIN_PASSWORD=") || !strings.Contains(string(data), "GITHUB_TOKEN=") {
			return fmt.Errorf("secrets not saved before apply:\n%s", data)
		}
		return nil
	}
	if err := h.e.Apply(context.Background(), fits); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if !strings.Contains(h.out.String(), `stages="compute,dns,ingress,vcs,images,environments,alerts,cicd"`) {
		t.Errorf("run_started does not list the stages:\n%s", h.out.String())
	}
	want := append([]string{"run_started"}, stageEvents("compute", "dns", "ingress", "vcs", "images", "environments", "alerts", "cicd", "verify")...)
	want = append(want, "run_completed")
	if got := h.events(); !slices.Equal(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestApplyOnly(t *testing.T) {
	for _, tc := range []struct {
		only   []string
		stages []string
	}{
		{[]string{"cicd"}, []string{"cicd"}},
		// The environments stage holds the registry credential.
		{[]string{"images"}, []string{"images", "environments"}},
		{[]string{"environments", "alerts"}, []string{"environments", "alerts"}},
	} {
		h := newHarness(t, providertest.Manifest, providertest.Env)
		opts := fits
		opts.Only = tc.only
		if err := h.e.Apply(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		var applied []string
		for _, ev := range h.events() {
			if id, ok := strings.CutPrefix(ev, "stage_completed "); ok && id != "verify" {
				applied = append(applied, id)
			}
		}
		if !slices.Equal(applied, tc.stages) {
			t.Errorf("--only %v applied %v, want %v", tc.only, applied, tc.stages)
		}
		if !strings.Contains(h.out.String(), `stages="`+strings.Join(tc.stages, ",")+`"`) {
			t.Errorf("--only %v: run_started does not list %v", tc.only, tc.stages)
		}
	}
}

func TestApplyRejectsOnlyThatAppliesNothing(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	for only, want := range map[string]string{"metrics": "metrics is not in ownstack.yaml", "registry": `unknown aspect "registry"`} {
		err := h.e.Apply(context.Background(), ApplyOptions{Only: []string{only}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("--only %s: err = %v", only, err)
		}
	}
}

func TestApplyFailureIsRedactedAndPrunesNothing(t *testing.T) {
	env := withoutEnv()
	env["GITHUB_TOKEN"] = `gh"tok\en99`
	h := newHarness(t, providertest.Manifest, env)
	zot := &fake{aspect: "images", log: &h.log}
	h.extra[catalog.Key("images", "zot")] = removable{zot}
	h.fake("images").apply = func(*stack.Runtime) error {
		return fmt.Errorf("push denied for token %s", env["GITHUB_TOKEN"])
	}
	opts := fits
	opts.Prune = true
	err := h.e.Apply(context.Background(), opts)
	if err == nil {
		t.Fatal("apply succeeded")
	}
	want := append([]string{"run_started"}, stageEvents("compute", "dns", "ingress", "vcs")...)
	want = append(want, "stage_started images", "stage_failed images", "run_failed")
	if got := h.events(); !slices.Equal(got, want) {
		t.Errorf("events:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(h.out.String(), "en99") {
		t.Errorf("secret leaked:\n%s", h.out.String())
	}
	if slices.Contains(h.log, "remove images") || !strings.Contains(h.out.String(), "nothing was removed") {
		t.Errorf("pruned after a failed apply: %v", h.log)
	}
}

func TestPruneRunsAfterTheNewStack(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.extra[catalog.Key("images", "zot")] = removable{&fake{aspect: "images", log: &h.log}}
	opts := fits
	opts.Prune = true
	if err := h.e.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	events := h.events()
	if i := slices.Index(events, "stage_started prune-zot"); i < 0 || events[i-1] != "stage_completed cicd" || events[i+2] != "stage_started verify" {
		t.Errorf("prune is not between the stages and verify:\n%s", strings.Join(events, "\n"))
	}
	if h.log[len(h.log)-1] != "remove images" {
		t.Errorf("log = %v", h.log)
	}

	// --only limits pruning to the selected aspects.
	h = newHarness(t, providertest.Manifest, providertest.Env)
	h.extra[catalog.Key("images", "zot")] = removable{&fake{aspect: "images", log: &h.log}}
	opts.Only = []string{"cicd"}
	if err := h.e.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(h.log, "remove images") {
		t.Errorf("--only cicd pruned the registry: %v", h.log)
	}
}

func TestVerify(t *testing.T) {
	defer func(timeout, interval time.Duration) { VerifyTimeout, verifyInterval = timeout, interval }(VerifyTimeout, verifyInterval)
	VerifyTimeout, verifyInterval = 50*time.Millisecond, 5*time.Millisecond

	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("cicd").checks = []stack.Check{{Name: "pods", State: stack.Fail, Detail: "0/1 workloads ready"}}
	err := h.e.Apply(context.Background(), fits)
	if err == nil || !strings.Contains(err.Error(), "pods 0/1 workloads ready") {
		t.Fatalf("err = %v", err)
	}
	if events := h.events(); !slices.Contains(events, "stage_failed verify") || events[len(events)-1] != "run_failed" {
		t.Errorf("events:\n%s", strings.Join(events, "\n"))
	}

	// A check that recovers within the timeout passes.
	VerifyTimeout = time.Minute
	h = newHarness(t, providertest.Manifest, providertest.Env)
	calls := 0
	h.fake("cicd").checks = []stack.Check{{Name: "pods", State: stack.Fail}}
	h.fake("cicd").apply = func(*stack.Runtime) error { return nil }
	h.e.lookup = recoverAfter(h.e.lookup, &calls)
	if err := h.e.Apply(context.Background(), fits); err != nil {
		t.Fatalf("recovering stack failed verify: %v", err)
	}
}

// recoverAfter wraps cicd's fake so its status turns healthy on the second poll.
func recoverAfter(lookup Lookup, calls *int) Lookup {
	return func(aspect, id string) (stack.Provider, bool) {
		p, ok := lookup(aspect, id)
		if aspect != "cicd" || !ok {
			return p, ok
		}
		return recovering{p.(*fake), calls}, true
	}
}

type recovering struct {
	*fake
	calls *int
}

func (r recovering) Status(context.Context, *stack.Runtime) []stack.Check {
	*r.calls++
	if *r.calls > 1 {
		return []stack.Check{{Name: "pods", State: stack.OK}}
	}
	return []stack.Check{{Name: "pods", State: stack.Fail}}
}

func TestStatusTakesTheWorstState(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("dns").checks = []stack.Check{{Name: "a", State: stack.OK}, {Name: "b", State: stack.Warn}}
	h.fake("alerts").checks = []stack.Check{{Name: "c", State: stack.Unknown}}
	st := h.e.Status(context.Background())
	if st.State != stack.Warn {
		t.Errorf("state = %s", st.State)
	}
	h.fake("cicd").checks = []stack.Check{{Name: "d", State: stack.Fail}}
	st = h.e.Status(context.Background())
	states := map[string]string{}
	for _, s := range st.Stages {
		states[s.ID] = s.State
	}
	if st.State != stack.Fail || states["dns"] != stack.Warn || states["alerts"] != stack.Unknown || states["compute"] != stack.OK {
		t.Errorf("status = %s %v", st.State, states)
	}
}

func TestPlanListsSecretsInCatalogOrder(t *testing.T) {
	var first []string
	for i := 0; i < 5; i++ {
		h := newHarness(t, providertest.Manifest, nil)
		h.store.Generate, h.store.Save = false, false
		plan := h.e.Plan(context.Background())
		if len(plan.Missing) != 3 || len(plan.Generated) != 2 {
			t.Fatalf("missing %v, generated %v", plan.Missing, plan.Generated)
		}
		got := append(plan.Missing, plan.Generated...)
		order := []string{"CLOUDFLARE_API_TOKEN", "GITHUB_TOKEN", "SLACK_BOT_TOKEN", "HARBOR_ADMIN_PASSWORD", "JENKINS_ADMIN_PASSWORD"}
		for j, name := range order {
			if !strings.Contains(got[j], name) {
				t.Fatalf("secret %d = %q, want %s", j, got[j], name)
			}
		}
		if first == nil {
			first = got
		} else if !slices.Equal(first, got) {
			t.Fatalf("order changed between runs: %v vs %v", first, got)
		}
	}
}

func TestBootstrapChecksEverySecretBeforeTheRepo(t *testing.T) {
	h := newHarness(t, providertest.Manifest, withoutEnv("CLOUDFLARE_API_TOKEN"))
	h.fake("vcs").bootable = true
	err := h.e.Bootstrap(context.Background(), []byte("manifest"), t.TempDir(), fits)
	if err == nil || !strings.Contains(err.Error(), "CLOUDFLARE_API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if h.fake("vcs").booted || strings.Contains(h.out.String(), "stage_started") {
		t.Error("the repo was bootstrapped before the missing secret was found")
	}

	h = newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("vcs").bootable = true
	if err := h.e.Bootstrap(context.Background(), []byte("manifest"), t.TempDir(), ApplyOptions{MemTotalMiB: 1024}); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("too small a server was not refused before the repo: %v", err)
	}
	if err := h.e.Bootstrap(context.Background(), []byte("manifest"), t.TempDir(), fits); err != nil {
		t.Fatal(err)
	}
	if !h.fake("vcs").booted || !slices.Equal(h.events(), stageEvents("repo")) {
		t.Errorf("booted %v, events %v", h.fake("vcs").booted, h.events())
	}
}

// pullSecret writes the stubbed cluster's pull secret.
func pullSecret(t *testing.T, h *harness, server, user, pass string) {
	t.Helper()
	config, _ := stack.DockerConfigJSON(server, user, pass)
	data := `{"data":{".dockerconfigjson":"` + base64.StdEncoding.EncodeToString([]byte(config)) + `"}}`
	if err := os.WriteFile(filepath.Join(h.stubs, "secret.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func registry(user, pass string) func(rt *stack.Runtime) {
	return func(rt *stack.Runtime) {
		rt.Out.Images = &stack.ImagesOutput{Host: "harbor.example.com", Project: "product", PushUsername: user, PushPassword: pass, PullSecretName: "harbor-robot"}
	}
}

func TestCredentialChangeWarnsUnappliedConsumers(t *testing.T) {
	const warning = "registry credential changed; run `ownstackctl apply --only cicd`"
	// The stubbed cluster never takes the new pull secret, so verify is skipped.
	opts := ApplyOptions{MemTotalMiB: fits.MemTotalMiB, SkipVerify: true}
	only := func(ids ...string) ApplyOptions { o := opts; o.Only = ids; return o }

	// Harbor recreated its robot during the images stage.
	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("images").resolve = registry("robot$ci", "old-robot-pw")
	h.fake("images").apply = func(rt *stack.Runtime) error {
		rt.Out.Images.PushPassword = "new-robot-pw"
		return nil
	}
	if err := h.e.Apply(context.Background(), only("images")); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, warning) || strings.Contains(out, "robot-pw") {
		t.Errorf("output:\n%s", out)
	}

	// A changed password reached the environments, not yet CI.
	h = newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("images").resolve = registry("ci", "new-pw")
	pullSecret(t, h, "harbor.example.com", "ci", "old-pw")
	if err := h.e.Apply(context.Background(), only("environments")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), warning) {
		t.Errorf("no warning for a stale pull secret:\n%s", h.out.String())
	}

	// A full apply updates every consumer: nothing to warn about.
	h = newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("images").resolve = registry("ci", "new-pw")
	pullSecret(t, h, "harbor.example.com", "ci", "old-pw")
	if err := h.e.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.out.String(), "credential changed") {
		t.Errorf("warned on a full apply:\n%s", h.out.String())
	}
}

func TestEnvironmentsStatusDetectsStalePullSecret(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	registry("robot$ci", "current-pw")(h.e.RT)
	env := environments{rt: h.e.RT}
	pullSecret(t, h, "harbor.example.com", "robot$ci", "rotated-pw")
	checks := env.status(context.Background())
	if len(checks) == 0 || checks[0].State != stack.Fail || checks[0].Detail != "stale pull secret (different password); rerun apply --only environments" {
		t.Fatalf("checks = %+v", checks)
	}
	for _, c := range checks {
		if strings.Contains(c.Detail, "-pw") {
			t.Errorf("detail contains a password: %q", c.Detail)
		}
	}
	pullSecret(t, h, "harbor.example.com", "robot$ci", "current-pw")
	if checks := env.status(context.Background()); checks[0].State != stack.OK {
		t.Errorf("current pull secret reported %+v", checks[0])
	}
}

func TestParseRoutes(t *testing.T) {
	ingressRoutes := `{"items":[{"metadata":{"name":"shop"},"spec":{"routes":[
		{"match":"Host(` + "`shop.example.com`" + `) || Host(` + "`www.shop.example.com`" + `)"}]}}]}`
	ingresses := `{"items":[{"metadata":{"name":"ownstack-web"},"spec":{"rules":[
		{"host":"app.ownstack.io","http":{}},{"host":"*.ownstack.io"},{"http":{}},{"host":"shop.example.com"}]}}]}`
	httpRoutes := `{"items":[{"metadata":{"name":"docs"},"spec":{"hostnames":["docs.example.com"],"rules":[{"matches":[]}]}}]}`

	var routes []route
	for _, list := range []string{ingressRoutes, ingresses, httpRoutes, "not json"} {
		routes = append(routes, parseRoutes(list)...)
	}
	want := []route{
		{"shop", "shop.example.com"}, {"shop", "www.shop.example.com"},
		{"ownstack-web", "app.ownstack.io"}, {"docs", "docs.example.com"},
	}
	if got := uniqueHosts(routes); !slices.Equal(got, want) {
		t.Errorf("routes = %v", got)
	}
}

func TestMemoryFits(t *testing.T) {
	for _, tc := range []struct {
		total, need int
		fits        bool
	}{
		{3800, 3928, true}, // a 4 GB server and the Standard preset
		{3143, 3928, true}, // 80% of the estimate
		{3100, 3928, false},
		{1642, 1028, true},
		{0, 1, false},
	} {
		if got := MemoryFits(tc.total, tc.need); got != tc.fits {
			t.Errorf("MemoryFits(%d, %d) = %v", tc.total, tc.need, got)
		}
	}
	data, err := os.ReadFile("../../examples/standard.yaml")
	if err != nil {
		t.Fatal(err)
	}
	standard := newEngine(t, string(data))
	if err := standard.memoryPreflight(ApplyOptions{MemTotalMiB: 3800}); err != nil {
		t.Errorf("Standard refused on a 4 GB server: %v", err)
	}
}
