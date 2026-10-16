package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// fakeRegistry speaks the Docker v2 token flow, like Harbor, GHCR, and Docker Hub.
func fakeRegistry(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if r.Header.Get("Authorization") == "Bearer issued" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="harbor-registry"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			user, pass, _ := r.BasicAuth()
			if user != "robot$ci" || pass != "pw" || !strings.Contains(r.URL.Query().Get("scope"), "repository:product/") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"token": "issued"})
		}
	}))
	return srv
}

func runtimeFor(t *testing.T, password string) *stack.Runtime {
	rt, _ := providertest.Runtime(t)
	rt = rt.For("images")
	rt.Out.Images = &stack.ImagesOutput{Host: "unused", Project: "product", PushUsername: "robot$ci", PushPassword: password}
	return rt
}

func TestTokenFlow(t *testing.T) {
	srv := fakeRegistry(t)
	defer srv.Close()
	p := Provider{Client: srv.Client(), Scheme: "http"}
	rt := runtimeFor(t, "pw")
	rt.Out.Images.Host = strings.TrimPrefix(srv.URL, "http://")
	if err := p.verify(context.Background(), rt); err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}
	rt.Out.Images.PushPassword = "wrong"
	if err := p.verify(context.Background(), rt); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func TestDockerHubPullServer(t *testing.T) {
	rt, _ := providertest.Runtime(t)
	rt.Manifest.Spec.Components["images"].Provider = "registry"
	rt.Manifest.Spec.Components["images"].Settings = map[string]string{"host": "docker.io", "project": "acme", "username": "acme"}
	rt.Secrets.Overrides = map[string]string{"REGISTRY_PASSWORD": "pw"}
	if err := (Provider{}).Resolve(context.Background(), rt.For("images")); err != nil {
		t.Fatal(err)
	}
	if got := rt.Out.Images.PullSecretServer(); got != "https://index.docker.io/v1/" {
		t.Errorf("pull server = %q", got)
	}
}
