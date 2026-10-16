package stack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
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

func (k *Kube) Apply(ctx context.Context, obj object) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = k.outputInput(ctx, string(data), "kubectl", "apply", "-f", "-")
	return err
}

func (k *Kube) EnsureNamespace(ctx context.Context, name string) error {
	return k.Apply(ctx, object{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   object{"name": name},
	})
}

// ApplySecret creates or updates an Opaque (or typed) secret.
func (k *Kube) ApplySecret(ctx context.Context, namespace, name, secretType string, data map[string]string, labels map[string]string) error {
	if secretType == "" {
		secretType = "Opaque"
	}
	meta := object{"name": name, "namespace": namespace}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	return k.Apply(ctx, object{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       secretType,
		"metadata":   meta,
		"stringData": data,
	})
}

// ApplyPullSecret creates or updates a kubernetes.io/dockerconfigjson secret.
func (k *Kube) ApplyPullSecret(ctx context.Context, namespace, name, server, username, password string) error {
	auth := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	config, err := json.Marshal(object{"auths": object{server: object{
		"username": username, "password": password, "auth": auth,
	}}})
	if err != nil {
		return err
	}
	return k.ApplySecret(ctx, namespace, name, "kubernetes.io/dockerconfigjson",
		map[string]string{".dockerconfigjson": string(config)}, nil)
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

// Reachable reports whether the cluster API answers.
func (k *Kube) Reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := k.Exec.Output(ctx, "kubectl", "get", "--raw", "/readyz")
	return err == nil
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
	Timeout   time.Duration
}

// HelmUpgrade installs or upgrades a release and waits for it to be ready.
// Values files contain secrets, so they are written 0600 and removed after.
func (k *Kube) HelmUpgrade(ctx context.Context, c Chart) error {
	dir, err := os.MkdirTemp("", "ownstack-helm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	args := []string{
		"upgrade", "--install", c.Release, c.Name,
		"--repo", c.Repo,
		"--version", c.Version,
		"--namespace", c.Namespace, "--create-namespace",
		"--wait", "--timeout", fmt.Sprintf("%ds", int(timeoutOr(c.Timeout, 15*time.Minute).Seconds())),
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

type Release struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Status     string `json:"status"`
	Chart      string `json:"chart"`
	AppVersion string `json:"app_version"`
}

// Release returns the installed release, or ok=false when it is not
// installed or the cluster cannot be reached.
func (k *Kube) Release(ctx context.Context, name, namespace string) (Release, bool) {
	out, err := k.Exec.Output(ctx, "helm", "list", "-n", namespace, "--filter", "^"+name+"$", "-o", "json")
	if err != nil {
		return Release{}, false
	}
	var releases []Release
	if json.Unmarshal([]byte(out), &releases) != nil || len(releases) == 0 {
		return Release{}, false
	}
	return releases[0], true
}

func (k *Kube) outputInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	return k.Exec.outputInput(ctx, stdin, name, args...)
}

func timeoutOr(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}
