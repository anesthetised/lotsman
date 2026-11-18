//go:build linux && integration

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/helper"
)

// TestMain lets this test binary be a proxy helper: serve starts helpers as
// /proc/self/exe, which is the test binary here.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == helper.Subcommand {
		// A crash on demand, in the test binary only: a production helper
		// simply dies on SIGUSR2, which only root or its own user may send.
		usr2 := make(chan os.Signal, 1)
		signal.Notify(usr2, syscall.SIGUSR2)
		go func() {
			<-usr2
			panic("test-induced panic")
		}()
		os.Exit(helper.Main())
	}
	// Another lotsman process, such as a second serve (see runLotsman).
	if os.Getenv("LOTSMAN_TEST_RUN") == "1" {
		if err := run(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "lotsman:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperPIDs lists this process's proxy helpers.
func helperPIDs(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...
		_, rest, _ := strings.Cut(string(stat), ") ")
		f := strings.Fields(rest)
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if len(f) > 1 && f[1] == strconv.Itoa(os.Getpid()) && strings.Contains(string(cmdline), helper.Subcommand) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// procStatus returns the named fields of /proc/<pid>/status.
func procStatus(t *testing.T, pid int) map[string]string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.Lines(string(b)) {
		k, v, _ := strings.Cut(line, ":")
		out[k] = strings.Join(strings.Fields(v), " ")
	}
	return out
}

// proxyUser makes sure the dedicated helper user exists, creating it as the
// install steps do and removing it afterwards, and returns it.
func proxyUser(t *testing.T) *user.User {
	t.Helper()
	if _, err := user.Lookup(helper.ProxyUser); err != nil {
		out, err := exec.Command("useradd", "--system", "--user-group", "--no-create-home",
			"--shell", "/usr/sbin/nologin", helper.ProxyUser).CombinedOutput()
		if err != nil {
			t.Fatalf("useradd: %v: %s", err, out)
		}
		t.Cleanup(func() { exec.Command("userdel", helper.ProxyUser).Run() })
	}
	u, err := user.Lookup(helper.ProxyUser)
	if err != nil {
		t.Fatal(err)
	}
	if u.Uid == "65534" || u.Gid == "65534" {
		t.Fatalf("%s is %s:%s: the test cannot tell it from the fallback", helper.ProxyUser, u.Uid, u.Gid)
	}
	return u
}

// A proxy upstream runs in an unprivileged helper process, as the dedicated
// lotsman-proxy user and with no capabilities. When the helper
// is killed or panics, the upstream is down at once, its clients fail over
// to AWG, an AWG client's open connection is untouched, and the helper comes
// back. Stopping the daemon leaves no helper behind.
func TestEndToEndProxyHelper(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	proxy := proxyUser(t)
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	_, key := newOutline(t)
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(key+"\n"), 0o600)
	cfgPath := filepath.Join(dir, "lotsman.yaml")
	// down_after rounds take 5 s, so ol going down within a second can only
	// come from the helper's exit.
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: nl, conf: `+filepath.Join(dir, "nl.conf")+`, provider: awg}
  - {name: ol, kind: outline, conf: `+filepath.Join(dir, "ol.key")+`, provider: outline}
profiles:
  - {name: p, prefer: [{provider: outline}, {provider: awg}]}
  - {name: a, prefer: [{provider: awg}]}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 10, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			if err := <-served; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	t.Cleanup(stop)
	eventually(t, "daemon status", 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	clientOf := func(name, profile string) *client {
		out, err := capture(t, "-config", cfgPath, "user", "add", name, "-profile", profile)
		if err != nil {
			t.Fatal(err)
		}
		return newClient(t, out)
	}
	alice, bob := clientOf("alice", "p"), clientOf("bob", "a")
	exitsVia := func(c *client, want string) func() bool {
		return func() bool { got, err := c.exitVia(2 * time.Second); return err == nil && got == want }
	}
	eventually(t, "alice via ol", 15*time.Second, exitsVia(alice, "ol"))
	eventually(t, "bob via nl", 15*time.Second, exitsVia(bob, "nl"))
	bobStream := bob.openStream(t)
	bobAlive := func(when string) {
		t.Helper()
		if via, err := bobStream.ping(when); err != nil || via != "nl" {
			t.Fatalf("bob's connection %s: %q, %v", when, via, err)
		}
	}
	bobAlive("before")

	pids := helperPIDs(t)
	if len(pids) != 1 {
		t.Fatalf("helpers = %v, want one for ol", pids)
	}
	st := procStatus(t, pids[0])
	// The dedicated user, not nobody; real, effective, saved and filesystem ids.
	ids, gids := strings.TrimSpace(strings.Repeat(proxy.Uid+" ", 4)), strings.TrimSpace(strings.Repeat(proxy.Gid+" ", 4))
	for k, want := range map[string]string{"Uid": ids, "Gid": gids, "Groups": "",
		"CapInh": "0000000000000000", "CapPrm": "0000000000000000", "CapEff": "0000000000000000", "CapAmb": "0000000000000000"} {
		if st[k] != want {
			t.Errorf("helper %s = %q, want %q", k, st[k], want)
		}
	}
	var mem syscall.Stat_t
	if err := syscall.Stat("/proc/"+strconv.Itoa(pids[0])+"/mem", &mem); err != nil || mem.Uid != 0 {
		t.Errorf("helper memory owned by uid %d (%v), want root: the helper must not be dumpable", mem.Uid, err)
	}

	crash := func(sig syscall.Signal, restarts string) {
		t.Helper()
		before := helperPIDs(t)
		if len(before) != 1 {
			t.Fatalf("helpers = %v", before)
		}
		syscall.Kill(before[0], sig)
		eventually(t, "ol down at once after "+sig.String(), 1500*time.Millisecond, func() bool {
			body, _, err := scrape()
			return err == nil && strings.Contains(body, `lotsman_upstream_up{upstream="ol"} 0`) &&
				strings.Contains(body, `lotsman_proxy_restarts_total{upstream="ol"} `+restarts)
		})
		eventually(t, "alice fails over to nl", 5*time.Second, exitsVia(alice, "nl"))
		bobAlive("after " + sig.String())
		eventually(t, "a new helper and alice back via ol", 20*time.Second, func() bool {
			now := helperPIDs(t)
			return len(now) == 1 && now[0] != before[0] && exitsVia(alice, "ol")()
		})
		if err := alice.dnsVia(1); err != nil {
			t.Errorf("udp through the restarted helper: %v", err)
		}
	}
	crash(syscall.SIGKILL, "1")
	crash(syscall.SIGUSR2, "2") // panics, through TestMain's hook
	bobAlive("at the end")

	stop()
	if left := helperPIDs(t); len(left) != 0 {
		t.Errorf("helpers left after stop: %v", left)
	}
}
