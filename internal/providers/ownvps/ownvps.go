// Package ownvps implements the compute contract on the server ownstackctl
// runs on: a single-node k3s cluster with its bundled Traefik disabled.
package ownvps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/stack"
)

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.Compute = &stack.ComputeOutput{
		PublicIP:     rt.Setting("host"),
		Kubeconfig:   stack.Kubeconfig,
		StorageClass: "local-path",
		APIServer:    "https://" + rt.Setting("host") + ":6443",
		// ownstackctl runs on the node, so its own architecture is the node's.
		Arch: runtime.GOARCH,
	}
	if ca, err := os.ReadFile(serverCA); err == nil {
		rt.Out.Compute.CACert = string(ca)
	}
	return nil
}

const serverCA = "/var/lib/rancher/k3s/server/tls/server-ca.crt"

// k3sConfig is the k3s config file; tests point it at a temporary directory.
var k3sConfig = "/etc/rancher/k3s/config.yaml"

// helmVersion is the Helm release installed on servers that have none. The
// official install script verifies its checksum.
const helmVersion = "v3.22.0"

// ensureK3sConfig merges Ownstack's settings into the k3s config file: the
// bundled Traefik is disabled (Ownstack installs its own) and the public IP is
// a TLS SAN of the API server, so remote CI can deploy. It reports whether
// the file changed.
func ensureK3sConfig(host string) (bool, error) {
	config := map[string]any{}
	data, err := os.ReadFile(k3sConfig)
	if err == nil {
		if err := yaml.Unmarshal(data, &config); err != nil {
			return false, fmt.Errorf("%s: %w", k3sConfig, err)
		}
		if config == nil {
			config = map[string]any{}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	// k3s splits both settings on commas (util.SplitStringSlice) and trims
	// the components it disables.
	changed := addToList(config, "disable", "traefik", true)
	changed = addToList(config, "tls-san", host, false) || changed
	if !changed {
		return false, nil
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(k3sConfig), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(k3sConfig, append([]byte("# Managed in part by ownstackctl.\n"), out...), 0o600)
}

// addToList adds value to a k3s list setting unless it is already there. The
// setting may be a YAML list or a string; both may hold comma-separated
// values, as k3s splits them. Existing entries are kept as they are.
func addToList(config map[string]any, key, value string, trim bool) bool {
	var list []any
	switch v := config[key].(type) {
	case []any:
		list = v
	case string:
		list = []any{v}
	}
	for _, item := range list {
		for _, part := range strings.Split(fmt.Sprint(item), ",") {
			if trim {
				part = strings.TrimSpace(part)
			}
			if part == value {
				return false
			}
		}
	}
	config[key] = append(list, value)
	return true
}

func (Provider) Plan(ctx context.Context, rt *stack.Runtime) []stack.Step {
	var steps []stack.Step
	if missing := missingTools(rt); len(missing) > 0 {
		steps = append(steps, stack.Step{Action: "install", Target: "base packages", Detail: strings.Join(missing, ", ")})
	}
	if gib := rt.SettingInt("swapGiB"); gib > 0 && swapTotalKiB() == 0 {
		steps = append(steps, stack.Step{Action: "create", Target: "swapfile", Detail: fmt.Sprintf("%d GiB, swappiness 10 (the server has no swap)", gib)})
	}
	if version, ok := k3sVersion(ctx, rt); ok {
		want := rt.Setting("k3sVersion")
		switch action, err := k3sAction(version, want); {
		case err != nil:
			steps = append(steps, stack.Step{Action: "verify", Target: "k3s", Detail: version + " installed; apply refuses: " + err.Error()})
		case action == k3sUpgrade:
			steps = append(steps, stack.Step{Action: "upgrade", Target: "k3s", Detail: version + " -> " + normalizeK3sVersion(want) + "; restarts the API server briefly"})
		default:
			steps = append(steps, stack.Step{Action: "verify", Target: "k3s", Detail: version + " installed"})
		}
	} else {
		steps = append(steps, stack.Step{Action: "install", Target: "k3s", Detail: k3sTarget(rt) + ", bundled Traefik disabled, API certificate valid for " + rt.Setting("host")})
	}
	if rt.Exec.Has("helm") {
		steps = append(steps, stack.Step{Action: "verify", Target: "helm", Detail: "installed"})
	} else {
		steps = append(steps, stack.Step{Action: "install", Target: "helm", Detail: helmVersion + ", official install script with checksum verification"})
	}
	return steps
}

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("own-vps must be applied as root on the server itself")
	}

	if missing := missingTools(rt); len(missing) > 0 {
		rt.Logf("Installing base packages: %s", strings.Join(missing, ", "))
		err := stack.Retry(ctx, rt.Console, "apt install", 5, 15*time.Second, func() error {
			return rt.Exec.Shell(ctx, "export DEBIAN_FRONTEND=noninteractive\napt-get update -qq\napt-get install -y -qq ca-certificates curl git jq")
		})
		if err != nil {
			return err
		}
	}

	if err := ensureSwap(ctx, rt); err != nil {
		return err
	}

	// Decide about the running k3s before touching its config, so a refused
	// k3sVersion leaves the node as it was.
	version, running := k3sVersion(ctx, rt)
	action := k3sNone
	if running {
		var err error
		if action, err = k3sAction(version, rt.Setting("k3sVersion")); err != nil {
			return fmt.Errorf("k3s %s is running: %w", version, err)
		}
	}
	configChanged, err := ensureK3sConfig(rt.Setting("host"))
	if err != nil {
		return err
	}
	switch {
	case running && action == k3sUpgrade:
		// The install script restarts k3s, which also loads a changed config.
		rt.Logf("Upgrading k3s %s -> %s; the API server restarts briefly", version, normalizeK3sVersion(rt.Setting("k3sVersion")))
		if err := installK3s(ctx, rt); err != nil {
			return err
		}
	case running:
		rt.Logf("k3s %s already running", version)
		if configChanged {
			rt.Logf("k3s configuration changed; restarting k3s")
			if err := rt.Exec.Run(ctx, "systemctl", "restart", "k3s"); err != nil {
				return err
			}
		}
	default:
		rt.Logf("Installing k3s (%s)", k3sTarget(rt))
		if err := installK3s(ctx, rt); err != nil {
			return err
		}
	}

	// Convenience for operators running kubectl by hand.
	if data, err := os.ReadFile(stack.Kubeconfig); err == nil {
		_ = os.MkdirAll("/root/.kube", 0o700)
		_ = os.WriteFile("/root/.kube/config", data, 0o600)
	}

	if !rt.Exec.Has("helm") {
		rt.Logf("Installing helm %s", helmVersion)
		if err := stack.Retry(ctx, rt.Console, "helm install", 5, 15*time.Second, func() error {
			return rt.Exec.Shell(ctx, helmInstallScript)
		}); err != nil {
			return err
		}
	}

	defer func() {
		if ca, err := os.ReadFile(serverCA); err == nil {
			rt.Out.Compute.CACert = string(ca)
		}
	}()
	return stack.Retry(ctx, rt.Console, "node ready", 40, 5*time.Second, func() error {
		nodes, err := rt.Kube.Nodes(ctx)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.Ready {
				rt.Logf("Node %s is ready (%s)", n.Name, n.Version)
				return nil
			}
		}
		return fmt.Errorf("no ready node yet")
	})
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	var checks []stack.Check
	if version, ok := k3sVersion(ctx, rt); ok {
		check := stack.Check{Name: "k3s", State: stack.OK, Detail: version}
		want := rt.Setting("k3sVersion")
		if action, err := k3sAction(version, want); err != nil {
			check.State, check.Detail = stack.Warn, version+"; apply refuses: "+err.Error()
		} else if action == k3sUpgrade {
			check.State, check.Detail = stack.Warn, version+"; k3sVersion pins "+normalizeK3sVersion(want)+", rerun apply to upgrade"
		}
		checks = append(checks, check)
	} else {
		checks = append(checks, stack.Check{Name: "k3s", State: stack.Fail, Detail: "k3s service is not active"})
		return checks
	}

	nodes, err := rt.Kube.Nodes(ctx)
	switch {
	case err != nil:
		checks = append(checks, stack.Check{Name: "node", State: stack.Fail, Detail: err.Error()})
	case len(nodes) == 0:
		checks = append(checks, stack.Check{Name: "node", State: stack.Fail, Detail: "no nodes registered"})
	default:
		for _, n := range nodes {
			state := stack.OK
			if !n.Ready {
				state = stack.Fail
			}
			checks = append(checks, stack.Check{Name: "node " + n.Name, State: state, Detail: readyWord(n.Ready)})
		}
	}

	if total, avail, ok := memory(); ok {
		pct := avail * 100 / total
		state := stack.OK
		if pct < 10 {
			state = stack.Warn
		}
		checks = append(checks, stack.Check{Name: "memory", State: state,
			Detail: fmt.Sprintf("%d MiB of %d MiB available", avail/1024, total/1024)})
	}
	if out, err := rt.Exec.Output(ctx, "df", "-Pk", "/"); err == nil {
		if total, avail, ok := parseDF(out); ok {
			pct := avail * 100 / total
			state := stack.OK
			if pct < 10 {
				state = stack.Warn
			}
			checks = append(checks, stack.Check{Name: "disk", State: state,
				Detail: fmt.Sprintf("%d GiB of %d GiB free", avail/1024/1024, total/1024/1024)})
		}
	}
	return checks
}

