// Package engine plans, applies, and inspects a stack by running each
// aspect's provider in catalog order and passing contract outputs forward.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/catalog"
	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers"
	"github.com/getownstack/ownstack-cluster-template/internal/secrets"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// EnvironmentsStage prepares the deployment namespaces. It is not an aspect;
// it runs once the registry has delivered its pull credential.
const EnvironmentsStage = "environments"

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

func (u unit) title() string { return u.aspect.Name + " · " + u.spec.Name }

func (e *Engine) units() []unit {
	var units []unit
	for _, aspect := range e.Catalog.Aspects {
		c, ok := e.Manifest.Component(aspect.ID)
		if !ok {
			continue
		}
		spec, _ := e.Catalog.Provider(aspect.ID, c.Provider)
		impl, ok := e.lookup(aspect.ID, c.Provider)
		if !ok {
			continue
		}
		units = append(units, unit{aspect: aspect, spec: spec, impl: impl, rt: e.RT.For(aspect.ID)})
	}
	return units
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
	// Only limits apply to these stages: aspects in the manifest, or the
	// environments stage. Selecting images also runs the environments stage,
	// which hands the registry credential to every environment.
	Only []string
	// AllowLowMemory skips the memory preflight.
	AllowLowMemory bool
	// Prune removes providers that are installed but no longer in the
	// manifest, including their data.
	Prune bool
	// SkipVerify skips the final health check.
	SkipVerify bool
	// MemTotalMiB overrides the detected server memory (tests).
	MemTotalMiB int
}

