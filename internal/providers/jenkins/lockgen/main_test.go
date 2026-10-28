package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getownstack/ownstack/internal/providers/jenkins/pluginlock"
)

const index = `apiVersion: v1
entries:
  jenkins:
  - version: 7.0.0
    appVersion: 2.600.1
  - version: 6.1.0
    appVersion: 2.590.1
  - version: 6.2.0-rc.1
    appVersion: 2.595.1
  - version: 5.9.72
    appVersion: 2.580.1
  - version: 5.8.79
    appVersion: 2.516.1
  - version: 5.8.80
    appVersion: 2.516.2
`

// fakeUpstream serves the chart index, the update center fixture for every
// core (2.516.1 redirected to 2.516.3), and Docker Hub tags (all but
// 2.516.2-jdk21 exist).
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	uc, err := os.ReadFile("../pluginlock/testdata/update-center.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.yaml":
			w.Write([]byte(index))
		case r.URL.Path == "/uc":
			// Like updates.jenkins.io: a core goes to its line's newest release.
			version := r.URL.Query().Get("version")
			if version == "2.516.1" {
				version = "2.516.3"
			}
			http.Redirect(w, r, "/dynamic-stable-"+version+"/update-center.actual.json", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/update-center.actual.json"):
			w.Write(uc)
		case r.URL.Path == "/tags/2.516.2-jdk21":
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/tags/"):
			w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fixtureRepo struct {
	t    *testing.T
	root string
	srv  *httptest.Server
}

func newRepo(t *testing.T, plugins string) *fixtureRepo {
	t.Helper()
	root := t.TempDir()
	for path, content := range map[string]string{
		inputPath:   plugins,
		chartGoPath: "package jenkins\n\nconst ChartVersion = \"5.8.79\"\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &fixtureRepo{t, root, fakeUpstream(t)}
}

func (r *fixtureRepo) run(args ...string) (code int, stdout, stderr string) {
	r.t.Helper()
	var out, errOut bytes.Buffer
	cfg := config{
		root:            r.root,
		indexURL:        r.srv.URL + "/index.yaml",
		updateCenterURL: func(core string) string { return r.srv.URL + "/uc?version=" + core },
		imageTagURL:     func(tag string) string { return r.srv.URL + "/tags/" + tag },
		client:          r.srv.Client(),
		stdout:          &out, stderr: &errOut,
	}
	for _, a := range args {
		switch {
		case a == "-latest-chart":
			cfg.latestChart = true
		case a == "-allow-warnings":
			cfg.allowWarnings = true
		case strings.HasPrefix(a, "-chart="):
			cfg.chart = strings.TrimPrefix(a, "-chart=")
		default:
			r.t.Fatalf("unknown arg %s", a)
		}
	}
	return generate(context.Background(), cfg), out.String(), errOut.String()
}

func (r *fixtureRepo) read(path string) string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(path)))
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatal(err)
	}
	return string(data)
}

