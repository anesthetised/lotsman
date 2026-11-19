// Package controltest has fakes of the daemon for tests of the control API
// and of its clients.
package controltest

import (
	"net/netip"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
)

// Subnet is where Users allocates peer addresses.
var Subnet = netip.MustParsePrefix("10.77.0.0/24")

// Upstreams are what Users runs by default: NL by acme and beta, and DE
// by acme.
var Upstreams = []config.Upstream{
	{Name: "nl-ac", Geo: "NL", Provider: "acme"},
	{Name: "nl-be", Geo: "NL", Provider: "beta"},
	{Name: "de-ac", Geo: "DE", Provider: "acme"},
}

// Users is the daemon's user side (control.Users) over a real store in a
// temporary directory. It renders a peer's config as "config of p_<id>"
// and counts the changes it applied.
type Users struct {
	St    *store.Store
	Limit int               // MaxDevices
	Ups   []config.Upstream // Upstreams

	mu      sync.Mutex
	applied int
}

// NewUsers opens an empty store.
func NewUsers(t testing.TB) *Users {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lotsman.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Users{St: st, Limit: config.DefaultMaxDevices, Ups: Upstreams}
}

func (u *Users) Store() *store.Store          { return u.St }
func (u *Users) MaxDevices() int              { return u.Limit }
func (u *Users) Upstreams() []config.Upstream { return u.Ups }

func (u *Users) PeerConfig(p store.Peer) string { return "config of p_" + strconv.FormatInt(p.ID, 10) }

func (u *Users) AddPeer(user, device string, route policy.Route, createUser bool) (store.Peer, error) {
	add := u.St.AddPeer
	if createUser {
		add = u.St.AddUserPeer
	}
	p, err := add(user, device, route, Subnet)
	return p, u.apply(err)
}

func (u *Users) SetRoute(user, device string, route policy.Route) (store.Peer, error) {
	p, err := u.St.SetRoute(user, device, route)
	return p, u.apply(err)
}

func (u *Users) SetRights(user string, r store.Rights) (store.User, error) {
	usr, err := u.St.SetRights(user, r)
	return usr, u.apply(err)
}

func (u *Users) RemovePeer(user, device string) error { return u.apply(u.St.DeletePeer(user, device)) }

func (u *Users) RemoveUser(name string) error { return u.apply(u.St.DeleteUser(name)) }

// Applied is the number of changes applied so far.
func (u *Users) Applied() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.applied
}

func (u *Users) apply(err error) error {
	if err == nil {
		u.mu.Lock()
		u.applied++
		u.mu.Unlock()
	}
	return err
}