// ensureSwap creates a swapfile when the server has no swap at all. Memory
// spikes (image builds, Jenkins starting) then slow down instead of getting
// services killed. Existing swap, of any size, is left alone.
func ensureSwap(ctx context.Context, rt *stack.Runtime) error {
	gib := rt.SettingInt("swapGiB")
	if gib <= 0 || swapTotalKiB() > 0 {
		return nil
	}
	rt.Logf("Creating a %d GiB swapfile (the server has no swap)", gib)
	script := fmt.Sprintf(`set -e
fallocate -l %dG /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=%d status=none
chmod 600 /swapfile
mkswap -q /swapfile
swapon /swapfile
grep -q '^/swapfile ' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
sysctl -q -w vm.swappiness=10
echo 'vm.swappiness=10' > /etc/sysctl.d/90-ownstack-swap.conf
`, gib, gib*1024)
	return rt.Exec.Shell(ctx, script)
}

func swapTotalKiB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfo(string(data))["SwapTotal"]
}

func missingTools(rt *stack.Runtime) []string {
	var missing []string
	for _, tool := range []string{"curl", "git", "jq"} {
		if !rt.Exec.Has(tool) {
			missing = append(missing, tool)
		}
	}
	return missing
}

func k3sVersion(ctx context.Context, rt *stack.Runtime) (string, bool) {
	if _, err := rt.Exec.Output(ctx, "systemctl", "is-active", "--quiet", "k3s"); err != nil {
		return "", false
	}
	out, err := rt.Exec.Output(ctx, "k3s", "--version")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(out)
	if len(fields) >= 3 {
		return fields[2], true
	}
	return out, true
}