func (e *Engine) Apply(ctx context.Context, opts ApplyOptions) (err error) {
	console := e.RT.Console
	if err := e.Validate(); err != nil {
		return err
	}
	if err := e.checkOnly(opts.Only); err != nil {
		return err
	}
	only := expandOnly(opts.Only)

	start := time.Now()
	console.Event("run_started", map[string]string{
		"stack":  e.Manifest.Metadata.Name,
		"domain": e.Manifest.Spec.Domain,
		"stages": strings.Join(e.stageIDs(opts), ","),
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

	var applied []stage
	var credentialChanged bool
	for _, stage := range e.stages() {
		if !selected(only, stage.id) {
			continue
		}
		credentialBefore := e.pushCredential()
		if stage.id == EnvironmentsStage && e.pullSecretsStale(ctx) {
			credentialChanged = true
		}
		console.Event("stage_started", map[string]string{"id": stage.id, "title": stage.title, "provider": stage.provider})
		stageStart := time.Now()
		if err := stage.apply(ctx); err != nil {
			console.Event("stage_failed", map[string]string{"id": stage.id, "error": err.Error()})
			if opts.Prune {
				console.Printf("--prune: nothing was removed because apply failed")
			}
			return fmt.Errorf("%s: %w", stage.title, err)
		}
		console.Event("stage_completed", map[string]string{"id": stage.id, "duration": time.Since(stageStart).Round(time.Second).String()})
		applied = append(applied, stage)
		if e.pushCredential() != credentialBefore {
			credentialChanged = true
		}
	}
	if credentialChanged {
		e.warnCredentialConsumers(only)
	}
	// Prune only once the new stack runs, so replacing a provider (Harbor by
	// Zot) never leaves the node without one when the new one fails.
	if err := e.pruneOrphans(ctx, only, opts.Prune); err != nil {
		return err
	}
	if opts.SkipVerify {
		return nil
	}
	return e.verify(ctx, applied)
}

// prepare runs what apply and up need before changing anything: the memory
// preflight, then every provider's outputs and secrets, which it saves.
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
	return nil
}

// checkOnly rejects --only ids that would apply nothing.
func (e *Engine) checkOnly(only []string) error {
	for _, id := range only {
		if id == EnvironmentsStage {
			continue
		}
		if _, ok := e.Catalog.Aspect(id); !ok {
			return fmt.Errorf("--only: unknown aspect %q", id)
		}
		if _, ok := e.Manifest.Component(id); !ok {
			return fmt.Errorf("--only: %s is not in ownstack.yaml", id)
		}
	}
	return nil
}

// expandOnly adds the stages a selection implies: the environments stage
// holds the registry's pull credential, so applying images refreshes it too.
func expandOnly(only []string) []string {
	if len(only) == 0 || !selected(only, "images") || selected(only, EnvironmentsStage) {
		return only
	}
	return append(append([]string(nil), only...), EnvironmentsStage)
}

// pushCredential identifies the registry credential for change detection.
// It is only compared, never printed.
func (e *Engine) pushCredential() [2]string {
	if images := e.RT.Out.Images; images != nil {
		return [2]string{images.PushUsername, images.PushPassword}
	}
	return [2]string{}
}

// pullSecretsStale reports whether an existing environment pull secret holds
// a different credential than the registry now provides (a rotated robot or
// a changed password).
func (e *Engine) pullSecretsStale(ctx context.Context) bool {
	images := e.RT.Out.Images
	if images == nil || images.PushUsername == "" {
		return false
	}
	for _, env := range e.Manifest.Environments() {
		raw, ok := e.RT.Kube.SecretValue(ctx, env, images.PullSecretName, ".dockerconfigjson")
		if ok && stack.PullSecretMismatch(raw, images.PullSecretServer(), images.PushUsername, images.PushPassword) != "" {
			return true
		}
	}
	return false
}

// warnCredentialConsumers names the stages that still hold the old registry
// credential because this run did not apply them.
func (e *Engine) warnCredentialConsumers(only []string) {
	var stale []string
	for _, s := range e.stages() {
		if selected(only, s.id) {
			continue
		}
		aspect, _ := e.Catalog.Aspect(s.id)
		if s.id == EnvironmentsStage || slices.Contains(aspect.Consumes, "images") {
			stale = append(stale, s.id)
		}
	}
	if len(stale) > 0 {
		e.RT.Console.Printf("warning: registry credential changed; run `ownstackctl apply --only %s` so they use the new one", strings.Join(stale, ","))
	}
}

// VerifyTimeout bounds the final health check; verifyInterval is how often
// it polls.
var (
	VerifyTimeout  = 5 * time.Minute
	verifyInterval = 15 * time.Second
)

// verify waits until every applied stage reports no failing checks. A stack
// whose services fall over once everything runs together (for example on a
// server that is too small) fails here instead of being reported as ready.
func (e *Engine) verify(ctx context.Context, applied []stage) error {
	console := e.RT.Console
	console.Event("stage_started", map[string]string{"id": "verify", "title": "Health check", "provider": "ownstack"})
	start := time.Now()
	deadline := start.Add(VerifyTimeout)
	for {
		var failing []string
		for _, s := range applied {
			for _, c := range s.status(ctx) {
				if c.State == stack.Fail {
					failing = append(failing, fmt.Sprintf("%s: %s %s", s.title, c.Name, c.Detail))
				}
			}
		}
		if len(failing) == 0 {
			console.Printf("All %d stages report healthy", len(applied))
			console.Event("stage_completed", map[string]string{"id": "verify", "duration": time.Since(start).Round(time.Second).String()})
			return nil
		}
		if time.Now().After(deadline) {
			msg := "applied, but not healthy after " + VerifyTimeout.String() + ":\n  " + strings.Join(failing, "\n  ")
			console.Event("stage_failed", map[string]string{"id": "verify", "error": strings.Join(failing, "; ")})
			return errors.New(msg)
		}
		console.Printf("  waiting: %d check(s) failing, first: %s", len(failing), failing[0])
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyInterval):
		}
	}
}

