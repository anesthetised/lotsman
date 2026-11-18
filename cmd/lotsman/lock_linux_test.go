package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLockInstanceIsExclusive(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	release, err := lockInstance(a)
	if err != nil {
		t.Fatal(err)
	}
	// The network namespace is shared even when the state directories differ.
	if _, err := lockInstance(b); err == nil || !strings.Contains(err.Error(), "network namespace") {
		t.Errorf("second instance with another state_dir: %v", err)
	}
	if _, err := lockStateDir(a); err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Errorf("second lock on the same state_dir: %v", err)
	}
	release()

	// A refused state_dir releases the network namespace again.
	held, err := lockStateDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(a); err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Errorf("instance on a locked state_dir: %v", err)
	}
	held.Close()
	release, err = lockInstance(a)
	if err != nil {
		t.Fatalf("after both locks were released: %v", err)
	}
	release()
}
