//go:build linux && integration

package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// The integration topology mirrors an upstream in the daemon: a kernel TUN
// with its own address, a routing table whose default route is the TUN and a
// rule "from <addr> oif <tun>" as for health probes. Clients are sockets
// bound to that address and interface. Targets live in a separate network
// namespace, reached by the Shadowsocks servers through host routing.
const (
	destNS   = "lmo-dest"
	destAddr = "10.66.0.2"
	tunMTU   = 1420
)

var (
	kernelEcho = netip.MustParseAddrPort(destAddr + ":7")
	kernelDNS  = netip.MustParseAddrPort(destAddr + ":53")
	kernelSink = netip.MustParseAddrPort(destAddr + ":9")
	kernelGen  = netip.MustParseAddrPort(destAddr + ":19")
)

func run(t testing.TB, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// inNamespace runs fn on a thread inside the named network namespace, so
// sockets fn opens belong to it. The thread is never unlocked and exits with
// its goroutine.
func inNamespace(t testing.TB, ns string, fn func() error) {
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

// destination starts the TCP echo, DNS, discard and generator targets in
// their own namespace.
func destination(t testing.TB) {
	t.Helper()
	run(t, "ip", "netns", "add", destNS)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", destNS).Run() })
	run(t, "ip", "link", "add", "lmo-d0", "type", "veth", "peer", "name", "lmo-d1")
	t.Cleanup(func() { exec.Command("ip", "link", "del", "lmo-d0").Run() })
	run(t, "ip", "link", "set", "lmo-d1", "netns", destNS)
	run(t, "ip", "addr", "add", "10.66.0.1/24", "dev", "lmo-d0")
	run(t, "ip", "link", "set", "lmo-d0", "up")
	run(t, "ip", "-n", destNS, "addr", "add", destAddr+"/24", "dev", "lmo-d1")
	run(t, "ip", "-n", destNS, "link", "set", "lmo-d1", "up")
	run(t, "ip", "-n", destNS, "link", "set", "lo", "up")
	var echo, sink, gen net.Listener
	var dns net.PacketConn
	inNamespace(t, destNS, func() (err error) {
		if echo, err = net.Listen("tcp4", kernelEcho.String()); err != nil {
			return err
		}
		if sink, err = net.Listen("tcp4", kernelSink.String()); err != nil {
			return err
		}
		if gen, err = net.Listen("tcp4", kernelGen.String()); err != nil {
			return err
		}
		dns, err = net.ListenPacket("udp4", kernelDNS.String())
		return err
	})
	proxytest.Serve(t, echo, proxytest.Echo)
	proxytest.ServeDNS(t, dns)
	proxytest.Serve(t, sink, func(c net.Conn) { io.Copy(io.Discard, c) })
	proxytest.Serve(t, gen, func(c net.Conn) {
		chunk := make([]byte, 64<<10)
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	})
}

type kernelUpstream struct {
	name   string
	addr   netip.Addr
	server *proxytest.Server
	stack  *Stack
}

// newKernelUpstream creates the TUN as the daemon does, routes it like an
// upstream and starts a Stack with its own Shadowsocks server on it.
func newKernelUpstream(t testing.TB, name, addr string, table int, password string) *kernelUpstream {
	t.Helper()
	dev, err := tunnel.CreateTUN(name, tunMTU)
	if err != nil {
		t.Fatal(err)
	}
	server := proxytest.NewServer(t, password, nil)
	s, err := New(dev, dialers(t, server), Options{}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if err != nil {
		dev.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tbl := fmt.Sprint(table)
	run(t, "ip", "addr", "add", addr+"/32", "dev", name)
	run(t, "ip", "link", "set", name, "up")
	run(t, "sysctl", "-qw", "net.ipv4.conf."+name+".rp_filter=0")
	run(t, "ip", "route", "add", "default", "dev", name, "table", tbl)
	run(t, "ip", "rule", "add", "from", addr, "oif", name, "lookup", tbl, "priority", "4000")
	t.Cleanup(func() {
		exec.Command("ip", "rule", "del", "from", addr, "oif", name, "lookup", tbl, "priority", "4000").Run()
	})
	return &kernelUpstream{name: name, addr: netip.MustParseAddr(addr), server: server, stack: s}
}

// dialer binds to the upstream's address and interface, as health probes do.
func (u *kernelUpstream) dialer() *net.Dialer {
	return &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: u.addr.AsSlice()},
		Timeout:   5 * time.Second,
		Control: func(_, _ string, raw syscall.RawConn) error {
			var bindErr error
			if err := raw.Control(func(fd uintptr) { bindErr = unix.BindToDevice(int(fd), u.name) }); err != nil {
				return err
			}
			return bindErr
		},
	}
}

func (u *kernelUpstream) echo(t *testing.T, msg string) {
	t.Helper()
	c, err := u.dialer().Dial("tcp4", kernelEcho.String())
	if err != nil {
		t.Fatalf("%s: dial: %v", u.name, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if c.LocalAddr().(*net.TCPAddr).AddrPort().Addr() != u.addr {
		t.Fatalf("%s: client bound to %s", u.name, c.LocalAddr())
	}
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != msg {
		t.Fatalf("%s: echo returned %d bytes, %v; want the %d sent", u.name, len(got), err, len(msg))
	}
}

func (u *kernelUpstream) dns(t *testing.T, id uint16) {
	t.Helper()
	d := u.dialer()
	d.LocalAddr = &net.UDPAddr{IP: u.addr.AsSlice()}
	c, err := d.Dial("udp4", kernelDNS.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proxytest.ExchangeDNS(c, id); err != nil {
		t.Fatalf("%s: dns: %v", u.name, err)
	}
}

func TestKernelTUNs(t *testing.T) {
	destination(t)
	a := newKernelUpstream(t, "lmo-a", "10.200.0.1", 201, "secret-a")
	b := newKernelUpstream(t, "lmo-b", "10.200.0.2", 202, "secret-b")
	for i, u := range []*kernelUpstream{a, b} {
		u.echo(t, "hello via "+u.name)
		u.echo(t, strings.Repeat("bulk ", 400_000)) // 2 MB, many segments and windows
		u.dns(t, uint16(i))
	}
	want := []string{"tcp " + kernelEcho.String(), "tcp " + kernelEcho.String(), "udp " + kernelDNS.String()}
	for _, u := range []*kernelUpstream{a, b} {
		if got := u.server.Seen(); !slices.Equal(got, want) {
			t.Errorf("%s server saw %q, want %q", u.name, got, want)
		}
	}

	// The stack must not answer pings itself: Shadowsocks carries no ICMP.
	if out, err := exec.Command("ping", "-n", "-c", "1", "-W", "1", "-I", a.name, destAddr).CombinedOutput(); err == nil {
		t.Errorf("ping through %s was answered:\n%s", a.name, out)
	}
	if n := a.stack.net.Stats().ICMP.V4.PacketsReceived.EchoRequest.Value(); n != 1 {
		t.Errorf("stack received %d echo requests, want 1", n)
	}

	// One upstream closing leaves the other carrying traffic.
	if err := a.stack.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := net.InterfaceByName(a.name); err == nil {
		t.Errorf("%s still exists after Close", a.name)
	}
	b.echo(t, "after a closed")
	b.dns(t, 7)
	if err := b.stack.Close(); err != nil {
		t.Fatal(err)
	}
	if leaked := stackGoroutines(); leaked != "" {
		t.Fatalf("goroutines left after Close:\n%s", leaked)
	}
}

// TestThroughput reports rough numbers for one bulk TCP flow; it is not a
// pass/fail check. The same in-process Shadowsocks server and sink serve
// both runs, so the difference between them is the cost of the TUN and the
// userspace stack. Run with LOTSMAN_PROXY_BENCH=1.
func TestThroughput(t *testing.T) {
	if os.Getenv("LOTSMAN_PROXY_BENCH") == "" {
		t.Skip("set LOTSMAN_PROXY_BENCH=1")
	}
	destination(t)
	u := newKernelUpstream(t, "lmo-a", "10.200.0.1", 201, "secret")
	streams := dialers(t, u.server).Streams
	direct := func(target netip.AddrPort) (net.Conn, error) {
		return streams.DialStream(context.Background(), target.String())
	}
	tunneled := func(target netip.AddrPort) (net.Conn, error) {
		return u.dialer().Dial("tcp4", target.String())
	}
	for range 2 {
		measure(t, "upload, shadowsocks only", true, direct)
		measure(t, "upload, kernel TUN + gVisor + shadowsocks", true, tunneled)
		measure(t, "download, shadowsocks only", false, direct)
		measure(t, "download, kernel TUN + gVisor + shadowsocks", false, tunneled)
	}
	st := u.stack.net.Stats()
	t.Logf("stack: tcp retransmits %d, fast retransmits %d, timeouts %d, dropped packets %d, tx queue full %d",
		st.TCP.Retransmits.Value(), st.TCP.FastRetransmit.Value(), st.TCP.Timeouts.Value(),
		st.DroppedPackets.Value(), st.NICs.TxPacketsDroppedNoBufferSpace.Value())
}

// measure moves 1 GiB over one TCP flow, to the sink for an upload or from
// the generator for a download, and logs throughput and the process CPU
// time spent, which includes the in-process server and targets.
func measure(t *testing.T, name string, upload bool, dial func(netip.AddrPort) (net.Conn, error)) {
	t.Helper()
	const total = 1 << 30
	target := kernelGen
	if upload {
		target = kernelSink
	}
	c, err := dial(target)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer c.Close()
	chunk := make([]byte, 64<<10)
	var before, after unix.Rusage
	unix.Getrusage(unix.RUSAGE_SELF, &before)
	start := time.Now()
	if upload {
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := c.Write(chunk); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
		}
	} else if _, err := io.CopyN(io.Discard, c, total); err != nil {
		t.Fatalf("%s: read: %v", name, err)
	}
	elapsed := time.Since(start)
	unix.Getrusage(unix.RUSAGE_SELF, &after)
	cpu := time.Duration(after.Utime.Nano() - before.Utime.Nano() + after.Stime.Nano() - before.Stime.Nano())
	t.Logf("%s: %d MiB in %v = %.0f Mbit/s, CPU %v (%.2f cores, %.1f ns/byte), GOMAXPROCS %d",
		name, total>>20, elapsed.Round(time.Millisecond), float64(total)*8/elapsed.Seconds()/1e6,
		cpu.Round(time.Millisecond), cpu.Seconds()/elapsed.Seconds(), float64(cpu.Nanoseconds())/float64(total), runtime.GOMAXPROCS(0))
}
