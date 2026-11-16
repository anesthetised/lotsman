//go:build linux && integration

package main

import (
	"context"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// newOutline starts an in-process Shadowsocks server whose "internet" mirrors
// the fake providers': 198.51.100.1 answers TCP :443 with "ol", echoes on
// :445 with an "ol:" prefix, and resolves DNS on :53 over UDP and TCP. It
// returns the server and its access key.
func newOutline(t *testing.T) (*proxytest.Server, string) {
	t.Helper()
	listen := func() net.Listener {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	name, echo := listen(), listen()
	proxytest.Serve(t, name, func(c net.Conn) { c.Write([]byte("ol")) })
	proxytest.Serve(t, echo, func(c net.Conn) {
		buf := make([]byte, 64)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			c.Write([]byte("ol:" + string(buf[:n])))
		}
	})
	dns := proxytest.DNS(t)
	server := proxytest.NewServer(t, "outline-secret", map[string]string{
		netip.AddrPortFrom(internet, 443).String(): name.Addr().String(),
		netip.AddrPortFrom(internet, 445).String(): echo.Addr().String(),
		netip.AddrPortFrom(internet, 53).String():  dns.String(),
	})
	return server, server.Key()
}

// blockLoopback drops what the host sends to a loopback port over proto
// inside the disposable container.
func blockLoopback(t *testing.T, proto string, port uint16) func() {
	t.Helper()
	rule := []string{"OUTPUT", "-d", "127.0.0.1", "-p", proto, "--dport", strconv.Itoa(int(port)), "-j", "DROP"}
	if out, err := exec.Command("iptables", append([]string{"-I"}, rule...)...).CombinedOutput(); err != nil {
		t.Fatalf("block %s/%d: %v: %s", proto, port, err, out)
	}
	blocked := true
	restore := func() {
		if blocked {
			if out, err := exec.Command("iptables", append([]string{"-D"}, rule...)...).CombinedOutput(); err != nil {
				t.Errorf("restore %s/%d: %v: %s", proto, port, err, out)
			}
			blocked = false
		}
	}
	t.Cleanup(restore)
	return restore
}

// blockOutline cuts the Shadowsocks server off, TCP and UDP.
func blockOutline(t *testing.T, server *proxytest.Server) func() {
	t.Helper()
	tcp := blockLoopback(t, "tcp", server.Addr().Port())
	udp := blockLoopback(t, "udp", server.Addr().Port())
	return func() { tcp(); udp() }
}

// dnsVia sends a DNS query to the fake internet through the client's tunnel.
func (c *client) dnsVia(id uint16) error {
	conn, err := c.tnet.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(internet, 53))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	return proxytest.ExchangeDNS(conn, id)
}

