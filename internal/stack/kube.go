package stack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Kube wraps kubectl and helm. Manifests are built in Go and piped to
// `kubectl apply`, so secret values never pass through a shell.
type Kube struct {
	Exec *Exec
}

type object = map[string]any

// Apply server-side applies objects in one kubectl call, as a v1 List in
// order (a namespace before the objects in it). ownstackctl owns the fields
// it sets, and --force-conflicts takes them back from anyone who changed
// them. Server-side apply keeps no last-applied annotation, so secret values
// are stored only in the secrets themselves.
func (k *Kube) Apply(ctx context.Context, objects ...map[string]any) error {
	data, err := json.Marshal(object{"apiVersion": "v1", "kind": "List", "items": objects})
	if err != nil {
		return err
	}
	_, err = k.outputInput(ctx, string(data), "kubectl", "apply", "--server-side", "--force-conflicts", "--field-manager=ownstack", "-f", "-")
	return err
}

// ApplySecret creates or updates an Opaque (or typed) secret.
func (k *Kube) ApplySecret(ctx context.Context, namespace, name, secretType string, data map[string]string, labels map[string]string) error {
	return k.Apply(ctx, SecretObject(namespace, name, secretType, data, labels))
}

// SecretObject is an Opaque (or typed) secret holding data.
func SecretObject(namespace, name, secretType string, data map[string]string, labels map[string]string) map[string]any {
	if secretType == "" {
		secretType = "Opaque"
	}
	meta := object{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	encoded := map[string]string{}
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return object{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       secretType,
		"metadata":   meta,
		"data":       encoded,
	}
}

// DockerConfigJSON is the .dockerconfigjson of a pull secret for one registry.
func DockerConfigJSON(server, username, password string) (string, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	config, err := json.Marshal(object{"auths": object{server: object{
		"username": username, "password": password, "auth": auth,
	}}})
	return string(config), err
}

// PullSecretMismatch compares a stored .dockerconfigjson with the registry
// credential it should hold. It returns "" when they match, or a reason that
// never contains a credential value. An empty password compares only the
// server and the username.
func PullSecretMismatch(raw, server, username, password string) string {
	var config struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal([]byte(raw), &config) != nil {
		return "unreadable"
	}
	entry, ok := config.Auths[server]
	if !ok {
		return "no entry for " + server
	}
	user, pass := entry.Username, entry.Password
	if entry.Auth != "" {
		// The kubelet prefers auth over the separate fields.
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return "unreadable"
		}
		user, pass, _ = strings.Cut(string(decoded), ":")
	}
	switch {
	case user != username:
		return "different username"
	case password != "" && pass != password:
		return "different password"
	}
	return ""
}

// SecretValue reads one key of a secret; ok is false when it does not exist.
func (k *Kube) SecretValue(ctx context.Context, namespace, name, key string) (string, bool) {
	out, err := k.Exec.Output(ctx, "kubectl", "get", "secret", name, "-n", namespace, "-o", "json")
	if err != nil {
		return "", false
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal([]byte(out), &s) != nil {
		return "", false
	}
	raw, ok := s.Data[key]
	if !ok {
		return "", false
	}
	value, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", false
	}
	return string(value), true
}

type NodeStatus struct {
	Name    string
	Ready   bool
	Version string
}

