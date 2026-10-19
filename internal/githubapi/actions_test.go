package githubapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func TestSealOpensWithRecipientKey(t *testing.T) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(base64.StdEncoding.EncodeToString(pub[:]), "kubeconfig-data")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(sealed)
	opened, ok := box.OpenAnonymous(nil, raw, pub, priv)
	if !ok || string(opened) != "kubeconfig-data" {
		t.Fatalf("sealed box did not open: %q %v", opened, ok)
	}
	if _, err := Seal("not-a-key", "x"); err == nil {
		t.Error("invalid key accepted")
	}
}

func TestReposPaginatesUserAndOrgs(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		var repos []map[string]any
		switch {
		case r.URL.Path == "/user/repos" && r.URL.Query().Get("page") == "1":
			for i := 0; i < 100; i++ {
				repos = append(repos, map[string]any{"name": fmt.Sprintf("r%d", i), "owner": map[string]string{"login": "Acme"}})
			}
		case r.URL.Path == "/user/repos":
			repos = []map[string]any{
				{"name": "last", "owner": map[string]string{"login": "acme"}},
				{"name": "old", "owner": map[string]string{"login": "acme"}, "archived": true},
				{"name": "fork", "owner": map[string]string{"login": "someone"}},
			}
		case r.URL.Path == "/orgs/shop/repos":
			repos = []map[string]any{{"name": "api", "owner": map[string]string{"login": "shop"}}}
		}
		json.NewEncoder(w).Encode(repos)
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}

	names, err := c.Repos(context.Background(), "acme", "ACME")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 101 || names[100] != "last" {
		t.Errorf("user repos: %d, last %q", len(names), names[len(names)-1])
	}
	// A short page ends the listing.
	if len(paths) != 2 {
		t.Errorf("requests: %v", paths)
	}
	if names, err := c.Repos(context.Background(), "shop", "acme"); err != nil || len(names) != 1 || names[0] != "api" {
		t.Errorf("org repos: %v %v", names, err)
	}
	if last := paths[len(paths)-1]; !strings.HasPrefix(last, "/orgs/shop/repos?") {
		t.Errorf("other owners are listed as organizations: %s", last)
	}
}

func TestFileContentAndDeletes(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/contents/a.yml":
			encoded := base64.StdEncoding.EncodeToString([]byte("name: ownstack\n"))
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": encoded[:8] + "\n" + encoded[8:]})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/GONE"):
			http.NotFound(w, r)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/DENIED"):
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible"}`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()

	if got, err := c.FileContent(ctx, "acme", "app", "a.yml"); err != nil || string(got) != "name: ownstack\n" {
		t.Errorf("content = %q, %v", got, err)
	}
	if _, err := c.FileContent(ctx, "acme", "app", "missing.yml"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	if err := c.DeleteSecret(ctx, "acme", "app", "OWNSTACK_KUBECONFIG"); err != nil {
		t.Error(err)
	}
	if err := c.DeleteVariable(ctx, "acme", "app", "OWNSTACK_DOMAIN"); err != nil {
		t.Error(err)
	}
	if err := c.DeleteSecret(ctx, "acme", "app", "GONE"); err != nil {
		t.Errorf("a missing secret is already deleted: %v", err)
	}
	if err := c.DeleteVariable(ctx, "acme", "app", "DENIED"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("refusal: %v", err)
	}
	want := []string{"/repos/acme/app/actions/secrets/OWNSTACK_KUBECONFIG", "/repos/acme/app/actions/variables/OWNSTACK_DOMAIN"}
	if strings.Join(deleted, " ") != strings.Join(want, " ") {
		t.Errorf("deleted %v", deleted)
	}
}
