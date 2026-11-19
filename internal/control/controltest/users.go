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
	"github.com/anesthetised/lotsman/internal/store"
)

// Subnet is where Users allocates peer addresses.
var Subnet = netip.MustParsePrefix("10.77.0.0/24")

// Users is the daemon's user side (control.Users) over a real store in a
// temporary directory. It renders a peer's config as "config of p_<id>"
// and counts the changes it applied.
type Users struct {
	St    *store.Store
	Limit int // MaxDevices

	mu      sync.Mutex
	applied int
}

// NewUsers opens a store with the given profiles, each without tiers.
func NewUsers(t testing.TB, profiles ...string) *Users {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "lotsman.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, name := range profiles {
		if err := st.AddProfile(config.Profile{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	return &Users{St: st, Limit: config.DefaultMaxDevices}
}

func (u *Users) Store() *store.Store { return u.St }
func (u *Users) MaxDevices() int     { return u.Limit }

func (u *Users) PeerConfig(p store.Peer) string { return "config of p_" + strconv.FormatInt(p.ID, 10) }

func (u *Users) AddPeer(user, device, profile string, createUser bool) (store.Peer, error) {
	add := u.St.AddPeer
	if createUser {
		add = u.St.AddUserPeer
	}
	p, err := add(user, device, profile, Subnet)
	return p, u.apply(err)
}

func (u *Users) RemovePeers(user, device, profile string) error {
	if profile == "" {
		return u.apply(u.St.DeleteDevice(user, device))
	}
	return u.apply(u.St.DeletePeer(user, device, profile))
}

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
