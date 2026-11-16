package health

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"
)

// DNSOnInterface sends one DNS query over network, "udp" or "tcp", through
// the named interface with the given source address, and measures the time
// to the answer. Any answer counts, whatever its response code: it proves
// that data passed both ways, which a TCP handshake alone does not for an
// upstream that terminates TCP locally. Like ProbeOnInterface, it never
// falls back to an unbound socket.
func DNSOnInterface(ctx context.Context, network, iface string, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error) {
	if iface == "" {
		return 0, fmt.Errorf("probe interface is required")
	}
	d := net.Dialer{Timeout: timeout}
	switch network {
	case "udp":
		d.LocalAddr = &net.UDPAddr{IP: source.AsSlice()}
	case "tcp":
		d.LocalAddr = &net.TCPAddr{IP: source.AsSlice()}
	default:
		return 0, fmt.Errorf("dns probe: unknown network %q", network)
	}
	if err := bindProbe(&d, iface); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	c, err := d.DialContext(ctx, network, target.String())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if err := exchangeDNS(ctx, c); err != nil {
		return 0, fmt.Errorf("dns over %s to %s: %w", network, target, err)
	}
	return time.Since(start), nil
}

// rootNS asks for the root zone's name servers, which every recursive
// resolver has cached.
func rootNS(id uint16) []byte {
	q := binary.BigEndian.AppendUint16(nil, id)
	q = append(q, 0x01, 0x00) // a query, recursion desired
	q = append(q, 0, 1, 0, 0, 0, 0, 0, 0)
	return append(q, 0, 0, 2, 0, 1) // root, type NS, class IN
}

// exchangeDNS sends a query on c, a UDP or TCP connection, and waits until
// ctx is done for a response with the same ID.
func exchangeDNS(ctx context.Context, c net.Conn) error {
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	id := uint16(rand.Uint32())
	query := rootNS(id)
	_, stream := c.(*net.TCPConn)
	if stream {
		query = append(binary.BigEndian.AppendUint16(nil, uint16(len(query))), query...)
	}
	if _, err := c.Write(query); err != nil {
		return err
	}
	buf := make([]byte, 1232)
	for {
		var resp []byte
		if stream {
			var size uint16
			if err := binary.Read(c, binary.BigEndian, &size); err != nil {
				return err
			}
			resp = make([]byte, size)
			if _, err := io.ReadFull(c, resp); err != nil {
				return err
			}
		} else {
			n, err := c.Read(buf)
			if err != nil {
				return err
			}
			resp = buf[:n]
		}
		if len(resp) >= 12 && binary.BigEndian.Uint16(resp) == id && resp[2]&0x80 != 0 {
			return nil
		}
		if stream {
			return errors.New("malformed response")
		}
		// A stray datagram; keep waiting for ours.
	}
}
