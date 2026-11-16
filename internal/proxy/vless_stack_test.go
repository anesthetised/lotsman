//go:build !race

// The tests here run Xray's VLESS server, which races (see vlesstest), so
// they are left out of race-detector builds.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

type vlessFlow struct {
	server *vlesstest.Server
	stack  *Stack
	client *netstack.Net
}

// helloTarget says hello and ends the stream.
var helloTarget = netip.MustParseAddrPort("198.51.100.7:443")

// newVLESSServer starts a server whose echo, hello and DNS targets are the
// test targets, redirected to loopback.
func newVLESSServer(t *testing.T) *vlesstest.Server {
	t.Helper()
	listen := func(handle func(net.Conn)) string {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		proxytest.Serve(t, ln, handle)
		return ln.Addr().String()
	}
	return vlesstest.NewServer(t, map[string]string{
		echoTarget.String():  listen(proxytest.Echo),
		helloTarget.String(): listen(func(c net.Conn) { io.WriteString(c, "hello") }),
		dnsTarget.String():   proxytest.DNS(t).String(),
	}, nil)
}

// hello reads what helloTarget says: the target's end of stream must reach
// the client as a clean one.
func (f *vlessFlow) hello(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := f.client.DialContextTCPAddrPort(ctx, helloTarget)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(c); err != nil || string(got) != "hello" {
		t.Fatalf("hello = %q, %v", got, err)
	}
}

// newVLESSFlow runs a Stack in front of server, configured from link as the
// daemon would, and reached by a userspace client.
func newVLESSFlow(t *testing.T, server *vlesstest.Server, link string) *vlessFlow {
	t.Helper()
	key, err := ParseSpecKey(link)
	if err != nil {
		t.Fatal(err)
	}
	d, err := key.Dialers(server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dev, d, Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &vlessFlow{server: server, stack: s, client: client}
}

func (f *vlessFlow) dial(t *testing.T) *gonet.TCPConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := f.client.DialContextTCPAddrPort(ctx, echoTarget)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	return c
}

// echo sends msg and reads it back. It does not half-close: VLESS carries
// no half-close to the target, which then never ends its echo.
func (f *vlessFlow) echo(t *testing.T, msg string) {
	t.Helper()
	c := f.dial(t)
	go io.WriteString(c, msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
		t.Fatalf("echo of %d bytes: %v", len(msg), err)
	}
}

func (f *vlessFlow) dns(t *testing.T, id uint16) {
	t.Helper()
	c, err := f.client.DialUDPAddrPort(netip.AddrPort{}, dnsTarget)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := proxytest.ExchangeDNS(c, id); err != nil {
		t.Fatalf("dns exchange %d: %v", id, err)
	}
}

// Two VLESS upstreams in one process carry TCP and UDP independently.
func TestVLESSStacksCarryTCPAndUDP(t *testing.T) {
	sa, sb := newVLESSServer(t), newVLESSServer(t)
	vlesstest.WaitReady(t, sa, sb)
	a := newVLESSFlow(t, sa, sa.Link())
	b := newVLESSFlow(t, sb, sb.LinkWith(url.Values{"flow": {flowVisionUDP443}, "fp": {"firefox"}}))
	for i, f := range []*vlessFlow{a, b} {
		f.echo(t, "hello")
		f.echo(t, strings.Repeat("bulk ", 200_000)) // 1 MB, many segments
		f.hello(t)
		f.dns(t, uint16(i))
	}
	if st := a.stack.Stats(); st.Refused != 0 {
		t.Errorf("stats = %+v", st)
	}

	// Closing one stack, and its Xray instance, leaves the other working.
	if err := a.stack.Close(); err != nil {
		t.Fatal(err)
	}
	b.echo(t, "after a closed")
	b.dns(t, 9)
}

// New dialers, as after the server moved or its link changed, take new
// flows; closing the old Xray instance leaves its open flows working.
func TestVLESSSetDialers(t *testing.T) {
	a, b := newVLESSServer(t), newVLESSServer(t)
	vlesstest.WaitReady(t, a, b)
	f := newVLESSFlow(t, a, a.Link())
	held := f.dial(t)
	go io.WriteString(held, "old")
	if _, err := io.ReadFull(held, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	key, err := ParseSpecKey(b.Link())
	if err != nil {
		t.Fatal(err)
	}
	d, err := key.Dialers(b.Addr())
	if err != nil {
		t.Fatal(err)
	}
	f.stack.SetDialers(d)
	a.Close() // no new connections to a: new flows must use b
	f.echo(t, "via b")
	go io.WriteString(held, "still")
	if _, err := io.ReadFull(held, make([]byte, 5)); err != nil {
		t.Fatalf("open flow after the switch: %v", err)
	}
}

// A VLESS dial returns before the server is reached, so the stack accepts
// the client's handshake; a server that is down must then reset the flow
// quickly rather than leave it hanging.
func TestVLESSServerDownResetsClient(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	f := newVLESSFlow(t, s, s.Link())
	f.echo(t, "up")
	s.Close()
	c := f.dial(t)
	start := time.Now()
	io.WriteString(c, "x")
	_, err := c.Read(make([]byte, 1))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("read with the server down: %v, want a reset", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("reset took %v", d)
	}
}

// A client with the wrong server key never gets data through: the server
// treats it as a stranger and relays it to the camouflage site.
func TestVLESSWrongKey(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	f := newVLESSFlow(t, s, s.LinkWith(url.Values{"pbk": {"wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E"}}))
	c := f.dial(t)
	io.WriteString(c, "hello")
	if _, err := io.ReadFull(c, make([]byte, 5)); err == nil {
		t.Fatal("echo succeeded with a wrong server key")
	}
}

// Xray's log reaches slog.
func TestVLESSLogsXray(t *testing.T) {
	s := newVLESSServer(t)
	vlesstest.WaitReady(t, s)
	var logs syncLog
	LogXray(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { LogXray(slog.New(slog.DiscardHandler)) })
	f := newVLESSFlow(t, s, s.Link())
	s.Close()
	for range 2 {
		c := f.dial(t)
		io.WriteString(c, "x")
		c.Read(make([]byte, 1))
		c.Close()
	}
	waitFor(t, "two failed flows logged", func() bool {
		return strings.Count(logs.String(), "failed to find an available destination") >= 2
	})
	// Xray reports a failed flow as info: a debug message here.
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "failed to find an available destination") &&
			(!strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "component=xray")) {
			t.Errorf("failed flow logged as %s", line)
		}
	}
	if strings.Contains(logs.String(), s.ID()) {
		t.Error("the log shows the user ID")
	}
}

type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
