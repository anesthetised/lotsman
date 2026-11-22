package control

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/types/known/durationpb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/ratelimit"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// A user may add AddBurst devices at once, and then one every AddEvery:
// enough to set up a household, too few to churn keys and addresses.
const (
	AddBurst = 10
	AddEvery = 6 * time.Minute
)

// userSelfService is what a user does on their own page. The panel resolves
// the link and names the user in the Lotsman-User header; the daemon trusts
// it, as only the panel can reach the socket. Another user's peer is
// NotFound rather than PermissionDenied, so its existence is not confirmed.
type userSelfService struct {
	src  Source
	now  func() time.Time
	adds *ratelimit.Limiter[string]
	// addMu makes counting a user's devices and adding one a single step,
	// so concurrent adds cannot pass max_devices together.
	addMu sync.Mutex
}

func newUserSelfService(src Source) *userSelfService {
	return &userSelfService{src: src, now: time.Now, adds: ratelimit.New[string](AddEvery, AddBurst)}
}

// ResolveLink is the panel's: it turns a link's token into its user. The
// error never repeats the token.
func (s *userSelfService) ResolveLink(_ context.Context, req *lotsmanv1.ResolveLinkRequest) (*lotsmanv1.ResolveLinkResponse, error) {
	user, err := s.src.Store().ResolveLink(req.GetToken())
	if err != nil {
		return nil, notFound(err, "unknown or revoked link")
	}
	return &lotsmanv1.ResolveLinkResponse{User: user}, nil
}

func (s *userSelfService) Get(ctx context.Context, _ *lotsmanv1.GetSelfRequest) (*lotsmanv1.User, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	peers, err := s.src.Store().UserPeers(u.Name)
	if err != nil {
		return nil, internal(err)
	}
	m := userMessage(u, peers)
	for _, d := range m.Devices {
		hideProviders(u, d.Route)
	}
	return m, nil
}

// Watch is StatusService.Watch narrowed to the user; see userSnapshot. It
// ends when the user is removed, and follows changes to their rights.
func (s *userSelfService) Watch(ctx context.Context, req *lotsmanv1.WatchStatusRequest, stream lotsmanv1connect.UserSelfServiceWatchServerStream) error {
	u, err := s.actingUser(ctx)
	if err != nil {
		return err
	}
	next := func() (*lotsmanv1.StatusSnapshot, error) {
		st, tr := s.src.Status(), s.src.Traffic()
		u, err := s.src.Store().User(u.Name)
		if err != nil {
			return nil, unauthenticated(err)
		}
		return userSnapshot(s.now(), s.src.ClientMTU(), st, tr, u), nil
	}
	return streamSnapshots(ctx, req, s.src.Ready(), next, stream.Send)
}

// UpstreamHistory is HistoryService.Upstream for an upstream the user's
// Watch shows, without bytes: those are every user's traffic. The daemon
// answers before the check, so another upstream fails exactly as a missing
// one does, a bad range included.
func (s *userSelfService) UpstreamHistory(ctx context.Context, req *lotsmanv1.HistoryRequest) (*lotsmanv1.HistoryResponse, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	r, err := historyRange(req)
	if err != nil {
		return nil, err
	}
	res, err := s.src.UpstreamHistory(req.GetName(), r)
	if err == nil && !userUpstreams(s.src.Status(), u)[req.GetName()] {
		err = store.ErrNotFound
	}
	out, err := historyResponse(res, err)
	for _, sample := range out.GetSamples() {
		sample.DownBytes, sample.UpBytes = 0, 0
	}
	return out, err
}

// PeerHistory is HistoryService.Peer for one of the user's own peers.
// Another user's peer fails exactly as a missing one does.
func (s *userSelfService) PeerHistory(ctx context.Context, req *lotsmanv1.HistoryRequest) (*lotsmanv1.HistoryResponse, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "name is required")
	}
	r, err := historyRange(req)
	if err != nil {
		return nil, err
	}
	id, ok := ParsePeerID(req.GetName())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no such peer")
	}
	res, err := s.src.PeerHistory(id, r)
	if err == nil {
		var p store.Peer
		if p, err = s.src.Store().PeerByID(id); err == nil && p.User != u.Name {
			err = store.ErrNotFound
		}
	}
	return historyResponse(res, err)
}