func k3sTarget(rt *stack.Runtime) string {
	if v := rt.Setting("k3sVersion"); v != "" {
		return "version " + normalizeK3sVersion(v)
	}
	return "channel " + rt.Setting("k3sChannel")
}

func k3sInstallEnv(rt *stack.Runtime) string {
	if v := rt.Setting("k3sVersion"); v != "" {
		return "INSTALL_K3S_VERSION=" + shellQuote(normalizeK3sVersion(v))
	}
	return "INSTALL_K3S_CHANNEL=" + shellQuote(rt.Setting("k3sChannel"))
}

// installK3s runs the official install script, which installs k3s or
// upgrades it in place and (re)starts it.
func installK3s(ctx context.Context, rt *stack.Runtime) error {
	script := "curl -sfL https://get.k3s.io | " + k3sInstallEnv(rt) + " sh -s -"
	return stack.Retry(ctx, rt.Console, "k3s install", 5, 15*time.Second, func() error {
		return rt.Exec.Shell(ctx, script)
	})
}

// helmInstallScript installs helmVersion with the install script of that
// same release; the script verifies the download's checksum by default.
var helmInstallScript = "curl -fsSL https://raw.githubusercontent.com/helm/helm/" + helmVersion + "/scripts/get-helm-3 | DESIRED_VERSION=" + helmVersion + " bash"

