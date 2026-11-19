//go:build linux && integration

package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/control"
	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// bulkPort is where the fake internet streams bytes to whoever connects.
const bulkPort = 446

// streamBulk writes to c, about 6 MB/s, until it fails. A write that stalls
// for a second fails too: once the test ends the client is gone without a
// reset, and the test's cleanup waits for this handler.
func streamBulk(c net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		c.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := c.Write(buf); err != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// download reads from the fake internet's bulk port through the client's
// tunnel until the test ends.
func (c *client) download(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := c.tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(internet, bulkPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
}

// TestTrafficRates downloads through an AWG and an Outline upstream, one
// client each, and checks that Watch shows the downloads as down on the
// peer and on its upstream within 3 s.
func TestTrafficRates(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: creates TUNs and programs the kernel")
	}
	dir := t.TempDir()
	nl := newProvider(t, "nl", netip.MustParsePrefix("10.100.0.0/24"), 51001)
	os.WriteFile(filepath.Join(dir, "nl.conf"), []byte(nl.conf), 0o600)
	bulk, err := nl.tnet.ListenTCPAddrPort(netip.AddrPortFrom(internet, bulkPort))
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, bulk, streamBulk)

	name, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, name, func(c net.Conn) { c.Write([]byte("ol")) })
	olBulk, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, olBulk, streamBulk)
	ss := proxytest.NewServer(t, "outline-secret", map[string]string{
		netip.AddrPortFrom(internet, 443).String():      name.Addr().String(),
		netip.AddrPortFrom(internet, bulkPort).String(): olBulk.Addr().String(),
		netip.AddrPortFrom(internet, 53).String():       proxytest.DNS(t).String(),
	})
	os.WriteFile(filepath.Join(dir, "ol.key"), []byte(ss.Key()+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lotsman.yaml")
	os.WriteFile(cfgPath, []byte(`
listen: 127.0.0.1:51820
endpoint: 127.0.0.1:51820
state_dir: `+filepath.Join(dir, "state")+`
upstreams:
  - {name: nl, conf: `+filepath.Join(dir, "nl.conf")+`, provider: awg}
  - {name: ol, kind: outline, conf: `+filepath.Join(dir, "ol.key")+`, provider: outline}
health: {interval: 500ms, probe: 198.51.100.1:443, dns: 198.51.100.1:53, down_after: 2, up_after: 1}
`), 0o600)
	mustRun(t, "-config", cfgPath, "profile", "add", "awg", "provider=awg")
	mustRun(t, "-config", cfgPath, "profile", "add", "outline", "provider=outline")
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
		_, err := os.Stat(filepath.Join(dir, "state", "status.json"))
		return err == nil
	})
	clients := map[string]*client{}
	for user, profile := range map[string]string{"alice": "awg", "bob": "outline"} {
		out, err := capture(t, "-config", cfgPath, "user", "add", user, "-profile", profile)
		if err != nil {
			t.Fatal(err)
		}
		clients[user] = newClient(t, out)
	}
	for user, via := range map[string]string{"alice": "nl", "bob": "ol"} {
		eventually(t, user+" exits via "+via, 15*time.Second, func() bool {
			got, err := clients[user].exitVia(2 * time.Second)
			return err == nil && got == via
		})
	}

	stream, err := controlClient(control.SocketPath).Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	clients["alice"].download(t)
	clients["bob"].download(t)
	// A download is down: more bytes towards the client than from it.
	downloading := func(tr *lotsmanv1.Traffic) bool {
		return tr != nil && tr.DownRate > 100_000 && tr.DownRate > 4*tr.UpRate && tr.DownBytes > tr.UpBytes
	}
	var got *lotsmanv1.StatusSnapshot
	for {
		if got, err = stream.Receive(); err != nil {
			t.Fatal(err)
		}
		done := len(got.Peers) == 2 && len(got.Upstreams) == 2
		var peerRate uint64
		for _, p := range got.Peers {
			done = done && downloading(p.Traffic)
			peerRate += p.Traffic.GetDownRate()
		}
		for _, u := range got.Upstreams {
			done = done && downloading(u.Traffic)
		}
		if done {
			if got.Total.GetDownRate() != peerRate {
				t.Errorf("total down rate %d, want the peers' sum %d", got.Total.GetDownRate(), peerRate)
			}
			break
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("no snapshot with both downloads 3 s after they started; last: %v", got)
		}
	}
	t.Logf("rates %s after the downloads started: %v", time.Since(start).Round(time.Millisecond), got)
}
