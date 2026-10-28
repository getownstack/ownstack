// Command lockgen resolves the Jenkins plugin lock: every plugin in
// plugins.in and its non-optional dependencies, at the versions the Jenkins
// update center offers for the Jenkins core of one jenkins/jenkins Helm chart.
//
// Run it from the repository:
//
//	go run ./internal/providers/jenkins/lockgen                # re-resolve for the locked chart
//	go run ./internal/providers/jenkins/lockgen -chart 5.9.72  # a specific chart
//	go run ./internal/providers/jenkins/lockgen -latest-chart  # the newest 5.x/6.x chart
//
// It writes internal/providers/jenkins/plugins.lock, sets JenkinsChartVersion
// in internal/catalog/data.go to the lock's chart, and regenerates the Jenkins
// provider's golden Helm values. It refuses a set where a plugin needs a newer
// core or a newer dependency than the update center offers, and, unless
// -allow-warnings, one where a published security warning matches a locked
// version or the core.
//
// Exit status: 0 on success, 1 when the plugins cannot be locked, 2 for usage
// errors, 3 when only security warnings stopped it. `go run` reports every
// failure as status 1, so scripts that branch on the status build lockgen
// first (go build -o <path> ./internal/providers/jenkins/lockgen) and run the
// binary from the repository. The change summary goes to standard output,
// diagnostics to standard error.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
)

const (
	module = "github.com/getownstack/ownstack"

	inputPath  = "internal/providers/jenkins/plugins.in"
	lockPath   = "internal/providers/jenkins/plugins.lock"
	dataGoPath = "internal/catalog/data.go"

	exitFailed     = 1
	exitUsage      = 2
	exitAdvisories = 3
)

type config struct {
	root          string
	chart         string
	latestChart   bool
	allowWarnings bool
	// golden regenerates the provider's golden values with go test.
	golden bool

	indexURL        string
	updateCenterURL func(core string) string
	// imageTagURL returns the registry URL that answers 200 when the
	// controller image tag exists; nil skips the check.
	imageTagURL func(tag string) string
	client      *http.Client

	stdout, stderr io.Writer
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg := config{
		indexURL:        "https://charts.jenkins.io/index.yaml",
		updateCenterURL: pluginlock.UpdateCenterURL,
		imageTagURL: func(tag string) string {
			return "https://hub.docker.com/v2/namespaces/jenkins/repositories/jenkins/tags/" + tag
		},
		client: &http.Client{Timeout: 2 * time.Minute},
		golden: true,
		stdout: stdout, stderr: stderr,
	}
	fs := flag.NewFlagSet("lockgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.chart, "chart", "", "jenkins/jenkins chart version to lock for (default: the locked chart)")
	fs.BoolVar(&cfg.latestChart, "latest-chart", false, "lock for the newest released 5.x or 6.x chart")
	fs.BoolVar(&cfg.allowWarnings, "allow-warnings", false, "lock even when security warnings match a locked version (they are printed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || (cfg.chart != "" && cfg.latestChart) {
		fmt.Fprintln(stderr, "usage: lockgen [-chart <version> | -latest-chart] [-allow-warnings]")
		return exitUsage
	}
	root, err := findRoot()
	if err != nil {
		fmt.Fprintln(stderr, "lockgen:", err)
		return exitUsage
	}
	cfg.root = root
	return generate(ctx, cfg)
}

// findRoot is the repository root: the nearest directory up from the working
// directory whose go.mod declares this module.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && bytes.Contains(data, []byte("module "+module+"\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("run lockgen inside the %s repository", module)
		}
		dir = parent
	}
}

// errAdvisories means only security warnings stopped the lock.
var errAdvisories = errors.New("security warnings")

func generate(ctx context.Context, cfg config) int {
	err := lock(ctx, cfg)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errAdvisories):
		fmt.Fprintln(cfg.stderr, "lockgen: security warnings match the locked set; wait for fixed releases, drop the plugin, or rerun with -allow-warnings to lock anyway")
		return exitAdvisories
	default:
		fmt.Fprintln(cfg.stderr, "lockgen:", err)
		return exitFailed
	}
}

