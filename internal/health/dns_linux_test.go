//go:build linux && integration

package health

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// Run only in the disposable Linux integration container.
func TestDNSOnInterfaceBinding(t *testing.T) {
	resolver := proxytest.DNS(t)
	source := netip.MustParseAddr("127.0.0.2")
	for _, network := range []string{"udp", "tcp"} {
		latency, err := DNSOnInterface(context.Background(), network, "lo", source, resolver, time.Second)
		if err != nil || latency <= 0 {
			t.Fatalf("dns over %s on lo: latency %v, error %v", network, latency, err)
		}
		_, err = DNSOnInterface(context.Background(), network, "lm-no-such-if", source, resolver, time.Second)
		if !errors.Is(err, unix.ENODEV) || !strings.Contains(err.Error(), "lm-no-such-if") {
			t.Fatalf("dns over %s on a missing interface: %v, want named ENODEV error", network, err)
		}
	}
}