// AddDevice adds a device within max_devices and the rate limit;
// max_devices 0 refuses every addition. Without edit_routes the device gets
// the default route and naming one is refused. The operator's AddDevice
// has neither limit.
func (s *userSelfService) AddDevice(ctx context.Context, req *lotsmanv1.AddSelfDeviceRequest) (*lotsmanv1.DeviceConfig, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	device := req.GetDevice()
	if err := checkNames("device", device); err != nil {
		return nil, err
	}
	route := policy.DefaultRoute()
	if m := req.GetRoute(); m != nil {
		if err := checkRoute(u, m); err != nil {
			return nil, err
		}
		if route, err = validRoute(s.src, u.Rights, m); err != nil {
			return nil, err
		}
	}
	s.addMu.Lock()
	defer s.addMu.Unlock()
	peers, err := s.src.Store().UserPeers(u.Name)
	if err != nil {
		return nil, internal(err)
	}
	if slices.ContainsFunc(peers, func(p store.Peer) bool { return p.Device == device }) {
		return nil, addError(store.ErrExists, u.Name, device)
	}
	limit := s.src.MaxDevices()
	if limit == 0 {
		return nil, connect.NewError(connect.CodePermissionDenied, "adding devices is turned off; ask the operator")
	}
	if len(peers) >= limit {
		return nil, limitError("at most "+strconv.Itoa(limit)+" devices; remove one first or ask the operator",
			&lotsmanv1.SelfServiceLimit{Reason: lotsmanv1.SelfServiceLimit_REASON_MAX_DEVICES, MaxDevices: uint32(limit)})
	}
	if now := s.now(); !s.adds.Allow(u.Name, now) {
		return nil, limitError("too many devices added lately; try again in a few minutes",
			&lotsmanv1.SelfServiceLimit{Reason: lotsmanv1.SelfServiceLimit_REASON_RATE_LIMITED, RetryAfter: durationpb.New(s.adds.Wait(u.Name, now))})
	}
	p, err := s.src.AddPeer(u.Name, device, route, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, unauthenticated(err) // removed meanwhile
	}
	if err != nil {
		return nil, addError(err, u.Name, device)
	}
	return deviceConfig(s.src, p), nil
}

// SetRoute is the operator's SetRoute for one of the user's devices, within
// their rights.
func (s *userSelfService) SetRoute(ctx context.Context, req *lotsmanv1.SetSelfRouteRequest) (*lotsmanv1.Device, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkRoute(u, req.GetRoute()); err != nil {
		return nil, err
	}
	p, err := setRoute(s.src, u, req.GetDevice(), req.GetRoute())
	if err != nil {
		return nil, err
	}
	return deviceMessage(p), nil
}

// checkRoute applies the user's rights to a route they set: only with
// edit_routes, and naming providers only with show_providers.
func checkRoute(u store.User, m *lotsmanv1.Route) error {
	if !u.Rights.EditRoutes {
		return connect.NewError(connect.CodePermissionDenied, "your routes are set by the operator")
	}
	if !u.Rights.ShowProviders && slices.ContainsFunc(m.GetCountries(), func(c *lotsmanv1.Country) bool { return len(c.GetProviders()) > 0 }) {
		return connect.NewError(connect.CodeInvalidArgument, "route: providers are not shown to you, so a route may name countries only")
	}
	return nil
}

func (s *userSelfService) RemoveDevice(ctx context.Context, req *lotsmanv1.RemoveSelfDeviceRequest) (*lotsmanv1.RemoveDeviceResponse, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetDevice() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "device is required")
	}
	if err := s.src.RemovePeer(u.Name, req.GetDevice()); err != nil {
		return nil, notFound(err, "no such device")
	}
	return &lotsmanv1.RemoveDeviceResponse{}, nil
}

