package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getownstack/ownstack-cluster-template/internal/providers/providertest"
	"github.com/getownstack/ownstack-cluster-template/internal/stack"
)

// fakeRegistry speaks the Docker v2 token flow, like Harbor, GHCR, and Docker
// Hub, and issues token.
func fakeRegistry(t *testing.T, token string) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			if r.Header.Get("Authorization") == "Bearer "+token {
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
			json.NewEncoder(w).Encode(map[string]string{"token": token})
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

// unsignedJWT builds a token like Harbor's and Docker Hub's, with an access
// claim when access is not nil. The signature is never checked here.
func unsignedJWT(access any) string {
	claims := map[string]any{"iss": "harbor-token-issuer", "sub": "robot$ci"}
	if access != nil {
		claims["access"] = access
	}
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + ".c2lnbmF0dXJl"
}

func TestTokenFlow(t *testing.T) {
	access := func(name string, actions ...string) []map[string]any {
		return []map[string]any{{"type": "repository", "name": name, "actions": actions}}
	}
	for _, tc := range []struct {
		name    string
		token   string
		canPush bool
		wantErr string
	}{
		// GHCR issues opaque tokens: only the login is proven.
		{"opaque token", "issued", false, ""},
		{"jwt without access claim", unsignedJWT(nil), false, ""},
		{"jwt with push", unsignedJWT(access("product/ownstack-check", "pull", "push")), true, ""},
		{"jwt with every action", unsignedJWT(access("product/ownstack-check", "*")), true, ""},
		{"jwt case-insensitive name", unsignedJWT(access("Product/ownstack-check", "push", "pull")), true, ""},
		{"pull-only robot", unsignedJWT(access("product/ownstack-check", "pull")), false, "cannot push to product"},
		{"no access to the project", unsignedJWT([]any{}), false, "cannot push to product"},
		{"push on another repository", unsignedJWT(access("other/ownstack-check", "push")), false, "cannot push to product"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeRegistry(t, tc.token)
			defer srv.Close()
			p := Provider{Client: srv.Client(), Scheme: "http"}
			rt := runtimeFor(t, "pw")
			rt.Out.Images.Host = strings.TrimPrefix(srv.URL, "http://")
			canPush, err := p.verify(context.Background(), rt)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("valid credential rejected: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			case canPush != tc.canPush:
				t.Errorf("canPush = %v, want %v", canPush, tc.canPush)
			}
			if err != nil && strings.Contains(err.Error(), tc.token) {
				t.Errorf("error contains the token: %v", err)
			}

			rt.Out.Images.PushPassword = "wrong"
			if _, err := p.verify(context.Background(), rt); err == nil {
				t.Fatal("wrong password accepted")
			}
		})
	}
}

func TestStatusClaimsOnlyWhatWasProven(t *testing.T) {
	for token, want := range map[string]string{
		"issued": "robot$ci can log in to ",
		unsignedJWT([]map[string]any{{"type": "repository", "name": "product/ownstack-check", "actions": []string{"pull", "push"}}}): "robot$ci can push to ",
	} {
		srv := fakeRegistry(t, token)
		p := Provider{Client: srv.Client(), Scheme: "http"}
		rt := runtimeFor(t, "pw")
		rt.Out.Images.Host = strings.TrimPrefix(srv.URL, "http://")
		checks := p.Status(context.Background(), rt)
		if len(checks) != 1 || checks[0].State != stack.OK || !strings.HasPrefix(checks[0].Detail, want) {
			t.Errorf("token %.10s…: checks = %+v, want detail %q", token, checks, want)
		}
		srv.Close()
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
