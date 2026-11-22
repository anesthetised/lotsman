// Package control serves the daemon's control API, lotsman.v1 over Connect,
// to the web panel. It listens on a unix socket whose peer is trusted: the
// socket's file mode is the boundary, and nothing past it authenticates.
package control

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/flows"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// MinInterval is the shortest, and the default, interval between snapshots.
const MinInterval = time.Second

// Source is what the API reads and changes: the daemon, or a fake in tests.
type Source interface {
	Ready() <-chan struct{}
	Status() daemon.Status
	ClientMTU() int
	Traffic() traffic.Snapshot
	ConnectionSource
	HistorySource
	IncidentSource
	Users
	ApplyOutlineUsage(map[string]store.OutlineUsage) error
}

// NewServer registers every service of the API. An RPC whose issue has not
// landed yet fails with CodeUnimplemented.
func NewServer(src Source) *connect.Server {
	return newServer(src, connectionService{src: src, conntrack: flows.Conntrack{}})
}

func newServer(src Source, connections connectionService) *connect.Server {
	s := connect.NewServer()
	lotsmanv1connect.RegisterStatusServiceHandler(s, statusService{src})
	lotsmanv1connect.RegisterHistoryServiceHandler(s, historyService{src})
	lotsmanv1connect.RegisterIncidentServiceHandler(s, incidentService{src})
	lotsmanv1connect.RegisterConnectionServiceHandler(s, connections)
	lotsmanv1connect.RegisterUserServiceHandler(s, userService{src})
	lotsmanv1connect.RegisterUserSelfServiceHandler(s, newUserSelfService(src))
	return s
}

// Handler is the API on its HTTP routes.
func Handler(src Source) http.Handler {
	return handler(NewServer(src))
}

func handler(server *connect.Server) http.Handler {
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	return mux
}

type statusService struct{ src Source }

// Watch sends a snapshot as soon as the daemon is ready and then every
// interval, until the client goes away or the daemon stops (both cancel ctx).
func (s statusService) Watch(ctx context.Context, req *lotsmanv1.WatchStatusRequest, stream lotsmanv1connect.StatusServiceWatchServerStream) error {
	next := func() (*lotsmanv1.StatusSnapshot, error) {
		return snapshot(time.Now(), s.src.ClientMTU(), s.src.Status(), s.src.Traffic()), nil
	}
	return streamSnapshots(ctx, req, s.src.Ready(), next, stream.Send)
}

func (s statusService) ReportOutlineUsage(ctx context.Context, req *lotsmanv1.ReportOutlineUsageRequest) (*emptypb.Empty, error) {
	readings := make(map[string]store.OutlineUsage, len(req.GetUpstreams()))
	for name, reading := range req.GetUpstreams() {
		if name == "" || len(reading.GetKeyHash()) != 32 ||
			(reading.GetObservedAt() == nil && (reading.GetExhausted() || reading.GetExpiresAt() != nil)) {
			return nil, connect.NewError(connect.CodeInvalidArgument, "invalid Outline usage reading")
		}
		value := store.OutlineUsage{KeyHash: reading.GetKeyHash(), Exhausted: reading.GetExhausted()}
		if observed := reading.GetObservedAt(); observed != nil {
			if err := observed.CheckValid(); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, "invalid Outline usage observation time")
			}
			value.ObservedAt = observed.AsTime()
		}
		if expires := reading.GetExpiresAt(); expires != nil {
			if err := expires.CheckValid(); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, "invalid Outline subscription expiry")
			}
			value.ExpiresAt = expires.AsTime()
		}
		readings[name] = value
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.src.Ready():
	}
	if err := s.src.ApplyOutlineUsage(readings); err != nil {
		return nil, connect.NewError(connect.CodeInternal, "could not apply Outline usage")
	}
	return &emptypb.Empty{}, nil
}

