package githubapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestFileContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/app/contents/a.yml":
			encoded := base64.StdEncoding.EncodeToString([]byte("name: ownstack\n"))
			json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": encoded[:8] + "\n" + encoded[8:]})
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
}
