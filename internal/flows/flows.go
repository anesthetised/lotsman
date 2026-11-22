// Package flows reports the connections clients have open through the gateway
// and their throughput, from the kernel's connection tracking table.
//
// Forwarding happens in the kernel, so the daemon never sees the packets.
// Conntrack counts the bytes of every flow in each direction (with
// net.netfilter.nf_conntrack_acct=1). A Sampler observes the open flows
// in memory, and a Meter turns their counters into rates for each viewer.
package flows

import (
	"cmp"
	"net/netip"
	"slices"
	"time"
)

// DefaultLimit caps the flows in a snapshot when Meter.Limit is zero.
const DefaultLimit = 500

// Protocol is the IANA protocol number of a flow.
type Protocol uint8

const (
	ICMP Protocol = 1
	TCP  Protocol = 6
	UDP  Protocol = 17
)

// Entry is one conntrack entry as the kernel reports it. The original
// direction is the one that opened the flow: for a client's flow, from the
// client to the internet.
type Entry struct {
	Protocol    Protocol
	Source      netip.AddrPort // port 0 for protocols without ports
	Destination netip.AddrPort
	OrigBytes   uint64    // sent in the original direction
	ReplyBytes  uint64    // sent back
	Start       time.Time // zero unless net.netfilter.nf_conntrack_timestamp is on

	sampled              bool
	started, lastTraffic time.Time
}

// Source lists the current conntrack entries.
type Source interface {
	Entries() ([]Entry, error)
}

// Peer is who a client tunnel address belongs to, and the upstream its new
// flows leave through.
type Peer struct {
	ID       string
	Upstream string // "" when blocked
}

// Peers returns the current peers by client tunnel address. The daemon
// implements it.
type Peers interface {
	Peers() map[netip.Addr]Peer
}

// Flow is one client connection. Directions are from the client's side:
// down is towards the client.
type Flow struct {
	PeerID      string
	Protocol    Protocol
	Source      netip.AddrPort // the client's address; port 0 for protocols without ports
	Destination netip.AddrPort
	Upstream    string
	Started     time.Time // zero when unknown
	LastTraffic time.Time // zero until new bytes have been observed
	DownBytes   uint64    // since the flow started
	UpBytes     uint64
	DownRate    uint64 // bytes per second since the previous snapshot
	UpRate      uint64
}

type Snapshot struct {
	Time      time.Time
	Flows     []Flow // fastest first
	Truncated bool   // more than the limit; the fastest are kept
}

// Meter turns conntrack counters into per-flow rates. Each viewer gets its
// own Meter, since rates are deltas between that viewer's snapshots. It is
// not safe for concurrent use.
type Meter struct {
	Source  Source
	Peers   Peers
	Subnet  netip.Prefix    // the client subnet
	PeerIDs map[string]bool // empty for every peer
	Limit   int             // DefaultLimit when zero
	Now     func() time.Time

	polled time.Time      // of the previous snapshot; zero before the first
	seen   map[key]record // the flows of the previous snapshot
}

type key struct {
	peer        string
	protocol    Protocol
	source      netip.AddrPort
	destination netip.AddrPort
}

type record struct {
	orig, reply uint64
	start       time.Time // from the kernel
	started     time.Time // reported: the kernel's start, else when first seen
	lastTraffic time.Time // latest snapshot that saw new bytes
}

// Snapshot reads the table and returns the current flows of clients that are
// peers, with their rates since the previous call; the first call has no
// rates. When reading fails, the previous call stays the reference.
func (m *Meter) Snapshot() (Snapshot, error) {
	entries, err := m.Source.Entries()
	if err != nil {
		return Snapshot{}, err
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	var elapsed time.Duration
	if !m.polled.IsZero() {
		elapsed = now.Sub(m.polled)
	}
	peers := m.Peers.Peers()
	seen := map[key]record{}
	var out []Flow
	for _, e := range entries {
		p, ok := clientFlow(e, m.Subnet, peers)
		if !ok || (len(m.PeerIDs) != 0 && !m.PeerIDs[p.ID]) {
			continue
		}
		k := key{p.ID, e.Protocol, e.Source, e.Destination}
		prev, known := m.seen[k]
		cur, up, down := observe(e, prev, known, !m.polled.IsZero(), now)
		seen[k] = cur
		out = append(out, Flow{
			PeerID: p.ID, Protocol: e.Protocol, Source: e.Source, Destination: e.Destination,
			Upstream: p.Upstream, Started: cur.started, LastTraffic: cur.lastTraffic,
			DownBytes: e.ReplyBytes, UpBytes: e.OrigBytes,
			DownRate: rate(down, elapsed), UpRate: rate(up, elapsed),
		})
	}
	m.polled, m.seen = now, seen

	slices.SortFunc(out, func(a, b Flow) int {
		return cmp.Or(
			cmp.Compare(b.DownRate+b.UpRate, a.DownRate+a.UpRate),
			cmp.Compare(b.DownBytes+b.UpBytes, a.DownBytes+a.UpBytes),
			a.Source.Compare(b.Source),
			a.Destination.Compare(b.Destination),
			cmp.Compare(a.Protocol, b.Protocol),
		)
	})
	limit := cmp.Or(m.Limit, DefaultLimit)
	s := Snapshot{Time: now, Flows: out}
	if len(out) > limit {
		s.Flows, s.Truncated = out[:limit], true
	}
	return s, nil
}

// Only flows clients opened into the internet, including their DNS queries.
func clientFlow(e Entry, subnet netip.Prefix, peers map[netip.Addr]Peer) (Peer, bool) {
	p, ok := peers[e.Source.Addr()]
	return p, ok && subnet.Contains(e.Source.Addr()) && !subnet.Contains(e.Destination.Addr())
}

// observe returns the current flow record and newly observed bytes, up then
// down. A reused tuple is a new flow when its counters reset or start moves.
func observe(e Entry, prev record, known, polled bool, now time.Time) (cur record, up, down uint64) {
	cur = record{orig: e.OrigBytes, reply: e.ReplyBytes, start: e.Start, started: e.Start}
	if e.sampled && !e.started.IsZero() {
		cur.start = e.started // also identifies replacements observed between Watch updates
	}
	switch {
	case known && cur.orig >= prev.orig && cur.reply >= prev.reply && cur.start.Equal(prev.start):
		up, down = cur.orig-prev.orig, cur.reply-prev.reply
		cur.started, cur.lastTraffic = prev.started, prev.lastTraffic
	case polled:
		up, down = cur.orig, cur.reply
		if cur.started.IsZero() {
			cur.started = now
		}
	}
	switch {
	case e.sampled:
		cur.started, cur.lastTraffic = e.started, e.lastTraffic
	case up > 0 || down > 0:
		cur.lastTraffic = now
	}
	return cur, up, down
}

func rate(bytes uint64, elapsed time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	return uint64(float64(bytes) / elapsed.Seconds())
}
