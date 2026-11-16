package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// The client side runs on amneziawg-go's netstack TUN, so these tests need
// no privileges. Targets are TEST-NET addresses that the test server
// redirects to loopback listeners.
var (
	echoTarget = netip.MustParseAddrPort("198.51.100.7:7")
	dnsTarget  = netip.MustParseAddrPort("198.51.100.7:53")
)

type testFlow struct {
	server *proxytest.Server
	stack  *Stack
	client *netstack.Net
}

// dialers parses the server's access key as the daemon does.
func dialers(t testing.TB, server *proxytest.Server) Dialers {
	t.Helper()
	key, err := ParseKey(server.Key())
	if err != nil {
		t.Fatal(err)
	}
	d, err := key.Dialers(server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// newTestFlow starts a server with its own echo and DNS targets and a Stack
// in front of it, reached by a userspace client.
func newTestFlow(t *testing.T, password string, opts Options) *testFlow {
	t.Helper()
	server := newTestServer(t, password)
	return newStack(t, server, dialers(t, server), opts)
}

// newTestServer starts a server with its own echo and DNS targets.
func newTestServer(t *testing.T, password string) *proxytest.Server {
	t.Helper()
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	dns := proxytest.DNS(t)
	return proxytest.NewServer(t, password, map[string]string{
		echoTarget.String(): echo.Addr().String(),
		dnsTarget.String():  dns.String(),
	})
}

// newStack starts a Stack on d, reached by a userspace client.
func newStack(t *testing.T, server *proxytest.Server, d Dialers, opts Options) *testFlow {
	t.Helper()
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dev, d, opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &testFlow{server: server, stack: s, client: client}
}

// dial opens a TCP flow to the echo target.
func (f *testFlow) dial(t *testing.T) (*gonet.TCPConn, error) {
	t.Helper()
	// Generous: gVisor is very slow under the race detector.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := f.client.DialContextTCPAddrPort(ctx, echoTarget)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(30 * time.Second))
	return c, nil
}

// echo sends msg, half-closes and expects msg back followed by the end of
// the stream, which needs half-close to pass both ways.
func (f *testFlow) echo(t *testing.T, msg string) {
	t.Helper()
	c, err := f.dial(t)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || string(got) != msg {
		t.Fatalf("echo returned %d bytes, %v; want the %d sent", len(got), err, len(msg))
	}
}

func (f *testFlow) dialDNS(t *testing.T) net.Conn {
	t.Helper()
	c, err := f.client.DialUDPAddrPort(netip.AddrPort{}, dnsTarget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (f *testFlow) dns(t *testing.T, c net.Conn, id uint16) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proxytest.ExchangeDNS(c, id); err != nil {
		t.Fatalf("dns exchange %d: %v", id, err)
	}
}

func TestStacksCarryTCPAndUDP(t *testing.T) {
	a := newTestFlow(t, "secret-a", Options{})
	b := newTestFlow(t, "secret-b", Options{})
	a.echo(t, "hello through a")
	b.echo(t, strings.Repeat("bulk ", 20_000)) // many segments
	a.dns(t, a.dialDNS(t), 1)
	bDNS := b.dialDNS(t)
	b.dns(t, bDNS, 2)

	want := []string{"tcp " + echoTarget.String(), "udp " + dnsTarget.String()}
	for name, f := range map[string]*testFlow{"a": a, "b": b} {
		if got := f.server.Seen(); !slices.Equal(got, want) {
			t.Errorf("server %s saw %q, want %q", name, got, want)
		}
	}

	// Closing one stack leaves the other working, open flows included.
	if err := a.stack.Close(); err != nil {
		t.Fatal(err)
	}
	b.dns(t, bDNS, 3)
	b.echo(t, "after a closed")
}

func TestDialFailureResetsClient(t *testing.T) {
	f := newTestFlow(t, "secret", Options{})
	f.server.Close()
	start := time.Now()
	if _, err := f.dial(t); err == nil {
		t.Fatal("dial through a closed server succeeded")
	} else if time.Since(start) > 10*time.Second {
		t.Fatalf("dial hung: %v", err)
	}
}

func TestUDPIdleTimeout(t *testing.T) {
	// The timeout runs from the query, so it must outlast a slow exchange
	// (a loaded runner under -race takes over a second).
	f := newTestFlow(t, "secret", Options{DNSIdleTimeout: 3 * time.Second, UDPIdleTimeout: time.Hour})
	c := f.dialDNS(t)
	// Traffic within the timeout keeps the flow on one server socket.
	for id := range uint16(4) {
		f.dns(t, c, id)
		time.Sleep(100 * time.Millisecond)
	}
	if n := f.server.UDPClients(); n != 1 {
		t.Fatalf("server saw %d client sockets, want 1", n)
	}
	// After DNSIdleTimeout, not the general UDP one, the same client flow
	// starts a new one.
	time.Sleep(4 * time.Second)
	f.dns(t, c, 9)
	if n := f.server.UDPClients(); n != 2 {
		t.Fatalf("server saw %d client sockets after idle, want 2", n)
	}
	if st := f.stack.Stats(); st.Flows != 1 {
		t.Fatalf("stats = %+v, want 1 open flow", st)
	}
}

func TestMaxFlows(t *testing.T) {
	f := newTestFlow(t, "secret", Options{MaxFlows: 2, DNSIdleTimeout: time.Hour})
	// One TCP and one UDP flow fill the limit.
	held, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	f.dns(t, f.dialDNS(t), 1)
	if st := f.stack.Stats(); st.Flows != 2 || st.Refused != 0 {
		t.Fatalf("stats = %+v, want 2 flows", st)
	}

	// A third TCP flow is reset at once, a third UDP flow gets no answer.
	if _, err := f.dial(t); err == nil {
		t.Fatal("TCP flow over the limit was accepted")
	}
	c := f.dialDNS(t)
	c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if err := proxytest.ExchangeDNS(c, 2); err == nil {
		t.Fatal("UDP flow over the limit was answered")
	}
	if st := f.stack.Stats(); st.Flows != 2 || st.Refused < 2 {
		t.Fatalf("stats = %+v, want 2 flows and at least 2 refused", st)
	}

	// Closing a flow makes room; so does raising the limit.
	held.Close()
	oneLeft := func() bool { return f.stack.Stats().Flows == 1 }
	waitFor(t, "the closed flow to end", oneLeft)
	f.echo(t, "room again")
	waitFor(t, "the echo flow to end", oneLeft)
	if _, err := f.dial(t); err != nil {
		t.Fatal(err)
	}
	f.stack.SetMaxFlows(3)
	f.dns(t, c, 3)
}

func TestSetDialers(t *testing.T) {
	f := newTestFlow(t, "secret", Options{})
	held, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	// A server at another address takes over new flows, as after the
	// daemon re-resolved a moved server; the open flow stays.
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	moved := proxytest.NewServer(t, "secret", map[string]string{echoTarget.String(): echo.Addr().String()})
	f.stack.SetDialers(dialers(t, moved))
	f.echo(t, "via the moved server")
	if got := moved.Seen(); len(got) != 1 {
		t.Errorf("moved server saw %q, want one flow", got)
	}
	if _, err := io.WriteString(held, "old"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(held, buf); err != nil || string(buf) != "old" {
		t.Fatalf("open flow after the switch: %q, %v", buf, err)
	}
}

func TestCloseStopsEverything(t *testing.T) {
	f := newTestFlow(t, "secret", Options{})
	if _, err := f.dial(t); err != nil {
		t.Fatal(err)
	}
	f.dns(t, f.dialDNS(t), 1)
	// Both flows are open: Close must abort them, not wait for them.
	if err := f.stack.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.stack.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if leaked := stackGoroutines(); leaked != "" {
		t.Fatalf("goroutines left after Close:\n%s", leaked)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stackGoroutines returns the stacks of live goroutines running Stack or
// gVisor code, waiting briefly for exiting ones. Closing the Stack also
// closes the test client's netstack, which owns the TUN.
func stackGoroutines() string {
	var leaked []string
	for range 50 {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		leaked = leaked[:0]
		for g := range strings.SplitSeq(string(buf), "\n\n") {
			if strings.Contains(g, "internal/proxy.(*Stack)") || strings.Contains(g, "gvisor.dev/gvisor") {
				leaked = append(leaked, g)
			}
		}
		if len(leaked) == 0 {
			return ""
		}
		time.Sleep(20 * time.Millisecond)
	}
	return strings.Join(leaked, "\n\n")
}
