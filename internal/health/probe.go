package health

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// ProbeOnInterface measures a TCP handshake through the named interface with
// the given source address. It fails if interface binding is unavailable;
// it never falls back to an unbound connection.
func ProbeOnInterface(ctx context.Context, iface string, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error) {
	if iface == "" {
		return 0, fmt.Errorf("probe interface is required")
	}
	return probe(ctx, iface, source, target, timeout)
}

func probe(ctx context.Context, iface string, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error) {
	d := net.Dialer{
		Timeout:   timeout,
		LocalAddr: &net.TCPAddr{IP: source.AsSlice()},
	}
	if iface != "" {
		if err := bindProbe(&d, iface); err != nil {
			return 0, err
		}
	}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", target.String())
	if err != nil {
		return 0, err
	}
	c.Close()
	return time.Since(start), nil
}
