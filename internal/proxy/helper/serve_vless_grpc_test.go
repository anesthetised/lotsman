package helper

// Xray's VLESS server over gRPC runs no Vision, so unlike serve_vless_test.go
// this runs under the race detector too.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
	"github.com/anesthetised/lotsman/internal/proxy/vlesstest"
)

// Serve carries TCP and UDP through VLESS over gRPC from a vless:// link,
// and moves new flows to another server on request.
func TestServeVLESSGRPC(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	dnsTarget := netip.MustParseAddrPort("198.51.100.7:53")
	redirect := map[string]string{echoTarget.String(): echo.Addr().String(), dnsTarget.String(): proxytest.DNS(t).String()}
	a, b := vlesstest.NewGRPCServer(t, redirect, "a"), vlesstest.NewGRPCServer(t, redirect, "b")
	vlesstest.WaitReady(t, a, b)
	dev, client, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.0.0.2")}, nil, 1420)
	if err != nil {
		t.Fatal(err)
	}
	daemon, helper := net.Pipe()
	defer daemon.Close()
	var logs syncBuffer
	served := make(chan error, 1)
	go func() { served <- Serve(helper, func() (tun.Device, error) { return dev, nil }, &logs) }()
	flows := func(msg string, id uint16) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := client.DialContextTCPAddrPort(ctx, echoTarget)
		if err != nil {
			t.Fatalf("dial through the helper's stack: %v", err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(30 * time.Second))
		go io.WriteString(c, msg) // no half-close: VLESS does not carry it
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
			t.Fatalf("echo = %q, %v", got, err)
		}
		u, err := client.DialUDPAddrPort(netip.AddrPort{}, dnsTarget)
		if err != nil {
			t.Fatal(err)
		}
		defer u.Close()
		u.SetDeadline(time.Now().Add(30 * time.Second))
		if err := proxytest.ExchangeDNS(u, id); err != nil {
			t.Fatalf("dns: %v", err)
		}
	}

	spec := proxy.Spec{Key: a.Link(), Server: a.Addr()}
	if resp := call(t, daemon, request{Op: opOpen, Spec: &spec, LogLevel: slog.LevelDebug}); resp.Error != "" {
		t.Fatalf("open: %s", resp.Error)
	}
	flows("ping", 1)
	// Another server and its link: new flows go there.
	if resp := call(t, daemon, request{Op: opServer, Key: b.Link(), Server: b.Addr()}); resp.Error != "" {
		t.Fatalf("server: %s", resp.Error)
	}
	a.Close()
	flows("pong", 2)

	daemon.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve after close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after the daemon closed the connection")
	}
	if strings.Contains(logs.String(), a.ID()) || strings.Contains(logs.String(), b.ID()) {
		t.Error("the helper logged a user ID")
	}
}
