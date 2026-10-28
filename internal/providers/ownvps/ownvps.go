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

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/stack"
)

// Spec is the own-vps provider's catalog entry.
var Spec = catalog.Provider{
	ID: "own-vps", Aspect: "compute", Name: "Own VPS", Mono: "VPS", Hosting: catalog.SelfHosted,
	Summary:   "Any Ubuntu server: bare metal, homelab, or a VPS from any host. k3s is installed on it and ownstackctl runs on the node itself.",
	Resources: catalog.Resources{MemoryMiB: 800, DiskGiB: 10},
	Params: []catalog.Param{
		{Key: "host", Label: "Public IP", Kind: catalog.KindIP, Required: true, Placeholder: "203.0.113.10",
			Help: "Public IPv4 address of the server. DNS records point here."},
		{Key: "swapGiB", Label: "Swap (GiB)", Kind: catalog.KindInt, Default: "2", Advanced: true,
			Help: "Swapfile created when the server has no swap, so a build spike does not take services down. 0 disables."},
	},
}

type Provider struct{}

func (Provider) Resolve(_ context.Context, rt *stack.Runtime) error {
	rt.Out.Compute = &stack.ComputeOutput{
		PublicIP:  rt.Setting("host"),
		APIServer: "https://" + rt.Setting("host") + ":6443",
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

// k3sVersion is the k3s release a node runs: the stable channel's release
// when this version of ownstackctl was cut. New nodes install it, and apply
// upgrades older nodes to it one minor version at a time.
const k3sVersion = "v1.36.5+k3s1"

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

func (Provider) Apply(ctx context.Context, rt *stack.Runtime) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("own-vps must be applied as root on the server itself")
	}

	// curl downloads k3s and Helm. Stock Ubuntu has it, so apt rarely runs.
	if !rt.Exec.Has("curl") {
		rt.Logf("Installing curl")
		err := stack.Retry(ctx, rt.Console, "apt install", 5, 15*time.Second, func() error {
			return rt.Exec.Shell(ctx, "export DEBIAN_FRONTEND=noninteractive\napt-get update -qq\napt-get install -y -qq ca-certificates curl")
		})
		if err != nil {
			return err
		}
	}

	if err := ensureSwap(ctx, rt); err != nil {
		return err
	}

	// Decide about the running k3s before touching its config, so a refused
	// upgrade leaves the node as it was.
	version, running := runningK3s(ctx, rt)
	action := k3sNone
	if running {
		var err error
		if action, err = k3sAction(version, k3sVersion); err != nil {
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
		rt.Logf("Upgrading k3s %s -> %s; the API server restarts briefly", version, k3sVersion)
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
		rt.Logf("Installing k3s %s", k3sVersion)
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
	return stack.WaitFor(ctx, rt, "node ready", 5*time.Minute, func(ctx context.Context) (bool, string) {
		return nodeReady(ctx, rt)
	})
}

// nodeReady reports whether a node is Ready, logging it when one is, or
// why not.
func nodeReady(ctx context.Context, rt *stack.Runtime) (bool, string) {
	nodes, err := rt.Kube.Nodes(ctx)
	if err != nil {
		return false, err.Error()
	}
	for _, n := range nodes {
		if n.Ready {
			rt.Logf("Node %s is ready (%s)", n.Name, n.Version)
			return true, ""
		}
	}
	return false, "no ready node yet"
}

func (Provider) Status(ctx context.Context, rt *stack.Runtime) []stack.Check {
	var checks []stack.Check
	if version, ok := runningK3s(ctx, rt); ok {
		check := stack.Check{Name: "k3s", State: stack.OK, Detail: version}
		if action, err := k3sAction(version, k3sVersion); err != nil {
			check.State, check.Detail = stack.Warn, version+"; apply refuses: "+err.Error()
		} else if action == k3sUpgrade {
			check.State, check.Detail = stack.Warn, version+"; rerun apply to upgrade to "+k3sVersion
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

// runningK3s returns the version of the k3s running on the node.
func runningK3s(ctx context.Context, rt *stack.Runtime) (string, bool) {
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

// k3sInstallScript runs the official install script for k3sVersion, which
// installs k3s or upgrades it in place and (re)starts it.
const k3sInstallScript = "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='" + k3sVersion + "' sh -s -"

func installK3s(ctx context.Context, rt *stack.Runtime) error {
	return stack.Retry(ctx, rt.Console, "k3s install", 5, 15*time.Second, func() error {
		return rt.Exec.Shell(ctx, k3sInstallScript)
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

// k3sAction decides what apply does when k3s is running installed and the
// node should run want (k3sVersion). Upgrades go one minor version at a
// time, as Kubernetes supports; downgrades are refused.
func k3sAction(installed, want string) (string, error) {
	w, ok := parseK3sVersion(want)
	if !ok {
		return "", fmt.Errorf("%q is not a k3s release", want)
	}
	have, ok := parseK3sVersion(installed)
	if !ok {
		return "", fmt.Errorf("cannot read the installed k3s version %q", installed)
	}
	if w.major != have.major {
		return "", fmt.Errorf("this ownstackctl runs k3s %s, another major version; k3s does not support that upgrade", want)
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
	if order == 0 && have.build >= 0 {
		order = cmp(w.build, have.build)
	}
	if order == 0 && have.pre && !w.pre {
		order = 1 // the release after its release candidate
	}
	switch {
	case order == 0:
		return k3sNone, nil
	case order < 0:
		return "", fmt.Errorf("this ownstackctl runs k3s %s, which is older; k3s does not support downgrades", want)
	case w.minor > have.minor+1:
		return "", fmt.Errorf("this ownstackctl runs k3s %s, more than one minor version newer; upgrade one minor version at a time: run curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=v%d.%d sh -s - and apply again", want, have.major, have.minor+1)
	}
	return k3sUpgrade, nil
}

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
