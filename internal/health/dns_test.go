package health

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

func TestExchangeDNS(t *testing.T) {
	resolver := proxytest.DNS(t)
	// A server that accepts and keeps quiet, as outline-ss-server does with
	// a wrong key: a TCP connect alone would pass.
	quietTCP, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, quietTCP, func(c net.Conn) { c.Read(make([]byte, 1)) })
	quietUDP, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quietUDP.Close() })

	for _, tc := range []struct {
		network, addr string
		ok            bool
	}{
		{"udp", resolver.String(), true},
		{"tcp", resolver.String(), true},
		{"udp", quietUDP.LocalAddr().String(), false},
		{"tcp", quietTCP.Addr().String(), false},
	} {
		t.Run(tc.network+" "+tc.addr, func(t *testing.T) {
			c, err := net.Dial(tc.network, tc.addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := exchangeDNS(ctx, c); (err == nil) != tc.ok {
				t.Fatalf("exchange: %v, want success %v", err, tc.ok)
			}
		})
	}
}
