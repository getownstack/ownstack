package stack

import (
	"testing"

	"github.com/getownstack/ownstack/internal/catalog"
	"github.com/getownstack/ownstack/internal/manifest"
	"github.com/getownstack/ownstack/internal/secrets"
)

// A provider reads only the settings and secrets its Spec declares; any
// other key is a bug, so it panics instead of reading nothing.
func TestUndeclaredKeysPanic(t *testing.T) {
	m, err := manifest.Parse([]byte("apiVersion: ownstack.io/v2\ndomain: example.com\ncompute: {host: 203.0.113.10}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog.Default()
	cat.Providers = []catalog.Provider{{ID: "own-vps", Aspect: "compute", Name: "Own VPS", Params: []catalog.Param{
		{Key: "host", Kind: catalog.KindIP},
		{Key: "token", Kind: catalog.KindSecret, Env: "TEST_TOKEN"},
	}}}
	store := secrets.NewStore(t.TempDir() + "/secrets.env")
	store.Getenv = func(string) string { return "token-value" }
	rt := NewRuntime(m, cat, store, NewConsole(nil)).For("compute")

	if host := rt.Setting("host"); host != "203.0.113.10" {
		t.Errorf("host = %q", host)
	}
	if token, err := rt.Secret("token"); err != nil || token != "token-value" {
		t.Errorf("token = %q, %v", token, err)
	}
	for name, read := range map[string]func(){
		"unknown setting":      func() { rt.Setting("hots") },
		"secret as a setting":  func() { rt.Setting("token") },
		"setting as a secret":  func() { _, _ = rt.Secret("host") },
		"another aspect's key": func() { rt.For("dns").Setting("host") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			read()
		}()
	}
}
