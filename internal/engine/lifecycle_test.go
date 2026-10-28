package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// The environments stage calls kubectl. This stub answers like an empty,
// reachable cluster.
const kubectlStub = `#!/bin/sh
case "$*" in
  "apply "*) cat >/dev/null ;;
  "get namespace "*) ;;
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
	aspect  string
	log     *[]string
	resolve func(rt *stack.Runtime)
	apply   func(rt *stack.Runtime) error

	mu     sync.Mutex
	checks []stack.Check
	// status, when set, answers the n-th status call (from 1) instead.
	status func(n int) []stack.Check
	calls  int
}

func (f *fake) Resolve(_ context.Context, rt *stack.Runtime) error {
	if f.resolve != nil {
		f.resolve(rt)
	}
	return nil
}

func (f *fake) Apply(_ context.Context, rt *stack.Runtime) error {
	*f.log = append(*f.log, "apply "+f.aspect)
	if f.apply != nil {
		return f.apply(rt)
	}
	return nil
}

func (f *fake) Status(context.Context, *stack.Runtime) []stack.Check {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	switch {
	case f.status != nil:
		return f.status(f.calls)
	case f.checks == nil:
		return []stack.Check{{Name: f.aspect, State: stack.OK}}
	}
	return f.checks
}

func (f *fake) statusCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type harness struct {
	e      *Engine
	out    bytes.Buffer
	log    []string
	fakes  map[string]*fake
	stubs  string
	store  *secrets.Store
	secret string // secrets file path
	state  string // $OWNSTACK_DIR
}

// newHarness builds an engine over src whose providers are all fakes. env
// holds the secret environment.
func newHarness(t *testing.T, src string, env map[string]string) *harness {
	t.Helper()
	h := &harness{fakes: map[string]*fake{}, stubs: stubTools(t), state: t.TempDir()}
	t.Setenv("OWNSTACK_DIR", h.state)
	m, err := manifest.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	h.secret = filepath.Join(h.state, "secrets.env")
	h.store = secrets.NewStore(h.secret)
	h.store.Getenv = func(k string) string { return env[k] }
	h.store.Out = io.Discard
	cat := providers.Catalog()
	lookup := func(aspect, id string) (stack.Provider, bool) {
		if m.Provider(cat, aspect) != id {
			return nil, false
		}
		return h.fake(aspect), true
	}
	h.e = NewWith(m, h.store, stack.NewConsole(&h.out), cat, lookup)
	return h
}

func (h *harness) fake(aspect string) *fake {
	if h.fakes[aspect] == nil {
		h.fakes[aspect] = &fake{aspect: aspect, log: &h.log}
	}
	return h.fakes[aspect]
}

// events lists "name id" for every structured event line, and fails the
// test when one is not a JSON object of strings.
func (h *harness) events(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(h.out.String(), "\n") {
		data, ok := strings.CutPrefix(line, stack.EventPrefix)
		if !ok {
			continue
		}
		var fields map[string]string
		if err := json.Unmarshal([]byte(data), &fields); err != nil || fields["event"] == "" {
			t.Fatalf("event line is not JSON: %q (%v)", line, err)
		}
		out = append(out, strings.TrimSpace(fields["event"]+" "+fields["id"]))
	}
	return out
}

// event returns the fields of the first event named name.
func (h *harness) event(name string) map[string]string {
	for _, line := range strings.Split(h.out.String(), "\n") {
		var fields map[string]string
		if data, ok := strings.CutPrefix(line, stack.EventPrefix); ok && json.Unmarshal([]byte(data), &fields) == nil && fields["event"] == name {
			return fields
		}
	}
	return nil
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
	// Secrets and the node's copy of the manifest are saved before the first
	// stage, including a generated secret that no fake Resolve asks for.
	h.fake("compute").apply = func(*stack.Runtime) error {
		data, _ := os.ReadFile(h.secret)
		if !strings.Contains(string(data), "JENKINS_ADMIN_PASSWORD=") || !strings.Contains(string(data), "GITHUB_TOKEN=") {
			return fmt.Errorf("secrets not saved before apply:\n%s", data)
		}
		if data, _ := os.ReadFile(filepath.Join(h.state, "ownstack.yaml")); string(data) != providertest.Manifest {
			return fmt.Errorf("manifest not saved before apply: %q", data)
		}
		return nil
	}
	// The vcs provider gets the file as applied, to mirror it.
	h.fake("vcs").apply = func(rt *stack.Runtime) error {
		if string(rt.ManifestFile()) != providertest.Manifest {
			return fmt.Errorf("manifest file = %q", rt.ManifestFile())
		}
		return nil
	}
	if err := h.e.Apply(context.Background(), fits); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	started := h.event("run_started")
	if started["domain"] != "example.com" || started["stages"] != "compute,dns,ingress,vcs,images,environments,cicd,verify" {
		t.Errorf("run_started = %v", started)
	}
	want := append([]string{"run_started"}, stageEvents("compute", "dns", "ingress", "vcs", "images", "environments", "cicd", "verify")...)
	want = append(want, "run_completed")
	if got := h.events(t); !slices.Equal(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !slices.Equal(h.log, []string{"apply compute", "apply dns", "apply ingress", "apply vcs", "apply images", "apply cicd"}) {
		t.Errorf("applied: %v", h.log)
	}
}

func TestApplyFailureIsRedacted(t *testing.T) {
	env := withoutEnv()
	env["GITHUB_TOKEN"] = `gh"tok\en99`
	h := newHarness(t, providertest.Manifest, env)
	h.fake("images").apply = func(*stack.Runtime) error {
		return fmt.Errorf("push denied for token %s", env["GITHUB_TOKEN"])
	}
	err := h.e.Apply(context.Background(), fits)
	if err == nil {
		t.Fatal("apply succeeded")
	}
	want := append([]string{"run_started"}, stageEvents("compute", "dns", "ingress", "vcs")...)
	want = append(want, "stage_started images", "stage_failed images", "run_failed")
	if got := h.events(t); !slices.Equal(got, want) {
		t.Errorf("events:\n%s", strings.Join(got, "\n"))
	}
	if strings.Contains(h.out.String(), "en99") {
		t.Errorf("secret leaked:\n%s", h.out.String())
	}
	if failed := h.event("stage_failed"); !strings.Contains(failed["error"], "push denied for token [redacted]") {
		t.Errorf("stage_failed = %v", failed)
	}
}

