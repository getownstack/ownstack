package stack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Exec runs host commands, streaming their output through the console.
type Exec struct {
	Console *Console
	Env     []string // extra KEY=VALUE entries
}

// Run executes a command and streams its combined output.
func (e *Exec) Run(ctx context.Context, name string, args ...string) error {
	return e.run(ctx, "", name, args...)
}

// RunInput executes a command with stdin, streaming its combined output.
func (e *Exec) RunInput(ctx context.Context, stdin, name string, args ...string) error {
	return e.run(ctx, stdin, name, args...)
}

// Shell runs a bash script, streaming its output.
func (e *Exec) Shell(ctx context.Context, script string) error {
	return e.run(ctx, script, "bash", "-euo", "pipefail", "-s")
}

// Output executes a command and returns trimmed stdout. Stderr is included in
// the error when the command fails.
func (e *Exec) Output(ctx context.Context, name string, args ...string) (string, error) {
	return e.outputInput(ctx, "", name, args...)
}

func (e *Exec) outputInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.env()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return "", fmt.Errorf("%s: %w: %s", name, err, lastLine(msg))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Has reports whether a binary is on PATH.
func (e *Exec) Has(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func (e *Exec) run(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.env()
	w := &lineWriter{console: e.Console, prefix: "  "}
	cmd.Stdout = w
	cmd.Stderr = w
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	w.Flush()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (e *Exec) env() []string {
	return append(os.Environ(), e.Env...)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Retry calls fn until it succeeds, the attempts run out, or ctx is done.
func Retry(ctx context.Context, console *Console, what string, attempts int, delay time.Duration, fn func() error) error {
	var err error
	for i := 1; i <= attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i == attempts {
			break
		}
		console.Printf("  waiting: %s (%v); retry %d/%d in %s", what, err, i, attempts-1, delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
