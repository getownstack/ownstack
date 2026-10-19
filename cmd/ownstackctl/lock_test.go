//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLockHelper holds the node lock in a child process until stdin closes.
func TestLockHelper(t *testing.T) {
	secretsFile := os.Getenv("OWNSTACK_LOCK_HELPER")
	if secretsFile == "" {
		t.Skip("helper for TestLockNodeIsExclusive")
	}
	unlock, err := lockNode(secretsFile)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("locked")
	_, _ = io.ReadAll(os.Stdin)
	// Keep the lock file reachable: its finalizer would close it, and so
	// release the lock, if a GC ran while waiting.
	unlock()
	os.Exit(0)
}

func TestLockNodeIsExclusive(t *testing.T) {
	secretsFile := filepath.Join(t.TempDir(), "ownstack", "secrets.env")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelper$")
	cmd.Env = append(os.Environ(), "OWNSTACK_LOCK_HELPER="+secretsFile)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer stdin.Close()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper did not take the lock: %q", line)
	}

	_, err := lockNode(secretsFile)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", cmd.Process.Pid)) {
		t.Fatalf("second run was not refused with the holder's pid: %v", err)
	}

	stdin.Close()
	cmd.Wait()
	unlock, err := lockNode(secretsFile)
	if err != nil {
		t.Fatalf("lock not released when the holder exited: %v", err)
	}
	unlock()
}
