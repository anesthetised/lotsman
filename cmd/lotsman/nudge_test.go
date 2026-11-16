package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCheckDaemonProc(t *testing.T) {
	const self = "/usr/local/bin/lotsman"
	proc := t.TempDir()
	mkproc := func(pid int) string {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "exe")
	}
	for pid, exe := range map[int]string{10: self, 11: "/usr/bin/sshd", 12: self + " (deleted)"} {
		if err := os.Symlink(exe, mkproc(pid)); err != nil {
			t.Fatal(err)
		}
	}
	// An exe that is not a symlink cannot be read as a link.
	if err := os.WriteFile(mkproc(13), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		pid     int
		wantErr string // substring; empty means accepted
	}{
		{"same executable", 10, ""},
		{"another program", 11, "runs /usr/bin/sshd"},
		{"replaced binary", 12, "restart the daemon"},
		{"no such process", 99, "not running"},
		{"unreadable exe", 13, "cannot inspect"},
	} {
		err := checkDaemonProc(proc, tc.pid, self)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.wantErr)
		}
	}
}
