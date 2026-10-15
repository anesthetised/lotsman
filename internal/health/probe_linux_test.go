//go:build linux && integration

package health

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Run only in the disposable Linux integration container.
func TestProbeOnInterfaceBinding(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan netip.Addr, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()
			c.Close()
		}
	}()
	target := ln.Addr().(*net.TCPAddr).AddrPort()
	source := netip.MustParseAddr("127.0.0.2")
	latency, err := ProbeOnInterface(context.Background(), "lo", source, target, time.Second)
	if err != nil || latency <= 0 {
		t.Fatalf("probe on lo: latency %v, error %v", latency, err)
	}
	select {
	case got := <-accepted:
		if got != source {
			t.Fatalf("probe source = %s, want %s", got, source)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not accept probe")
	}

	// The same reachable target must fail when the requested device is missing.
	// Ignoring SO_BINDTODEVICE would make this connection succeed.
	_, err = ProbeOnInterface(context.Background(), "lm-no-such-if", source, target, time.Second)
	if !errors.Is(err, unix.ENODEV) || !strings.Contains(err.Error(), "lm-no-such-if") {
		t.Fatalf("missing interface: %v, want named ENODEV error", err)
	}
}
