// Package health sends the stack's status to Ownstack and installs the
// systemd timer that keeps doing so, which is what lets the Ownstack map show
// live health. It is opt-in: it only runs when a run was started with an
// Ownstack report URL, it sends only `ownstackctl status --json` (names,
// states, and details; no secret values), and it can be removed with
// `systemctl disable --now ownstack-health.timer`.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

const (
	envFile     = "/etc/ownstack/health.env"
	serviceFile = "/etc/systemd/system/ownstack-health.service"
	timerFile   = "/etc/systemd/system/ownstack-health.timer"
	// Interval between health reports.
	Interval = 5 * time.Minute
)

// URLFromReport derives the health endpoint from a run's progress report URL
// (…/agent/jobs/<id>/report → …/agent/jobs/<id>/health).
func URLFromReport(reportURL string) string {
	if strings.HasSuffix(reportURL, "/report") {
		return strings.TrimSuffix(reportURL, "/report") + "/health"
	}
	return ""
}

// Send posts one status document.
func Send(ctx context.Context, url, token string, status any) error {
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("Ownstack answered HTTP %d", res.StatusCode)
	}
	return nil
}

// Install writes the timer that reports status every Interval. manifest and
// secretsFile are the absolute paths of ownstack.yaml and of the secrets file
// the run used on this server.
func Install(ctx context.Context, exec *stack.Exec, url, token, manifest, secretsFile string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll("/etc/ownstack", 0o700); err != nil {
		return err
	}
	env := "OWNSTACK_HEALTH_URL=" + strconv.Quote(url) + "\nOWNSTACK_REPORT_TOKEN=" + strconv.Quote(token) + "\n"
	if err := os.WriteFile(envFile, []byte(env), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(serviceFile, []byte(serviceUnit(binary, manifest, secretsFile)), 0o644); err != nil {
		return err
	}
	timer := fmt.Sprintf(`# Managed by ownstackctl.
[Unit]
Description=Report Ownstack stack health every %s

[Timer]
OnBootSec=2min
OnUnitActiveSec=%s
RandomizedDelaySec=30s

[Install]
WantedBy=timers.target
`, Interval, Interval)
	if err := os.WriteFile(timerFile, []byte(timer), 0o644); err != nil {
		return err
	}
	if err := exec.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return exec.Run(ctx, "systemctl", "enable", "--now", "ownstack-health.timer")
}

func serviceUnit(binary, manifest, secretsFile string) string {
	return fmt.Sprintf(`# Managed by ownstackctl: reports stack health to Ownstack.
# Disable with: systemctl disable --now ownstack-health.timer
[Unit]
Description=Report Ownstack stack health
After=network-online.target k3s.service

[Service]
Type=oneshot
EnvironmentFile=%s
ExecStart=%s status -f %s --secrets-file %s --no-prompt --health-url ${OWNSTACK_HEALTH_URL}
# status exits 2 when something is failing; that is a report, not an error.
SuccessExitStatus=2
`, envFile, binary, systemdQuote(manifest), systemdQuote(secretsFile))
}

// systemdQuote makes s one ExecStart argument: double-quoted, with quotes and
// backslashes escaped and systemd's specifier (%) and variable ($)
// expansion disabled.
func systemdQuote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s)
	return `"` + s + `"`
}
