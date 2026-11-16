package helper

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

var echoTarget = netip.MustParseAddrPort("198.51.100.7:7")

// syncBuffer collects log output written from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// call is the daemon's side of one request over a test connection.
func call(t *testing.T, conn net.Conn, req request) response {
	t.Helper()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := writeFrame(conn, req); err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	var resp response
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return resp
}

// Serve carries flows through the stack it starts, answers every request
// and stops when the daemon closes the connection.
func TestServe(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	server := proxytest.NewServer(t, "helper-secret", map[string]string{echoTarget.String(): echo.Addr().String()})
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	daemon, helper := net.Pipe()
	defer daemon.Close()
	var logs syncBuffer
	served := make(chan error, 1)
	go func() { served <- Serve(helper, func() (tun.Device, error) { return dev, nil }, &logs) }()

	spec := proxy.Spec{Key: server.Key(), Server: server.Addr(), MaxFlows: 5}
	if resp := call(t, daemon, request{Op: opOpen, Spec: &spec}); resp.Error != "" {
		t.Fatalf("open: %s", resp.Error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := client.DialContextTCPAddrPort(ctx, echoTarget)
	if err != nil {
		t.Fatalf("dial through the helper's stack: %v", err)
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	io.WriteString(c, "ping")
	c.CloseWrite()
	if got, err := io.ReadAll(c); err != nil || string(got) != "ping" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	c.Close()

	if resp := call(t, daemon, request{Op: opStats}); resp.Error != "" || resp.Stats == nil || resp.Stats.MaxFlows != 5 {
		t.Errorf("stats = %+v", resp)
	}
	if resp := call(t, daemon, request{Op: opMaxFlows, MaxFlows: 9}); resp.Error != "" {
		t.Errorf("max_flows: %s", resp.Error)
	}
	if resp := call(t, daemon, request{Op: opStats}); resp.Stats == nil || resp.Stats.MaxFlows != 9 {
		t.Errorf("stats after max_flows = %+v", resp)
	}
	// A rotated key and its server: new flows go there.
	rotated := proxytest.NewServer(t, "rotated-secret", map[string]string{echoTarget.String(): echo.Addr().String()})
	if resp := call(t, daemon, request{Op: opServer, Key: rotated.Key(), Server: rotated.Addr()}); resp.Error != "" {
		t.Fatalf("server: %s", resp.Error)
	}
	c, err = client.DialContextTCPAddrPort(ctx, echoTarget)
	if err != nil {
		t.Fatalf("dial after the key changed: %v", err)
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	io.WriteString(c, "pong")
	c.CloseWrite()
	if got, err := io.ReadAll(c); err != nil || string(got) != "pong" {
		t.Fatalf("echo after the key changed = %q, %v", got, err)
	}
	c.Close()
	if seen := rotated.Seen(); len(seen) != 1 || seen[0] != "tcp "+echoTarget.String() {
		t.Errorf("rotated server saw %v", seen)
	}
	if resp := call(t, daemon, request{Op: opServer, Server: rotated.Addr()}); resp.Error == "" {
		t.Error("server without a key accepted")
	}
	if resp := call(t, daemon, request{Op: "reboot"}); !strings.Contains(resp.Error, "unknown request") {
		t.Errorf("unknown op answered %+v", resp)
	}

	daemon.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve after close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after the daemon closed the connection")
	}
	if strings.Contains(logs.String(), "helper-secret") {
		t.Error("the helper logged the key")
	}
}

// A key the helper cannot use is reported to the daemon without quoting it.
func TestServeBadKey(t *testing.T) {
	daemon, helper := net.Pipe()
	defer daemon.Close()
	served := make(chan error, 1)
	var logs syncBuffer
	go func() {
		served <- Serve(helper, func() (tun.Device, error) {
			dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
			return dev, err
		}, &logs)
	}()
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("rc4-md5:helper-secret"))
	spec := proxy.Spec{Key: "ss://" + userinfo + "@192.0.2.1:8388", Server: netip.MustParseAddrPort("192.0.2.1:8388")}
	resp := call(t, daemon, request{Op: opOpen, Spec: &spec})
	if resp.Error == "" || strings.Contains(resp.Error, "helper-secret") || strings.Contains(resp.Error, userinfo) {
		t.Errorf("open answered %+v", resp)
	}
	if err := <-served; err == nil {
		t.Error("Serve returned no error")
	}
	// The first request must be open.
	daemon, helper = net.Pipe()
	defer daemon.Close()
	go func() { served <- Serve(helper, nil, io.Discard) }()
	if resp := call(t, daemon, request{Op: opStats}); !strings.Contains(resp.Error, "want open") {
		t.Errorf("stats before open answered %+v", resp)
	}
	<-served
}

// failingTUN is a TUN whose reads fail once fail is closed, as a kernel TUN
// that went away does.
type failingTUN struct {
	tun.Device
	fail   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newFailingTUN() (*failingTUN, error) {
	dev, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		return nil, err
	}
	return &failingTUN{Device: dev, fail: make(chan struct{}), closed: make(chan struct{})}, nil
}

func (f *failingTUN) Read([][]byte, []int, int) (int, error) {
	select {
	case <-f.fail:
		return 0, errors.New("tun gone")
	case <-f.closed:
		return 0, os.ErrClosed
	}
}

func (f *failingTUN) Close() error {
	f.once.Do(func() { close(f.closed) })
	return f.Device.Close()
}

// A stack that stops because its TUN failed ends Serve with that error, so
// the helper exits and is restarted; the daemon sees the connection end.
func TestServeTUNFailure(t *testing.T) {
	daemon, helper := net.Pipe()
	defer daemon.Close()
	ft, err := newFailingTUN()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- Serve(helper, func() (tun.Device, error) { return ft, nil }, io.Discard) }()
	server := proxytest.NewServer(t, "secret", nil)
	spec := proxy.Spec{Key: server.Key(), Server: server.Addr()}
	if resp := call(t, daemon, request{Op: opOpen, Spec: &spec}); resp.Error != "" {
		t.Fatalf("open: %s", resp.Error)
	}
	close(ft.fail)
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "tun gone") {
			t.Errorf("Serve = %v, want the TUN's error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve kept running after its stack stopped")
	}
	if err := readFrame(daemon, &response{}); err == nil {
		t.Error("the control connection stayed open")
	}
}

