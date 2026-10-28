package stack

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitFor(t *testing.T) {
	defer func(d time.Duration) { waitInterval = d }(waitInterval)
	waitInterval = time.Millisecond
	var out bytes.Buffer
	rt := &Runtime{Console: NewConsole(&out)}

	details := []string{"booting", "booting", "booting", "starting", "starting"}
	polls := 0
	err := WaitFor(context.Background(), rt, "service", time.Minute, func(context.Context) (bool, string) {
		if polls == len(details) {
			return true, ""
		}
		polls++
		return false, details[polls-1]
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "  waiting: service (booting)\n  waiting: service (starting)\n"; out.String() != want {
		t.Errorf("logged:\n%s\nwant one line per detail:\n%s", out.String(), want)
	}

	err = WaitFor(context.Background(), rt, "service", 5*time.Millisecond, func(context.Context) (bool, string) { return false, "HTTP 503" })
	if err == nil || !strings.Contains(err.Error(), "service: not ready after 5ms (HTTP 503)") {
		t.Errorf("timeout err = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitFor(ctx, rt, "service", time.Minute, func(context.Context) (bool, string) { return false, "" }); err != context.Canceled {
		t.Errorf("cancelled err = %v", err)
	}
}

func TestStateDir(t *testing.T) {
	t.Setenv("OWNSTACK_DIR", "")
	if got := StateDir(); got != "/etc/ownstack" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("OWNSTACK_DIR", "/tmp/os")
	if got := StateDir(); got != "/tmp/os" {
		t.Errorf("OWNSTACK_DIR = %q", got)
	}
}