// MemoryFits reports whether a stack that needs about need MiB fits a server
// with total MiB. The catalog estimates are generous, so 80% is enough. The
// apply preflight and doctor share this rule.
func MemoryFits(total, need int) bool {
	return float64(total) >= float64(need)*0.8
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
	need := 0
	var heavy []string
	for _, u := range e.units() {
		need += u.spec.Resources.MemoryMiB
		if u.spec.Resources.MemoryMiB >= 1000 {
			heavy = append(heavy, fmt.Sprintf("%s (~%.1f GB)", u.spec.Name, float64(u.spec.Resources.MemoryMiB)/1024))
		}
	}
	if MemoryFits(total, need) {
		return nil
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

type orphan struct {
	aspect catalog.Aspect
	spec   catalog.Provider
	impl   stack.Removable
}

func (o orphan) title() string { return o.aspect.Name + " · " + o.spec.Name }

// orphans are installed providers that the manifest no longer selects.
func (e *Engine) orphans(ctx context.Context) []orphan {
	if !e.RT.Kube.Reachable(ctx) {
		return nil
	}
	var out []orphan
	for _, p := range e.Catalog.Providers {
		if c, ok := e.Manifest.Component(p.Aspect); ok && c.Provider == p.ID {
			continue
		}
		impl, ok := e.lookup(p.Aspect, p.ID)
		if !ok {
			continue
		}
		removable, ok := impl.(stack.Removable)
		if !ok || !removable.Installed(ctx, e.RT.For(p.Aspect)) {
			continue
		}
		aspect, _ := e.Catalog.Aspect(p.Aspect)
		out = append(out, orphan{aspect: aspect, spec: p, impl: removable})
	}
	return out
}

// pruneOrphans removes the selected aspects' orphans with prune, and only
// warns about them otherwise.
func (e *Engine) pruneOrphans(ctx context.Context, only []string, prune bool) error {
	var orphans []orphan
	for _, o := range e.orphans(ctx) {
		if selected(only, o.aspect.ID) {
			orphans = append(orphans, o)
		}
	}
	if prune {
		return e.prune(ctx, orphans)
	}
	for _, o := range orphans {
		e.RT.Console.Printf("warning: %s is installed but no longer in ownstack.yaml; rerun with --prune to remove it and its data", o.title())
	}
	return nil
}

func (e *Engine) prune(ctx context.Context, orphans []orphan) error {
	console := e.RT.Console
	for _, o := range orphans {
		id := "prune-" + o.spec.ID
		console.Event("stage_started", map[string]string{"id": id, "title": "Remove " + o.title(), "provider": o.spec.ID})
		start := time.Now()
		if err := o.impl.Remove(ctx, e.RT.For(o.aspect.ID)); err != nil {
			console.Event("stage_failed", map[string]string{"id": id, "error": err.Error()})
			return fmt.Errorf("remove %s: %w", o.title(), err)
		}
		console.Event("stage_completed", map[string]string{"id": id, "duration": time.Since(start).Round(time.Second).String()})
	}
	return nil
}

type stage struct {
	id, title, provider string
	apply               func(context.Context) error
	plan                func(context.Context) []stack.Step
	status              func(context.Context) []stack.Check
}

// stages lists providers in catalog order with the environments stage after
// the registry (or after compute when there is no registry).
func (e *Engine) stages() []stage {
	var out []stage
	units := e.units()
	envAfter := "compute"
	for _, u := range units {
		if u.aspect.ID == "images" {
			envAfter = "images"
		}
	}
	for _, u := range units {
		u := u
		out = append(out, stage{
			id: u.aspect.ID, title: u.title(), provider: u.spec.ID,
			apply:  func(ctx context.Context) error { return u.impl.Apply(ctx, u.rt) },
			plan:   func(ctx context.Context) []stack.Step { return u.impl.Plan(ctx, u.rt) },
			status: func(ctx context.Context) []stack.Check { return u.impl.Status(ctx, u.rt) },
		})
		if u.aspect.ID == envAfter {
			env := environments{rt: e.RT}
			out = append(out, stage{
				id: EnvironmentsStage, title: "Environments · " + strings.Join(e.Manifest.Environments(), ", "), provider: "ownstack",
				apply: env.apply, plan: env.plan, status: env.status,
			})
		}
	}
	return out
}

func (e *Engine) stageIDs(opts ApplyOptions) []string {
	only := expandOnly(opts.Only)
	var ids []string
	for _, s := range e.stages() {
		if selected(only, s.id) {
			ids = append(ids, s.id)
		}
	}
	return ids
}

func selected(only []string, id string) bool {
	if len(only) == 0 {
		return true
	}
	for _, o := range only {
		if o == id {
			return true
		}
	}
	return false
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
		msg += "missing secrets (export them, add them to the secrets file, or run in a terminal to be prompted):\n" + strings.Join(missing, "\n")
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

type Plan struct {
	Stack     string            `json:"stack"`
	Valid     bool              `json:"valid"`
	Issues    []manifest.Issue  `json:"issues,omitempty"`
	Missing   []string          `json:"missingSecrets,omitempty"`
	Generated []string          `json:"generatedSecrets,omitempty"`
	Resources catalog.Resources `json:"resources"`
	// MemTotalMiB is this server's memory, when known.
	MemTotalMiB int         `json:"memTotalMiB,omitempty"`
	Stages      []StagePlan `json:"stages"`
}

type StagePlan struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Provider string       `json:"provider"`
	Steps    []stack.Step `json:"steps"`
}

func (e *Engine) Plan(ctx context.Context) Plan {
	plan := Plan{Stack: e.Manifest.Metadata.Name}
	plan.Issues = e.Manifest.Validate(e.Catalog)
	plan.Valid = len(plan.Issues) == 0
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
			plan.Issues = append(plan.Issues, manifest.Issue{Path: "spec." + issue.aspect, Message: err.Error()})
		}
	}
	for _, u := range e.units() {
		plan.Resources.MemoryMiB += u.spec.Resources.MemoryMiB
		plan.Resources.DiskGiB += u.spec.Resources.DiskGiB
	}
	for _, s := range e.stages() {
		plan.Stages = append(plan.Stages, StagePlan{ID: s.id, Title: s.title, Provider: s.provider, Steps: s.plan(ctx)})
	}
	for _, o := range e.orphans(ctx) {
		plan.Stages = append(plan.Stages, StagePlan{ID: "prune-" + o.spec.ID, Title: "Remove " + o.title(), Provider: o.spec.ID,
			Steps: []stack.Step{{Action: "remove", Target: o.spec.Name, Detail: "no longer in ownstack.yaml; apply --prune deletes it and its data"}}})
	}
	plan.MemTotalMiB = memTotalMiB()
	return plan
}

