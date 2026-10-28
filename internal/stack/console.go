package stack

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
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

// EventPrefix starts every structured progress line.
const EventPrefix = "ownstack.event "

// Event emits a structured progress line: the prefix, then one JSON object
// with the event name and its string fields.
//
//	ownstack.event {"event":"stage_started","id":"images","provider":"zot","title":"Image registry · Zot"}
//
// UIs parse these; humans can read them.
func (c *Console) Event(name string, fields map[string]string) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(EventPrefix + `{"event":`)
	b.Write(jsonString(name))
	for _, k := range keys {
		b.WriteByte(',')
		b.Write(jsonString(k))
		b.WriteByte(':')
		// Redact before marshalling: JSON escapes quotes and backslashes, after
		// which a registered secret would no longer match.
		b.Write(jsonString(c.Redacted(fields[k])))
	}
	b.WriteByte('}')
	c.Line(b.String())
}

func jsonString(s string) []byte {
	data, _ := json.Marshal(s) // a string always marshals
	return data
}

// RedactAll masks every registered secret in every exported string reachable
// from v, in place. v must be a pointer. Use it on documents printed or sent
// outside the console (plan and status JSON, health reports); redacting the
// marshalled JSON instead would miss secrets that JSON escapes.
func (c *Console) RedactAll(v any) {
	redactValue(reflect.ValueOf(v), c.Redacted)
}

func redactValue(v reflect.Value, redact func(string) string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		elem := v.Elem()
		if v.Kind() == reflect.Interface && !elem.CanSet() {
			// Values held in an interface are not addressable: redact a copy.
			cp := reflect.New(elem.Type()).Elem()
			cp.Set(elem)
			redactValue(cp, redact)
			if v.CanSet() {
				v.Set(cp)
			}
			return
		}
		redactValue(elem, redact)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				redactValue(v.Field(i), redact)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			redactValue(v.Index(i), redact)
		}
	case reflect.Map:
		if v.IsNil() {
			return
		}
		iter := v.MapRange()
		for iter.Next() {
			// Map values are not addressable: redact a copy and store it back.
			cp := reflect.New(iter.Value().Type()).Elem()
			cp.Set(iter.Value())
			redactValue(cp, redact)
			v.SetMapIndex(iter.Key(), cp)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(redact(v.String()))
		}
	}
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
