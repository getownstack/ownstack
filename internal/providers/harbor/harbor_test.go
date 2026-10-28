package harbor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getownstack/ownstack/internal/providers/providertest"
	"github.com/getownstack/ownstack/internal/stack"
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

func TestRobotAccess(t *testing.T) {
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/product/repositories" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	c := &client{base: srv.URL, user: "robot$ownstack-ci", password: "s3cret", http: srv.Client()}
	for _, tc := range []struct {
		code    int
		valid   bool
		wantErr bool
	}{
		{http.StatusOK, true, false},
		{http.StatusUnauthorized, false, false},
		{http.StatusForbidden, false, false},
		// Harbor restarting, a proxy in between, or a missing project prove
		// nothing about the credential.
		{http.StatusServiceUnavailable, false, true},
		{http.StatusBadGateway, false, true},
		{http.StatusNotFound, false, true},
	} {
		code = tc.code
		valid, err := c.robotAccess(context.Background(), "product")
		if valid != tc.valid || (err != nil) != tc.wantErr {
			t.Errorf("HTTP %d: valid=%v err=%v", tc.code, valid, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error contains the secret: %v", err)
		}
	}

	down := &client{base: "http://127.0.0.1:1", user: "u", password: "p", http: srv.Client()}
	if _, err := down.robotAccess(context.Background(), "product"); err == nil {
		t.Error("transport error reported as a definite answer")
	}
}

// A Harbor that keeps failing must make apply fail, never rotate the robot.
func TestCheckRobotDoesNotRotateOnServerErrors(t *testing.T) {
	defer func(d time.Duration) { robotCheckTimeout = d }(robotCheckTimeout)
	robotCheckTimeout = 0

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/projects/apps/repositories" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	rt, _ := providertest.Runtime(t, Spec)
	c := &client{base: srv.URL, user: "robot$ownstack-ci", password: "s3cret", http: srv.Client()}
	valid, err := checkRobot(context.Background(), rt, c)
	if err == nil || valid {
		t.Fatalf("valid=%v err=%v, want an error", valid, err)
	}
	if calls != 1 {
		t.Errorf("checked %d times, want 1", calls)
	}
}

// Every registry follows the same contract: images go to <host>/apps.
func TestResolveOutputs(t *testing.T) {
	rt, _ := providertest.Runtime(t, Spec)
	rt.Exec.Env = append(rt.Exec.Env, "PATH=") // no kubectl: no stored robot
	if err := (Provider{}).Resolve(context.Background(), rt.For("images")); err != nil {
		t.Fatal(err)
	}
	out := rt.Out.Images
	if out.Host != "harbor.admin.example.com" || out.Project != "apps" || out.PushUsername != "" {
		t.Errorf("images output = %+v", out)
	}
}
