package control

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// Users is what the user services need from the daemon besides its status:
// the database, the running upstreams routes are checked against, and the
// changes the daemon applies at once. The services validate; the daemon
// stores and applies.
type Users interface {
	Store() *store.Store
	Upstreams() []config.Upstream
	AddPeer(user, device string, route policy.Route, createUser bool) (store.Peer, error)
	SetRoute(user, device string, route policy.Route) (store.Peer, error)
	SetRights(user string, r store.Rights) (store.User, error)
	RemovePeer(user, device string) error
	RemoveUser(name string) error
	PeerConfig(p store.Peer) string
	MaxDevices() int
}

// userService is the operator's: any user, no limits.
type userService struct{ users Users }

func (s userService) List(context.Context, *lotsmanv1.ListUsersRequest) (*lotsmanv1.ListUsersResponse, error) {
	st := s.users.Store()
	users, err := st.ListUsers()
	if err != nil {
		return nil, internal(err)
	}
	peers, err := st.ListPeers()
	if err != nil {
		return nil, internal(err)
	}
	byUser := map[string][]store.Peer{}
	for _, p := range peers {
		byUser[p.User] = append(byUser[p.User], p)
	}
	res := &lotsmanv1.ListUsersResponse{}
	for _, u := range users {
		res.Users = append(res.Users, userMessage(u, byUser[u.Name]))
	}
	return res, nil
}

func (s userService) AddUser(_ context.Context, req *lotsmanv1.AddUserRequest) (*lotsmanv1.User, error) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	u, err := s.users.Store().CreateUser(req.GetUser())
	if errors.Is(err, store.ErrExists) {
		return nil, connect.NewError(connect.CodeAlreadyExists, "user "+req.GetUser()+" already exists")
	}
	if err != nil {
		return nil, internal(err)
	}
	return userMessage(u, nil), nil
}

// SetRights replaces every right of the user; the devices move off exits
// no longer allowed before it answers. Their routes stay as they are and
// simply match less.
func (s userService) SetRights(_ context.Context, req *lotsmanv1.SetRightsRequest) (*lotsmanv1.User, error) {
	if req.GetRights() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "rights are required")
	}
	r := rightsOf(req.GetRights())
	if err := policy.ValidateExits(r.Exits, s.users.Upstreams()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	u, err := s.users.SetRights(req.GetUser(), r)
	if err != nil {
		return nil, notFound(err, "no such user")
	}
	peers, err := s.users.Store().UserPeers(u.Name)
	if err != nil {
		return nil, internal(err)
	}
	return userMessage(u, peers), nil
}

// AddDevice creates the user if needed, as `device add` does. Without a
// route the device gets the default one.
func (s userService) AddDevice(_ context.Context, req *lotsmanv1.AddDeviceRequest) (*lotsmanv1.DeviceConfig, error) {
	if err := checkUser(req.GetUser()); err != nil {
		return nil, err
	}
	if err := checkNames("device", req.GetDevice()); err != nil {
		return nil, err
	}
	route := policy.DefaultRoute()
	if m := req.GetRoute(); m != nil {
		rights, err := s.rights(req.GetUser())
		if err != nil {
			return nil, err
		}
		if route, err = validRoute(s.users, rights, m); err != nil {
			return nil, err
		}
	}
	p, err := s.users.AddPeer(req.GetUser(), req.GetDevice(), route, true)
	if err != nil {
		return nil, addError(err, req.GetUser(), req.GetDevice())
	}
	return deviceConfig(s.users, p), nil
}

// rights are the user's, or the defaults of one AddDevice is about to
// create.
func (s userService) rights(user string) (store.Rights, error) {
	u, err := s.users.Store().User(user)
	switch {
	case err == nil:
		return u.Rights, nil
	case errors.Is(err, store.ErrNotFound):
		return store.DefaultRights(), nil
	}
	return store.Rights{}, internal(err)
}

