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
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/ratelimit"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// A user may add AddBurst devices or profiles at once, and then one every
// AddEvery: enough to set up a household or try every profile, too few to
// churn keys and addresses.
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
	return userMessage(u, peers), nil
}

// Watch is StatusService.Watch narrowed to the user; see userSnapshot. It
// ends when the user is removed.
func (s *userSelfService) Watch(ctx context.Context, req *lotsmanv1.WatchStatusRequest, stream lotsmanv1connect.UserSelfServiceWatchServerStream) error {
	u, err := s.actingUser(ctx)
	if err != nil {
		return err
	}
	next := func() (*lotsmanv1.StatusSnapshot, error) {
		st, tr := s.src.Status(), s.src.Traffic()
		if _, err := s.src.Store().User(u.Name); err != nil {
			return nil, unauthenticated(err)
		}
		profiles, err := s.profiles()
		if err != nil {
			return nil, err
		}
		return userSnapshot(s.now(), s.src.ClientMTU(), st, tr, u.Name, profiles), nil
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
	if err == nil {
		var profiles map[string]config.Profile
		if profiles, err = s.profiles(); err != nil {
			return nil, err
		}
		if !userUpstreams(s.src.Status(), u.Name, profiles)[req.GetName()] {
			err = store.ErrNotFound
		}
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

// profiles are the stored profiles by name.
func (s *userSelfService) profiles() (map[string]config.Profile, error) {
	stored, err := s.src.Store().ListProfiles()
	if err != nil {
		return nil, internal(err)
	}
	profiles := map[string]config.Profile{}
	for _, p := range stored {
		profiles[p.Name] = p
	}
	return profiles, nil
}

// AddDevice adds a device, or another profile to one of the user's
// devices, within max_devices and the rate limit; max_devices 0 refuses
// every addition. The operator's AddDevice has neither limit.
func (s *userSelfService) AddDevice(ctx context.Context, req *lotsmanv1.AddSelfDeviceRequest) (*lotsmanv1.DeviceConfig, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	device, profile := req.GetDevice(), req.GetProfile()
	if err := checkNames("device", device, "profile", profile); err != nil {
		return nil, err
	}
	s.addMu.Lock()
	defer s.addMu.Unlock()
	peers, err := s.src.Store().UserPeers(u.Name)
	if err != nil {
		return nil, internal(err)
	}
	var devices []string
	for _, p := range peers {
		if p.Device == device && p.Profile == profile {
			return nil, addError(store.ErrExists, u.Name, device, profile)
		}
		if !slices.Contains(devices, p.Device) {
			devices = append(devices, p.Device)
		}
	}
	limit := s.src.MaxDevices()
	if limit == 0 {
		return nil, connect.NewError(connect.CodePermissionDenied, "adding devices is turned off; ask the operator")
	}
	if !slices.Contains(devices, device) && len(devices) >= limit {
		return nil, limitError("at most "+strconv.Itoa(limit)+" devices; remove one first or ask the operator",
			&lotsmanv1.SelfServiceLimit{Reason: lotsmanv1.SelfServiceLimit_REASON_MAX_DEVICES, MaxDevices: uint32(limit)})
	}
	if now := s.now(); !s.adds.Allow(u.Name, now) {
		return nil, limitError("too many devices added lately; try again in a few minutes",
			&lotsmanv1.SelfServiceLimit{Reason: lotsmanv1.SelfServiceLimit_REASON_RATE_LIMITED, RetryAfter: durationpb.New(s.adds.Wait(u.Name, now))})
	}
	p, err := s.src.AddPeer(u.Name, device, profile, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, unauthenticated(err) // removed meanwhile
	}
	if err != nil {
		return nil, addError(err, u.Name, device, profile)
	}
	return deviceConfig(s.src, p), nil
}

// RemoveDevice removes one profile of a device, or the whole device when
// the profile is empty.
func (s *userSelfService) RemoveDevice(ctx context.Context, req *lotsmanv1.RemoveSelfDeviceRequest) (*lotsmanv1.RemoveDeviceResponse, error) {
	u, err := s.actingUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetDevice() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "device is required")
	}
	if err := s.src.RemovePeers(u.Name, req.GetDevice(), req.GetProfile()); err != nil {
		return nil, notFound(err, "no such device or profile")
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

// ListProfiles names the profiles a user may choose; their tiers and
// warnings are the operator's business.
func (s *userSelfService) ListProfiles(ctx context.Context, _ *lotsmanv1.ListProfilesRequest) (*lotsmanv1.ListProfilesResponse, error) {
	if _, err := s.actingUser(ctx); err != nil {
		return nil, err
	}
	profiles, err := s.src.Store().ListProfiles()
	if err != nil {
		return nil, internal(err)
	}
	res := &lotsmanv1.ListProfilesResponse{}
	for _, p := range profiles {
		res.Profiles = append(res.Profiles, &lotsmanv1.Profile{Name: p.Name})
	}
	return res, nil
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
// the upstreams their peers' profiles may use or a peer of theirs is routed
// through. Other users' peers make up an upstream's routed, active and
// traffic, so those stay unset; the total is the user's own peers' sum.
func userSnapshot(now time.Time, clientMTU int, st daemon.Status, tr traffic.Snapshot, user string, profiles map[string]config.Profile) *lotsmanv1.StatusSnapshot {
	var own daemon.Status
	var total traffic.Traffic
	for _, p := range st.Peers {
		if p.User != user {
			continue
		}
		own.Peers = append(own.Peers, p)
		t := tr.Peers[p.ID]
		total.DownBytes += t.DownBytes
		total.UpBytes += t.UpBytes
		total.DownRate += t.DownRate
		total.UpRate += t.UpRate
	}
	usable := userUpstreams(st, user, profiles)
	for _, u := range st.Upstreams {
		if usable[u.Name] {
			u.Routed, u.Active = 0, 0
			own.Upstreams = append(own.Upstreams, u)
		}
	}
	return snapshot(now, clientMTU, own, traffic.Snapshot{Time: tr.Time, Peers: tr.Peers, Total: total})
}

// userUpstreams names the upstreams a user may see: those their peers'
// profiles may use, and those a peer of theirs is routed through.
func userUpstreams(st daemon.Status, user string, profiles map[string]config.Profile) map[string]bool {
	usable := map[string]bool{}
	for _, u := range st.Upstreams {
		up := config.Upstream{Name: u.Name, Geo: u.Geo, Provider: u.Provider, Tags: u.Tags}
		usable[u.Name] = slices.ContainsFunc(st.Peers, func(p daemon.PeerStatus) bool {
			profile, ok := profiles[p.Profile]
			return p.User == user && (p.Upstream == u.Name || (ok && policy.Allows(profile, up)))
		})
	}
	return usable
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
