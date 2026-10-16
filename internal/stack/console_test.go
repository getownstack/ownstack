package stack

import (
	"bytes"
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
	if !strings.Contains(got, `ownstack.event=stage_started id="images" title="Image registry · Harbor"`) {
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
