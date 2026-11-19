//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFirewallFlushRepaired runs "nft flush ruleset" under a running daemon
// whose health round is an hour away: the firewall must be back, and the
// client forwarded again, within seconds (issue #81). Flushing the whole
// ruleset is safe only in a namespace of our own, so the test runs itself
// again in a fresh network namespace.
func TestFirewallFlushRepaired(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	if os.Getenv("LOTSMAN_TEST_OWN_NETNS") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFirewallFlushRepaired$", "-test.v", "-test.timeout=2m")
		cmd.Env = append(os.Environ(), "LOTSMAN_TEST_OWN_NETNS=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("in a fresh network namespace: %v\n%s", err, out)
		}
		t.Logf("in a fresh network namespace:\n%s", out)
		return
	}

	// A host's usual uplink: loose reverse-path filtering drops replies from
	// the internet when no route leads back to them.
	for _, args := range [][]string{
		{"link", "set", "lo", "up"},
		{"link", "add", "uplink", "type", "dummy"},
		{"link", "set", "uplink", "up"},
		{"route", "add", "default", "dev", "uplink"},
	} {
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("ip %v: %v: %s", args, err, out)
		}
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: nl, conf: `+filepath.Join(dir, "nl.conf")+`, geo: NL, provider: acme}
health: {interval: 1h, probe: 198.51.100.1:443, down_after: 2, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	eventually(t, "daemon status", 15*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
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

	if out, err := exec.Command("nft", "flush", "ruleset").CombinedOutput(); err != nil {
		t.Fatalf("nft flush ruleset: %v: %s", err, out)
	}
	flushed := time.Now()
	eventually(t, "exit via nl after the flush", 10*time.Second, func() bool {
		got, err := c.exitVia(time.Second)
		return err == nil && got == "nl"
	})
	t.Logf("forwarding again %s after the flush", time.Since(flushed).Round(time.Millisecond))
	if out, err := exec.Command("nft", "list", "table", "inet", "lotsman").CombinedOutput(); err != nil {
		t.Errorf("table not reinstalled: %v: %s", err, out)
	}
	if body, _, err := scrape(); err != nil || !strings.Contains(body, "lotsman_firewall_repairs_total 1") {
		t.Errorf("repair not counted: %v\n%s", err, body)
	}
}
