//go:build !linux

package main

// signalDaemon does nothing: the daemon runs only on Linux.
func signalDaemon(int) error { return nil }