// What apply does with a running k3s.
const (
	k3sNone    = "none"
	k3sUpgrade = "upgrade"
)

var k3sRelease = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?(?:\+k3s([0-9]+))?$`)

type k3sRel struct {
	major, minor, patch int
	pre                 bool // a pre-release such as -rc1
	build               int  // the N of +k3sN; -1 when not given
}

func parseK3sVersion(v string) (k3sRel, bool) {
	m := k3sRelease.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return k3sRel{}, false
	}
	n := func(s string) int { i, _ := strconv.Atoi(s); return i }
	r := k3sRel{major: n(m[1]), minor: n(m[2]), patch: n(m[3]), pre: m[4] != "", build: -1}
	if m[5] != "" {
		r.build = n(m[5])
	}
	return r, true
}

// normalizeK3sVersion adds the leading v that k3s release tags have.
func normalizeK3sVersion(v string) string {
	if v != "" && !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// k3sAction decides what apply does when k3s is running installed and the
// manifest pins want. Without a pin nothing changes: a channel only applies
// to new installs. Upgrades go one minor version at a time, as Kubernetes
// supports; downgrades are refused. A pin without +k3sN matches any build.
func k3sAction(installed, want string) (string, error) {
	if want == "" {
		return k3sNone, nil
	}
	w, ok := parseK3sVersion(want)
	if !ok {
		return "", fmt.Errorf("k3sVersion %q is not a k3s release such as v1.33.4+k3s1", want)
	}
	have, ok := parseK3sVersion(installed)
	if !ok {
		return "", fmt.Errorf("cannot read the installed k3s version %q", installed)
	}
	if w.major != have.major {
		return "", fmt.Errorf("k3sVersion %s changes the major version of %s; k3s does not support that", normalizeK3sVersion(want), installed)
	}
	cmp := func(a, b int) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}
	order := cmp(w.minor, have.minor)
	if order == 0 {
		order = cmp(w.patch, have.patch)
	}
	if order == 0 && w.build >= 0 && have.build >= 0 {
		order = cmp(w.build, have.build)
	}
	if order == 0 && have.pre && !w.pre {
		order = 1 // the release after its release candidate
	}
	switch {
	case order == 0:
		return k3sNone, nil
	case order < 0:
		return "", fmt.Errorf("k3sVersion %s is older than the running %s; k3s does not support downgrades", normalizeK3sVersion(want), installed)
	case w.minor > have.minor+1:
		return "", fmt.Errorf("k3sVersion %s skips a minor version from %s; upgrade one minor version at a time: pin v%d.%d first", normalizeK3sVersion(want), installed, have.major, have.minor+1)
	case w.build < 0:
		return "", fmt.Errorf("k3sVersion %s names no build; k3s releases are tagged like v%d.%d.%d+k3s1", normalizeK3sVersion(want), w.major, w.minor, w.patch)
	}
	return k3sUpgrade, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func readyWord(ready bool) string {
	if ready {
		return "Ready"
	}
	return "NotReady"
}

// memory returns MemTotal and MemAvailable in KiB from /proc/meminfo.
func memory() (total, avail int, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	info := parseMeminfo(string(data))
	return info["MemTotal"], info["MemAvailable"], info["MemTotal"] > 0
}

// parseMeminfo reads /proc/meminfo's "Key: value kB" lines.
func parseMeminfo(data string) map[string]int {
	out := map[string]int{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasSuffix(fields[0], ":") {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err == nil {
			out[strings.TrimSuffix(fields[0], ":")] = v
		}
	}
	return out
}

// parseDF reads total and available KiB from `df -Pk` output.
func parseDF(out string) (total, avail int, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, 0, false
	}
	total, err1 := strconv.Atoi(fields[1])
	avail, err2 := strconv.Atoi(fields[3])
	return total, avail, err1 == nil && err2 == nil && total > 0
}
