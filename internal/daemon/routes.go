package daemon

import "net/netip"

// PeerRoute is an added peer and the upstream its new flows leave through,
// for views of live connections.
type PeerRoute struct {
	ID                    int64 // store.Peer.ID
	User, Device, Profile string
	Upstream              string // "" when blocked
}

// PeerRoutes maps each added peer's tunnel address to its route.
func (d *Daemon) PeerRoutes() map[netip.Addr]PeerRoute {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[netip.Addr]PeerRoute, len(d.devicePeers))
	for _, p := range d.devicePeers {
		out[p.IP] = PeerRoute{ID: p.ID, User: p.User, Device: p.Device, Profile: p.Profile, Upstream: d.assignments[p.IP]}
	}
	return out
}
