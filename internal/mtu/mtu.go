// Package mtu computes the MTU a Lotsman client must use so that packets survive
// two AmneziaWG encapsulations: client → Lotsman and Lotsman → upstream provider.
package mtu

// Overhead of one WireGuard encapsulation over IPv6, the worst case:
// IPv6 header (40) + UDP header (8) + transport message header (16) + Poly1305 tag (16).
// Use the same conservative overhead for either outer address family.
const Overhead = 80

// DefaultWireGuard is what wg-quick assumes when a config sets no MTU.
const DefaultWireGuard = 1420

// Ethernet is the assumed outer path MTU on either AmneziaWG hop.
const Ethernet = 1500

// MinimumIPv4 is the smallest IPv4 MTU Linux accepts on a TUN interface.
const MinimumIPv4 = 68

type Upstream struct {
	MTU   int    // the upstream interface MTU (provider config or DefaultWireGuard)
	Proxy bool   // a proxy stack handles its own transport framing
	S4    uint16 // AmneziaWG transport padding on that upstream
	// ContentPadding is the upper bound of the AWG 3 ContentPaddingAddition
	// on that upstream, added to each transport message. The runtime also
	// caps it at the largest datagram exchanged with the provider, but that
	// can exceed this MTU when the provider sends larger packets.
	ContentPadding uint32
}

// PacketLimit bounds the original IP packet by the upstream interface and,
// for AWG, by the room left inside the assumed outer path MTU.
func (u Upstream) PacketLimit() int {
	inner := u.MTU
	if inner == 0 {
		inner = DefaultWireGuard
	}
	if u.Proxy {
		return inner
	}
	outer := int64(Ethernet-Overhead-int(u.S4)) - int64(u.ContentPadding)
	if int64(inner) <= outer {
		return inner
	}
	if u.ContentPadding == 0 {
		// Ordinary WireGuard padding rounds up to a multiple of 16,
		// capped by the TUN MTU. Here that cap is above the outer budget.
		const alignment = 16
		outer &^= alignment - 1
	}
	return int(max(0, outer))
}

// Client returns the largest packet a client can send into its tunnel such that it
// fits into every upstream tunnel. s4Down is the transport padding Lotsman itself uses.
func Client(s4Down uint16, upstreams []Upstream) int {
	best := Ethernet - Overhead - int(s4Down)
	for _, u := range upstreams {
		best = min(best, u.PacketLimit())
	}
	return best
}
