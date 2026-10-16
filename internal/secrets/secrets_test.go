package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	s := NewStore(filepath.Join(t.TempDir(), "secrets.env"))
	s.Getenv = func(k string) string { return env[k] }
	s.Out = &strings.Builder{}
	return s
}

func TestResolveOrderAndPersistence(t *testing.T) {
	s := newTestStore(t, map[string]string{"GITHUB_TOKEN": "from-env"})
	var redacted []string
	s.OnValue(func(v string) { redacted = append(redacted, v) })

	v, err := s.Get(Request{Ref: "env:GITHUB_TOKEN", Label: "token"})
	if err != nil || v != "from-env" {
		t.Fatalf("env: %q %v", v, err)
	}
	pw, err := s.Get(Request{Ref: "env:HARBOR_ADMIN_PASSWORD", Label: "pw", Generate: true})
	if err != nil || len(pw) != 24 {
		t.Fatalf("generate: %q %v", pw, err)
	}
	if _, err := s.Get(Request{Ref: "env:MISSING", Label: "missing"}); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
	if len(redacted) != 2 {
		t.Errorf("redaction callbacks = %d", len(redacted))
	}

	// A fresh store reads persisted values without the environment.
	again := newTestStore(t, nil)
	again.Path = s.Path
	if v, _ := again.Get(Request{Ref: "env:GITHUB_TOKEN"}); v != "from-env" {
		t.Errorf("persisted env value = %q", v)
	}
	if v, _ := again.Get(Request{Ref: "env:HARBOR_ADMIN_PASSWORD", Generate: true}); v != pw {
		t.Errorf("generated value not reused")
	}
}

func TestOverridesAndFileRefs(t *testing.T) {
	s := newTestStore(t, map[string]string{"GITHUB_TOKEN": "env"})
	s.Overrides = map[string]string{"GITHUB_TOKEN": "override"}
	if v, _ := s.Get(Request{Ref: "env:GITHUB_TOKEN"}); v != "override" {
		t.Errorf("override = %q", v)
	}
	path := filepath.Join(t.TempDir(), "token")
	os.WriteFile(path, []byte("file-token\n"), 0o600)
	if v, _ := s.Get(Request{Ref: "file:" + path}); v != "file-token" {
		t.Errorf("file ref = %q", v)
	}
}

func TestNoGenerate(t *testing.T) {
	s := newTestStore(t, nil)
	s.Generate = false
	if _, err := s.Get(Request{Ref: "env:X", Generate: true}); !errors.Is(err, ErrWouldGenerate) {
		t.Fatalf("err = %v", err)
	}
	s.Save = true
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Error("nothing should have been written")
	}
}

func TestQuotedValuesRoundTrip(t *testing.T) {
	s := newTestStore(t, map[string]string{"T": `a"b=c#d`})
	s.Get(Request{Ref: "env:T"})
	s.Flush()
	if got := readEnvFile(s.Path)["T"]; got != `a"b=c#d` {
		t.Errorf("round trip = %q", got)
	}
}
