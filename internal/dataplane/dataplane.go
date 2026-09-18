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
	// SetUpstreams makes the set of upstreams equal to ups without touching
	// the firewall table or the routes of upstreams that stay: new ones get a
	// table, route and probe rule, removed ones lose theirs. Peers routed
	// through a removed upstream must be unrouted first. Also applies mtu.
	SetUpstreams(ups []Interface, mtu int) error
	// Route makes new flows from peer leave through the upstream interface
	// with that name (one of the names given to Setup). Flows already open
	// through the peer's previous upstream stay pinned to it while that
	// upstream is alive (see SetAlive), so a move does not reset them.
	Route(peer netip.Addr, upstreamInterface string) error
	// Unroute removes every rule of the peer, pins included; its traffic is
	// then dropped, not leaked.
	Unroute(peer netip.Addr) error
	// SetAlive marks whether flows may stay pinned to an upstream. Marking one
	// dead drops every pin to it, so those flows follow their peer's current
	// route instead of a black hole.
	SetAlive(upstreamInterface string, alive bool) error
	// Teardown removes everything Setup and Route created.
	Teardown() error
}
