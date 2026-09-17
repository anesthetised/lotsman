// Package mtu computes the MTU a Lotsman client must use so that packets survive
// two AmneziaWG encapsulations: client → Lotsman and Lotsman → upstream provider.
package mtu

// Overhead of one WireGuard encapsulation over IPv4:
// IPv4 header (20) + UDP header (8) + transport message header (16) + Poly1305 tag (16).
const Overhead = 60

// DefaultWireGuard is what wg-quick assumes when a config sets no MTU.
const DefaultWireGuard = 1420

// Ethernet is the path MTU assumed between the client and Lotsman.
const Ethernet = 1500

type Upstream struct {
	MTU int    // the upstream interface MTU (provider config or DefaultWireGuard)
	S4  uint16 // AmneziaWG transport padding on that upstream
}

// Client returns the largest packet a client can send into its tunnel such that it
// fits into every upstream tunnel. s4Down is the transport padding Lotsman itself uses.
func Client(s4Down uint16, upstreams []Upstream) int {
	best := Ethernet - Overhead - int(s4Down)
	for _, u := range upstreams {
		m := u.MTU
		if m == 0 {
			m = DefaultWireGuard
		}
		if fits := m - int(u.S4); fits < best {
			best = fits
		}
	}
	return best
}
