package control

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/internal/store"
)

// Users is what the user services need from the daemon besides its status:
// the database and the changes the daemon applies at once.
type Users interface {
	Store() *store.Store
	AddPeer(user, device, profile string, createUser bool) (store.Peer, error)
	RemovePeers(user, device, profile string) error // profile "" = every profile
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

// AddDevice creates the user if needed, as `user add` does. An empty
// device is the default one, as in the CLI.
func (s userService) AddDevice(_ context.Context, req *lotsmanv1.AddDeviceRequest) (*lotsmanv1.DeviceConfig, error) {
	device := cmp.Or(req.GetDevice(), store.DefaultDevice)
	if err := checkNames("user", req.GetUser(), "device", device, "profile", req.GetProfile()); err != nil {
		return nil, err
	}
	p, err := s.users.AddPeer(req.GetUser(), device, req.GetProfile(), true)
	if err != nil {
		return nil, addError(err, req.GetUser(), device, req.GetProfile())
	}
	return deviceConfig(s.users, p), nil
}

func (s userService) RemoveDevice(_ context.Context, req *lotsmanv1.RemoveDeviceRequest) (*lotsmanv1.RemoveDeviceResponse, error) {
	device := cmp.Or(req.GetDevice(), store.DefaultDevice)
	if err := s.users.RemovePeers(req.GetUser(), device, req.GetProfile()); err != nil {
		return nil, notFound(err, "no such device or profile")
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

// userMessage is a user with their peers, which come sorted by device and
// profile as the store returns them.
func userMessage(u store.User, peers []store.Peer) *lotsmanv1.User {
	m := &lotsmanv1.User{Name: u.Name, HasLink: u.HasLink, Created: timestamp(u.CreatedAt)}
	for _, p := range peers {
		if n := len(m.Devices); n == 0 || m.Devices[n-1].Name != p.Device {
			m.Devices = append(m.Devices, &lotsmanv1.Device{Name: p.Device})
		}
		d := m.Devices[len(m.Devices)-1]
		d.Peers = append(d.Peers, &lotsmanv1.DevicePeer{
			PeerId: PeerID(p.ID), Profile: p.Profile, Address: p.IP.String(), Created: timestamppb.New(p.CreatedAt),
		})
	}
	return m
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

// maxNameBytes bounds user, device and profile names that come through the
// API. The CLI does not check names: its operator can fix what they typed.
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

func addError(err error, user, device, profile string) error {
	switch {
	case errors.Is(err, store.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, user+"/"+device+" already has profile "+profile)
	case errors.Is(err, store.ErrNoProfile):
		return connect.NewError(connect.CodeInvalidArgument, "no such profile: "+profile)
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