// A WebSocket key reaches the helper as text over the control connection,
// at start or as a rotated key, and carries TCP and UDP. The front is plain
// ws://: TLS is covered by the proxy package and the end-to-end test.
func TestServeWebSocket(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	dnsTarget := netip.MustParseAddrPort("198.51.100.7:53")
	redirect := map[string]string{echoTarget.String(): echo.Addr().String(), dnsTarget.String(): proxytest.DNS(t).String()}
	plain := proxytest.NewServer(t, "plain-secret", redirect)
	server := proxytest.NewServer(t, "ws-secret", redirect)
	front := server.WebSocket(t, false)
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	daemon, helper := net.Pipe()
	defer daemon.Close()
	var logs syncBuffer
	served := make(chan error, 1)
	go func() { served <- Serve(helper, func() (tun.Device, error) { return dev, nil }, &logs) }()
	flows := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := client.DialContextTCPAddrPort(ctx, echoTarget)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(30 * time.Second))
		io.WriteString(c, "ping")
		c.CloseWrite()
		if got, err := io.ReadAll(c); err != nil || string(got) != "ping" {
			t.Fatalf("echo = %q, %v", got, err)
		}
		u, err := client.DialUDPAddrPort(netip.AddrPort{}, dnsTarget)
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()
		u.SetDeadline(time.Now().Add(30 * time.Second))
		if err := proxytest.ExchangeDNS(u, 1); err != nil {
			t.Fatalf("dns: %v", err)
		}
	}

	config := front.Config(proxytest.Hostname)
	spec := proxy.Spec{Key: config, Server: front.Addr()}
	if resp := call(t, daemon, request{Op: opOpen, Spec: &spec}); resp.Error != "" {
		t.Fatalf("open: %s", resp.Error)
	}
	flows()
	if n := len(front.Handshakes()); n != 2 {
		t.Errorf("%d websockets opened, want 2 (tcp and udp)", n)
	}

	// Rotated to a plain key, and back.
	if resp := call(t, daemon, request{Op: opServer, Key: plain.Key(), Server: plain.Addr()}); resp.Error != "" {
		t.Fatalf("server: %s", resp.Error)
	}
	flows()
	if resp := call(t, daemon, request{Op: opServer, Key: config, Server: front.Addr()}); resp.Error != "" {
		t.Fatalf("server: %s", resp.Error)
	}
	flows()
	if n, seen := len(front.Handshakes()), plain.Seen(); n != 4 || len(seen) != 2 {
		t.Errorf("%d websockets opened, want 4; the plain server saw %q", n, seen)
	}

	daemon.Close()
	if err := <-served; err != nil {
		t.Errorf("Serve after close: %v", err)
	}
	if strings.Contains(logs.String(), "ws-secret") || strings.Contains(logs.String(), "s3cr3t") {
		t.Error("the helper logged the key or its URL")
	}
}
