//go:build linux && integration

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/tun/netstack"
	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// internet is an address every fake provider serves, like a real host every
// real upstream can reach. It answers TCP on :443 with the provider's name.
var internet = netip.MustParseAddr("198.51.100.1")

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// provider is a fake VPN provider: an AmneziaWG server on netstack that
// Lotsman's upstream device connects to.
type provider struct {
	name   string
	dev    *tunnel.Device
	conf   string // what the provider hands to its customer (Lotsman)
	params awgconf.Params
}

func newProvider(t *testing.T, name string, tunnelNet netip.Prefix, port uint16) *provider {
	t.Helper()
	serverKey, _ := awgconf.GeneratePrivateKey()
	clientKey, _ := awgconf.GeneratePrivateKey()
	serverAddr := tunnelNet.Addr().Next() // .1
	clientAddr := serverAddr.Next()       // .2, Lotsman's address inside this provider
	params := awgconf.Params{S1: 20, S2: 30, S3: 10, S4: 8,
		H1: awgconf.Range{Lo: 1000, Hi: 1100}, H2: awgconf.Range{Lo: 2000, Hi: 2100},
		H3: awgconf.Range{Lo: 3000, Hi: 3100}, H4: awgconf.Range{Lo: 4000, Hi: 4100}}

	tn, tnet, err := netstack.CreateNetTUN([]netip.Addr{serverAddr, internet}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	dev := tunnel.New("provider-"+name, tn, quiet)
	server := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: serverKey, ListenPort: port, Params: params},
		Peers:     []awgconf.Peer{{PublicKey: clientKey.Public(), AllowedIPs: []netip.Prefix{netip.PrefixFrom(clientAddr, 32)}}},
	}
	if err := dev.Configure(server.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)

	ln, err := tnet.ListenTCPAddrPort(netip.AddrPortFrom(internet, 443))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(name))
			c.Close()
		}
	}()
	// A long-lived echo on :445 for flow-pinning checks; replies are prefixed with the provider name.
	echo, err := tnet.ListenTCPAddrPort(netip.AddrPortFrom(internet, 445))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					c.Write([]byte(name + ":" + string(buf[:n])))
				}
			}()
		}
	}()

	customer := awgconf.Config{
		Interface: awgconf.Interface{PrivateKey: clientKey, Addresses: []netip.Prefix{netip.PrefixFrom(clientAddr, 32)}, MTU: 1400, Params: params},
		Peers: []awgconf.Peer{{PublicKey: serverKey.Public(), Endpoint: "localhost:" + strconv.Itoa(int(port)),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}, PersistentKeepalive: 5}},
	}
	return &provider{name: name, dev: dev, conf: customer.String(), params: params}
}

// client is a user's Amnezia app: a netstack device configured from the
// config `user add` printed.
type client struct {
	dev  *tunnel.Device
	tnet *netstack.Net
}

func newClient(t *testing.T, conf string) *client {
	t.Helper()
	cfg, err := awgconf.Parse(strings.NewReader(conf))
	if err != nil {
		t.Fatal(err)
	}
	tn, tnet, err := netstack.CreateNetTUN([]netip.Addr{cfg.Interface.Addresses[0].Addr()}, cfg.Interface.DNS, cfg.Interface.MTU)
	if err != nil {
		t.Fatal(err)
	}
	dev := tunnel.New("client", tn, quiet)
	if err := dev.Configure(cfg.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := dev.Up(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)
	return &client{dev: dev, tnet: tnet}
}

// stream is an open connection through the tunnel; ping returns which provider echoed.
type stream struct{ conn net.Conn }

func (c *client) openStream(t *testing.T) *stream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := c.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(internet, 445))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &stream{conn: conn}
}

func (s *stream) ping(msg string) (string, error) {
	s.conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := s.conn.Write([]byte(msg)); err != nil {
		return "", err
	}
	buf := make([]byte, 64)
	n, err := s.conn.Read(buf)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(buf[:n]), ":"+msg), nil
}

// exitVia dials the fake internet through the tunnel and returns which provider answered.
func (c *client) exitVia(timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := c.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(internet, 443))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(timeout))
	b, err := io.ReadAll(conn)
	return string(b), err
}

// scrape fetches the metrics endpoint the test config enables.
func scrape() (body, contentType string, err error) {
	resp, err := http.Get("http://127.0.0.1:19100/metrics")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.Header.Get("Content-Type"), err
}

func linkIndex(t *testing.T, name string) int {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return l.Attrs().Index
}

