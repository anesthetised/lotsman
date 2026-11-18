package proxy

// Xray's VLESS server over gRPC runs no Vision, so unlike the tests in
// vless_stack_test.go these run under the race detector too.

import (
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

const testServiceName = "test-service"

// newGRPCServer starts a VLESS + REALITY + gRPC server whose targets are the
// test targets, redirected to loopback.
func newGRPCServer(t *testing.T) *vlesstest.Server {
	t.Helper()
	return vlesstest.NewGRPCServer(t, vlessTargets(t), testServiceName)
}

// Two gRPC upstreams in one process, in gun and multi mode, carry TCP and
// UDP independently, UDP both as plain VLESS UDP (port 53) and as XUDP.
func TestVLESSGRPCCarriesTCPAndUDP(t *testing.T) {
	sa, sb := newGRPCServer(t), newGRPCServer(t)
	vlesstest.WaitReady(t, sa, sb)
	a := newVLESSFlow(t, sa, sa.Link())
	b := newVLESSFlow(t, sb, sb.LinkWith(url.Values{"mode": {"multi"}, "fp": {"firefox"}}))
	for i, f := range []*vlessFlow{a, b} {
		f.echo(t, "hello")
		// 200 kB: past HTTP/2's initial 64 kB window, and quick under the
		// race detector, which slows this path down tenfold.
		f.echo(t, strings.Repeat("bulk ", 40_000))
		f.hello(t)
		f.dns(t, uint16(i))
		f.dnsTo(t, xudpDNSTarget, uint16(10+i))
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

// gRPC carries every flow over one connection to the server; once the
// server is down, a new flow must reset quickly rather than hang.
func TestVLESSGRPCServerDownResetsClient(t *testing.T) {
	s := newGRPCServer(t)
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

// A link with the wrong service name or server key never gets data through.
func TestVLESSGRPCWrongLink(t *testing.T) {
	s := newGRPCServer(t)
	vlesstest.WaitReady(t, s)
	for name, params := range map[string]url.Values{
		"service name": {"serviceName": {"other-service"}},
		"server key":   {"pbk": {"wsxdeEB2O7Mqa1ny8bxMHOMyTzvWvvVQXqhNBNPMn2E"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newVLESSFlow(t, s, s.LinkWith(params))
			c := f.dial(t)
			io.WriteString(c, "hello")
			if _, err := io.ReadFull(c, make([]byte, 5)); err == nil {
				t.Fatal("echo succeeded with a wrong link")
			}
		})
	}
}
