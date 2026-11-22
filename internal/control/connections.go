package control

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/flows"
)

// ConnectionSource is what ConnectionService needs from the daemon: who
// each client address is and where its flows go. The flows themselves come
// from conntrack.
type ConnectionSource interface {
	Ready() <-chan struct{}
	Subnet() netip.Prefix
	PeerRoutes() map[netip.Addr]daemon.PeerRoute
}

type connectionService struct {
	src       ConnectionSource
	conntrack flows.Source
}

// Watch sends the flows as soon as the daemon is ready and then every
// interval, until the client goes away or the daemon stops (both cancel
// ctx). Each stream meters on its own, so its first snapshot has no rates.
func (s connectionService) Watch(ctx context.Context, req *lotsmanv1.WatchConnectionsRequest, stream lotsmanv1connect.ConnectionServiceWatchServerStream) error {
	interval, err := watchInterval(req.GetInterval())
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.src.Ready():
	}
	if id := req.GetPeerId(); id != "" && !s.hasPeer(id) {
		return connect.NewError(connect.CodeNotFound, "peer "+id+" not found")
	}
	m := &flows.Meter{Source: s.conntrack, Peers: peerRoutes{s.src}, Subnet: s.src.Subnet(), Peer: req.GetPeerId()}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		snap, err := m.Snapshot()
		if err != nil {
			return conntrackError(err)
		}
		if err := stream.Send(connectionSnapshot(snap)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s connectionService) hasPeer(id string) bool {
	for _, r := range s.src.PeerRoutes() {
		if PeerID(r.ID) == id {
			return true
		}
	}
	return false
}

// conntrackError keeps the kernel's error inside the daemon. Without
// conntrack at all, a non-Linux build, retrying cannot help.
func conntrackError(err error) error {
	if errors.Is(err, errors.ErrUnsupported) {
		return connect.NewError(connect.CodeFailedPrecondition, "connections: this platform has no conntrack")
	}
	return connect.NewError(connect.CodeUnavailable, "connections: reading conntrack failed")
}

// watchInterval applies the rules every Watch shares: MinInterval by
// default and at least.
func watchInterval(d *durationpb.Duration) (time.Duration, error) {
	if d == nil {
		return MinInterval, nil
	}
	if err := d.CheckValid(); err != nil {
		return 0, connect.NewError(connect.CodeInvalidArgument, "interval: "+err.Error())
	}
	return max(d.AsDuration(), MinInterval), nil
}

// peerRoutes gives a Meter the daemon's peers under their API ids.
type peerRoutes struct{ src ConnectionSource }

func (p peerRoutes) Peers() map[netip.Addr]flows.Peer {
	routes := p.src.PeerRoutes()
	out := make(map[netip.Addr]flows.Peer, len(routes))
	for addr, r := range routes {
		out[addr] = flows.Peer{ID: PeerID(r.ID), Upstream: r.Upstream}
	}
	return out
}

func connectionSnapshot(s flows.Snapshot) *lotsmanv1.ConnectionSnapshot {
	out := &lotsmanv1.ConnectionSnapshot{Time: timestamppb.New(s.Time), Truncated: s.Truncated}
	for _, f := range s.Flows {
		out.Connections = append(out.Connections, &lotsmanv1.Connection{
			PeerId: f.PeerID, Protocol: protocol(f.Protocol), Source: endpoint(f.Source), Destination: endpoint(f.Destination),
			Upstream: f.Upstream, Started: timestamp(f.Started), LastTraffic: timestamp(f.LastTraffic),
			Traffic: &lotsmanv1.Traffic{DownBytes: f.DownBytes, UpBytes: f.UpBytes, DownRate: f.DownRate, UpRate: f.UpRate},
		})
	}
	return out
}

func protocol(p flows.Protocol) lotsmanv1.Protocol {
	switch p {
	case flows.TCP:
		return lotsmanv1.Protocol_PROTOCOL_TCP
	case flows.UDP:
		return lotsmanv1.Protocol_PROTOCOL_UDP
	case flows.ICMP:
		return lotsmanv1.Protocol_PROTOCOL_ICMP
	}
	return lotsmanv1.Protocol_PROTOCOL_UNSPECIFIED
}

// endpoint is IP:port, or the bare IP for protocols without ports.
func endpoint(a netip.AddrPort) string {
	if a.Port() == 0 {
		return a.Addr().String()
	}
	return a.String()
}
