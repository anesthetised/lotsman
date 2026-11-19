//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/control"
)

// runLotsman runs this test binary as a separate lotsman process (see
// TestMain) and returns its stderr and exit error.
func runLotsman(t *testing.T, attr *syscall.SysProcAttr, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "LOTSMAN_TEST_RUN=1")
	cmd.SysProcAttr = attr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return stderr.String(), err
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("lotsman %v did not exit: %s", args, stderr.String())
		return "", nil
	}
}

// A second serve, whether it shares the running daemon's state_dir, only its
// network namespace or only its state_dir, is refused before it changes
// anything: the running daemon keeps its pid file, interface, rules and
// firewall table and control socket, and its client keeps working.
func TestSecondServeLeavesRunningGatewayAlone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	config := func(listen, stateDir string) string {
		return `
listen: ` + listen + `
endpoint: ` + listen + `
state_dir: ` + stateDir + `
upstreams:
  - {name: nl, conf: ` + filepath.Join(dir, "nl.conf") + `, geo: NL, provider: acme}
health: {interval: 500ms, probe: 198.51.100.1:443, down_after: 2, up_after: 1}
`
	}
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(config("127.0.0.1:51820", stateDir)), 0o600)
	// Same network namespace, another state_dir and listen port.
	otherState := filepath.Join(dir, "other-state")
	otherCfg := filepath.Join(dir, "other.yaml")
	os.WriteFile(otherCfg, []byte(config("127.0.0.1:51821", otherState)), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	eventually(t, "daemon status", 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(stateDir, "status.json"))
		return err == nil
	})
	out, err := capture(t, "-config", cfgPath, "device", "add", "alice/phone", "NL")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	eventually(t, "exit via nl", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})
	downIndex := linkIndex(t, "lm0")
	socket, err := os.Stat(control.SocketPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, config, want string
		attr               *syscall.SysProcAttr
	}{
		{"same state_dir", cfgPath, "network namespace", nil},
		{"same network namespace", otherCfg, "network namespace", nil},
		{"same state_dir in another network namespace", cfgPath, "state_dir",
			&syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr, err := runLotsman(t, tc.attr, "-config", tc.config, "serve")
			if err == nil || !strings.Contains(stderr, "another lotsman serve") || !strings.Contains(stderr, tc.want) {
				t.Errorf("second serve: %v: %s", err, stderr)
			}
			if pid, err := os.ReadFile(pidPath(stateDir)); err != nil || string(pid) != strconv.Itoa(os.Getpid()) {
				t.Errorf("pid file: %q, %v", pid, err)
			}
			if out, err := exec.Command("nft", "list", "table", "inet", "lotsman").CombinedOutput(); err != nil {
				t.Errorf("firewall table: %v: %s", err, out)
			}
			if linkIndex(t, "lm0") != downIndex {
				t.Error("lm0 was recreated")
			}
			if !hasPeerRule(t, "10.77.0.2") {
				t.Error("the client's routing rule is gone")
			}
			if got, err := c.exitVia(2 * time.Second); err != nil || got != "nl" {
				t.Errorf("client after the second serve: %q, %v", got, err)
			}
			if fi, err := os.Stat(control.SocketPath); err != nil || !os.SameFile(fi, socket) {
				t.Errorf("control socket replaced or removed: %v", err)
			}
			stream, err := controlClient(control.SocketPath).Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
			if err == nil {
				_, err = stream.Receive()
				stream.Close()
			}
			if err != nil {
				t.Errorf("control socket after the second serve: %v", err)
			}
		})
	}
	if _, err := os.Stat(otherState); !os.IsNotExist(err) {
		t.Errorf("the refused serve created its state_dir: %v", err)
	}
}

func hasPeerRule(t *testing.T, peer string) bool {
	t.Helper()
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Src != nil && r.Src.IP.String() == peer {
			return true
		}
	}
	return false
}
