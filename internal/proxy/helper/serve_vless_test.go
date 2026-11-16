//go:build !race

// Runs Xray's VLESS server, which races (see vlesstest).

package helper

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

// Serve carries flows through VLESS from a vless:// link, and moves new
// flows to another server on request.
func TestServeVLESS(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	redirect := map[string]string{echoTarget.String(): echo.Addr().String()}
	a, b := vlesstest.NewServer(t, redirect, nil), vlesstest.NewServer(t, redirect, nil)
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

	spec := proxy.Spec{Key: a.Link(), Server: a.Addr()}
	if resp := call(t, daemon, request{Op: opOpen, Spec: &spec, LogLevel: slog.LevelDebug}); resp.Error != "" {
		t.Fatalf("open: %s", resp.Error)
	}
	echoVia := func(msg string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := client.DialContextTCPAddrPort(ctx, echoTarget)
		if err != nil {
			t.Fatalf("dial through the helper's stack: %v", err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		go io.WriteString(c, msg) // no half-close: VLESS does not carry it
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
			t.Fatalf("echo = %q, %v", got, err)
		}
	}
	echoVia("ping")
	// Another server and its link: new flows go there.
	if resp := call(t, daemon, request{Op: opServer, Key: b.Link(), Server: b.Addr()}); resp.Error != "" {
		t.Fatalf("server: %s", resp.Error)
	}
	a.Close()
	echoVia("pong")

	daemon.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve after close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after the daemon closed the connection")
	}
	if !strings.Contains(logs.String(), `"component":"xray"`) {
		t.Errorf("no xray records in the helper's log:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), a.ID()) || strings.Contains(logs.String(), b.ID()) {
		t.Error("the helper logged a user ID")
	}
}
