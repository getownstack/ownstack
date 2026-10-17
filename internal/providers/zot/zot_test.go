package zot

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

func TestValuesRender(t *testing.T) {
	out, err := stack.Render(tmpl, map[string]any{
		"Host": "registry.admin.example.com", "StorageGiB": 10, "Htpasswd": "ownstack:$2a$10$abc",
		"Annotations": map[string]string{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"},
	})
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
