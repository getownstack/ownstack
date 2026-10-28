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

func TestFileRefs(t *testing.T) {
	s := newTestStore(t, map[string]string{"GITHUB_TOKEN": "env"})
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

func TestGeneratedIsAnnouncedOnceStored(t *testing.T) {
	s := newTestStore(t, nil)
	out := s.Out.(*strings.Builder)
	if _, err := s.Get(Request{Ref: "env:ZOT_PASSWORD", Label: "Zot password", Generate: true}); err != nil {
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
	if _, err := s.Get(Request{Ref: "env:ZOT_PASSWORD", Label: "Zot password", Generate: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("Flush succeeded")
	}
	if out := s.Out.(*strings.Builder).String(); strings.Contains(out, "Generated Zot") {
		t.Errorf("announced a secret that was not stored: %q", out)
	}
}
