// Package engine checks, applies, and inspects a stack by running each
// aspect's provider in catalog order and passing contract outputs forward.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// EnvironmentsStage prepares the deployment namespaces. It is not an aspect;
// it runs once the registry has delivered its pull credential.
const EnvironmentsStage = "environments"

// VerifyStage is apply's final health check.
const VerifyStage = "verify"

type Engine struct {
	Manifest *manifest.Manifest
	Catalog  *catalog.Catalog
	RT       *stack.Runtime

	lookup Lookup
}

// Lookup finds a provider's implementation.
type Lookup func(aspect, id string) (stack.Provider, bool)

// New builds an engine over the registered providers.
func New(m *manifest.Manifest, store *secrets.Store, console *stack.Console) *Engine {
	return NewWith(m, store, console, providers.Catalog(), providers.Lookup)
}

// NewWith builds an engine over a given catalog and set of providers (tests
// pass fakes).
func NewWith(m *manifest.Manifest, store *secrets.Store, console *stack.Console, cat *catalog.Catalog, lookup Lookup) *Engine {
	store.OnValue(console.Redact)
	return &Engine{Manifest: m, Catalog: cat, RT: stack.NewRuntime(m, cat, store, console), lookup: lookup}
}

type unit struct {
	aspect catalog.Aspect
	spec   catalog.Provider
	impl   stack.Provider
	rt     *stack.Runtime
}

// units are the stack's providers in catalog order; an unknown provider
// (an invalid manifest) is left out.
func (e *Engine) units() []unit {
	var units []unit
	for _, aspect := range e.Catalog.Aspects {
		id := e.Manifest.Provider(e.Catalog, aspect.ID)
		spec, ok := e.Catalog.Provider(aspect.ID, id)
		impl, found := e.lookup(aspect.ID, id)
		if !ok || !found {
			continue
		}
		units = append(units, unit{aspect: aspect, spec: spec, impl: impl, rt: e.RT.For(aspect.ID)})
	}
	return units
}

// Resources adds up the stack's providers' estimates.
func (e *Engine) Resources() catalog.Resources {
	var r catalog.Resources
	for _, u := range e.units() {
		r.MemoryMiB += u.spec.Resources.MemoryMiB
		r.DiskGiB += u.spec.Resources.DiskGiB
	}
	return r
}

// Secret is a secret the stack's providers need: what the designer and plan
// list.
type Secret struct {
	Aspect   string `json:"aspect"`
	Key      string `json:"key"`
	Env      string `json:"env"`
	Label    string `json:"label"`
	HelpURL  string `json:"helpURL"`
	Generate bool   `json:"generate"`
}

// Secrets lists the stack's secrets in catalog order.
func (e *Engine) Secrets() []Secret {
	out := []Secret{}
	for _, u := range e.units() {
		for _, param := range u.spec.Params {
			if param.IsSecret() {
				out = append(out, Secret{Aspect: u.aspect.ID, Key: param.Key, Env: param.Env, Label: param.Label, HelpURL: param.HelpURL, Generate: param.Generate})
			}
		}
	}
	return out
}

// Validate returns manifest issues as a single error.
func (e *Engine) Validate() error {
	issues := e.Manifest.Validate(e.Catalog)
	if len(issues) == 0 {
		return nil
	}
	lines := make([]string, len(issues))
	for i, issue := range issues {
		lines[i] = "  " + issue.String()
	}
	return fmt.Errorf("ownstack.yaml is not valid:\n%s", strings.Join(lines, "\n"))
}

// resolveIssue is a problem resolving one provider.
type resolveIssue struct {
	aspect string
	err    error
}

// resolve fills every provider's static outputs, in catalog order. It first
// resolves every secret the catalog declares for the provider, so all missing
// secrets are reported together and generated ones are saved by the next
// Flush even when Resolve does not ask for them. Outputs are still set as far
// as they could be.
func (e *Engine) resolve(ctx context.Context) []resolveIssue {
	var issues []resolveIssue
	for _, u := range e.units() {
		reported := map[string]bool{}
		for _, param := range u.spec.Params {
			if !param.IsSecret() || !(param.Required || param.Generate) {
				continue
			}
			if _, err := u.rt.Secret(param.Key); err != nil {
				issues = append(issues, resolveIssue{u.aspect.ID, err})
				reported[err.Error()] = true
			}
		}
		// Resolve usually fails on the same secret; report it once.
		if err := u.impl.Resolve(ctx, u.rt); err != nil && !reported[err.Error()] {
			issues = append(issues, resolveIssue{u.aspect.ID, err})
		}
	}
	return issues
}

