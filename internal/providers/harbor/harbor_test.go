package harbor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

func TestValuesRender(t *testing.T) {
	values, err := stack.Render(tmpl, map[string]any{
		"Host":        "harbor.admin.example.com",
		"Password":    `p"w\d`,
		"Annotations": map[string]string{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(values), &parsed); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, values)
	}
	if parsed["harborAdminPassword"] != `p"w\d` {
		t.Errorf("password = %v", parsed["harborAdminPassword"])
	}
}

func TestProjectAndRobot(t *testing.T) {
	var created map[string]any
	deleted := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "admin" || pass != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/projects":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/projects":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/robots":
			json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "name": "robot$ownstack-ci"}, {"id": 8, "name": "robot$other"}})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/robots/"):
			deleted = r.URL.Path
		case r.Method == http.MethodPost && r.URL.Path == "/robots":
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 9, "name": "robot$ownstack-ci", "secret": "s3cret"})
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	c := &client{base: srv.URL, user: "admin", password: "pw", http: srv.Client()}
	ctx := context.Background()
	if err := c.ensureProject(ctx, "product"); err != nil {
		t.Fatal(err)
	}
	name, secret, err := c.recreateRobot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if name != "robot$ownstack-ci" || secret != "s3cret" {
		t.Errorf("robot = %s %s", name, secret)
	}
	if deleted != "/robots/7" {
		t.Errorf("deleted %q, want only the old ownstack-ci robot", deleted)
	}
	if created["name"] != "ownstack-ci" || created["level"] != "system" {
		t.Errorf("created = %v", created)
	}
}
