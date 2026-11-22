//go:build linux && integration

package flows

import (
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/dataplane/linux"
)

const (
	clientNS   = "lmf-client"
	providerNS = "lmf-provider"
	reply      = 20 // the servers answer every byte with this many
)

func command(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// inNamespace runs fn on a thread inside the named network namespace, so
// sockets fn opens belong to it. The thread is never unlocked and exits with
// its goroutine.
func inNamespace(t *testing.T, ns string, fn func() error) {
	t.Helper()
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		f, err := os.Open("/var/run/netns/" + ns)
		if err != nil {
			errc <- err
			return
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			errc <- err
			return
		}
		errc <- fn()
	}()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// gateway forwards a client namespace through the real dataplane into a
// provider namespace, as the daemon does with lm0 and one upstream.
func gateway(t *testing.T) {
	t.Helper()
	for _, ns := range []string{clientNS, providerNS} {
		command(t, "ip", "netns", "add", ns)
		t.Cleanup(func() { exec.Command("ip", "netns", "del", ns).Run() })
	}
	for _, link := range []struct{ name, peer, ns, addr string }{
		{"lm0", "client0", clientNS, "10.77.0.2/24"},
		{"lm-up-a", "provider0", providerNS, "10.8.0.1/24"},
	} {
		command(t, "ip", "link", "add", link.name, "type", "veth", "peer", "name", link.peer)
		t.Cleanup(func() { exec.Command("ip", "link", "del", link.name).Run() })
		command(t, "ip", "link", "set", link.peer, "netns", link.ns)
		command(t, "ip", "-n", link.ns, "addr", "add", link.addr, "dev", link.peer)
		command(t, "ip", "-n", link.ns, "link", "set", link.peer, "up")
	}
	command(t, "ip", "-n", clientNS, "route", "add", "default", "via", "10.77.0.1")
	d := linux.New()
	t.Cleanup(func() {
		if err := d.Teardown(); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})
	if err := d.Setup(
		dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/24")},
		[]dataplane.Interface{{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/24")}}, 1380, netip.AddrPort{},
	); err != nil {
		t.Fatal(err)
	}
	if err := d.Route(netip.MustParseAddr("10.77.0.2"), "lm-up-a"); err != nil {
		t.Fatal(err)
	}
}

// servers answers in the provider namespace: TCP on :8080 and UDP on :5353
// send back reply bytes for every byte they receive.
func servers(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var pc net.PacketConn
	inNamespace(t, providerNS, func() (err error) {
		if ln, err = net.Listen("tcp4", "10.8.0.1:8080"); err != nil {
			return err
		}
		pc, err = net.ListenPacket("udp4", "10.8.0.1:5353")
		return err
	})
	t.Cleanup(func() { ln.Close(); pc.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(make([]byte, n*reply)); err != nil {
						return
					}
				}
			}()
		}
	}()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(make([]byte, n*reply), from)
		}
	}()
}

// exchange sends n bytes and reads the whole answer.
func exchange(t *testing.T, c net.Conn, n int) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, n*reply)); err != nil {
		t.Fatal(err)
	}
}

// A TCP and a UDP flow a peer opens through the gateway show up with their
// bytes in the right direction and a rate.
func TestConntrackFlows(t *testing.T) {
	gateway(t)
	// After Setup: the firewall loads nf_conntrack, which adds the sysctl.
	if err := EnableAccounting(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(AccountingSysctl); strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("accounting = %q", got)
	}
	servers(t)
	var tcp, udp net.Conn
	inNamespace(t, clientNS, func() (err error) {
		if tcp, err = net.Dial("tcp4", "10.8.0.1:8080"); err != nil {
			return err
		}
		udp, err = net.Dial("udp4", "10.8.0.1:5353")
		return err
	})
	t.Cleanup(func() { tcp.Close(); udp.Close() })

	peers := fakePeers{netip.MustParseAddr("10.77.0.2"): {ID: "alice", Upstream: "a"}}
	subnet := netip.MustParsePrefix("10.77.0.0/24")
	sampler := NewSampler(Conntrack{}, peers, subnet)
	m := &Meter{Source: sampler, Peers: peers, Subnet: subnet}
	exchange(t, tcp, 1000)
	exchange(t, udp, 100)
	sampler.sample(time.Now())
	if _, err := m.Snapshot(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	exchange(t, tcp, 1000)
	exchange(t, udp, 100)
	sampler.sample(time.Now())
	s, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	want := map[Protocol]netip.AddrPort{TCP: tcp.LocalAddr().(*net.TCPAddr).AddrPort(), UDP: udp.LocalAddr().(*net.UDPAddr).AddrPort()}
	dest := map[Protocol]string{TCP: "10.8.0.1:8080", UDP: "10.8.0.1:5353"}
	if len(s.Flows) != 2 {
		t.Fatalf("flows = %+v, want one TCP and one UDP", s.Flows)
	}
	for _, f := range s.Flows {
		if f.Source != want[f.Protocol] || f.Destination.String() != dest[f.Protocol] || f.PeerID != "alice" || f.Upstream != "a" {
			t.Errorf("flow = %+v", f)
		}
		// The servers answer with more than they get: down outweighs up.
		if f.UpBytes == 0 || f.DownBytes <= f.UpBytes || f.UpRate == 0 || f.DownRate <= f.UpRate {
			t.Errorf("protocol %d: up %d B at %d B/s, down %d B at %d B/s", f.Protocol, f.UpBytes, f.UpRate, f.DownBytes, f.DownRate)
		}
	}
	late := snapshot(t, &Meter{Source: sampler, Peers: peers, Subnet: subnet})
	if len(late.Flows) != 2 {
		t.Fatalf("late view = %+v, want both connections", late)
	}
	for _, f := range late.Flows {
		if f.LastTraffic.IsZero() || f.DownRate != 0 || f.UpRate != 0 {
			t.Errorf("late view flow = %+v, want observed traffic and no rates", f)
		}
	}
}
