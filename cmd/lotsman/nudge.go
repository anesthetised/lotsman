package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// checkDaemonProc reports whether pid is a process running the executable
// self, so a stale pid file never makes the CLI signal a stranger. procRoot
// is /proc; tests point it at a fake tree.
//
// The comparison is on /proc/<pid>/exe, which the kernel resolves to the real
// path of the running binary, so symlinks and the ExecStart path of the
// systemd unit agree with os.Executable. Unlike /proc/<pid>/comm it cannot be
// changed by the process or matched by a stranger that merely shares a name,
// and it names the file instead of a 15-byte truncation. A binary replaced on
// disk shows up as "<path> (deleted)" and does not match on purpose: that
// daemon predates the replacement and may not handle SIGUSR1, whose default
// action is to terminate the process.
func checkDaemonProc(procRoot string, pid int, self string) error {
	exe, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("process %d is not running (stale lotsman.pid?)", pid)
	case err != nil:
		return fmt.Errorf("cannot inspect process %d (run as the daemon's user): %w", pid, err)
	case exe != self:
		return fmt.Errorf("process %d runs %s, not %s; if the binary was replaced, restart the daemon", pid, exe, self)
	}
	return nil
}
