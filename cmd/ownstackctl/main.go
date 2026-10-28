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
	"syscall"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/engine"
	"github.com/getownstack/ownstack/internal/health"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/providers"
	"github.com/getownstack/ownstack/internal/providers/ghactions"
	"github.com/getownstack/ownstack/internal/report"
	"github.com/getownstack/ownstack/internal/secrets"
	"github.com/getownstack/ownstack/internal/stack"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

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
	stack.EngineVersion = version
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", redactError(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(stdout)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "apply":
		return cmdApply(ctx, rest)
	case "plan":
		return cmdPlan(ctx, rest)
	case "status":
		return cmdStatus(ctx, rest)
	case "validate":
		return cmdValidate(rest, stdin, stdout)
	case "catalog":
		return printJSON(stdout, providers.Catalog())
	case "workflow":
		fmt.Fprint(stdout, ghactions.Workflow)
		return nil
	case "version":
		fmt.Fprintln(stdout, "ownstackctl", version)
		return nil
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", cmd)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `ownstackctl reconciles this server towards ownstack.yaml.

Usage:
  ownstackctl apply    [-f FILE] [--report URL] [--allow-low-memory]
                       Converge this server to the manifest, then check it is healthy.
                       Safe to rerun. Keeps the applied manifest in $OWNSTACK_DIR.
  ownstackctl plan     [-f FILE] [--json]    Check before apply: the manifest, this server,
                                             its memory, the secrets, and the stages.
  ownstackctl status   [-f FILE] [--json] [--health-url URL]
                                             Check every part of the running stack.
  ownstackctl validate [-f FILE|-] [--json]  Check a manifest against the catalog
                                             (- reads stdin; YAML or JSON).
  ownstackctl catalog                        Print aspects, providers, and presets as JSON.
  ownstackctl workflow                       Print the GitHub Actions workflow for app repos
                                             (.github/workflows/ownstack.yml).
  ownstackctl version

-f defaults to $OWNSTACK_DIR/ownstack.yaml, the manifest the last apply used.
$OWNSTACK_DIR (default /etc/ownstack) also holds secrets.env and apply.lock.

Secrets are never stored in ownstack.yaml. They are read from environment
variables (GITHUB_TOKEN, CLOUDFLARE_API_TOKEN, ...), then from
$OWNSTACK_DIR/secrets.env, generated where possible, or prompted for on a
terminal.

