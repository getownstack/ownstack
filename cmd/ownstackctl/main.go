// ownstackctl reconciles a server towards its ownstack.yaml.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/getownstack/ownstack-cluster-template/internal/engine"
	"github.com/getownstack/ownstack-cluster-template/internal/health"
	"github.com/getownstack/ownstack-cluster-template/internal/manifest"
	"github.com/getownstack/ownstack-cluster-template/internal/providers"
	"github.com/getownstack/ownstack-cluster-template/internal/providers/ghactions"
	"github.com/getownstack/ownstack-cluster-template/internal/report"
	"github.com/getownstack/ownstack-cluster-template/internal/secrets"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultRepoDir = "/root/ownstack-cluster"

// activeConsole knows the secrets resolved in this run, so the final error
// message can be redacted too.
var activeConsole *stack.Console

func redactError(err error) string {
	if activeConsole == nil {
		return err.Error()
	}
	return activeConsole.Redacted(err.Error())
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", redactError(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(os.Stdout)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "up":
		return cmdUp(ctx, rest)
	case "apply":
		return cmdApply(ctx, rest)
	case "plan":
		return cmdPlan(ctx, rest)
	case "status":
		return cmdStatus(ctx, rest)
	case "validate":
		return cmdValidate(rest)
	case "catalog":
		return printJSON(providers.Catalog())
	case "workflow":
		fmt.Print(ghactions.Workflow)
		return nil
	case "doctor":
		return cmdDoctor(ctx, rest)
	case "version":
		fmt.Println("ownstackctl", version)
		return nil
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", cmd)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ownstackctl reconciles this server towards ownstack.yaml.

Usage:
  ownstackctl up       [-f ownstack.yaml] [--dir /root/ownstack-cluster]
                       Create or update your cluster repo with the manifest, then apply it.
                       Use this for the first run on a fresh server.
  ownstackctl apply    [-f ownstack.yaml] [--only aspect,...] [--prune]
                       Converge this server to the manifest, then check it is healthy.
                       Safe to rerun. --prune removes providers no longer in the manifest.
  ownstackctl plan     [-f ownstack.yaml] [--json]    Show what apply would do.
  ownstackctl status   [-f ownstack.yaml] [--json]    Check every part of the running stack.
  ownstackctl validate [-f ownstack.yaml] [--json]    Check the manifest against the catalog.
  ownstackctl doctor   [-f ownstack.yaml]             Check this server's prerequisites.
  ownstackctl catalog                                 Print aspects, providers, and presets as JSON.
  ownstackctl workflow                                Print the GitHub Actions workflow for app repos
                                                      (.github/workflows/ownstack.yml).
  ownstackctl version

Secrets are never stored in ownstack.yaml. They are read from environment
variables (GITHUB_TOKEN, CLOUDFLARE_API_TOKEN, ...), from the secrets file
(/etc/ownstack/secrets.env), generated where possible, or prompted for.

Flags shared by commands that read the manifest:
  -f PATH              manifest path (default ownstack.yaml)
  --secrets-file PATH  secrets file (default /etc/ownstack/secrets.env)
  --no-prompt          never prompt for secrets
  --report URL         also send progress to an Ownstack run (token in OWNSTACK_REPORT_TOKEN)

Flags for apply and up:
  --prune              remove installed providers that are no longer in the manifest
  --allow-low-memory   skip the check that the stack fits this server's memory
  --no-verify          skip the final health check
`)
}

// ── Shared flags ─────────────────────────────────────────────────────────────

type common struct {
	file        string
	secretsFile string
	noPrompt    bool
	jsonOut     bool
	reportURL   string
}

func newFlags(name string, c *common) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(&c.file, "f", manifest.FileName, "manifest path")
	fs.StringVar(&c.secretsFile, "secrets-file", envOr("OWNSTACK_SECRETS_FILE", secrets.DefaultFile), "secrets file")
	fs.BoolVar(&c.noPrompt, "no-prompt", false, "never prompt for secrets")
	fs.StringVar(&c.reportURL, "report", "", "Ownstack run URL to report progress to")
	return fs
}

type loaded struct {
	manifest  *manifest.Manifest
	raw       []byte
	legacy    bool
	overrides map[string]string
}

// load reads the manifest, falling back to the pre-manifest environment
// contract used by older Ownstack runners.
func load(path string) (*loaded, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		m, perr := manifest.Parse(data)
		if perr == nil {
			return &loaded{manifest: m, raw: data}, nil
		}
		if !errors.Is(perr, manifest.ErrNotV1) {
			return nil, fmt.Errorf("%s: %w", path, perr)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if m, secretValues, ok := manifest.FromLegacyEnv(os.Getenv); ok {
		fmt.Fprintln(os.Stderr, "note: no ownstack.io/v1 manifest found; using the legacy environment variables")
		raw, err := m.Marshal(providers.Catalog())
		if err != nil {
			return nil, err
		}
		return &loaded{manifest: m, raw: raw, legacy: true, overrides: secretValues}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s not found; create one in the Ownstack designer or pass -f", path)
	}
	return nil, fmt.Errorf("%s is not an ownstack.io/v1 manifest", path)
}

type session struct {
	engine   *engine.Engine
	console  *stack.Console
	store    *secrets.Store
	reporter *report.Reporter
	loaded   *loaded
}

func open(ctx context.Context, c common, out io.Writer) (*session, error) {
	l, err := load(c.file)
	if err != nil {
		return nil, err
	}
	console := stack.NewConsole(out)
	activeConsole = console
	store := secrets.NewStore(c.secretsFile)
	store.Prompt = !c.noPrompt
	store.Overrides = l.overrides
	s := &session{engine: engine.New(l.manifest, store, console), console: console, store: store, loaded: l}
	if c.reportURL != "" {
		token := os.Getenv("OWNSTACK_REPORT_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("--report needs OWNSTACK_REPORT_TOKEN")
		}
		s.reporter = report.New(c.reportURL, token)
		console.Redact(token)
		console.AddSink(s.reporter.Line)
		s.reporter.Start(ctx)
	}
	return s, nil
}

func (s *session) finish(err error) error {
	if s.reporter != nil {
		if err != nil {
			// main prints the error to the terminal; the run log needs it too.
			s.reporter.Line(s.console.Redacted("ERROR: " + err.Error()))
		}
		s.reporter.Finish(err)
	}
	return err
}

// ── Commands ─────────────────────────────────────────────────────────────────

func cmdApply(ctx context.Context, args []string) error {
	var c common
	var only string
	var opts engine.ApplyOptions
	var hf healthFlags
	fs := newFlags("apply", &c)
	applyFlags(fs, &opts)
	hf.register(fs)
	fs.StringVar(&only, "only", "", "comma-separated aspects to apply")
	fs.String("legacy-script", "", "ignored; kept for setup_system.sh from earlier template versions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := open(ctx, c, os.Stdout)
	if err != nil {
		return err
	}
	if only != "" {
		opts.Only = strings.Split(only, ",")
	}
	err = s.engine.Apply(ctx, opts)
	if err == nil {
		printSummary(s)
		setupHealth(ctx, s, hf, c.reportURL, c.file)
	}
	return s.finish(err)
}

func cmdUp(ctx context.Context, args []string) error {
	var c common
	var dir string
	var opts engine.ApplyOptions
	var hf healthFlags
	fs := newFlags("up", &c)
	applyFlags(fs, &opts)
	hf.register(fs)
	fs.StringVar(&dir, "dir", defaultRepoDir, "where to check out the cluster repo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := open(ctx, c, os.Stdout)
	if err != nil {
		return err
	}
	err = func() error {
		if err := s.engine.Bootstrap(ctx, s.loaded.raw, dir); err != nil {
			return err
		}
		checkout := filepath.Join(dir, manifest.FileName)
		m, err := manifest.Load(checkout)
		if err != nil {
			return fmt.Errorf("read %s: %w", checkout, err)
		}
		s.engine = engine.New(m, s.store, s.console)
		if err := s.engine.Apply(ctx, opts); err != nil {
			return err
		}
		printSummary(s)
		setupHealth(ctx, s, hf, c.reportURL, checkout)
		s.console.Printf("The stack definition lives in %s (and your cluster repo). Rerun `ownstackctl apply` there after changing it.", checkout)
		return nil
	}()
	return s.finish(err)
}

func cmdPlan(ctx context.Context, args []string) error {
	var c common
	fs := newFlags("plan", &c)
	fs.BoolVar(&c.jsonOut, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c.noPrompt = true
	s, err := open(ctx, c, os.Stderr)
	if err != nil {
		return err
	}
	s.store.Generate, s.store.Save = false, false
	plan := s.engine.Plan(ctx)
	if c.jsonOut {
		return printJSON(plan)
	}
	fmt.Printf("Plan for stack %q\n", plan.Stack)
	for _, issue := range plan.Issues {
		fmt.Printf("  ✗ %s\n", issue)
	}
	if !plan.Valid {
		return fmt.Errorf("ownstack.yaml is not valid")
	}
	fmt.Printf("Needs about %.1f GiB memory and %d GiB disk", float64(plan.Resources.MemoryMiB)/1024, plan.Resources.DiskGiB)
	if plan.MemTotalMiB > 0 {
		fmt.Printf(" (this server has %.1f GiB)", float64(plan.MemTotalMiB)/1024)
	}
	fmt.Print(".\n\n")
	for _, stage := range plan.Stages {
		fmt.Printf("%s\n", stage.Title)
		for _, step := range stage.Steps {
			fmt.Printf("  %-9s %s", step.Action, step.Target)
			if step.Detail != "" {
				fmt.Printf("  (%s)", step.Detail)
			}
			fmt.Println()
		}
	}
	if len(plan.Missing) > 0 {
		fmt.Println("\nSecrets apply will ask for:")
		for _, m := range plan.Missing {
			fmt.Printf("  %s\n", m)
		}
	}
	if len(plan.Generated) > 0 {
		fmt.Println("\nSecrets apply will generate and keep on this server:")
		for _, g := range plan.Generated {
			fmt.Printf("  %s\n", g)
		}
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	var c common
	var healthURL string
	fs := newFlags("status", &c)
	fs.BoolVar(&c.jsonOut, "json", false, "print JSON")
	fs.StringVar(&healthURL, "health-url", "", "also send the status to Ownstack (token in OWNSTACK_REPORT_TOKEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c.noPrompt = true
	s, err := open(ctx, c, os.Stderr)
	if err != nil {
		return err
	}
	s.store.Generate, s.store.Save = false, false
	st := s.engine.Status(ctx)
	if healthURL != "" {
		if err := health.Send(ctx, healthURL, os.Getenv("OWNSTACK_REPORT_TOKEN"), st); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not send health to Ownstack: %v\n", err)
		}
	}
	if c.jsonOut {
		return printJSON(st)
	}
	marks := map[string]string{stack.OK: "✓", stack.Warn: "!", stack.Fail: "✗", stack.Unknown: "?"}
	fmt.Printf("Stack %q: %s\n", st.Stack, st.State)
	for _, stage := range st.Stages {
		fmt.Printf("\n%s %s\n", marks[stage.State], stage.Title)
		for _, check := range stage.Checks {
			fmt.Printf("  %s %-24s %s\n", marks[check.State], check.Name, check.Detail)
		}
	}
	if st.State == stack.Fail {
		os.Exit(2)
	}
	return nil
}

func cmdValidate(args []string) error {
	var c common
	fs := newFlags("validate", &c)
	fs.BoolVar(&c.jsonOut, "json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := load(c.file)
	if err != nil {
		return err
	}
	issues := l.manifest.Validate(providers.Catalog())
	if c.jsonOut {
		return printJSON(map[string]any{"valid": len(issues) == 0, "issues": issues})
	}
	if len(issues) == 0 {
		fmt.Println("ownstack.yaml is valid")
		return nil
	}
	for _, issue := range issues {
		fmt.Printf("  ✗ %s\n", issue)
	}
	return fmt.Errorf("%d problem(s) in %s", len(issues), c.file)
}

func cmdDoctor(ctx context.Context, args []string) error {
	var c common
	fs := newFlags("doctor", &c)
	if err := fs.Parse(args); err != nil {
		return err
	}
	problems := 0
	report := func(ok bool, name, detail string) {
		mark := "✓"
		if !ok {
			mark = "✗"
			problems++
		}
		fmt.Printf("  %s %-14s %s\n", mark, name, detail)
	}
	fmt.Println("Server")
	report(os.Geteuid() == 0, "root", "apply installs system packages and must run as root")
	osName := "unknown"
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				osName = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
		}
	}
	report(strings.Contains(osName, "Ubuntu"), "os", osName+" (Ubuntu 22.04 or 24.04 expected)")
	report(runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64", "arch", runtime.GOARCH)
	for _, tool := range []string{"bash", "curl"} {
		e := &stack.Exec{}
		report(e.Has(tool), tool, "required")
	}

	if l, err := load(c.file); err == nil {
		fmt.Println("\nManifest")
		issues := l.manifest.Validate(providers.Catalog())
		report(len(issues) == 0, "valid", fmt.Sprintf("%d issue(s)", len(issues)))
		store := secrets.NewStore(c.secretsFile)
		store.Save, store.Generate = false, false
		store.Overrides = l.overrides
		eng := engine.New(l.manifest, store, stack.NewConsole(io.Discard))
		plan := eng.Plan(ctx)
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			var totalKiB int
			fmt.Sscanf(strings.SplitN(string(data), "\n", 2)[0], "MemTotal: %d kB", &totalKiB)
			need := plan.Resources.MemoryMiB
			report(totalKiB/1024 >= need, "memory", fmt.Sprintf("%d MiB available, stack needs about %d MiB", totalKiB/1024, need))
		}
		report(len(plan.Missing) == 0, "secrets", fmt.Sprintf("%d secret(s) still to provide", len(plan.Missing)))
	} else {
		fmt.Printf("\nManifest\n  - %v\n", err)
	}
	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	return nil
}

// printSummary tells the user where their stack is and how to sign in.
func printSummary(s *session) {
	out := s.engine.RT.Out
	m, cat := s.engine.Manifest, s.engine.Catalog
	name := func(aspect string) string {
		if c, ok := m.Component(aspect); ok {
			if p, ok := cat.Provider(aspect, c.Provider); ok {
				return p.Name
			}
		}
		return aspect
	}
	c := s.console
	c.Printf("")
	c.Printf("Your stack is ready.")
	if out.Ingress != nil {
		c.Printf("  %-18s https://%s", name("ingress")+" dashboard", m.Settings(cat, "ingress")["dashboardHost"])
	}
	if out.Images != nil {
		line := fmt.Sprintf("  %-18s %s/%s  (user %s)", name("images"), out.Images.Host, out.Images.Project, out.Images.PushUsername)
		if c, ok := m.Component("images"); ok && c.Provider == "harbor" {
			line = fmt.Sprintf("  %-18s https://%s  (user admin, password HARBOR_ADMIN_PASSWORD)", name("images"), out.Images.Host)
		}
		c.Printf("%s", line)
	}
	if out.CICD != nil {
		line := fmt.Sprintf("  %-18s %s", name("cicd"), out.CICD.URL)
		if c, ok := m.Component("cicd"); ok && c.Provider == "jenkins" {
			line += "  (user admin, password JENKINS_ADMIN_PASSWORD)"
		}
		c.Printf("%s", line)
	}
	c.Printf("  %-18s https://<app>.%s (each infrastructure/<app>/ in your repos)", "Apps", m.Spec.Domain)
	c.Printf("  %-18s %s on this server", "Secrets", s.store.Path)
	c.Printf("  %-18s ownstackctl status", "Health")
}

// healthFlags configure the health timer installed after a successful run.
type healthFlags struct {
	url      string
	disabled bool
}

func (h *healthFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.url, "health-url", "", "Ownstack health endpoint (default: derived from --report)")
	fs.BoolVar(&h.disabled, "no-health-reports", false, "do not install the health report timer")
}

// setupHealth installs the health timer and sends a first report. Failures
// are reported but never fail the run.
func setupHealth(ctx context.Context, s *session, h healthFlags, reportURL, manifestPath string) {
	url := h.url
	if url == "" {
		url = health.URLFromReport(reportURL)
	}
	token := os.Getenv("OWNSTACK_REPORT_TOKEN")
	if h.disabled || url == "" || token == "" || os.Geteuid() != 0 {
		return
	}
	abs, err := filepath.Abs(manifestPath)
	if err == nil {
		err = health.Install(ctx, s.engine.RT.Exec, url, token, abs)
	}
	if err == nil {
		err = health.Send(ctx, url, token, s.engine.Status(ctx))
	}
	if err != nil {
		s.console.Printf("note: health reports to Ownstack are not set up: %v", err)
		return
	}
	s.console.Printf("Health is reported to Ownstack every %s (stop: systemctl disable --now ownstack-health.timer)", health.Interval)
}

func applyFlags(fs *flag.FlagSet, opts *engine.ApplyOptions) {
	fs.BoolVar(&opts.Prune, "prune", false, "remove installed providers no longer in the manifest")
	fs.BoolVar(&opts.AllowLowMemory, "allow-low-memory", false, "skip the memory preflight")
	fs.BoolVar(&opts.SkipVerify, "no-verify", false, "skip the final health check")
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