// ── Apply ────────────────────────────────────────────────────────────────────

type ApplyOptions struct {
	// AllowLowMemory skips the memory preflight.
	AllowLowMemory bool
	// MemTotalMiB overrides the detected server memory (tests).
	MemTotalMiB int
}

// Apply converges the server to the manifest: preflight, secrets, every
// stage in order, then verify.
func (e *Engine) Apply(ctx context.Context, opts ApplyOptions) (err error) {
	console := e.RT.Console
	if err := e.Validate(); err != nil {
		return err
	}

	start := time.Now()
	stages := e.stages()
	ids := make([]string, 0, len(stages)+1)
	for _, s := range stages {
		ids = append(ids, s.id)
	}
	console.Event("run_started", map[string]string{
		"domain": e.Manifest.Domain,
		"stages": strings.Join(append(ids, VerifyStage), ","),
	})
	defer func() {
		fields := map[string]string{"duration": time.Since(start).Round(time.Second).String()}
		if err != nil {
			fields["error"] = err.Error()
			console.Event("run_failed", fields)
		} else {
			console.Event("run_completed", fields)
		}
	}()

	if err := e.prepare(ctx, opts); err != nil {
		return err
	}
	for _, stage := range stages {
		console.Event("stage_started", map[string]string{"id": stage.id, "title": stage.title, "provider": stage.provider})
		stageStart := time.Now()
		if err := stage.apply(ctx); err != nil {
			console.Event("stage_failed", map[string]string{"id": stage.id, "error": err.Error()})
			return fmt.Errorf("%s: %w", stage.title, err)
		}
		console.Event("stage_completed", map[string]string{"id": stage.id, "duration": time.Since(stageStart).Round(time.Second).String()})
	}
	return e.verify(ctx, stages)
}

// prepare runs what apply needs before changing anything: the memory
// preflight, every provider's outputs and secrets, which it saves, and the
// node's copy of the manifest.
func (e *Engine) prepare(ctx context.Context, opts ApplyOptions) error {
	if err := e.memoryPreflight(opts); err != nil {
		return err
	}
	if issues := e.resolve(ctx); len(issues) > 0 {
		return resolveError(issues)
	}
	// Persist generated and supplied secrets before anything uses them, so a
	// failed run never leaves an installed service with a forgotten password.
	if err := e.RT.Secrets.Flush(); err != nil {
		return fmt.Errorf("save secrets: %w", err)
	}
	// The node's copy is what later commands read by default (status, the
	// health timer, the next apply).
	if err := writeFile(stack.StateFile(manifest.FileName), e.Manifest.Source); err != nil {
		return fmt.Errorf("save the manifest: %w", err)
	}
	return nil
}

