package main

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

func envWithPID(t *testing.T, pid int) *env {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(pidPath(dir), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	return &env{cfg: &config.Config{StateDir: dir}}
}

// `user add` and `user rm` must ask the daemon to reconcile only: SIGHUP
// would also apply whatever half-edited lotsman.yaml is on disk.
func TestNudgeDaemonSendsReconcileSignal(t *testing.T) {
	hup, usr1 := make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	signal.Notify(usr1, syscall.SIGUSR1)
	t.Cleanup(func() { signal.Stop(hup); signal.Stop(usr1) })

	// This test binary is its own "daemon", so the identity check passes.
	if msg := captureStderr(t, func() { nudgeDaemon(envWithPID(t, os.Getpid())) }); msg != "" {
		t.Errorf("unexpected stderr: %q", msg)
	}
	select {
	case <-usr1:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGUSR1 not delivered")
	}
	select {
	case <-hup:
		t.Error("SIGHUP delivered: user commands must not trigger a config reload")
	case <-time.After(100 * time.Millisecond):
	}
}

// A pid file left over from a crash may name an unrelated process; it must
// get no signal, and the operator is told why.
func TestNudgeDaemonIgnoresForeignProcess(t *testing.T) {
	sleep := exec.Command("sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	exited := make(chan struct{})
	go func() { sleep.Wait(); close(exited) }()
	t.Cleanup(func() { sleep.Process.Kill(); <-exited })

	msg := captureStderr(t, func() { nudgeDaemon(envWithPID(t, sleep.Process.Pid)) })
	if !strings.Contains(msg, "not nudged") || !strings.Contains(msg, "sleep") {
		t.Errorf("stderr = %q, want an explanation naming the foreign executable", msg)
	}
	select {
	case <-exited:
		t.Error("a process that is not Lotsman was signalled and died")
	case <-time.After(300 * time.Millisecond):
	}
}