func lock(ctx context.Context, cfg config) error {
	path := func(rel string) string { return filepath.Join(cfg.root, filepath.FromSlash(rel)) }
	input, err := os.ReadFile(path(inputPath))
	if err != nil {
		return err
	}
	requested, err := pluginlock.ParseInput(input)
	if err != nil {
		return err
	}
	var prev *pluginlock.Lock
	if data, err := os.ReadFile(path(lockPath)); err == nil {
		if prev, err = pluginlock.ParseLock(data); err != nil {
			fmt.Fprintf(cfg.stderr, "lockgen: ignoring the current lock: %v\n", err)
			prev = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	charts, err := fetchCharts(ctx, cfg)
	if err != nil {
		return err
	}
	chart := cfg.chart
	switch {
	case cfg.latestChart:
		if chart, err = latestChart(charts); err != nil {
			return err
		}
	case chart == "" && prev != nil:
		chart = prev.Chart
	case chart == "":
		return fmt.Errorf("no plugins.lock yet: pass -chart <version> or -latest-chart")
	}
	core, ok := charts[chart]
	if !ok {
		return fmt.Errorf("chart jenkins %s is not in %s", chart, cfg.indexURL)
	}
	if !coreRelease.MatchString(core) {
		return fmt.Errorf("chart jenkins %s has appVersion %q, which is not a Jenkins release", chart, core)
	}
	fmt.Fprintf(cfg.stderr, "Resolving %d plugins for chart %s (Jenkins %s)\n", len(requested), chart, core)

	if cfg.imageTagURL != nil {
		tag := pluginlock.ImageTag(core)
		if err := fetchOK(ctx, cfg.client, cfg.imageTagURL(tag)); err != nil {
			return fmt.Errorf("controller image %s:%s: %w", pluginlock.ImageRepository, tag, err)
		}
	}

	data, served, err := fetchFrom(ctx, cfg.client, cfg.updateCenterURL(core))
	if err != nil {
		return err
	}
	uc, err := pluginlock.ParseUpdateCenter(data)
	if err != nil {
		return err
	}
	// The update site redirects a core to the newest release of its line
	// (dynamic-stable-<version>), whose plugins may need that newer core.
	if m := updateCenterTier.FindStringSubmatch(served); m != nil && m[1] != core {
		fmt.Fprintf(cfg.stderr, "note: the update center for Jenkins %s is the one for %s; plugins that need a newer core than %s are refused\n", core, m[1], core)
	}

	res := pluginlock.Resolve(uc, core, requested)
	for _, name := range res.Deprecated {
		fmt.Fprintf(cfg.stderr, "note: %s is deprecated: %s\n", name, uc.Deprecations[name].URL)
	}
	if len(res.Problems) > 0 {
		for _, p := range res.Problems {
			fmt.Fprintln(cfg.stderr, "error:", p)
		}
		return fmt.Errorf("%d problems; plugins.lock is unchanged", len(res.Problems))
	}
	for _, a := range res.Advisories {
		fmt.Fprintln(cfg.stderr, "security warning:", a)
	}
	if len(res.Advisories) > 0 && !cfg.allowWarnings {
		return errAdvisories
	}

	// Check data.go before writing anything, so a failure leaves both as they were.
	catalogData, err := readCatalog(path(dataGoPath))
	if err != nil {
		return err
	}
	next := pluginlock.New(chart, core, res.Plugins)
	if err := writeIfChanged(path(lockPath), next.Format()); err != nil {
		return err
	}
	chartChanged, err := setCatalogChart(path(dataGoPath), catalogData, chart)
	if err != nil {
		return err
	}
	changes := pluginlock.Changes(prev, next)
	if cfg.golden && (len(changes) > 0 || chartChanged) {
		fmt.Fprintln(cfg.stderr, "Regenerating the golden Helm values")
		cmd := exec.CommandContext(ctx, "go", "test", "./internal/providers/jenkins", "-run", "^TestValuesGolden$", "-update")
		cmd.Dir, cmd.Stdout, cmd.Stderr = cfg.root, cfg.stderr, cfg.stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("regenerate golden values: %w", err)
		}
	}

	fmt.Fprintf(cfg.stdout, "Jenkins %s (chart %s, image %s): %d locked plugins\n", next.Jenkins, next.Chart, next.Image, len(next.Plugins))
	if len(changes) == 0 {
		fmt.Fprintln(cfg.stdout, "plugins.lock is up to date")
	}
	for _, c := range changes {
		fmt.Fprintln(cfg.stdout, "- "+c)
	}
	if chartChanged {
		fmt.Fprintf(cfg.stderr, "The catalog's jenkins chartVersion default is now %s: run scripts/sync-catalog.sh in the portal repo (getownstack/ownstack-portal).\n", chart)
	}
	return nil
}

// fetchCharts maps each released jenkins chart version to its appVersion,
// the Jenkins core it runs.
func fetchCharts(ctx context.Context, cfg config) (map[string]string, error) {
	data, err := fetch(ctx, cfg.client, cfg.indexURL)
	if err != nil {
		return nil, err
	}
	var index struct {
		Entries map[string][]struct {
			Version    string `yaml:"version"`
			AppVersion string `yaml:"appVersion"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.indexURL, err)
	}
	charts := map[string]string{}
	for _, e := range index.Entries["jenkins"] {
		charts[e.Version] = e.AppVersion
	}
	if len(charts) == 0 {
		return nil, fmt.Errorf("%s lists no jenkins chart", cfg.indexURL)
	}
	return charts, nil
}

var (
	releaseVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	// coreRelease is a Jenkins LTS (2.580.1) or weekly (2.581) version.
	coreRelease = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
	// updateCenterTier finds the core an update center URL is tailored to.
	updateCenterTier = regexp.MustCompile(`/dynamic-(?:stable-)?([0-9][0-9.]*[0-9])/`)
)

// latestChart is the highest released (not pre-release) 5.x or 6.x chart.
// A new major version changes values and needs a deliberate upgrade.
func latestChart(charts map[string]string) (string, error) {
	best, bestKey := "", [3]int{}
	for version := range charts {
		m := releaseVersion.FindStringSubmatch(version)
		if m == nil {
			continue
		}
		var key [3]int
		for i := range key {
			key[i], _ = strconv.Atoi(m[i+1])
		}
		if key[0] != 5 && key[0] != 6 {
			continue
		}
		if best == "" || key[0] > bestKey[0] || key[0] == bestKey[0] && (key[1] > bestKey[1] || key[1] == bestKey[1] && key[2] > bestKey[2]) {
			best, bestKey = version, key
		}
	}
	if best == "" {
		return "", fmt.Errorf("no released 5.x or 6.x jenkins chart")
	}
	return best, nil
}

// jenkinsChartConst is the JenkinsChartVersion line in data.go.
var jenkinsChartConst = regexp.MustCompile(`(?m)^(const JenkinsChartVersion = )"([^"]*)"$`)

// readCatalog reads data.go and checks it has exactly one JenkinsChartVersion line.
func readCatalog(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if n := len(jenkinsChartConst.FindAllIndex(data, -1)); n != 1 {
		return nil, fmt.Errorf("%s: want one `const JenkinsChartVersion = \"...\"` line, found %d", path, n)
	}
	return data, nil
}

// setCatalogChart sets JenkinsChartVersion in data.go (read by readCatalog)
// and reports whether it changed.
func setCatalogChart(path string, data []byte, chart string) (bool, error) {
	if m := jenkinsChartConst.FindSubmatch(data); m != nil && string(m[2]) == chart {
		return false, nil
	}
	out := jenkinsChartConst.ReplaceAll(data, []byte(`${1}"`+chart+`"`))
	return true, writeIfChanged(path, out)
}

func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func request(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ownstack-lockgen")
	return client.Do(req)
}

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	data, _, err := fetchFrom(ctx, client, url)
	return data, err
}

// fetchFrom returns a body and the URL it came from after redirects.
func fetchFrom(ctx context.Context, client *http.Client, url string) ([]byte, string, error) {
	res, err := request(ctx, client, url)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s: HTTP %d", url, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 256<<20))
	if err != nil {
		return nil, "", fmt.Errorf("GET %s: %w", url, err)
	}
	return data, res.Request.URL.String(), nil
}

func fetchOK(ctx context.Context, client *http.Client, url string) error {
	res, err := request(ctx, client, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("not published (%s)", url)
	}
	return fmt.Errorf("GET %s: HTTP %d", url, res.StatusCode)
}