// A missing secret stops apply before anything is saved or changed.
func TestApplyStopsOnMissingSecret(t *testing.T) {
	h := newHarness(t, providertest.Manifest, withoutEnv("CLOUDFLARE_API_TOKEN"))
	err := h.e.Apply(context.Background(), fits)
	if err == nil || !strings.Contains(err.Error(), "CLOUDFLARE_API_TOKEN") {
		t.Fatalf("err = %v", err)
	}
	if len(h.log) > 0 || slices.Contains(h.events(t), "stage_started compute") {
		t.Errorf("stages ran: %v", h.log)
	}
	if _, err := os.Stat(filepath.Join(h.state, "ownstack.yaml")); !os.IsNotExist(err) {
		t.Error("manifest saved by a run that could not start")
	}
}

func TestVerifyFails(t *testing.T) {
	defer func(timeout time.Duration) { VerifyTimeout = timeout }(VerifyTimeout)
	VerifyTimeout = 50 * time.Millisecond

	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("cicd").checks = []stack.Check{{Name: "pods", State: stack.Fail, Detail: "0/1 workloads ready"}}
	err := h.e.Apply(context.Background(), fits)
	if err == nil || !strings.Contains(err.Error(), "pods 0/1 workloads ready") {
		t.Fatalf("err = %v", err)
	}
	if events := h.events(t); !slices.Contains(events, "stage_failed verify") || events[len(events)-1] != "run_failed" {
		t.Errorf("events:\n%s", strings.Join(events, "\n"))
	}
}

// Verify re-polls only the stages that failed, then checks every stage once
// more before it passes.
func TestVerifyRepollsFailingStages(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("cicd").status = func(n int) []stack.Check {
		if n == 1 {
			return []stack.Check{{Name: "pods", State: stack.Fail}}
		}
		return []stack.Check{{Name: "pods", State: stack.OK}}
	}
	if err := h.e.Apply(context.Background(), fits); err != nil {
		t.Fatalf("recovering stack failed verify: %v", err)
	}
	if got := h.fake("cicd").statusCalls(); got != 3 {
		t.Errorf("cicd checked %d times, want 3 (first pass, re-poll, final pass)", got)
	}
	if got := h.fake("dns").statusCalls(); got != 2 {
		t.Errorf("dns checked %d times, want 2 (first pass, final pass)", got)
	}
	if !strings.Contains(h.out.String(), "waiting: health checks (1 failing, first: CI/CD · Jenkins: pods)") {
		t.Errorf("output:\n%s", h.out.String())
	}
}

func TestStatusTakesTheWorstState(t *testing.T) {
	h := newHarness(t, providertest.Manifest, providertest.Env)
	h.fake("dns").checks = []stack.Check{{Name: "a", State: stack.OK}, {Name: "b", State: stack.Warn}}
	h.fake("vcs").checks = []stack.Check{{Name: "c", State: stack.Unknown}}
	st := h.e.Status(context.Background())
	if st.State != stack.Warn || st.Domain != "example.com" {
		t.Errorf("state = %s, domain %s", st.State, st.Domain)
	}
	h.fake("cicd").checks = []stack.Check{{Name: "d", State: stack.Fail}}
	st = h.e.Status(context.Background())
	var ids []string
	states := map[string]string{}
	for _, s := range st.Stages {
		ids = append(ids, s.ID)
		states[s.ID] = s.State
	}
	if st.State != stack.Fail || states["dns"] != stack.Warn || states["vcs"] != stack.Unknown || states["compute"] != stack.OK {
		t.Errorf("status = %s %v", st.State, states)
	}
	if strings.Join(ids, ",") != "compute,dns,ingress,vcs,images,environments,cicd" {
		t.Errorf("stages out of order: %v", ids)
	}
}

func TestPlanListsSecretsInCatalogOrder(t *testing.T) {
	var first []string
	for i := 0; i < 5; i++ {
		h := newHarness(t, providertest.Manifest, nil)
		h.store.ReadOnly = true
		plan := h.e.Plan(context.Background())
		if len(plan.Missing) != 2 || len(plan.Generated) != 2 {
			t.Fatalf("missing %v, generated %v", plan.Missing, plan.Generated)
		}
		got := append(plan.Missing, plan.Generated...)
		order := []string{"CLOUDFLARE_API_TOKEN", "GITHUB_TOKEN", "HARBOR_ADMIN_PASSWORD", "JENKINS_ADMIN_PASSWORD"}
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
		if _, err := os.Stat(h.secret); !os.IsNotExist(err) {
			t.Fatal("plan wrote the secrets file")
		}
	}
}
