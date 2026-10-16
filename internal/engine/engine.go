// Package engine plans, applies, and inspects a stack by running each
// aspect's provider in catalog order and passing contract outputs forward.
package engine

import (
	"context"
	"errors"
	"fmt"
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
}

func New(m *manifest.Manifest, store *secrets.Store, console *stack.Console) *Engine {
	cat := providers.Catalog()
	store.OnValue(console.Redact)
	return &Engine{Manifest: m, Catalog: cat, RT: stack.NewRuntime(m, cat, store, console)}
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
		impl, ok := providers.Lookup(aspect.ID, c.Provider)
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

// resolve fills every provider's static outputs. It returns per-aspect
// errors; outputs are still set as far as they could be.
func (e *Engine) resolve(ctx context.Context) map[string]error {
	errs := map[string]error{}
	for _, u := range e.units() {
		if err := u.impl.Resolve(ctx, u.rt); err != nil {
			errs[u.aspect.ID] = err
		}
	}
	return errs
}

// ── Apply ────────────────────────────────────────────────────────────────────

type ApplyOptions struct {
	// Only limits apply to these aspects (and the environments stage).
	Only []string
}

func (e *Engine) Apply(ctx context.Context, opts ApplyOptions) (err error) {
	console := e.RT.Console
	if err := e.Validate(); err != nil {
		return err
	}
	for _, id := range opts.Only {
		if _, ok := e.Catalog.Aspect(id); !ok && id != EnvironmentsStage {
			return fmt.Errorf("--only: unknown aspect %q", id)
		}
	}

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

	if errs := e.resolve(ctx); len(errs) > 0 {
		return resolveError(errs)
	}
	// Persist generated and supplied secrets before anything uses them, so a
	// failed run never leaves an installed service with a forgotten password.
	if err := e.RT.Secrets.Flush(); err != nil {
		return fmt.Errorf("save secrets: %w", err)
	}

	for _, stage := range e.stages() {
		if !selected(opts.Only, stage.id) {
			continue
		}
		console.Event("stage_started", map[string]string{"id": stage.id, "title": stage.title, "provider": stage.provider})
		stageStart := time.Now()
		if err := stage.apply(ctx); err != nil {
			console.Event("stage_failed", map[string]string{"id": stage.id, "error": err.Error()})
			return fmt.Errorf("%s: %w", stage.title, err)
		}
		console.Event("stage_completed", map[string]string{"id": stage.id, "duration": time.Since(stageStart).Round(time.Second).String()})
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
	var ids []string
	for _, s := range e.stages() {
		if selected(opts.Only, s.id) {
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

func resolveError(errs map[string]error) error {
	var missing, other []string
	for aspect, err := range errs {
		if errors.Is(err, secrets.ErrMissing) {
			missing = append(missing, "  "+err.Error())
		} else {
			other = append(other, fmt.Sprintf("  %s: %v", aspect, err))
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
	Stages    []StagePlan       `json:"stages"`
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
	for aspect, err := range e.resolve(ctx) {
		switch {
		case errors.Is(err, secrets.ErrMissing):
			plan.Missing = append(plan.Missing, err.Error())
		case errors.Is(err, secrets.ErrWouldGenerate):
			plan.Generated = append(plan.Generated, err.Error())
		default:
			plan.Issues = append(plan.Issues, manifest.Issue{Path: "spec." + aspect, Message: err.Error()})
		}
	}
	for _, u := range e.units() {
		plan.Resources.MemoryMiB += u.spec.Resources.MemoryMiB
		plan.Resources.DiskGiB += u.spec.Resources.DiskGiB
	}
	for _, s := range e.stages() {
		plan.Stages = append(plan.Stages, StagePlan{ID: s.id, Title: s.title, Provider: s.provider, Steps: s.plan(ctx)})
	}
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
// repo out in dir, using the stack's VCS provider.
func (e *Engine) Bootstrap(ctx context.Context, manifestYAML []byte, dir string) error {
	if err := e.Validate(); err != nil {
		return err
	}
	c, ok := e.Manifest.Component("vcs")
	if !ok {
		return fmt.Errorf("spec.vcs is required to bootstrap a cluster repo")
	}
	impl, _ := providers.Lookup("vcs", c.Provider)
	boot, ok := impl.(providers.RepoBootstrapper)
	if !ok {
		return fmt.Errorf("vcs provider %s cannot bootstrap a cluster repo", c.Provider)
	}
	rt := e.RT.For("vcs")
	if err := impl.Resolve(ctx, rt); err != nil {
		return resolveError(map[string]error{"vcs": err})
	}
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