func TestEndToEndOutline(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	ss, key := newOutline(t)
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(key+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	baseConfig := `
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: ` + filepath.Join(dir, "state") + `
upstreams:
  - {name: nl, conf: ` + filepath.Join(dir, "nl.conf") + `, provider: awg}
  - {name: ol, kind: outline, conf: ` + filepath.Join(dir, "ol.key") + `, provider: outline}
profiles:
  - {name: p, prefer: [{provider: outline}, {provider: awg}]}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 2, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`
	awgFirst := strings.Replace(baseConfig, "prefer: [{provider: outline}, {provider: awg}]", "prefer: [{provider: awg}, {provider: outline}]", 1)
	if awgFirst == baseConfig {
		t.Fatal("test setup: could not derive the awg-first config")
	}
	os.WriteFile(cfgPath, []byte(baseConfig), 0o600)

	out, err := capture(t, "-config", cfgPath, "config", "check")
	if err != nil || !strings.Contains(out, "lm-up-ol  kind outline") || strings.Contains(out, "outline-secret") {
		t.Fatalf("config check: %v\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath, slog.LevelInfo) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	eventually(t, "daemon status", 3*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	out, err = capture(t, "-config", cfgPath, "user", "add", "alice", "-profile", "p")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	exitsVia := func(want string) func() bool {
		return func() bool { got, err := c.exitVia(2 * time.Second); return err == nil && got == want }
	}

	// TCP and UDP from the client leave through the Outline server.
	eventually(t, "exit via ol", 15*time.Second, exitsVia("ol"))
	if err := c.dnsVia(1); err != nil {
		t.Fatalf("udp through ol: %v", err)
	}
	if seen := strings.Join(ss.Seen(), ","); !strings.Contains(seen, "udp 198.51.100.1:53") || !strings.Contains(seen, "tcp 198.51.100.1:443") {
		t.Errorf("outline server saw %s", seen)
	}
	eventually(t, "kind in status and metrics", 10*time.Second, func() bool {
		status, _ := capture(t, "-config", cfgPath, "upstream", "status")
		body, _, err := scrape()
		f := tableRow(status, "ol")
		return err == nil && len(f) == 8 && f[1] == "outline" && f[3] == "up" &&
			strings.Contains(body, `lotsman_upstream_info{upstream="ol",kind="outline",interface="lm-up-ol"} 1`) &&
			strings.Contains(body, `lotsman_upstream_flows{upstream="ol"}`)
	})

	// Flow pinning: a connection through ol survives the profile preferring
	// AWG while ol is alive; new connections use nl.
	long := c.openStream(t)
	if via, err := long.ping("a"); err != nil || via != "ol" {
		t.Fatalf("stream before move: %q, %v", via, err)
	}
	os.WriteFile(cfgPath, []byte(awgFirst), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "new connections via nl", 15*time.Second, exitsVia("nl"))
	if via, err := long.ping("b"); err != nil || via != "ol" {
		t.Errorf("pinned stream after move: %q, %v (want ol)", via, err)
	}

	// AWG blocked: fail over to Outline, and back once it recovers.
	restore := blockProviderEndpoint(t, 51001)
	eventually(t, "exit via ol while nl is blocked", 15*time.Second, exitsVia("ol"))
	if err := c.dnsVia(2); err != nil {
		t.Errorf("udp through ol after failover: %v", err)
	}
	restore()
	eventually(t, "back via nl", 15*time.Second, exitsVia("nl"))

	// Outline blocked: fail over to AWG, and back.
	os.WriteFile(cfgPath, []byte(baseConfig), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "outline preferred again", 15*time.Second, exitsVia("ol"))
	restore = blockOutline(t, ss)
	eventually(t, "ol down", 15*time.Second, func() bool {
		body, _, err := scrape()
		return err == nil && strings.Contains(body, `lotsman_upstream_up{upstream="ol"} 0`)
	})
	eventually(t, "exit via nl while ol is blocked", 15*time.Second, exitsVia("nl"))
	restore()
	eventually(t, "back via ol", 15*time.Second, exitsVia("ol"))

	// Both blocked: the client is cut off, never routed via the host.
	restoreNL, restoreOL := blockProviderEndpoint(t, 51001), blockOutline(t, ss)
	defer restoreNL()
	defer restoreOL()
	eventually(t, "peer rule removed", 20*time.Second, func() bool {
		rules, err := netlink.RuleList(netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if r.Src != nil && r.Src.IP.String() == "10.77.0.2" {
				return false
			}
		}
		return true
	})
	if _, err := c.exitVia(time.Second); err == nil {
		t.Error("client still reaches the internet with every upstream down")
	}
	if err := c.dnsVia(3); err == nil {
		t.Error("client still resolves with every upstream down")
	}
}

// TestEndToEndOutlineDynamicKey runs an Outline upstream from an ssconf://
// key: fetched at start, rotated to another server on reload without
// reopening the upstream, and taken from the cache when a restart finds the
// key's URL failing.
func TestEndToEndOutlineDynamicKey(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	a, keyA := newOutline(t)
	b, keyB := newOutline(t)
	var mu sync.Mutex
	served, status := keyA, http.StatusOK
	keys := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/s3cr3t-e2e" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		fmt.Fprintln(w, served)
	}))
	defer keys.Close()
	offer := func(key string, code int) {
		mu.Lock()
		defer mu.Unlock()
		served, status = key, code
	}
	// The daemon verifies the key server against the system roots; in this
	// test binary nothing has loaded them before, so the environment
	// variable Go reads for them still applies.
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keys.Certificate().Raw}), 0o600)
	t.Setenv("SSL_CERT_FILE", ca)
	url := strings.Replace(keys.URL, "https://", "ssconf://", 1) + "/s3cr3t-e2e#Outline"
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(url+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: ol, kind: outline, conf: `+filepath.Join(dir, "ol.key")+`, provider: outline}
profiles:
  - {name: p, prefer: [{provider: outline}]}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 2, up_after: 1}
`), 0o600)

	out, err := capture(t, "-config", cfgPath, "config", "check")
	if err != nil || !strings.Contains(out, "lm-up-ol  kind outline") || !strings.Contains(out, "dynamic  shadowsocks") ||
		strings.Contains(out, "s3cr3t") || strings.Contains(out, "outline-secret") {
		t.Fatalf("config check: %v\n%s", err, out)
	}

	start := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- serve(ctx, cfgPath, slog.LevelInfo) }()
		var once sync.Once
		stop = func() {
			once.Do(func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("serve: %v", err)
				}
			})
		}
		t.Cleanup(stop)
		eventually(t, "daemon status", 5*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
			return err == nil
		})
		return stop
	}
	exits := func(s *proxytest.Server) int {
		return strings.Count(strings.Join(s.Seen(), ","), "tcp 198.51.100.1:443")
	}

	stop := start()
	out, err = capture(t, "-config", cfgPath, "user", "add", "alice", "-profile", "p")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	eventually(t, "exit via the fetched key's server", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "ol" && exits(a) > 0
	})
	if info, err := os.Stat(filepath.Join(dir, "state", "ssconf", "ol.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cached key: %v", err)
	}

	// A rotated key applies on reload to the running upstream: new
	// connections reach the other server through the same interface.
	index := linkIndex(t, "lm-up-ol")
	offer(keyB, http.StatusOK)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "exit via the rotated key's server", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "ol" && exits(b) > 0
	})
	if linkIndex(t, "lm-up-ol") != index {
		t.Error("the upstream was reopened for a new key")
	}
	before := exits(a)
	if got, err := c.exitVia(2 * time.Second); err != nil || got != "ol" || exits(a) != before {
		t.Errorf("after rotation: %q, %v; old server got %d new connections", got, err, exits(a)-before)
	}

	// A restart while the URL fails runs on the cached key.
	offer("", http.StatusServiceUnavailable)
	stop()
	start()
	out, err = capture(t, "-config", cfgPath, "user", "show", "alice", "-profile", "p")
	if err != nil {
		t.Fatal(err)
	}
	c = newClient(t, out) // a fresh handshake with the restarted daemon
	before = exits(b)
	eventually(t, "exit via the cached key's server", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "ol" && exits(b) > before
	})
}