// ── Status ───────────────────────────────────────────────────────────────────

type Status struct {
	Stack     string        `json:"stack"`
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

func (e *Engine) Status(ctx context.Context) Status {
	st := Status{Stack: e.Manifest.Metadata.Name, CheckedAt: time.Now().UTC(), State: stack.OK}
	_ = e.resolve(ctx) // missing secrets surface as unknown checks
	for _, s := range e.stages() {
		checks := s.status(ctx)
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

// ── Bootstrap ────────────────────────────────────────────────────────────────

// Bootstrap creates the cluster repo, commits the manifest, and checks the
// repo out in dir, using the stack's VCS provider. It first runs the checks
// apply would (memory, every provider's secrets), so a run that cannot
// succeed fails before the repo is touched.
func (e *Engine) Bootstrap(ctx context.Context, manifestYAML []byte, dir string, opts ApplyOptions) error {
	if err := e.Validate(); err != nil {
		return err
	}
	c, ok := e.Manifest.Component("vcs")
	if !ok {
		return fmt.Errorf("spec.vcs is required to bootstrap a cluster repo")
	}
	impl, _ := e.lookup("vcs", c.Provider)
	boot, ok := impl.(providers.RepoBootstrapper)
	if !ok {
		return fmt.Errorf("vcs provider %s cannot bootstrap a cluster repo", c.Provider)
	}
	if err := e.prepare(ctx, opts); err != nil {
		return err
	}
	rt := e.RT.For("vcs")
	console := e.RT.Console
	console.Event("stage_started", map[string]string{"id": "repo", "title": "Cluster repo", "provider": c.Provider})
	start := time.Now()
	if err := boot.Bootstrap(ctx, rt, manifestYAML, dir); err != nil {
		console.Event("stage_failed", map[string]string{"id": "repo", "error": err.Error()})
		return err
	}
	console.Event("stage_completed", map[string]string{"id": "repo", "duration": time.Since(start).Round(time.Second).String()})
	return nil
}
