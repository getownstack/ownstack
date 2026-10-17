// Package secrets resolves the secret references in ownstack.yaml on the node.
//
// Resolution order for a secret:
//
//  1. The manifest reference (env:NAME or file:/path), or the provider's
//     conventional environment variable when none is given.
//  2. The node's secrets file (/etc/ownstack/secrets.env by default).
//  3. Generation, for secrets that may be generated (initial admin passwords).
//  4. An interactive prompt, when stdin is a terminal and prompting is allowed.
//
// Values found through 3 or 4, or supplied only in the environment, are
// written to the secrets file (mode 0600) so reruns and `ownstackctl status`
// work without asking again. The secrets file never leaves the node.
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

const DefaultFile = "/etc/ownstack/secrets.env"

// Request describes one secret a provider needs.
type Request struct {
	Ref      string // env:NAME or file:/path
	Label    string // human label used when prompting
	Help     string
	Generate bool
}

// Store resolves secrets and remembers every resolved value for redaction.
type Store struct {
	Path   string
	Prompt bool
	Save   bool
	// Generate allows generating secrets marked generatable. Plan and status
	// turn it off so they never invent values.
	Generate  bool
	Overrides map[string]string // env-name keyed values that win over the process environment
	Getenv    func(string) string
	In        io.Reader
	Out       io.Writer

	mu       sync.Mutex
	file     map[string]string
	loaded   bool
	resolved map[string]string
	dirty    bool
	onValue  func(string)
}

func NewStore(path string) *Store {
	if path == "" {
		path = DefaultFile
	}
	return &Store{
		Path:     path,
		Save:     true,
		Generate: true,
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
	// ErrWouldGenerate is returned instead of generating when Generate is off.
	ErrWouldGenerate = errors.New("generated on first apply")
)

// Get resolves a secret. It returns ErrMissing (wrapped) when the secret
// cannot be found and neither generation nor prompting applies.
func (s *Store) Get(req Request) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if v, ok := s.resolved[req.Ref]; ok {
		return v, nil
	}
	kind, name, _ := strings.Cut(req.Ref, ":")

	var value string
	persist := false
	switch kind {
	case "file":
		data, err := os.ReadFile(name)
		if err != nil {
			return "", fmt.Errorf("%s: read %s: %w", req.Label, name, err)
		}
		value = strings.TrimRight(string(data), "\r\n")
	case "env":
		if v := s.Overrides[name]; v != "" {
			value, persist = v, true
		} else if v := s.Getenv(name); v != "" {
			value, persist = v, true
		} else if v := s.fileValue(name); v != "" {
			value = v
		} else if req.Generate && !s.Generate {
			return "", fmt.Errorf("%s (%s): %w", req.Label, req.Ref, ErrWouldGenerate)
		} else if req.Generate {
			value, persist = generatePassword(24), true
			fmt.Fprintf(s.Out, "Generated %s; stored as %s in %s\n", req.Label, name, s.Path)
		} else if s.Prompt && isTerminal(s.In) {
			v, err := s.prompt(req, name)
			if err != nil {
				return "", err
			}
			value, persist = v, true
		}
	default:
		return "", fmt.Errorf("%s: unsupported secret reference %q", req.Label, req.Ref)
	}

	if value == "" {
		return "", fmt.Errorf("%s (%s): %w", req.Label, req.Ref, ErrMissing)
	}
	s.resolved[req.Ref] = value
	if persist && kind == "env" && s.fileValue(name) != value {
		s.file[name] = value
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
	if !s.Save || !s.dirty {
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
	return nil
}

func (s *Store) fileValue(name string) string {
	if !s.loaded {
		s.file = readEnvFile(s.Path)
		s.loaded = true
	}
	return s.file[name]
}

func (s *Store) prompt(req Request, name string) (string, error) {
	fmt.Fprintf(s.Out, "\n%s (%s)\n", req.Label, name)
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
