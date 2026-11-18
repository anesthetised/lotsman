package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
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

var (
	// helloTarget says hello and ends the stream.
	helloTarget = netip.MustParseAddrPort("198.51.100.7:443")
	// xudpDNSTarget resolves like dnsTarget. Without Vision, Xray carries UDP
	// to port 53 as plain VLESS UDP and to other ports as XUDP.
	xudpDNSTarget = netip.MustParseAddrPort("198.51.100.7:5353")
)

// vlessTargets starts the test targets on loopback and maps echoTarget,
// helloTarget and dnsTarget to them, and xudpDNSTarget to the DNS server
// too, for a vlesstest server's redirect.
func vlessTargets(t *testing.T) map[string]string {
	t.Helper()
	listen := func(handle func(net.Conn)) string {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		proxytest.Serve(t, ln, handle)
		return ln.Addr().String()
	}
	dns := proxytest.DNS(t).String()
	return map[string]string{
		echoTarget.String():    listen(proxytest.Echo),
		helloTarget.String():   listen(func(c net.Conn) { io.WriteString(c, "hello") }),
		dnsTarget.String():     dns,
		xudpDNSTarget.String(): dns,
	}
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
	f.dnsTo(t, dnsTarget, id)
}

func (f *vlessFlow) dnsTo(t *testing.T, target netip.AddrPort, id uint16) {
	t.Helper()
	c, err := f.client.DialUDPAddrPort(netip.AddrPort{}, target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := proxytest.ExchangeDNS(c, id); err != nil {
		t.Fatalf("dns exchange %d: %v", id, err)
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