// providerHandshake is when the provider last completed a handshake with Lotsman.
func providerHandshake(t *testing.T, p *provider) time.Time {
	t.Helper()
	peers, err := p.dev.Peers()
	if err != nil || len(peers) != 1 {
		t.Fatalf("provider %s peers: %v, %v", p.name, peers, err)
	}
	return peers[0].LastHandshake
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEnd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	de := newProvider(t, "de", netip.MustParsePrefix("10.101.0.0/24"), 51002)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	os.WriteFile(filepath.Join(dir, "de.conf"), []byte(de.conf), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	baseConfig := `
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: ` + filepath.Join(dir, "state") + `
upstreams:
  - {name: nl, conf: ` + filepath.Join(dir, "nl.conf") + `, geo: NL}
  - {name: de, conf: ` + filepath.Join(dir, "de.conf") + `, geo: DE}
profiles:
  - {name: eu, prefer: [{geo: NL}, {geo: DE}]}
health: {interval: 500ms, probe: 198.51.100.1:443, down_after: 2, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`
	os.WriteFile(cfgPath, []byte(baseConfig), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, cfgPath) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})

	eventually(t, "daemon status", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	out, err := capture(t, "-config", cfgPath, "user", "add", "alice", "-profile", "eu")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)

	// Golden path: traffic exits through the preferred upstream.
	eventually(t, "exit via nl", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})

	// The metrics endpoint reflects the routing above.
	eventually(t, "metrics endpoint", 10*time.Second, func() bool {
		body, ct, err := scrape()
		return err == nil && strings.HasPrefix(ct, "text/plain; version=0.0.4") && strings.Contains(body, `lotsman_upstream_up{upstream="nl"} 1`)
	})
	if body, _, _ := scrape(); !strings.Contains(body, `lotsman_peer_upstream{user="alice",device="default",profile="eu",upstream="nl"} 1`) {
		t.Errorf("peer metric missing:\n%s", body)
	}

	// Reload without changes: nothing may be touched. The interface keeps its
	// index and the provider sees no new handshake.
	nlIndex := linkIndex(t, "lm-up-nl")
	handshakeBefore := providerHandshake(t, nl)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	time.Sleep(2 * time.Second)
	if got, err := c.exitVia(2 * time.Second); err != nil || got != "nl" {
		t.Fatalf("after no-op reload: %q, %v", got, err)
	}
	if linkIndex(t, "lm-up-nl") != nlIndex {
		t.Error("no-op reload recreated lm-up-nl")
	}
	if !providerHandshake(t, nl).Equal(handshakeBefore) {
		t.Error("no-op reload caused a new handshake with the provider")
	}

	// The provider file changes: the upstream is replaced with a fresh
	// interface that must be fully configured, and traffic keeps flowing.
	edited := strings.Replace(nl.conf, "PersistentKeepalive = 5", "PersistentKeepalive = 7", 1)
	if edited == nl.conf {
		t.Fatal("test setup: expected PersistentKeepalive = 5 in the provider config")
	}
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(edited), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "exit via nl after replacing its config", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})
	if linkIndex(t, "lm-up-nl") == nlIndex {
		t.Error("changed provider config did not recreate lm-up-nl")
	}
	if l, err := netlink.LinkByName("lm-up-nl"); err == nil {
		if addrs, _ := netlink.AddrList(l, netlink.FAMILY_V4); len(addrs) != 1 {
			t.Errorf("recreated lm-up-nl has addresses %v", addrs)
		}
	}

	// Flow pinning: a connection opened through NL survives the profile
	// being repointed at DE while NL is still alive; new connections use DE.
	long := c.openStream(t)
	if via, err := long.ping("a"); err != nil || via != "nl" {
		t.Fatalf("stream before move: %q, %v", via, err)
	}
	os.WriteFile(cfgPath, []byte(strings.Replace(baseConfig, "prefer: [{geo: NL}, {geo: DE}]", "prefer: [{geo: DE}, {geo: NL}]", 1)), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "new connections via de", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "de"
	})
	if via, err := long.ping("b"); err != nil || via != "nl" {
		t.Errorf("pinned stream after move: %q, %v (want nl)", via, err)
	}
	os.WriteFile(cfgPath, []byte(baseConfig), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "back via nl", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "nl"
	})

	if body, _, _ := scrape(); !strings.Contains(body, `lotsman_reloads_total{result="ok"} `) || strings.Contains(body, `lotsman_reloads_total{result="ok"} 0`) {
		t.Errorf("reloads not counted:\n%s", body)
	}

	// NL is removed from the config and the daemon reloaded on SIGHUP:
	// traffic moves to DE without the client reconnecting, and the NL TUN is gone.
	withoutNL := strings.Replace(baseConfig, "  - {name: nl, conf: "+filepath.Join(dir, "nl.conf")+", geo: NL}\n", "", 1)
	withoutNL = strings.Replace(withoutNL, "prefer: [{geo: NL}, {geo: DE}]", "prefer: [{geo: DE}]", 1)
	if withoutNL == baseConfig {
		t.Fatal("test setup: could not derive the config without nl")
	}
	os.WriteFile(cfgPath, []byte(withoutNL), 0o600)
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	eventually(t, "reroute to de after reload", 15*time.Second, func() bool {
		got, err := c.exitVia(2 * time.Second)
		return err == nil && got == "de"
	})
	if _, err := netlink.LinkByName("lm-up-nl"); err == nil {
		t.Error("lm-up-nl still exists after being removed from the config")
	}
	nl.dev.Close()

	// Every upstream is gone: the daemon must drop the peer's rule so the
	// firewall blocks it instead of letting it out through the host uplink.
	de.dev.Close()
	eventually(t, "remaining upstream down", 15*time.Second, func() bool {
		status, _ := capture(t, "-config", cfgPath, "upstream", "status")
		return strings.Count(status, "down") == 1 && !strings.Contains(status, "up ")
	})
	eventually(t, "peer rule removed", 5*time.Second, func() bool {
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
}