// writeFile replaces path with data atomically, unless it already holds it.
func writeFile(path string, data []byte) error {
	if current, err := os.ReadFile(path); err == nil && string(current) == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// VerifyTimeout bounds the final health check.
var VerifyTimeout = 5 * time.Minute

// verify waits until every applied stage reports no failing checks. A stack
// whose services fall over once everything runs together (for example on a
// server that is too small) fails here instead of being reported as ready.
// Between polls it re-checks only the stages that failed, then makes one
// full pass before it reports success.
func (e *Engine) verify(ctx context.Context, applied []stage) error {
	console := e.RT.Console
	console.Event("stage_started", map[string]string{"id": VerifyStage, "title": "Health check", "provider": "ownstack"})
	start := time.Now()
	pending, full := applied, true
	var failing []string
	err := stack.WaitFor(ctx, e.RT, "health checks", VerifyTimeout, func(ctx context.Context) (bool, string) {
		var still []stage
		still, failing = failingStages(ctx, pending)
		if len(still) == 0 && !full {
			still, failing = failingStages(ctx, applied)
		}
		if len(still) == 0 {
			return true, ""
		}
		pending, full = still, false
		return false, fmt.Sprintf("%d failing, first: %s", len(failing), failing[0])
	})
	if err == nil {
		console.Printf("All %d stages report healthy", len(applied))
		console.Event("stage_completed", map[string]string{"id": VerifyStage, "duration": time.Since(start).Round(time.Second).String()})
		return nil
	}
	if len(failing) > 0 && ctx.Err() == nil {
		err = errors.New("applied, but not healthy after " + VerifyTimeout.String() + ":\n  " + strings.Join(failing, "\n  "))
	}
	console.Event("stage_failed", map[string]string{"id": VerifyStage, "error": strings.Join(failing, "; ")})
	return err
}

// failingStages checks stages concurrently and returns those with failing
// checks, and the failing checks.
func failingStages(ctx context.Context, stages []stage) ([]stage, []string) {
	results := checkAll(ctx, stages)
	var still []stage
	var failing []string
	for i, s := range stages {
		failed := false
		for _, c := range results[i] {
			if c.State == stack.Fail {
				failing = append(failing, strings.TrimSpace(fmt.Sprintf("%s: %s %s", s.title, c.Name, c.Detail)))
				failed = true
			}
		}
		if failed {
			still = append(still, s)
		}
	}
	return still, failing
}

// checkAll runs every stage's status checks at once; they only read.
func checkAll(ctx context.Context, stages []stage) [][]stack.Check {
	results := make([][]stack.Check, len(stages))
	var wg sync.WaitGroup
	for i, s := range stages {
		wg.Add(1)
		go func(i int, s stage) {
			defer wg.Done()
			results[i] = s.status(ctx)
		}(i, s)
	}
	wg.Wait()
	return results
}

// memoryPreflight refuses stacks that clearly do not fit the server. The
// estimate comes from the catalog's per-provider resources.
func (e *Engine) memoryPreflight(opts ApplyOptions) error {
	if opts.AllowLowMemory {
		return nil
	}
	total := opts.MemTotalMiB
	if total == 0 {
		total = memTotalMiB()
	}
	if total == 0 {
		return nil
	}
	need := e.Resources().MemoryMiB
	if catalog.MemoryFits(total, need) {
		return nil
	}
	var heavy []string
	for _, u := range e.units() {
		if u.spec.Resources.MemoryMiB >= 1000 {
			heavy = append(heavy, fmt.Sprintf("%s (~%.1f GB)", u.spec.Name, float64(u.spec.Resources.MemoryMiB)/1024))
		}
	}
	msg := fmt.Sprintf("this stack needs about %.1f GB of memory but the server has %.1f GB", float64(need)/1024, float64(total)/1024)
	if len(heavy) > 0 {
		msg += "; the heavy parts are " + strings.Join(heavy, ", ")
	}
	return errors.New(msg + ". Use lighter providers (the Starter preset: Zot and GitHub Actions fit 2 GB), a bigger server, or --allow-low-memory to try anyway")
}

func memTotalMiB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var kib int
	fmt.Sscanf(strings.SplitN(string(data), "\n", 2)[0], "MemTotal: %d kB", &kib)
	return kib / 1024
}

type stage struct {
	id, title, provider string
	apply               func(context.Context) error
	status              func(context.Context) []stack.Check
}

// stages lists providers in catalog order with the environments stage right
// after the registry, which hands it the pull credential.
func (e *Engine) stages() []stage {
	var out []stage
	for _, u := range e.units() {
		u := u
		out = append(out, stage{
			id: u.aspect.ID, title: u.aspect.Name + " · " + u.spec.Name, provider: u.spec.ID,
			apply:  func(ctx context.Context) error { return u.impl.Apply(ctx, u.rt) },
			status: func(ctx context.Context) []stack.Check { return u.impl.Status(ctx, u.rt) },
		})
		if u.aspect.ID == "images" {
			env := environments{rt: e.RT}
			var names []string
			for _, environment := range e.RT.Environments() {
				names = append(names, environment.Name)
			}
			out = append(out, stage{
				id: EnvironmentsStage, title: "Environments · " + strings.Join(names, ", "), provider: "ownstack",
				apply: env.apply, status: env.status,
			})
		}
	}
	return out
}

func resolveError(issues []resolveIssue) error {
	var missing, other []string
	for _, issue := range issues {
		if errors.Is(issue.err, secrets.ErrMissing) {
			missing = append(missing, "  "+issue.err.Error())
		} else {
			other = append(other, fmt.Sprintf("  %s: %v", issue.aspect, issue.err))
		}
	}
	msg := ""
	if len(missing) > 0 {
		msg += "missing secrets (export them, add them to " + stack.StateFile("secrets.env") + ", or run in a terminal to be prompted):\n" + strings.Join(missing, "\n")
	}
	if len(other) > 0 {
		if msg != "" {
			msg += "\n"
		}
		msg += "could not resolve:\n" + strings.Join(other, "\n")
	}
	return errors.New(msg)
}

// ── Plan ─────────────────────────────────────────────────────────────────────

