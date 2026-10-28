// Package secrets resolves provider secrets on the node.
//
// A secret is read from, in order:
//
//  1. its environment variable (the catalog's Env, such as GITHUB_TOKEN);
//  2. the node's secrets file ($OWNSTACK_DIR/secrets.env);
//  3. generation, for secrets that may be generated (initial passwords);
//  4. a hidden prompt, when stdin is a terminal.
//
// Values found through 1, 3, or 4 are written to the secrets file (mode
// 0600) by Flush, so reruns and `ownstackctl status` work without asking
// again. The secrets file never leaves the node.
package secrets

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Request describes one secret a provider needs.
type Request struct {
	Env      string // environment variable, also its name in the secrets file
	Label    string // human label used in messages and prompts
	Help     string
	Generate bool
}

// Store resolves secrets and remembers every resolved value for redaction.
// It is safe for concurrent use.
type Store struct {
	Path string
	// ReadOnly never generates, prompts, or saves: plan and status.
	ReadOnly bool
	Getenv   func(string) string
	In       io.Reader
	Out      io.Writer

	mu       sync.Mutex
	file     map[string]string
	loaded   bool
	resolved map[string]string
	dirty    bool
	onValue  func(string)
	// generated notes which secrets were generated, announced once Flush
	// has stored them.
	generated []string
}

func NewStore(path string) *Store {
	return &Store{
		Path:     path,
		Getenv:   os.Getenv,
		In:       os.Stdin,
		Out:      os.Stderr,
		resolved: map[string]string{},
	}
}

// OnValue registers a callback invoked with every resolved secret value, so
// the console can redact it from logs.
func (s *Store) OnValue(fn func(string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onValue = fn
	for _, v := range s.resolved {
		fn(v)
	}
}

var (
	ErrMissing = errors.New("secret not provided")
	// ErrWouldGenerate is returned instead of generating when ReadOnly.
	ErrWouldGenerate = errors.New("generated on first apply")
)

// Get resolves a secret. It returns ErrMissing (wrapped) when the secret
// cannot be found and neither generation nor prompting applies.
func (s *Store) Get(req Request) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if v, ok := s.resolved[req.Env]; ok {
		return v, nil
	}
	value, persist := s.Getenv(req.Env), true
	switch {
	case value != "":
	case s.fileValue(req.Env) != "":
		value, persist = s.fileValue(req.Env), false
	case req.Generate && s.ReadOnly:
		return "", fmt.Errorf("%s (%s): %w", req.Label, req.Env, ErrWouldGenerate)
	case req.Generate:
		value = generatePassword(24)
		s.generated = append(s.generated, fmt.Sprintf("Generated %s; stored as %s in %s", req.Label, req.Env, s.Path))
	case !s.ReadOnly && isTerminal(s.In):
		v, err := s.prompt(req)
		if err != nil {
			return "", err
		}
		value = v
	}
	if value == "" {
		return "", fmt.Errorf("%s (%s): %w", req.Label, req.Env, ErrMissing)
	}
	s.resolved[req.Env] = value
	if persist && s.fileValue(req.Env) != value {
		s.file[req.Env] = value
		s.dirty = true
	}
	if s.onValue != nil {
		s.onValue(value)
	}
	return value, nil
}

// Flush writes newly resolved values to the secrets file.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadOnly || !s.dirty {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	keys := make([]string, 0, len(s.file))
	for k := range s.file {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Ownstack secrets for this node. Managed by ownstackctl; keep this file private.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, strconv.Quote(s.file[k]))
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	s.dirty = false
	for _, note := range s.generated {
		fmt.Fprintln(s.Out, note)
	}
	s.generated = nil
	return nil
}

func (s *Store) fileValue(name string) string {
	if !s.loaded {
		s.file = readEnvFile(s.Path)
		s.loaded = true
	}
	return s.file[name]
}

func (s *Store) prompt(req Request) (string, error) {
	fmt.Fprintf(s.Out, "\n%s (%s)\n", req.Label, req.Env)
	if req.Help != "" {
		fmt.Fprintf(s.Out, "  %s\n", req.Help)
	}
	fmt.Fprint(s.Out, "  value (input hidden): ")
	f := s.In.(*os.File)
	data, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(s.Out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		out[strings.TrimSpace(key)] = value
	}
	return out
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func generatePassword(n int) string {
	var b strings.Builder
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(passwordAlphabet[idx.Int64()])
	}
	return b.String()
}
