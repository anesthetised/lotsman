//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

// newVLESS starts an in-process Xray VLESS + REALITY server, with Vision
// over TCP or, if grpc, over gRPC, whose "internet" mirrors the fake
// providers': 198.51.100.1 answers TCP :443 with "vl", echoes on :445 with a
// "vl:" prefix, and resolves DNS on :53 over UDP and TCP. The upstream's
// helper is a child process; the server runs here.
func newVLESS(t *testing.T, grpc bool) *vlesstest.Server {
	t.Helper()
	listen := func(handle func(net.Conn)) string {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		proxytest.Serve(t, ln, handle)
		return ln.Addr().String()
	}
	redirect := map[string]string{
		netip.AddrPortFrom(internet, 443).String(): listen(func(c net.Conn) { c.Write([]byte("vl")) }),
		netip.AddrPortFrom(internet, 445).String(): listen(func(c net.Conn) {
			buf := make([]byte, 64)
			for {
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				c.Write([]byte("vl:" + string(buf[:n])))
			}
		}),
		netip.AddrPortFrom(internet, 53).String(): proxytest.DNS(t).String(),
	}
	server := vlesstest.NewServer(t, redirect, nil)
	if grpc {
		server = vlesstest.NewGRPCServer(t, redirect, "e2e")
	}
	vlesstest.WaitReady(t, server)
	return server
}

// TestEndToEndVLESS runs a VLESS upstream next to an AWG one: TCP and UDP
// through VLESS, status and metrics, flow pinning, failover both ways and
// fail-closed with both down.
func TestEndToEndVLESS(t *testing.T) {
	endToEndVLESS(t, false, "vless reality xtls-rprx-vision")
}

// TestEndToEndVLESSGRPC is TestEndToEndVLESS over gRPC, which carries every
// flow over one connection to the server.
func TestEndToEndVLESSGRPC(t *testing.T) {
	endToEndVLESS(t, true, "vless reality grpc")
}

func endToEndVLESS(t *testing.T, grpc bool, transport string) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	vl := newVLESS(t, grpc)
	os.WriteFile(filepath.Join(dir, "vl.key"), []byte(vl.Link()+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	baseConfig := `
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: ` + filepath.Join(dir, "state") + `
upstreams:
  - {name: nl, conf: ` + filepath.Join(dir, "nl.conf") + `, geo: NL, provider: awg}
  - {name: vl, kind: vless, conf: ` + filepath.Join(dir, "vl.key") + `, geo: SE, provider: vless}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 2, up_after: 1}
metrics: {listen: 127.0.0.1:19100}
`
	os.WriteFile(cfgPath, []byte(baseConfig), 0o600)

	out, err := capture(t, "-config", cfgPath, "config", "check")
	if err != nil || !strings.Contains(out, "lm-up-vl  kind vless") || !strings.Contains(out, transport) ||
		strings.Contains(out, vl.ID()) {
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
	out, err = capture(t, "-config", cfgPath, "device", "add", "alice/phone", "SE", "NL")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, out)
	exitsVia := func(want string) func() bool {
		return func() bool { got, err := c.exitVia(2 * time.Second); return err == nil && got == want }
	}

	// TCP and UDP from the client leave through the VLESS server.
	eventually(t, "exit via vl", 15*time.Second, exitsVia("vl"))
	if err := c.dnsVia(1); err != nil {
		t.Fatalf("udp through vl: %v", err)
	}
	if len(helperPIDs(t)) != 1 {
		t.Errorf("helpers = %v, want one for vl", helperPIDs(t))
	}
	eventually(t, "kind in status and metrics", 10*time.Second, func() bool {
		status, _ := capture(t, "-config", cfgPath, "upstream", "status")
		body, _, err := scrape()
		f := tableRow(status, "vl")
		return err == nil && len(f) == 10 && f[1] == "vless" && f[3] == "up" &&
			strings.Contains(body, `lotsman_upstream_info{upstream="vl",kind="vless",interface="lm-up-vl"} 1`) &&
			strings.Contains(body, `lotsman_upstream_flows{upstream="vl"}`)
	})

	// Flow pinning: a connection through vl survives the route preferring
	// AWG while vl is alive; new connections use nl.
	long := c.openStream(t)
	if via, err := long.ping("a"); err != nil || via != "vl" {
		t.Fatalf("stream before move: %q, %v", via, err)
	}
	mustRun(t, "-config", cfgPath, "device", "route", "alice/phone", "NL", "SE")
	eventually(t, "new connections via nl", 15*time.Second, exitsVia("nl"))
	if via, err := long.ping("b"); err != nil || via != "vl" {
		t.Errorf("pinned stream after move: %q, %v (want vl)", via, err)
	}

	// AWG blocked: fail over to VLESS, and back once it recovers.
	restore := blockProviderEndpoint(t, 51001)
	eventually(t, "exit via vl while nl is blocked", 15*time.Second, exitsVia("vl"))
	if err := c.dnsVia(2); err != nil {
		t.Errorf("udp through vl after failover: %v", err)
	}
	restore()
	eventually(t, "back via nl", 15*time.Second, exitsVia("nl"))

	// VLESS blocked (it uses TCP only, UDP included): fail over to AWG,
	// and back.
	mustRun(t, "-config", cfgPath, "device", "route", "alice/phone", "SE", "NL")
	eventually(t, "vless preferred again", 15*time.Second, exitsVia("vl"))
	restore = blockLoopback(t, "tcp", vl.Addr().Port())
	eventually(t, "vl down", 15*time.Second, func() bool {
		body, _, err := scrape()
		return err == nil && strings.Contains(body, `lotsman_upstream_up{upstream="vl"} 0`)
	})
	eventually(t, "exit via nl while vl is blocked", 15*time.Second, exitsVia("nl"))
	restore()
	eventually(t, "back via vl", 15*time.Second, exitsVia("vl"))

	// Both blocked: the client is cut off, never routed via the host.
	restoreNL, restoreVL := blockProviderEndpoint(t, 51001), blockLoopback(t, "tcp", vl.Addr().Port())
	defer restoreNL()
	defer restoreVL()
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
