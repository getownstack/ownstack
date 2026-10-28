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

	v, err := s.Get(Request{Env: "GITHUB_TOKEN", Label: "token"})
	if err != nil || v != "from-env" {
		t.Fatalf("env: %q %v", v, err)
	}
	pw, err := s.Get(Request{Env: "HARBOR_ADMIN_PASSWORD", Label: "pw", Generate: true})
	if err != nil || len(pw) != 24 {
		t.Fatalf("generate: %q %v", pw, err)
	}
	if _, err := s.Get(Request{Env: "MISSING", Label: "missing"}); !errors.Is(err, ErrMissing) {
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
	if v, _ := again.Get(Request{Env: "GITHUB_TOKEN"}); v != "from-env" {
		t.Errorf("persisted env value = %q", v)
	}
	if v, _ := again.Get(Request{Env: "HARBOR_ADMIN_PASSWORD", Generate: true}); v != pw {
		t.Errorf("generated value not reused")
	}
}

// The environment wins over the secrets file, and a changed value is saved.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	s := newTestStore(t, map[string]string{"GITHUB_TOKEN": "old"})
	s.Get(Request{Env: "GITHUB_TOKEN"})
	s.Flush()
	again := newTestStore(t, map[string]string{"GITHUB_TOKEN": "new"})
	again.Path = s.Path
	if v, _ := again.Get(Request{Env: "GITHUB_TOKEN"}); v != "new" {
		t.Errorf("value = %q", v)
	}
	again.Flush()
	if got := readEnvFile(s.Path)["GITHUB_TOKEN"]; got != "new" {
		t.Errorf("saved %q", got)
	}
}

// Plan and status never generate, prompt, or write.
func TestReadOnly(t *testing.T) {
	s := newTestStore(t, map[string]string{"T": "from-env"})
	s.ReadOnly = true
	if _, err := s.Get(Request{Env: "X", Generate: true}); !errors.Is(err, ErrWouldGenerate) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Get(Request{Env: "Y"}); !errors.Is(err, ErrMissing) {
		t.Fatalf("err = %v", err)
	}
	if v, err := s.Get(Request{Env: "T"}); err != nil || v != "from-env" {
		t.Fatalf("T = %q %v", v, err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Error("nothing should have been written")
	}
}

func TestQuotedValuesRoundTrip(t *testing.T) {
	s := newTestStore(t, map[string]string{"T": `a"b=c#d`})
	s.Get(Request{Env: "T"})
	s.Flush()
	if got := readEnvFile(s.Path)["T"]; got != `a"b=c#d` {
		t.Errorf("round trip = %q", got)
	}
}

func TestGeneratedIsAnnouncedOnceStored(t *testing.T) {
	s := newTestStore(t, nil)
	out := s.Out.(*strings.Builder)
	if _, err := s.Get(Request{Env: "ZOT_PASSWORD", Label: "Zot password", Generate: true}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("announced before it was stored: %q", out.String())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "Generated Zot password; stored as ZOT_PASSWORD in " + s.Path + "\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
	s.Flush()
	if strings.Count(out.String(), "Generated Zot") != 1 {
		t.Errorf("announced twice: %q", out.String())
	}
}

func TestGeneratedIsNotAnnouncedWhenSaveFails(t *testing.T) {
	s := newTestStore(t, nil)
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	s.Path = filepath.Join(blocker, "secrets.env") // its directory is a file
	if _, err := s.Get(Request{Env: "ZOT_PASSWORD", Label: "Zot password", Generate: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("Flush succeeded")
	}
	if out := s.Out.(*strings.Builder).String(); strings.Contains(out, "Generated Zot") {
		t.Errorf("announced a secret that was not stored: %q", out)
	}
}
