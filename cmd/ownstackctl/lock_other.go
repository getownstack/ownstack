//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package main

// lockNode is a no-op where flock is unavailable; ownstackctl only applies
// stacks on Linux.
func lockNode(string) (func(), error) { return func() {}, nil }
