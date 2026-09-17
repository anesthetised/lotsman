// Package dataplane programs the host so that packets from the downstream
// interface are forwarded through the upstream chosen for each peer.
package dataplane

import "net/netip"

type Interface struct {
	Name string
	Addr netip.Prefix
}

type Dataplane interface {
	// Setup assigns addresses and MTU, creates a routing table per upstream and
	// installs the firewall rules (NAT, MSS clamp, fail-closed drop). It is
	// idempotent so a restart can reconcile leftovers from a previous run.
	Setup(down Interface, ups []Interface, mtu int) error
	// Route makes traffic from peer leave through the upstream interface with
	// that name (one of the names given to Setup), replacing any earlier choice.
	Route(peer netip.Addr, upstreamInterface string) error
	// Unroute removes the peer's route; its traffic is then dropped, not leaked.
	Unroute(peer netip.Addr) error
	// Teardown removes everything Setup and Route created.
	Teardown() error
}
