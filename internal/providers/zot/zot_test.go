package zot

import (
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/stack"
)

func values(keepTags int) map[string]any {
	return map[string]any{
		"Host": "registry.admin.example.com", "StorageGiB": 10, "Htpasswd": "ownstack:$2a$10$abc",
		"Annotations": map[string]string{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"},
		"KeepTags":    keepTags,
	}
}

func TestValuesRender(t *testing.T) {
	out, err := stack.Render(tmpl, values(0))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		ConfigFiles map[string]string `yaml:"configFiles"`
		SecretFiles map[string]string `yaml:"secretFiles"`
	}
	if err := yaml.Unmarshal([]byte(out), &values); err != nil {
		t.Fatalf("values: %v\n%s", err, out)
	}
	var config struct {
		HTTP struct {
			Compat []string `json:"compat"`
			Auth   struct {
				Htpasswd struct{ Path string } `json:"htpasswd"`
			} `json:"auth"`
		} `json:"http"`
	}
	if err := json.Unmarshal([]byte(values.ConfigFiles["config.json"]), &config); err != nil {
		t.Fatalf("config.json: %v", err)
	}
	// docker push sends Docker v2 schema 2 manifests; without compat Zot
	// rejects them as "manifest invalid".
	if len(config.HTTP.Compat) != 1 || config.HTTP.Compat[0] != "docker2s2" {
		t.Errorf("compat = %v", config.HTTP.Compat)
	}
	if config.HTTP.Auth.Htpasswd.Path != "/secret/htpasswd" || values.SecretFiles["htpasswd"] != "ownstack:$2a$10$abc" {
		t.Errorf("auth wiring wrong: %+v %v", config.HTTP.Auth, values.SecretFiles)
	}
}

// valuesBeforeRetention is values.yaml.tmpl as it was before keepTags. With
// retention off the rendered values must stay identical, or every existing
// Zot would restart on its next apply.
const valuesBeforeRetention = `service:
  type: ClusterIP
  port: 5000
ingress:
  enabled: true
  className: traefik
  pathtype: Prefix
  annotations:
{{- range $key, $value := .Annotations }}
    {{ $key }}: {{ printf "%q" $value }}
{{- end }}
  hosts:
    - host: {{ .Host }}
      paths:
        - path: /
persistence: true
pvc:
  create: true
  storage: {{ .StorageGiB }}Gi
mountConfig: true
configFiles:
  config.json: |-
    {
      "storage": { "rootDirectory": "/var/lib/registry", "gc": true },
      "http": {
        "address": "0.0.0.0",
        "port": "5000",
        "realm": "ownstack",
        "compat": ["docker2s2"],
        "auth": { "htpasswd": { "path": "/secret/htpasswd" } }
      },
      "log": { "level": "info" }
    }
mountSecret: true
secretFiles:
  htpasswd: {{ printf "%q" .Htpasswd }}
`

func TestRetentionOffKeepsValuesIdentical(t *testing.T) {
	before, err := stack.Render(template.Must(template.New("before").Parse(valuesBeforeRetention)), values(0))
	if err != nil {
		t.Fatal(err)
	}
	after, err := stack.Render(tmpl, values(0))
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("values changed with keepTags 0:\n%s\nwant:\n%s", after, before)
	}
}

func TestRetentionPolicy(t *testing.T) {
	out, err := stack.Render(tmpl, values(15))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ConfigFiles map[string]string `yaml:"configFiles"`
	}
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("values: %v\n%s", err, out)
	}
	// The shape zot v2.1 reads (pkg/api/config.ImageRetention). One keepTags
	// entry: zot applies only the first entry whose patterns match a tag, and
	// ORs the rules within it.
	var config struct {
		Storage struct {
			RootDirectory string `json:"rootDirectory"`
			GC            bool   `json:"gc"`
			Retention     struct {
				Policies []struct {
					Repositories   []string         `json:"repositories"`
					DeleteUntagged bool             `json:"deleteUntagged"`
					KeepTags       []map[string]any `json:"keepTags"`
				} `json:"policies"`
			} `json:"retention"`
		} `json:"storage"`
		HTTP struct {
			Compat []string `json:"compat"`
		} `json:"http"`
	}
	if err := json.Unmarshal([]byte(v.ConfigFiles["config.json"]), &config); err != nil {
		t.Fatalf("config.json: %v\n%s", err, v.ConfigFiles["config.json"])
	}
	st := config.Storage
	if st.RootDirectory != "/var/lib/registry" || !st.GC || len(config.HTTP.Compat) != 1 {
		t.Errorf("config = %+v", config)
	}
	if len(st.Retention.Policies) != 1 {
		t.Fatalf("policies = %+v", st.Retention.Policies)
	}
	p := st.Retention.Policies[0]
	if len(p.Repositories) != 1 || p.Repositories[0] != "**" || !p.DeleteUntagged || len(p.KeepTags) != 1 ||
		p.KeepTags[0]["mostRecentlyPushedCount"] != float64(15) || p.KeepTags[0]["pulledWithin"] != "720h" || len(p.KeepTags[0]) != 2 {
		t.Errorf("policy = %+v", p)
	}
}

func TestReuseHtpasswd(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("zot-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	line := "ownstack:" + string(hash)
	existing := "other:" + string(hash) + "\n  " + line + "  \n"
	if got, ok := reuseHtpasswd(existing, "ownstack", "zot-pw"); !ok || got != line {
		t.Errorf("same password: %q %v, want the stored line", got, ok)
	}
	if _, ok := reuseHtpasswd(existing, "ownstack", "new-pw"); ok {
		t.Error("changed password reused the old hash")
	}
	if _, ok := reuseHtpasswd("other:"+string(hash), "ownstack", "zot-pw"); ok {
		t.Error("another user's line was reused")
	}
	if _, ok := reuseHtpasswd("", "ownstack", "zot-pw"); ok {
		t.Error("empty file reused")
	}
	if got, _ := reuseHtpasswd(existing, "ownstack", "zot-pw"); strings.Contains(got, "zot-pw") {
		t.Error("line contains the password")
	}
}
