package proxy

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// useRoots makes wss:// dials trust roots for the test. Call it before the
// stack starts, so the stack's cleanup, which waits for its flows, runs
// before the roots are restored.
func useRoots(t *testing.T, roots *x509.CertPool) {
	t.Helper()
	old := wsRoots
	wsRoots = roots
	t.Cleanup(func() { wsRoots = old })
}

// newWSFlow runs a stack through a WebSocket front of a test server, with
// the key the front's config gives for host. As in the daemon, the stack
// dials the front's address: host is never looked up. With trust set, the
// front's CA is trusted.
func newWSFlow(t *testing.T, secure bool, host string, trust bool) (*testFlow, *proxytest.WebSocket) {
	t.Helper()
	server := newTestServer(t, "ws-secret")
	front := server.WebSocket(t, secure)
	if trust {
		useRoots(t, front.Roots())
	}
	key, err := ParseConfig(front.Config(host))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%s:%d", host, front.Addr().Port()); key.Server() != want {
		t.Fatalf("server %q, want %q", key.Server(), want)
	}
	d, err := key.Dialers(front.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return newStack(t, server, d, Options{}), front
}

func TestWebSocketCarriesTCPAndUDP(t *testing.T) {
	for _, secure := range []bool{true, false} {
		t.Run(fmt.Sprintf("tls=%v", secure), func(t *testing.T) {
			f, front := newWSFlow(t, secure, proxytest.Hostname, true)
			f.echo(t, "hello over a websocket")
			f.echo(t, strings.Repeat("bulk ", 20_000)) // many messages, half-closed both ways
			c := f.dialDNS(t)
			f.dns(t, c, 1)
			f.dns(t, c, 2) // the same association

			want := []string{"tcp " + echoTarget.String(), "tcp " + echoTarget.String(), "udp " + dnsTarget.String(), "udp " + dnsTarget.String()}
			if got := f.server.Seen(); !slices.Equal(got, want) {
				t.Errorf("server saw %q, want %q", got, want)
			}
			sni := ""
			if secure {
				sni = proxytest.Hostname
			}
			host := fmt.Sprintf("%s:%d", proxytest.Hostname, front.Addr().Port())
			tcp := proxytest.Handshake{Path: proxytest.TCPPath, Host: host, SNI: sni}
			udp := proxytest.Handshake{Path: proxytest.UDPPath, Host: host, SNI: sni}
			wantHS := []proxytest.Handshake{tcp, tcp, udp}
			if got := front.Handshakes(); !slices.Equal(got, wantHS) {
				t.Errorf("handshakes %+v, want %+v", got, wantHS)
			}
		})
	}
}

// The certificate is checked against the URL's host, with the system's
// roots unless the test injects its CA.
func TestWebSocketVerifiesCertificate(t *testing.T) {
	for name, tc := range map[string]struct {
		host  string
		trust bool
	}{
		"unknown CA": {host: proxytest.Hostname},
		"wrong name": {host: "other.lotsman.test", trust: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := newWSFlow(t, true, tc.host, tc.trust)
			if _, err := f.dial(t); err == nil {
				t.Error("a TCP flow went through an unverified server")
			}
			c := f.dialDNS(t)
			c.SetDeadline(time.Now().Add(2 * time.Second))
			if err := proxytest.ExchangeDNS(c, 1); err == nil {
				t.Error("a UDP flow went through an unverified server")
			}
			if seen := f.server.Seen(); len(seen) > 0 {
				t.Errorf("the server got requests: %q", seen)
			}
		})
	}
}

func TestWebSocketConnectErrors(t *testing.T) {
	server := newTestServer(t, "ws-secret")
	front := server.WebSocket(t, false)
	ws, _, err := parseWSURL(front.URL(proxytest.Hostname, "/s3cr3t-wrong-path"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ws.connect(context.Background(), front.Addr(), false)
	if err == nil || !strings.Contains(err.Error(), "HTTP status 404") || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("wrong path: %v", err)
	}

	// A server that accepts the connection but never answers the upgrade:
	// cancelling the dial ends it at once.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, ln, func(c net.Conn) { io.Copy(io.Discard, c) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err = ws.connect(ctx, ln.Addr().(*net.TCPAddr).AddrPort(), false)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("silent server: %v after %v", err, time.Since(start))
	}

	// Nothing listening: refused, quoting the address, not the URL.
	_, err = ws.connect(context.Background(), netip.MustParseAddrPort("127.0.0.1:1"), false)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("refused: %v", err)
	}
}
