package daemon

import (
	"errors"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/store"
)

// The API's user changes apply before they return: a new peer is on the
// device and routed, a removed one is gone.
func TestUserChangesApplyAtOnce(t *testing.T) {
	h := newHarness(t)
	h.run(t)
	<-h.d.Ready()
	onDevice := func(id int64) bool {
		for _, p := range h.d.Status().Peers {
			if p.ID == id {
				return p.Upstream != ""
			}
		}
		return false
	}

	if _, err := h.d.AddPeer("alice", "phone", nlRoute, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AddPeer for a missing user without createUser: %v", err)
	}
	phone, err := h.d.AddPeer("alice", "phone", nlRoute, true)
	if err != nil {
		t.Fatal(err)
	}
	if !onDevice(phone.ID) {
		t.Error("the new peer is not routed when AddPeer returns")
	}
	laptop, err := h.d.AddPeer("alice", "laptop", euRoute, false)
	if err != nil {
		t.Fatal(err)
	}
	conf := h.d.PeerConfig(laptop)
	if !strings.Contains(conf, laptop.PrivateKey.String()) || !strings.Contains(conf, laptop.IP.String()) {
		t.Errorf("config of the laptop:\n%s", conf)
	}

	// A new route moves the device before SetRoute returns.
	if _, err := h.d.SetRoute("alice", "phone", route("DE")); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(phone.IP); got != "lm-up-de" {
		t.Errorf("phone after SetRoute: %q, want lm-up-de", got)
	}
	// Rights apply before SetRights returns: without DE the phone is blocked.
	if _, err := h.d.SetRights("alice", store.Rights{Exits: []string{"nl"}}); err != nil {
		t.Fatal(err)
	}
	if onDevice(phone.ID) {
		t.Error("the phone is still routed outside its exits")
	}
	if _, err := h.d.SetRoute("alice", "tablet", nlRoute); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetRoute of a missing device: %v", err)
	}

	if err := h.d.RemovePeer("alice", "phone"); err != nil {
		t.Fatal(err)
	}
	if peers, _ := h.d.down.Peers(); len(peers) != 1 {
		t.Errorf("the removed peer is still on the device: %d peers", len(peers))
	}
	if err := h.d.RemovePeer("alice", "phone"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing a missing device: %v", err)
	}
	if err := h.d.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if onDevice(laptop.ID) {
		t.Error("the removed user's peer is still on the device")
	}
	if h.d.MaxDevices() != 5 {
		t.Errorf("MaxDevices = %d, want the default", h.d.MaxDevices())
	}
}

// A change made before the first round is not lost: the round reads the
// store, and a change it missed wakes it again.
func TestUserChangeBeforeReady(t *testing.T) {
	h := newHarness(t)
	p, err := h.d.AddPeer("alice", "phone", nlRoute, true)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	eventually(t, "the early peer routed", func() bool {
		for _, s := range h.d.Status().Peers {
			if s.ID == p.ID && s.Upstream != "" {
				return true
			}
		}
		return false
	})
}