func (s *userSelfService) GetConfig(ctx context.Context, req *lotsmanv1.GetConfigRequest) (*lotsmanv1.DeviceConfig, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	p, err := peerByID(s.src.Store(), req.GetPeerId())
	if err == nil && p.User != u.Name {
		err = notFound(store.ErrNotFound, "no such peer") // the same answer as for no peer at all
	}
	if err != nil {
		return nil, err
	}
	return deviceConfig(s.src, p), nil
}

// actingUser is the user the panel named in the Lotsman-User header. A
// call without exactly one, or naming nobody, is Unauthenticated.
func (s *userSelfService) actingUser(ctx context.Context) (store.User, error) {
	var names []string
	if info, ok := connect.CallInfoForServerContext(ctx); ok {
		names = info.RequestHeader().Values(UserHeader)
	}
	if len(names) != 1 || names[0] == "" {
		return store.User{}, connect.NewError(connect.CodeUnauthenticated, "no user")
	}
	u, err := s.src.Store().User(names[0])
	if err != nil {
		return store.User{}, unauthenticated(err)
	}
	return u, nil
}

// unauthenticated maps a user that does not exist (any more) to
// Unauthenticated, anything else to Internal.
func unauthenticated(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeUnauthenticated, "unknown user")
	}
	return internal(err)
}

// userSnapshot is the status as one user may see it: their own peers, and
// the upstreams of userUpstreams. Other users' peers make up an upstream's
// routed, active and traffic, so those stay unset; the total is the user's
// own peers' sum. Incidents are the operator's. Without show_providers no
// upstream has a provider.
func userSnapshot(now time.Time, clientMTU int, st daemon.Status, tr traffic.Snapshot, u store.User) *lotsmanv1.StatusSnapshot {
	var own daemon.Status
	var total traffic.Traffic
	for _, p := range st.Peers {
		if p.User != u.Name {
			continue
		}
		own.Peers = append(own.Peers, p)
		t := tr.Peers[p.ID]
		total.DownBytes += t.DownBytes
		total.UpBytes += t.UpBytes
		total.DownRate += t.DownRate
		total.UpRate += t.UpRate
	}
	visible := userUpstreams(st, u)
	for _, up := range st.Upstreams {
		if visible[up.Name] {
			up.Routed, up.Active, up.Incidents = 0, 0, nil
			up.ExpiresAt = time.Time{}
			if up.RoutingBlock != daemon.RoutingAllowed {
				up.RoutingBlock = daemon.RoutingUnavailable
			}
			if !u.Rights.ShowProviders {
				up.Provider = ""
			}
			own.Upstreams = append(own.Upstreams, up)
		}
	}
	return snapshot(now, clientMTU, own, traffic.Snapshot{Time: tr.Time, Peers: tr.Peers, Total: total})
}

// userUpstreams names the upstreams a user may see: their allowed exits,
// and any a device of theirs is still routed through, as briefly after their
// exits change.
func userUpstreams(st daemon.Status, u store.User) map[string]bool {
	visible := map[string]bool{}
	for _, up := range st.Upstreams {
		visible[up.Name] = policy.Allows(u.Rights.Exits, up.Name) ||
			slices.ContainsFunc(st.Peers, func(p daemon.PeerStatus) bool { return p.User == u.Name && p.Upstream == up.Name })
	}
	return visible
}

// hideProviders clears the providers of a route shown to u when u may not
// see them; the operator may still have named some.
func hideProviders(u store.User, r *lotsmanv1.Route) {
	if u.Rights.ShowProviders {
		return
	}
	for _, c := range r.GetCountries() {
		c.Providers = nil
	}
}

// limitError is a resource_exhausted AddDevice whose detail says which limit
// refused it, so clients need not read the message.
func limitError(msg string, limit *lotsmanv1.SelfServiceLimit) error {
	err := connect.NewError(connect.CodeResourceExhausted, msg)
	if detail, derr := connectproto.NewErrorDetail(limit); derr == nil {
		err = err.WithDetail(detail)
	}
	return err
}