func TestGenerateLock(t *testing.T) {
	repo := newRepo(t, "# top level\napp\n")

	if code, _, stderr := repo.run(); code != exitFailed || !strings.Contains(stderr, "no plugins.lock yet") {
		t.Fatalf("first run without a chart: %d %s", code, stderr)
	}

	code, stdout, stderr := repo.run("-chart=5.9.72")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := pluginlock.Header + "\n# chart: 5.9.72\n# jenkins: 2.580.1\n# image: jenkins/jenkins:2.580.1-jdk21\n" +
		"api:2.263.v941d21defef7\napp:4560.vc25b_83890c34\nlib:1.10\n"
	if got := repo.read(lockPath); got != want {
		t.Errorf("plugins.lock:\n%s", got)
	}
	if got := repo.read(chartGoPath); !strings.Contains(got, `const ChartVersion = "5.9.72"`) {
		t.Errorf("chart.go:\n%s", got)
	}
	for _, line := range []string{"Jenkins 2.580.1 (chart 5.9.72, image jenkins/jenkins:2.580.1-jdk21): 3 locked plugins", "- chart (none) → 5.9.72", "- added app 4560.vc25b_83890c34"} {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	if !strings.Contains(stderr, "The Jenkins chart is now 5.9.72") {
		t.Errorf("stderr does not mention the chart change:\n%s", stderr)
	}

	// Rerunning resolves the locked chart again; nothing changes.
	code, stdout, stderr = repo.run()
	if code != 0 || !strings.Contains(stdout, "plugins.lock is up to date") || strings.Contains(stderr, "chart is now") || repo.read(lockPath) != want {
		t.Errorf("rerun: %d\n%s\n%s", code, stdout, stderr)
	}

	// The newest released 5.x or 6.x chart: not 7.0.0, not a pre-release.
	code, stdout, stderr = repo.run("-latest-chart")
	if code != 0 || !strings.Contains(stdout, "- chart 5.9.72 → 6.1.0") || !strings.Contains(stdout, "- Jenkins 2.580.1 → 2.590.1") {
		t.Errorf("latest chart: %d\n%s\n%s", code, stdout, stderr)
	}
	if got := repo.read(chartGoPath); !strings.Contains(got, `const ChartVersion = "6.1.0"`) {
		t.Errorf("chart.go:\n%s", got)
	}
	locked := repo.read(lockPath)

	// A chart whose core is older than a plugin needs fails and changes nothing.
	code, _, stderr = repo.run("-chart=5.8.79")
	if code != exitFailed || !strings.Contains(stderr, "error: app 4560.vc25b_83890c34 requires Jenkins 2.516.3, newer than 2.516.1") ||
		!strings.Contains(stderr, "note: the update center for Jenkins 2.516.1 is the one for 2.516.3") {
		t.Errorf("old core: %d\n%s", code, stderr)
	}
	if repo.read(lockPath) != locked || !strings.Contains(repo.read(chartGoPath), `"6.1.0"`) {
		t.Error("a failed run changed the lock or the chart")
	}

	for chart, msg := range map[string]string{
		"9.9.9":  "chart jenkins 9.9.9 is not in",
		"5.8.80": "controller image jenkins/jenkins:2.516.2-jdk21: not published",
	} {
		if code, _, stderr := repo.run("-chart=" + chart); code != exitFailed || !strings.Contains(stderr, msg) {
			t.Errorf("chart %s: %d\n%s", chart, code, stderr)
		}
	}
}

func TestGenerateRefusesSecurityWarnings(t *testing.T) {
	repo := newRepo(t, "vulnerable\n")
	code, _, stderr := repo.run("-chart=5.9.72")
	if code != exitAdvisories || !strings.Contains(stderr, "security warning: vulnerable 1.36: SECURITY-1 Stored XSS vulnerability") {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if repo.read(lockPath) != "" || strings.Contains(repo.read(chartGoPath), "5.9.72") {
		t.Error("a refused lock was written")
	}
	code, stdout, stderr := repo.run("-chart=5.9.72", "-allow-warnings")
	if code != 0 || !strings.Contains(stderr, "security warning: vulnerable 1.36") || !strings.Contains(stdout, "- added vulnerable 1.36") {
		t.Errorf("allow warnings: %d\n%s\n%s", code, stdout, stderr)
	}
}

func TestGenerateRejectsBadInput(t *testing.T) {
	repo := newRepo(t, "app:1.0\n")
	if code, _, stderr := repo.run("-chart=5.9.72"); code != exitFailed || !strings.Contains(stderr, "versions belong in the lock") {
		t.Errorf("versioned plugins.in: %d %s", code, stderr)
	}
	repo = newRepo(t, "dangling\n")
	if code, _, stderr := repo.run("-chart=5.9.72"); code != exitFailed || !strings.Contains(stderr, "ghost (needed by dangling)") {
		t.Errorf("missing dependency: %d %s", code, stderr)
	}
}

func TestChartNeedsOneConst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chart.go")
	for _, content := range []string{"package jenkins\n", "const ChartVersion = \"1\"\nconst ChartVersion = \"2\"\n"} {
		os.WriteFile(path, []byte(content), 0o644)
		if _, err := readChart(path); err == nil {
			t.Errorf("accepted %q", content)
		}
	}
	// lockgen checks chart.go before it writes the lock.
	repo := newRepo(t, "app\n")
	os.WriteFile(filepath.Join(repo.root, filepath.FromSlash(chartGoPath)), []byte("package jenkins\n"), 0o644)
	if code, _, stderr := repo.run("-chart=5.9.72"); code != exitFailed || !strings.Contains(stderr, "ChartVersion") || repo.read(lockPath) != "" {
		t.Errorf("chart.go without the const: %d %s", code, stderr)
	}
}

func TestLatestChart(t *testing.T) {
	if got, err := latestChart(map[string]string{"5.10.2": "", "5.9.72": "", "6.0.0-beta": "", "4.99.0": "", "10.0.0": ""}); err != nil || got != "5.10.2" {
		t.Errorf("latest = %q %v", got, err)
	}
	if _, err := latestChart(map[string]string{"4.1.0": ""}); err == nil {
		t.Error("no 5.x/6.x chart accepted")
	}
}