--report URL sends progress to an Ownstack run (token in
OWNSTACK_REPORT_TOKEN) and then keeps reporting health every 5 minutes.
`)
}

// manifestPath is -f's default: the manifest apply saved on this server.
func manifestPath() string { return stack.StateFile(manifest.FileName) }

// load reads and parses the manifest; "-" reads stdin.
func load(path string, stdin io.Reader) (*manifest.Manifest, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if os.IsNotExist(err) {
		if path == manifestPath() {
			return nil, fmt.Errorf("%s not found: no stack was applied on this server yet; pass -f ownstack.yaml (from the Ownstack designer)", path)
		}
		return nil, fmt.Errorf("%s not found", path)
	}
	if err != nil {
		return nil, err
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

type session struct {
	engine   *engine.Engine
	console  *stack.Console
	store    *secrets.Store
	reporter *report.Reporter
}

// open loads the manifest for apply, plan, and status. readOnly stores
// never generate, prompt for, or save secrets.
func open(ctx context.Context, file string, readOnly bool, reportURL string, out io.Writer) (*session, error) {
	m, err := load(file, os.Stdin)
	if err != nil {
		return nil, err
	}
	console := stack.NewConsole(out)
	activeConsole = console
	store := secrets.NewStore(stack.StateFile("secrets.env"))
	store.ReadOnly = readOnly
	s := &session{engine: engine.New(m, store, console), console: console, store: store}
	if reportURL != "" {
		token := os.Getenv("OWNSTACK_REPORT_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("--report needs OWNSTACK_REPORT_TOKEN")
		}
		s.reporter = report.New(reportURL, token)
		console.Redact(token)
		console.AddSink(s.reporter.Line)
		s.reporter.Start(ctx)
	}
	return s, nil
}

func (s *session) finish(err error) error {
	if s.reporter != nil {
		var reported error
		if err != nil {
			// main prints the error to the terminal; the run log needs it too.
			s.reporter.Line(s.console.Redacted("ERROR: " + err.Error()))
			reported = errors.New(s.console.Redacted(err.Error()))
		}
		s.reporter.Finish(reported)
	}
	return err
}

// ── Commands ─────────────────────────────────────────────────────────────────

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	file := fs.String("f", manifestPath(), "manifest path")
	reportURL := fs.String("report", "", "Ownstack run URL to report progress to (token in OWNSTACK_REPORT_TOKEN)")
	var opts engine.ApplyOptions
	fs.BoolVar(&opts.AllowLowMemory, "allow-low-memory", false, "skip the memory preflight")
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(ctx, *file, false, *reportURL, os.Stdout)
	if err != nil {
		return err
	}
	unlock, err := lockNode(stack.StateDir())
	if err != nil {
		return s.finish(err)
	}
	defer unlock()
	err = s.engine.Apply(ctx, opts)
	if err == nil {
		printSummary(s)
		if *reportURL != "" {
			setupHealth(ctx, s, *reportURL)
		}
	}
	return s.finish(err)
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	file := fs.String("f", manifestPath(), "manifest path")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(ctx, *file, true, "", os.Stderr)
	if err != nil {
		return err
	}
	plan := s.engine.Plan(ctx)
	s.console.RedactAll(&plan)
	if *jsonOut {
		return printJSON(os.Stdout, plan)
	}
	marks := map[string]string{stack.OK: "✓", stack.Fail: "✗"}
	fmt.Printf("Plan for %s\n", plan.Domain)
	for _, issue := range plan.Issues {
		fmt.Printf("  ✗ %s\n", issue)
	}
	fmt.Println("\nServer")
	for _, check := range plan.Server {
		fmt.Printf("  %s %-8s %s\n", marks[check.State], check.Name, check.Detail)
	}
	fmt.Printf("\nThe stack needs about %.1f GiB memory and %d GiB disk.\n", float64(plan.Resources.MemoryMiB)/1024, plan.Resources.DiskGiB)
	if len(plan.Stages) > 0 {
		fmt.Println("\nStages")
		for _, stage := range plan.Stages {
			fmt.Printf("  %s\n", stage.Title)
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
	if n := plan.Problems(); n > 0 {
		return fmt.Errorf("%d problem(s) would stop apply", n)
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	file := fs.String("f", manifestPath(), "manifest path")
	jsonOut := fs.Bool("json", false, "print JSON")
	healthURL := fs.String("health-url", "", "also send the status to Ownstack (token in OWNSTACK_REPORT_TOKEN)")
	if err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(ctx, *file, true, "", os.Stderr)
	if err != nil {
		return err
	}
	// A file that is not a valid manifest would otherwise report a stack
	// with no stages as healthy.
	if err := s.engine.Validate(); err != nil {
		return err
	}
	st := s.engine.Status(ctx)
	// Check details are free-form provider and API errors.
	s.console.RedactAll(&st)
	if *healthURL != "" {
		if err := health.Send(ctx, *healthURL, os.Getenv("OWNSTACK_REPORT_TOKEN"), st); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not send health to Ownstack: %v\n", err)
		}
	}
	if *jsonOut {
		if err := printJSON(os.Stdout, st); err != nil {
			return err
		}
	} else {
		marks := map[string]string{stack.OK: "✓", stack.Warn: "!", stack.Fail: "✗", stack.Unknown: "?"}
		fmt.Printf("Stack %s: %s\n", st.Domain, st.State)
		for _, stage := range st.Stages {
			fmt.Printf("\n%s %s\n", marks[stage.State], stage.Title)
			for _, check := range stage.Checks {
				fmt.Printf("  %s %-24s %s\n", marks[check.State], check.Name, check.Detail)
			}
		}
	}
	if st.State == stack.Fail {
		os.Exit(2)
	}
	return nil
}

// Validation is validate --json's answer. The Ownstack designer reads it.
type Validation struct {
	Valid    bool             `json:"valid"`
	Issues   []manifest.Issue `json:"issues"`
	Manifest string           `json:"manifest"`
	// Resources are the providers' estimates, and the least memory a server
	// needs for them (apply's memory preflight).
	Resources struct {
		MemoryMiB    int `json:"memoryMiB"`
		DiskGiB      int `json:"diskGiB"`
		MinServerMiB int `json:"minServerMiB"`
	} `json:"resources"`
	Secrets []engine.Secret `json:"secrets"`
}

// validate checks a manifest without the network or secrets.
func validate(file string, stdin io.Reader) Validation {
	v := Validation{Issues: []manifest.Issue{}, Secrets: []engine.Secret{}}
	m, err := load(file, stdin)
	if err != nil {
		v.Issues = append(v.Issues, manifest.Issue{Path: "", Message: err.Error()})
		return v
	}
	cat := providers.Catalog()
	v.Issues = append(v.Issues, m.Validate(cat)...)
	v.Valid = len(v.Issues) == 0
	if out, err := m.Marshal(cat); err == nil {
		v.Manifest = string(out)
	}
	e := engine.New(m, secrets.NewStore(""), stack.NewConsole(io.Discard))
	r := e.Resources()
	v.Resources.MemoryMiB, v.Resources.DiskGiB, v.Resources.MinServerMiB = r.MemoryMiB, r.DiskGiB, catalog.MinServerMiB(r.MemoryMiB)
	v.Secrets = e.Secrets()
	return v
}

func cmdValidate(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	file := fs.String("f", manifestPath(), "manifest path, or - for stdin")
	jsonOut := fs.Bool("json", false, "print JSON and always exit 0")
	if err := parse(fs, args); err != nil {
		return err
	}
	v := validate(*file, stdin)
	if *jsonOut {
		return printJSON(stdout, v)
	}
	if v.Valid {
		fmt.Fprintf(stdout, "%s is valid\n", *file)
		return nil
	}
	for _, issue := range v.Issues {
		if issue.Path == "" {
			return errors.New(issue.Message)
		}
		fmt.Fprintf(stdout, "  ✗ %s\n", issue)
	}
	return fmt.Errorf("%d problem(s) in %s", len(v.Issues), *file)
}

// parse parses flags and refuses positional arguments, which would
// otherwise be ignored silently.
func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%s: unexpected argument %q", fs.Name(), fs.Arg(0))
	}
	return nil
}

// printSummary tells the user where their stack is and how to sign in.
func printSummary(s *session) {
	out := s.engine.RT.Out
	m, cat := s.engine.Manifest, s.engine.Catalog
	name := func(aspect string) string {
		if p, ok := cat.Provider(aspect, m.Provider(cat, aspect)); ok {
			return p.Name
		}
		return aspect
	}
	c := s.console
	c.Printf("")
	c.Printf("Your stack is ready.")
	if out.Ingress != nil && out.Ingress.DashboardURL != "" {
		c.Printf("  %-18s %s", name("ingress")+" dashboard", out.Ingress.DashboardURL)
	}
	if out.Images != nil {
		line := fmt.Sprintf("  %-18s %s/%s  (user %s)", name("images"), out.Images.Host, out.Images.Project, out.Images.PushUsername)
		if out.Images.Login != "" {
			line = fmt.Sprintf("  %-18s https://%s  (%s)", name("images"), out.Images.Host, out.Images.Login)
		}
		c.Printf("%s", line)
	}
	if out.CICD != nil {
		line := fmt.Sprintf("  %-18s %s", name("cicd"), out.CICD.URL)
		if out.CICD.Login != "" {
			line += "  (" + out.CICD.Login + ")"
		}
		c.Printf("%s", line)
	}
	c.Printf("  %-18s https://<app>.%s (each infrastructure/<app>/ in your repos)", "Apps", m.Domain)
	if out.VCS != nil && out.VCS.ClusterRepoURL != "" {
		c.Printf("  %-18s %s (mirrors the applied manifest)", "Cluster repo", out.VCS.ClusterRepoURL)
	}
	c.Printf("  %-18s %s and %s on this server", "Manifest, secrets", manifestPath(), s.store.Path)
	c.Printf("  %-18s ownstackctl status", "Health")
}

// setupHealth installs the timer that reports health to the run's Ownstack
// stack, and starts the first report. Failures are reported but never fail
// the run.
func setupHealth(ctx context.Context, s *session, reportURL string) {
	url, token := health.URLFromReport(reportURL), os.Getenv("OWNSTACK_REPORT_TOKEN")
	if url == "" || os.Geteuid() != 0 {
		return
	}
	if err := health.Install(ctx, s.engine.RT.Exec, url, token); err != nil {
		s.console.Printf("note: health reports to Ownstack are not set up: %v", err)
		return
	}
	s.console.Printf("Health is reported to Ownstack every %s (stop: systemctl disable --now ownstack-health.timer)", health.Interval)
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
