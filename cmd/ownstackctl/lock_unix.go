//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockNode makes apply and up exclusive on this server. Two runs would
// otherwise each generate passwords into the same secrets file (the last
// writer wins) and race on the same Helm releases. The lock file lives next
// to the secrets file; the kernel releases the lock when the process exits,
// so it never goes stale.
func lockNode(secretsFile string) (unlock func(), err error) {
	dir := filepath.Dir(secretsFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "apply.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder := "unknown"
			if data, _ := os.ReadFile(path); len(strings.TrimSpace(string(data))) > 0 {
				holder = strings.TrimSpace(string(data))
			}
			return nil, fmt.Errorf("another ownstackctl apply or up is running on this server (pid %s); wait for it to finish", holder)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// The PID is only for the message above.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { f.Close() }, nil
}
