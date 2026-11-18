package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// netnsLockName is an abstract Unix socket name. Abstract names belong to
// the network namespace, so it is exclusive exactly where the daemon's
// interfaces, rules, routes and nftables table are, whatever its state_dir.
const netnsLockName = "@lotsman"

// lockInstance makes this process the only daemon in its network namespace
// and the only one using stateDir, before serve touches anything either
// shares: the interfaces, rules and firewall, the pid file, the identity and
// the database. The kernel drops both locks when the process exits, crash
// included, so nothing stale is ever left to clean up.
func lockInstance(stateDir string) (release func(), err error) {
	netns, err := lockNetns()
	if err != nil {
		return nil, err
	}
	dir, err := lockStateDir(stateDir)
	if err != nil {
		unix.Close(netns)
		return nil, err
	}
	return func() {
		dir.Close()
		unix.Close(netns)
	}, nil
}

// lockNetns binds netnsLockName. The socket never listens, so nobody can
// connect to it.
func lockNetns() (int, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("instance lock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: netnsLockName}); err != nil {
		unix.Close(fd)
		if errors.Is(err, unix.EADDRINUSE) {
			return -1, errors.New("another lotsman serve is running in this network namespace")
		}
		return -1, fmt.Errorf("instance lock: %w", err)
	}
	return fd, nil
}

// lockStateDir takes an exclusive flock on state_dir/serve.lock. The file is
// never removed: unlinking a lock file lets a later process lock a new file
// while another still holds the old one.
func lockStateDir(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "serve.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another lotsman serve is using state_dir %s", stateDir)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return f, nil
}