// Plan is apply's preflight: whether the manifest is valid, whether this
// server can take the stack, which secrets apply would ask for or generate,
// and the stages it would run. It changes nothing.
type Plan struct {
	Domain    string            `json:"domain"`
	Valid     bool              `json:"valid"`
	Issues    []manifest.Issue  `json:"issues,omitempty"`
	Server    []stack.Check     `json:"server"`
	Missing   []string          `json:"missingSecrets,omitempty"`
	Generated []string          `json:"generatedSecrets,omitempty"`
	Resources catalog.Resources `json:"resources"`
	// MemTotalMiB is this server's memory, when known.
	MemTotalMiB int         `json:"memTotalMiB,omitempty"`
	Stages      []StagePlan `json:"stages"`
}

type StagePlan struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
}

// Problems counts what would stop apply: manifest issues and failed server
// checks. Missing secrets are not problems; apply asks for them.
func (p Plan) Problems() int {
	n := len(p.Issues)
	for _, c := range p.Server {
		if c.State == stack.Fail {
			n++
		}
	}
	return n
}

// Plan runs the preflight. The engine's secrets store should be read-only.
func (e *Engine) Plan(ctx context.Context) Plan {
	plan := Plan{Domain: e.Manifest.Domain, Resources: e.Resources()}
	plan.Issues = e.Manifest.Validate(e.Catalog)
	plan.Valid = len(plan.Issues) == 0
	plan.MemTotalMiB = memTotalMiB()
	plan.Server = serverChecks(plan.MemTotalMiB, plan.Resources.MemoryMiB)
	if !plan.Valid {
		return plan
	}
	for _, issue := range e.resolve(ctx) {
		switch err := issue.err; {
		case errors.Is(err, secrets.ErrMissing):
			plan.Missing = append(plan.Missing, err.Error())
		case errors.Is(err, secrets.ErrWouldGenerate):
			plan.Generated = append(plan.Generated, err.Error())
		default:
			plan.Issues = append(plan.Issues, manifest.Issue{Path: issue.aspect, Message: err.Error()})
		}
	}
	for _, s := range e.stages() {
		plan.Stages = append(plan.Stages, StagePlan{ID: s.id, Title: s.title, Provider: s.provider})
	}
	return plan
}

// serverChecks are apply's prerequisites on this server. memTotal is 0 when
// unknown.
func serverChecks(memTotal, need int) []stack.Check {
	check := func(ok bool, name, detail string) stack.Check {
		state := stack.OK
		if !ok {
			state = stack.Fail
		}
		return stack.Check{Name: name, State: state, Detail: detail}
	}
	osName := "unknown"
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				osName = strings.Trim(v, `"`)
			}
		}
	}
	checks := []stack.Check{
		check(os.Geteuid() == 0, "root", "apply installs system packages and must run as root"),
		check(strings.Contains(osName, "Ubuntu"), "os", osName+" (Ubuntu expected)"),
		check(runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64", "arch", runtime.GOARCH),
	}
	for _, tool := range []string{"bash", "curl"} {
		checks = append(checks, check((&stack.Exec{}).Has(tool), tool, "required"))
	}
	if memTotal > 0 {
		checks = append(checks, check(catalog.MemoryFits(memTotal, need), "memory",
			fmt.Sprintf("%d MiB on this server, the stack needs about %d MiB", memTotal, need)))
	}
	return checks
}

// ── Status ───────────────────────────────────────────────────────────────────

type Status struct {
	Domain    string        `json:"domain"`
	CheckedAt time.Time     `json:"checkedAt"`
	State     string        `json:"state"`
	Stages    []StageStatus `json:"stages"`
}

type StageStatus struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Provider string        `json:"provider"`
	State    string        `json:"state"`
	Checks   []stack.Check `json:"checks"`
}

// Status checks every stage at once. The engine's secrets store should be
// read-only.
func (e *Engine) Status(ctx context.Context) Status {
	st := Status{Domain: e.Manifest.Domain, CheckedAt: time.Now().UTC(), State: stack.OK}
	_ = e.resolve(ctx) // missing secrets surface as unknown checks
	stages := e.stages()
	for i, checks := range checkAll(ctx, stages) {
		s := stages[i]
		state := worst(checks)
		st.Stages = append(st.Stages, StageStatus{ID: s.id, Title: s.title, Provider: s.provider, State: state, Checks: checks})
		st.State = worse(st.State, state)
	}
	return st
}

var severity = map[string]int{stack.OK: 0, stack.Unknown: 1, stack.Warn: 2, stack.Fail: 3}

func worse(a, b string) string {
	if severity[b] > severity[a] {
		return b
	}
	return a
}

func worst(checks []stack.Check) string {
	state := stack.OK
	for _, c := range checks {
		state = worse(state, c.State)
	}
	return state
}
