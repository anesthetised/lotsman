package health

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// Probe opens a TCP connection to target with the given source address and
// returns how long the handshake took. Binding the source address to an
// upstream's tunnel address makes the kernel policy rules send the probe
// through that upstream, so a success means the whole path works.
func Probe(ctx context.Context, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error) {
	d := net.Dialer{
		Timeout:   timeout,
		LocalAddr: &net.TCPAddr{IP: source.AsSlice()},
	}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", target.String())
	if err != nil {
		return 0, err
	}
	c.Close()
	return time.Since(start), nil
}