func (k *Kube) Nodes(ctx context.Context) ([]NodeStatus, error) {
	out, err := k.Exec.Output(ctx, "kubectl", "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
			Status   struct {
				Conditions []struct{ Type, Status string } `json:"conditions"`
				NodeInfo   struct {
					KubeletVersion string `json:"kubeletVersion"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, err
	}
	var nodes []NodeStatus
	for _, item := range list.Items {
		n := NodeStatus{Name: item.Metadata.Name, Version: item.Status.NodeInfo.KubeletVersion}
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// WorkloadsReady reports how many Deployments and StatefulSets in a
// namespace are fully available.
func (k *Kube) WorkloadsReady(ctx context.Context, namespace string) (ready, total int, err error) {
	out, err := k.Exec.Output(ctx, "kubectl", "get", "deployments,statefulsets", "-n", namespace, "-o", "json")
	if err != nil {
		return 0, 0, err
	}
	var list struct {
		Items []struct {
			Spec struct {
				Replicas *int `json:"replicas"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return 0, 0, err
	}
	for _, item := range list.Items {
		want := 1
		if item.Spec.Replicas != nil {
			want = *item.Spec.Replicas
		}
		total++
		if item.Status.ReadyReplicas >= want {
			ready++
		}
	}
	return ready, total, nil
}

// ── Helm ──────────────────────────────────────────────────────────────────────

type Chart struct {
	Release   string
	Namespace string
	Repo      string // chart repository URL
	Name      string // chart name in the repository
	Version   string
	Values    string         // rendered provider values (YAML)
	Overrides map[string]any // manifest `values:` merged on top
	Timeout   time.Duration  // for the release to become ready; 10 minutes when zero
}

// HelmUpgrade installs or upgrades a release and waits for it to be ready.
// The chart is downloaded once into StateDir()/charts, so a rerun does not
// depend on the chart repository. Values files contain secrets, so they are
// written 0600 and removed after.
func (k *Kube) HelmUpgrade(ctx context.Context, c Chart) error {
	chart, err := k.pullChart(ctx, c)
	if err != nil {
		return err
	}
	if err := k.recoverRelease(ctx, c.Release, c.Namespace); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ownstack-helm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	args := []string{
		"upgrade", "--install", c.Release, chart,
		"--namespace", c.Namespace, "--create-namespace",
		"--wait", "--timeout", fmt.Sprintf("%ds", int(timeout.Seconds())),
	}
	for i, values := range []any{c.Values, c.Overrides} {
		var data []byte
		switch v := values.(type) {
		case string:
			data = []byte(v)
		case map[string]any:
			if len(v) == 0 {
				continue
			}
			if data, err = yaml.Marshal(v); err != nil {
				return err
			}
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		path := fmt.Sprintf("%s/values-%d.yaml", dir, i)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		args = append(args, "-f", path)
	}
	return k.Exec.Run(ctx, "helm", args...)
}

// pullChart returns the path of c's chart archive,
// StateDir()/charts/<name>-<version>.tgz, downloading it first when it is
// not there. The download lands in a temporary directory and is renamed
// into place, so an interrupted one leaves nothing behind.
func (k *Kube) pullChart(ctx context.Context, c Chart) (string, error) {
	dir := StateFile("charts")
	path := filepath.Join(dir, c.Name+"-"+c.Version+".tgz")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dir, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	what := "download chart " + c.Name + " " + c.Version
	if err := Retry(ctx, k.Exec.Console, what, 3, 10*time.Second, func() error {
		_, err := k.Exec.Output(ctx, "helm", "pull", c.Name, "--repo", c.Repo, "--version", c.Version, "--destination", tmp)
		return err
	}); err != nil {
		return "", err
	}
	archives, _ := filepath.Glob(filepath.Join(tmp, "*.tgz"))
	if len(archives) != 1 {
		return "", fmt.Errorf("%s: helm pull wrote %d chart archives, want 1", what, len(archives))
	}
	return path, os.Rename(archives[0], path)
}

type Release struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Status     string `json:"status"`
	Chart      string `json:"chart"`
	AppVersion string `json:"app_version"`
}

// Release returns the installed release in any state (pending ones
// included), or ok=false when it is not installed or the cluster cannot be
// reached.
func (k *Kube) Release(ctx context.Context, name, namespace string) (Release, bool) {
	out, err := k.Exec.Output(ctx, "helm", "list", "-a", "-n", namespace, "--filter", "^"+name+"$", "-o", "json")
	if err != nil {
		return Release{}, false
	}
	var releases []Release
	if json.Unmarshal([]byte(out), &releases) != nil || len(releases) == 0 {
		return Release{}, false
	}
	return releases[0], true
}

type helmRevision struct {
	Revision int    `json:"revision"`
	Status   string `json:"status"`
}

// recoverRelease unblocks a release that an interrupted run (a closed SSH
// session, Ctrl-C, a reboot) left in a pending state, where Helm refuses any
// further upgrade. It is safe because apply holds the node lock, so no other
// ownstackctl is running Helm. A release that never installed is removed
// (StatefulSet volumes stay); an interrupted upgrade or rollback is rolled
// back, without waiting, to the last deployed revision (or the one before it)
// so the upgrade that follows can converge it.
func (k *Kube) recoverRelease(ctx context.Context, release, namespace string) error {
	out, err := k.Exec.Output(ctx, "helm", "history", release, "--namespace", namespace, "--max", "20", "-o", "json")
	if err != nil {
		return nil // not installed yet, or Helm will report the real problem
	}
	var history []helmRevision
	if json.Unmarshal([]byte(out), &history) != nil || len(history) == 0 {
		return nil
	}
	action, target := recoveryAction(history)
	latest := history[len(history)-1]
	switch action {
	case "uninstall":
		k.Exec.Console.Printf("Helm release %s/%s was left %s by an interrupted run; removing it to install again", namespace, release, latest.Status)
		return k.Exec.Run(ctx, "helm", "uninstall", release, "--namespace", namespace, "--wait", "--timeout", "5m")
	case "rollback":
		k.Exec.Console.Printf("Helm release %s/%s was left %s (revision %d) by an interrupted run; rolling back to revision %d", namespace, release, latest.Status, latest.Revision, target)
		if err := k.Exec.Run(ctx, "helm", "rollback", release, fmt.Sprint(target), "--namespace", namespace); err != nil {
			return fmt.Errorf("recover Helm release %s/%s from %s: %w", namespace, release, latest.Status, err)
		}
	}
	return nil
}

// recoveryAction decides how to unblock a release from its history (oldest
// first): "" when its latest revision is not pending, "uninstall" when it never
// got past installing, or "rollback" to the returned revision.
func recoveryAction(history []helmRevision) (string, int) {
	latest := history[len(history)-1]
	if !strings.HasPrefix(latest.Status, "pending-") {
		return "", 0
	}
	if latest.Status == "pending-install" || len(history) == 1 {
		return "uninstall", 0
	}
	target := 0
	for _, r := range history[:len(history)-1] {
		if r.Status == "deployed" || r.Status == "superseded" {
			target = r.Revision
		}
	}
	if target == 0 {
		target = history[len(history)-2].Revision
	}
	return "rollback", target
}

func (k *Kube) outputInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	return k.Exec.outputInput(ctx, stdin, name, args...)
}
