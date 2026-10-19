// Package ownvps implements the compute contract on the server ownstackctl
// runs on: a single-node k3s cluster with its bundled Traefik disabled.
package ownvps

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
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

const (
	k3sConfig = "/etc/rancher/k3s/config.yaml"
	serverCA  = "/var/lib/rancher/k3s/server/tls/server-ca.crt"
)

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
	changed := addToList(config, "disable", "traefik")
	changed = addToList(config, "tls-san", host) || changed
	if !changed {
		return false, nil
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll("/etc/rancher/k3s", 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(k3sConfig, append([]byte("# Managed in part by ownstackctl.\n"), out...), 0o600)
}

func addToList(config map[string]any, key, value string) bool {
	var list []any
	switch v := config[key].(type) {
	case []any:
		list = v
	case string:
		list = []any{v}
	}
	for _, item := range list {
		if fmt.Sprint(item) == value {
			return false
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
		steps = append(steps, stack.Step{Action: "verify", Target: "k3s", Detail: version + " installed"})
	} else {
		steps = append(steps, stack.Step{Action: "install", Target: "k3s", Detail: k3sTarget(rt) + ", bundled Traefik disabled, API certificate valid for " + rt.Setting("host")})
	}
	if rt.Exec.Has("helm") {
		steps = append(steps, stack.Step{Action: "verify", Target: "helm", Detail: "installed"})
	} else {
		steps = append(steps, stack.Step{Action: "install", Target: "helm", Detail: "official install script"})
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

	configChanged, err := ensureK3sConfig(rt.Setting("host"))
	if err != nil {
		return err
	}
	if version, ok := k3sVersion(ctx, rt); ok {
		rt.Logf("k3s %s already running", version)
		if configChanged {
			rt.Logf("k3s configuration changed; restarting k3s")
			if err := rt.Exec.Run(ctx, "systemctl", "restart", "k3s"); err != nil {
				return err
			}
		}
	} else {
		rt.Logf("Installing k3s (%s)", k3sTarget(rt))
		script := "curl -sfL https://get.k3s.io | " + k3sInstallEnv(rt) + " sh -s -"
		if err := stack.Retry(ctx, rt.Console, "k3s install", 5, 15*time.Second, func() error {
			return rt.Exec.Shell(ctx, script)
		}); err != nil {
			return err
		}
	}

	// Convenience for operators running kubectl by hand.
	if data, err := os.ReadFile(stack.Kubeconfig); err == nil {
		_ = os.MkdirAll("/root/.kube", 0o700)
		_ = os.WriteFile("/root/.kube/config", data, 0o600)
	}

	if !rt.Exec.Has("helm") {
		rt.Logf("Installing helm")
		if err := stack.Retry(ctx, rt.Console, "helm install", 5, 15*time.Second, func() error {
			return rt.Exec.Shell(ctx, "curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash")
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
		checks = append(checks, stack.Check{Name: "k3s", State: stack.OK, Detail: version})
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
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "SwapTotal:" {
			v, _ := strconv.Atoi(fields[1])
			return v
		}
	}
	return 0
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
		return "version " + v
	}
	return "channel " + rt.Setting("k3sChannel")
}

func k3sInstallEnv(rt *stack.Runtime) string {
	if v := rt.Setting("k3sVersion"); v != "" {
		return "INSTALL_K3S_VERSION=" + shellQuote(v)
	}
	return "INSTALL_K3S_CHANNEL=" + shellQuote(rt.Setting("k3sChannel"))
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
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.Atoi(fields[1])
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	return total, avail, total > 0
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
