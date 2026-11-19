package flows

import (
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Conntrack reads the IPv4 conntrack table of the calling process's network
// namespace, which is where the daemon forwards.
type Conntrack struct{}

// Entries dumps the table. A dump the kernel interrupted because the table
// changed meanwhile is retried, since a partial one would make the missing
// flows look new on the next poll.
func (Conntrack) Entries() ([]Entry, error) {
	var (
		cts []*netlink.ConntrackFlow
		err error
	)
	for range 3 {
		if cts, err = netlink.ConntrackTableList(netlink.ConntrackTable, unix.AF_INET); !errors.Is(err, netlink.ErrDumpInterrupted) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(cts))
	for _, ct := range cts {
		e := Entry{
			Protocol:    Protocol(ct.Forward.Protocol),
			Source:      addrPort(ct.Forward.SrcIP, ct.Forward.SrcPort),
			Destination: addrPort(ct.Forward.DstIP, ct.Forward.DstPort),
			OrigBytes:   ct.Forward.Bytes,
			ReplyBytes:  ct.Reverse.Bytes,
		}
		if ct.TimeStart != 0 {
			e.Start = time.Unix(0, int64(ct.TimeStart))
		}
		out = append(out, e)
	}
	return out, nil
}

func addrPort(ip net.IP, port uint16) netip.AddrPort {
	a, _ := netip.AddrFromSlice(ip)
	return netip.AddrPortFrom(a.Unmap(), port)
}
