package main

import (
	"fmt"
	"os"
	"syscall"
)

// signalDaemon sends SIGUSR1 (reconcile only, no config reload) to pid after
// checking that it is a Lotsman daemon.
func signalDaemon(pid int) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}
	if err := checkDaemonProc("/proc", pid, self); err != nil {
		return err
	}
	return syscall.Kill(pid, syscall.SIGUSR1)
}