// SetRoute moves the device before it answers; its config stays valid.
func (s userService) SetRoute(_ context.Context, req *lotsmanv1.SetRouteRequest) (*lotsmanv1.Device, error) {
	u, err := s.users.Store().User(req.GetUser())
	if err != nil {
		return nil, notFound(err, "no such device")
	}
	p, err := setRoute(s.users, u, req.GetDevice(), req.GetRoute())
	if err != nil {
		return nil, err
	}
	return deviceMessage(p), nil
}

func (s userService) RemoveDevice(_ context.Context, req *lotsmanv1.RemoveDeviceRequest) (*lotsmanv1.RemoveDeviceResponse, error) {
	if err := s.users.RemovePeer(req.GetUser(), req.GetDevice()); err != nil {
		return nil, notFound(err, "no such device")
	}
	return &lotsmanv1.RemoveDeviceResponse{}, nil
}

func (s userService) RemoveUser(_ context.Context, req *lotsmanv1.RemoveUserRequest) (*lotsmanv1.RemoveUserResponse, error) {
	if err := s.users.RemoveUser(req.GetUser()); err != nil {
		return nil, notFound(err, "no such user")
	}
	return &lotsmanv1.RemoveUserResponse{}, nil
}

func (s userService) GetConfig(_ context.Context, req *lotsmanv1.GetConfigRequest) (*lotsmanv1.DeviceConfig, error) {
	p, err := peerByID(s.users.Store(), req.GetPeerId())
	if err != nil {
		return nil, err
	}
	return deviceConfig(s.users, p), nil
}

// IssueLink returns the token once: only its hash is kept.
func (s userService) IssueLink(_ context.Context, req *lotsmanv1.IssueLinkRequest) (*lotsmanv1.IssueLinkResponse, error) {
	token, err := s.users.Store().IssueLink(req.GetUser())
	if err != nil {
		return nil, notFound(err, "no such user")
	}
	return &lotsmanv1.IssueLinkResponse{Token: token}, nil
}

func (s userService) RevokeLink(_ context.Context, req *lotsmanv1.RevokeLinkRequest) (*lotsmanv1.RevokeLinkResponse, error) {
	if err := s.users.Store().RevokeLink(req.GetUser()); err != nil {
		return nil, notFound(err, "no such user")
	}
	return &lotsmanv1.RevokeLinkResponse{}, nil
}

// setRoute gives u's device a valid route, for both services: the
// self-service checks the user's rights first.
func setRoute(users Users, u store.User, device string, m *lotsmanv1.Route) (store.Peer, error) {
	if m == nil {
		return store.Peer{}, connect.NewError(connect.CodeInvalidArgument, "route is required")
	}
	route, err := validRoute(users, u.Rights, m)
	if err != nil {
		return store.Peer{}, err
	}
	p, err := users.SetRoute(u.Name, device, route)
	if err != nil {
		return store.Peer{}, notFound(err, "no such device")
	}
	return p, nil
}