// streamSnapshots sends next's snapshot once ready is closed and then every
// requested interval, until next fails or ctx is done.
func streamSnapshots(ctx context.Context, req *lotsmanv1.WatchStatusRequest, ready <-chan struct{},
	next func() (*lotsmanv1.StatusSnapshot, error), send func(*lotsmanv1.StatusSnapshot) error) error {
	interval, err := watchInterval(req.GetInterval())
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		s, err := next()
		if err != nil {
			return err
		}
		if err := send(s); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// PeerID is a peer's API identifier: opaque to clients, stable for the
// peer's life. It is the store's row id.
func PeerID(id int64) string { return "p_" + strconv.FormatInt(id, 10) }

// snapshot joins the status with the latest traffic sample. A peer or
// upstream the sampler has not read, such as one added since, has no
// traffic yet, and the total is unset before the first sample.
func snapshot(now time.Time, clientMTU int, st daemon.Status, tr traffic.Snapshot) *lotsmanv1.StatusSnapshot {
	s := &lotsmanv1.StatusSnapshot{Time: timestamppb.New(now), ClientMtu: uint32(clientMTU)}
	if !tr.Time.IsZero() {
		s.Total = trafficOf(tr.Total, true)
	}
	for _, u := range st.Upstreams {
		t, ok := tr.Upstreams[u.Name]
		s.Upstreams = append(s.Upstreams, &lotsmanv1.UpstreamStatus{
			Name: u.Name, Kind: upstreamKind(u.Kind), Geo: u.Geo, Provider: u.Provider,
			State: upstreamState(u), Latency: duration(u.Latency), LastHandshake: timestamp(u.LastHandshake),
			Routed: uint32(u.Routed), Active: uint32(u.Active), Traffic: trafficOf(t, ok), Speed: speed(u.Speed),
			Incidents: incidentKinds(u.Incidents), ExpiresAt: timestamp(u.ExpiresAt), RoutingBlock: routingBlock(u.RoutingBlock),
		})
	}
	for _, p := range st.Peers {
		t, ok := tr.Peers[p.ID]
		ps := &lotsmanv1.PeerStatus{
			PeerId: PeerID(p.ID), User: p.User, Device: p.Device,
			Upstream: p.Upstream, LastHandshake: timestamp(p.LastHandshake), Traffic: trafficOf(t, ok),
		}
		if p.Address.IsValid() {
			ps.Address = p.Address.String()
		}
		s.Peers = append(s.Peers, ps)
	}
	return s
}

// speed is unset before the first speed test.
func speed(s health.Speed) *lotsmanv1.Speed {
	if s.Time.IsZero() {
		return nil
	}
	return &lotsmanv1.Speed{DownRate: s.Down, UpRate: s.Up, Time: timestamppb.New(s.Time)}
}

func incidentKinds(kinds []store.IncidentKind) []lotsmanv1.IncidentKind {
	var out []lotsmanv1.IncidentKind
	for _, k := range kinds {
		out = append(out, incidentKind(k))
	}
	return out
}

func trafficOf(t traffic.Traffic, ok bool) *lotsmanv1.Traffic {
	if !ok {
		return nil
	}
	return &lotsmanv1.Traffic{DownBytes: t.DownBytes, UpBytes: t.UpBytes, DownRate: t.DownRate, UpRate: t.UpRate}
}

func upstreamKind(kind string) lotsmanv1.UpstreamKind {
	switch kind {
	case config.KindAWG:
		return lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG
	case config.KindOutline:
		return lotsmanv1.UpstreamKind_UPSTREAM_KIND_OUTLINE
	case config.KindVLESS:
		return lotsmanv1.UpstreamKind_UPSTREAM_KIND_VLESS
	}
	return lotsmanv1.UpstreamKind_UPSTREAM_KIND_UNSPECIFIED
}

// upstreamState folds the transfer probes into the state, as `lotsman
// upstream status` does: degraded is an up upstream failing them.
func routingBlock(reason daemon.RoutingBlock) lotsmanv1.RoutingBlock {
	switch reason {
	case daemon.SubscriptionExpired:
		return lotsmanv1.RoutingBlock_ROUTING_BLOCK_SUBSCRIPTION_EXPIRED
	case daemon.QuotaExhausted:
		return lotsmanv1.RoutingBlock_ROUTING_BLOCK_QUOTA_EXHAUSTED
	case daemon.RoutingUnavailable:
		return lotsmanv1.RoutingBlock_ROUTING_BLOCK_UNAVAILABLE
	default:
		return lotsmanv1.RoutingBlock_ROUTING_BLOCK_UNSPECIFIED
	}
}

func upstreamState(u daemon.UpstreamStatus) lotsmanv1.UpstreamState {
	switch u.State {
	case health.Unknown:
		return lotsmanv1.UpstreamState_UPSTREAM_STATE_UNKNOWN
	case health.Up:
		if u.Degraded {
			return lotsmanv1.UpstreamState_UPSTREAM_STATE_DEGRADED
		}
		return lotsmanv1.UpstreamState_UPSTREAM_STATE_UP
	case health.Down:
		return lotsmanv1.UpstreamState_UPSTREAM_STATE_DOWN
	}
	return lotsmanv1.UpstreamState_UPSTREAM_STATE_UNSPECIFIED
}

// duration and timestamp leave zero values unset: no measurement yet, or
// never.
func duration(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

func timestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
