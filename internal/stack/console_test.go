package stack

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestConsoleRedactsAndForwards(t *testing.T) {
	var out bytes.Buffer
	c := NewConsole(&out)
	var forwarded []string
	c.AddSink(func(l string) { forwarded = append(forwarded, l) })
	c.Redact("abc") // too short to redact safely
	c.Redact("supersecret")
	c.Redact("supersecret-longer")

	c.Printf("token=supersecret-longer other=supersecret abc")
	c.Event("stage_started", map[string]string{"id": "images", "title": "Image registry · Harbor"})

	got := out.String()
	if strings.Contains(got, "supersecret") {
		t.Fatalf("secret leaked: %s", got)
	}
	if !strings.Contains(got, "token=[redacted] other=[redacted] abc") {
		t.Errorf("output = %q", got)
	}
	if !strings.Contains(got, `ownstack.event {"event":"stage_started","id":"images","title":"Image registry · Harbor"}`+"\n") {
		t.Errorf("event = %q", got)
	}
	if len(forwarded) != 2 || strings.Contains(forwarded[0], "supersecret") {
		t.Errorf("forwarded = %q", forwarded)
	}
}

func TestLineWriterSplitsCarriageReturns(t *testing.T) {
	var out bytes.Buffer
	w := &lineWriter{console: NewConsole(&out), prefix: "> "}
	w.Write([]byte("one\r\ntwo\rthree"))
	w.Write([]byte(" four\n"))
	w.Flush()
	if got := out.String(); got != "> one\n> two\n> three four\n" {
		t.Errorf("got %q", got)
	}
}

// Event fields are JSON strings; a secret with a quote or backslash must be
// redacted before marshalling, or its escaped form slips through.
func TestEventRedactsBeforeMarshalling(t *testing.T) {
	var out bytes.Buffer
	c := NewConsole(&out)
	secret := `pa"ss\word99`
	c.Redact(secret)
	c.Event("stage_failed", map[string]string{"id": "images", "error": "login failed for " + secret + "\nnext \"line\""})
	got := out.String()
	if strings.Contains(got, "word99") {
		t.Fatalf("secret leaked: %s", got)
	}
	line, ok := strings.CutPrefix(strings.TrimSuffix(got, "\n"), EventPrefix)
	var fields map[string]string
	if !ok || strings.Contains(line, "\n") || json.Unmarshal([]byte(line), &fields) != nil {
		t.Fatalf("not one JSON event line: %q", got)
	}
	if fields["event"] != "stage_failed" || fields["error"] != "login failed for [redacted]\nnext \"line\"" {
		t.Errorf("fields = %q", fields)
	}
}

func TestRedactAll(t *testing.T) {
	type check struct {
		Name, Detail string
		hidden       string
	}
	doc := struct {
		Title  string
		Checks []check
		Steps  []*check
		Notes  map[string]string
		Any    any
		Count  int
	}{
		Title:  "token abcd-secret",
		Checks: []check{{Name: "x", Detail: `rejected "abcd-secret"`, hidden: "abcd-secret"}},
		Steps:  []*check{{Detail: "abcd-secret"}, nil},
		Notes:  map[string]string{"k": "abcd-secret!"},
		Any:    check{Detail: "abcd-secret"},
		Count:  3,
	}
	c := NewConsole(&bytes.Buffer{})
	c.Redact("abcd-secret")
	c.RedactAll(&doc)
	if doc.Title != "token [redacted]" || doc.Checks[0].Detail != `rejected "[redacted]"` || doc.Steps[0].Detail != "[redacted]" ||
		doc.Notes["k"] != "[redacted]!" || doc.Any.(check).Detail != "[redacted]" {
		t.Errorf("not redacted: %+v", doc)
	}
	if doc.Checks[0].hidden != "abcd-secret" || doc.Count != 3 {
		t.Errorf("unexported or non-string fields changed: %+v", doc)
	}
}
