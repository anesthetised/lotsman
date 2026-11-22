// Package flows reports the connections clients have open through the gateway
// and their throughput, from the kernel's connection tracking table.
//
// Forwarding happens in the kernel, so the daemon never sees the packets.
// Conntrack counts the bytes of every flow in each direction (with
// net.netfilter.nf_conntrack_acct=1); a Meter polls it and turns the
// counters into rates from the deltas between polls. Nothing here is stored
// or logged: a snapshot lives as long as its caller keeps it.
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
	LastTraffic time.Time // zero until this meter sees new bytes
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
	Source Source
	Peers  Peers
	Subnet netip.Prefix // the client subnet
	Peer   string       // only this peer's flows; "" for every peer
	Limit  int          // DefaultLimit when zero
	Now    func() time.Time

	polled time.Time      // of the previous snapshot; zero before the first
	seen   map[key]record // the flows of the previous snapshot
}

type key struct {
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
		// Only flows clients opened into the internet: not the host's own,
		// not a client's ping to the gateway.
		if !m.Subnet.Contains(e.Source.Addr()) || m.Subnet.Contains(e.Destination.Addr()) {
			continue
		}
		p, ok := peers[e.Source.Addr()]
		if !ok || (m.Peer != "" && p.ID != m.Peer) {
			continue
		}
		k := key{e.Protocol, e.Source, e.Destination}
		cur := record{orig: e.OrigBytes, reply: e.ReplyBytes, start: e.Start, started: e.Start}
		var up, down uint64 // bytes since the previous snapshot
		prev, known := m.seen[k]
		switch {
		case known && cur.orig >= prev.orig && cur.reply >= prev.reply && cur.start.Equal(prev.start):
			up, down = cur.orig-prev.orig, cur.reply-prev.reply
			cur.started = prev.started
			cur.lastTraffic = prev.lastTraffic
		case !m.polled.IsZero():
			// New since the previous snapshot, or the tuple was reused by a
			// new flow (counters went back or the start moved): all of its
			// bytes are from this interval.
			up, down = cur.orig, cur.reply
			if cur.started.IsZero() {
				cur.started = now
			}
		}
		if up > 0 || down > 0 {
			cur.lastTraffic = now
		}
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

func rate(bytes uint64, elapsed time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	return uint64(float64(bytes) / elapsed.Seconds())
}
