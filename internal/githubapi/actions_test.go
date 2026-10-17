package githubapi

import (
	"crypto/rand"
	"encoding/base64"
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