// validRoute is m once it passes policy.Route.Validate against the running
// upstreams that rights allow.
func validRoute(users Users, rights store.Rights, m *lotsmanv1.Route) (policy.Route, error) {
	route := routeOf(m)
	if err := route.Validate(users.Upstreams(), rights.Exits); err != nil {
		return policy.Route{}, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return route, nil
}

// userMessage is a user with their peers, which come sorted by device as
// the store returns them.
func userMessage(u store.User, peers []store.Peer) *lotsmanv1.User {
	m := &lotsmanv1.User{Name: u.Name, HasLink: u.HasLink, Created: timestamp(u.CreatedAt), Rights: &lotsmanv1.Rights{
		EditRoutes: u.Rights.EditRoutes, ShowProviders: u.Rights.ShowProviders, Exits: u.Rights.Exits,
	}}
	for _, p := range peers {
		m.Devices = append(m.Devices, deviceMessage(p))
	}
	return m
}

func deviceMessage(p store.Peer) *lotsmanv1.Device {
	return &lotsmanv1.Device{
		PeerId: PeerID(p.ID), Name: p.Device, Address: p.IP.String(), Created: timestamppb.New(p.CreatedAt), Route: routeMessage(p.Route),
	}
}

func routeMessage(r policy.Route) *lotsmanv1.Route {
	m := &lotsmanv1.Route{Fastest: r.Fastest, Strict: r.Strict}
	for _, c := range r.Countries {
		m.Countries = append(m.Countries, &lotsmanv1.Country{Geo: c.Geo, Providers: c.Providers})
	}
	return m
}

func routeOf(m *lotsmanv1.Route) policy.Route {
	r := policy.Route{Fastest: m.GetFastest(), Strict: m.GetStrict()}
	for _, c := range m.GetCountries() {
		r.Countries = append(r.Countries, policy.Country{Geo: c.GetGeo(), Providers: c.GetProviders()})
	}
	return r
}

func rightsOf(m *lotsmanv1.Rights) store.Rights {
	return store.Rights{EditRoutes: m.GetEditRoutes(), ShowProviders: m.GetShowProviders(), Exits: m.GetExits()}
}

func deviceConfig(users Users, p store.Peer) *lotsmanv1.DeviceConfig {
	return &lotsmanv1.DeviceConfig{PeerId: PeerID(p.ID), Config: users.PeerConfig(p)}
}

// peerByID finds a peer by its API identifier. Anything that names no
// peer, malformed or not, is NotFound.
func peerByID(st *store.Store, peerID string) (store.Peer, error) {
	id, err := parsePeerID(peerID)
	if err == nil {
		var p store.Peer
		if p, err = st.PeerByID(id); err == nil {
			return p, nil
		}
	}
	return store.Peer{}, notFound(err, "no such peer")
}

func parsePeerID(s string) (int64, error) {
	digits, ok := strings.CutPrefix(s, "p_")
	id, err := strconv.ParseInt(digits, 10, 64)
	if !ok || err != nil || id <= 0 || PeerID(id) != s {
		return 0, store.ErrNotFound
	}
	return id, nil
}

// maxNameBytes bounds user and device names that come through the API. The
// CLI does not check names: its operator can fix what they typed.
const maxNameBytes = 64

// checkNames refuses names a browser could use to garble listings and
// logs: empty, overlong, not UTF-8, with control characters or with
// spaces around them. Each pair is what is named and its name.
func checkNames(pairs ...string) error {
	for i := 0; i+1 < len(pairs); i += 2 {
		what, name := pairs[i], pairs[i+1]
		if name == "" {
			return connect.NewError(connect.CodeInvalidArgument, what+" is required")
		}
		if len(name) > maxNameBytes || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) ||
			strings.TrimSpace(name) != name {
			return connect.NewError(connect.CodeInvalidArgument, what+" must be printable UTF-8 of at most 64 bytes, without spaces around it")
		}
	}
	return nil
}

// checkUser is checkNames for a new user, whose name must not hold a slash
// either: the CLI names a device USER/DEVICE.
func checkUser(name string) error {
	if err := checkNames("user", name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return connect.NewError(connect.CodeInvalidArgument, "user must not contain /")
	}
	return nil
}

func addError(err error, user, device string) error {
	switch {
	case errors.Is(err, store.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, user+" already has a device "+device)
	case errors.Is(err, store.ErrSubnetFull):
		return connect.NewError(connect.CodeResourceExhausted, "the gateway's subnet has no free address")
	}
	return internal(err)
}

// notFound maps store.ErrNotFound to NotFound with msg, anything else to
// Internal.
func notFound(err error, msg string) error {
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, msg)
	}
	return internal(err)
}

// internal hides a failure's detail, such as a database path or SQL, from
// the caller: it may be a browser.
func internal(error) error { return connect.NewError(connect.CodeInternal, "users: internal error") }
