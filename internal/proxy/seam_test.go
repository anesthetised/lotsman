package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"golang.getoutline.org/sdk/transport"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// A server that fails mid-flow must reach the client as a reset, not as
// the clean end of a stream it would take for complete.
func TestRemoteFailureResetsClient(t *testing.T) {
	f := newTestFlow(t, "secret", Options{})
	c, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	f.server.Reset()
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("read after the server failed: %v, want a reset", err)
	}
	// A clean end still arrives as one: echo half-closes back.
	f.echo(t, "still clean")
}

// lazyDialer stands in for a dialer that returns before it reaches the
// server, as VLESS does: the connection lives on the dial context, so a
// context cancelled after the dial would kill it.
type lazyDialer struct {
	inner  transport.StreamDialer
	delay  atomic.Int64 // time.Duration
	closed atomic.Int32
}

func (d *lazyDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	time.Sleep(time.Duration(d.delay.Load())) // ignores ctx, as a slow dialer may
	c, err := d.inner.DialStream(context.Background(), addr)
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { c.Close() })
	return c, nil
}

func (d *lazyDialer) Close() error {
	d.closed.Add(1)
	return nil
}

func TestDialersSeam(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	server := proxytest.NewServer(t, "secret", map[string]string{echoTarget.String(): echo.Addr().String()})
	ss := dialers(t, server)
	lazy := &lazyDialer{inner: ss.Streams}
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dev, Dialers{Streams: lazy, Packets: ss.Packets, Closer: lazy}, Options{DialTimeout: time.Second}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &testFlow{server: server, stack: s, client: client}

	// The flow outlives its dial.
	f.echo(t, "the flow keeps its context")

	// A dial that returns after DialTimeout is refused even if it worked.
	lazy.delay.Store(int64(2 * time.Second))
	if _, err := f.dial(t); err == nil {
		t.Fatal("a dial past the timeout was accepted")
	}

	// Replacing the dialers closes the old ones; closing the stack, the new.
	next := &lazyDialer{inner: ss.Streams}
	s.SetDialers(Dialers{Streams: next, Packets: ss.Packets, Closer: next})
	if lazy.closed.Load() != 1 {
		t.Errorf("replaced dialers closed %d times", lazy.closed.Load())
	}
	f.echo(t, "via the new dialers")
	s.Close()
	if next.closed.Load() != 1 || lazy.closed.Load() != 1 {
		t.Errorf("after Close: new dialers closed %d times, old %d", next.closed.Load(), lazy.closed.Load())
	}
}
