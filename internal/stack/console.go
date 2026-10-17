package stack

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Console is the single output path of ownstackctl. Every line is redacted
// of known secret values, written to the terminal, and forwarded to sinks
// such as the Ownstack progress reporter.
type Console struct {
	mu      sync.Mutex
	out     io.Writer
	secrets []string
	sinks   []func(string)
}

func NewConsole(out io.Writer) *Console { return &Console{out: out} }

// Redact registers a secret value that must never be printed.
func (c *Console) Redact(value string) {
	if len(value) < 4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.secrets {
		if s == value {
			return
		}
	}
	c.secrets = append(c.secrets, value)
	// Longest first so a secret containing another is fully masked.
	sort.Slice(c.secrets, func(i, j int) bool { return len(c.secrets[i]) > len(c.secrets[j]) })
}

func (c *Console) AddSink(fn func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sinks = append(c.sinks, fn)
}

// Redacted masks every registered secret in s.
func (c *Console) Redacted(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.redact(s)
}

func (c *Console) redact(line string) string {
	for _, s := range c.secrets {
		line = strings.ReplaceAll(line, s, "[redacted]")
	}
	return line
}

func (c *Console) Line(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line = c.redact(line)
	fmt.Fprintln(c.out, line)
	for _, sink := range c.sinks {
		sink(line)
	}
}

func (c *Console) Printf(format string, args ...any) {
	for _, line := range strings.Split(strings.TrimRight(fmt.Sprintf(format, args...), "\n"), "\n") {
		c.Line(line)
	}
}

// Event emits a structured progress line:
//
//	ownstack.event=stage_started aspect="images" id="images" provider="harbor"
//
// UIs parse these; humans can read them.
func (c *Console) Event(name string, fields map[string]string) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{"ownstack.event=" + name}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, fields[k]))
	}
	c.Line(strings.Join(parts, " "))
}

// lineWriter splits written bytes into lines for the console.
type lineWriter struct {
	console *Console
	prefix  string
	buf     []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexAny(string(w.buf), "\n\r")
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), " \t")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.console.Line(w.prefix + line)
		}
	}
	return len(p), nil
}

func (w *lineWriter) Flush() {
	if len(w.buf) > 0 {
		w.console.Line(w.prefix + string(w.buf))
		w.buf = nil
	}
}
