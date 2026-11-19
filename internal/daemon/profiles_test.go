package daemon

import (
	"errors"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

// A profile edit moves the peers that use it at once, without a reload or a
// restart, and leaves everyone else where they are.
func TestProfileEditReroutesPeers(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	euPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)
	nlPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
	h.run(t)
	routedVia := func(peer netip.Addr) string { got, _ := h.dp.RouteOf(peer); return got }
	eventually(t, "initial routing", func() bool {
		return routedVia(euPeer.IP) == "lm-up-nl" && routedVia(nlPeer.IP) == "lm-up-nl"
	})
	h.d.mu.Lock()
	links := []link{h.d.ups[0].link, h.d.ups[1].link}
	h.d.mu.Unlock()

	// Through the API: PutProfile returns once the peers have moved.
	warnings, err := h.d.PutProfile(config.Profile{Name: "eu", Prefer: []config.Match{{Geo: "DE"}, {Geo: "NL"}}})
	if err != nil || len(warnings) != 0 {
		t.Fatalf("PutProfile = %q, %v", warnings, err)
	}
	if got := routedVia(euPeer.IP); got != "lm-up-de" {
		t.Errorf("eu peer routed via %q after the edit, want lm-up-de", got)
	}
	if got := routedVia(nlPeer.IP); got != "lm-up-nl" {
		t.Errorf("nl peer moved to %q by another profile's edit", got)
	}

	// Through the CLI: `profile set` writes the store and nudges the daemon,
	// which reconciles.
	if err := h.st.SetProfile(config.Profile{Name: "eu", Prefer: []config.Match{{Geo: "NL"}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := routedVia(euPeer.IP); got != "lm-up-nl" {
		t.Errorf("eu peer routed via %q after the store edit, want lm-up-nl", got)
	}

	h.d.mu.Lock()
	same := len(h.d.ups) == 2 && h.d.ups[0].link == links[0] && h.d.ups[1].link == links[1]
	h.d.mu.Unlock()
	if !same {
		t.Error("a profile edit reopened upstreams")
	}
}

func TestPutProfileValidatesAgainstRunningUpstreams(t *testing.T) {
	h := newHarness(t)
	before, _ := h.st.ListProfiles()
	for _, p := range []config.Profile{
		{Name: "Bad Name"},
		{Name: "fi", Prefer: []config.Match{{Geo: "FI"}}},
		// A typo in one tier is refused although another tier matches.
		{Name: "nl2", Prefer: []config.Match{{Geo: "NL"}, {Geo: "FI"}}},
	} {
		if _, err := h.d.PutProfile(p); !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("PutProfile(%+v) = %v, want ErrInvalidProfile", p, err)
		}
	}
	if after, _ := h.st.ListProfiles(); !reflect.DeepEqual(after, before) {
		t.Errorf("a refused PutProfile changed the store: %+v", after)
	}

	// A tier whose upstreams an earlier tier takes is stored and reported.
	want := []string{`profile "nl2": tier 2 geo=NL never applies: every upstream it matches is taken by tier 1 any`}
	warnings, err := h.d.PutProfile(config.Profile{Name: "nl2", Prefer: []config.Match{{}, {Geo: "NL"}}})
	if err != nil || !slices.Equal(warnings, want) {
		t.Fatalf("PutProfile = %q, %v; want %q", warnings, err, want)
	}
	profiles, warnings, err := h.d.Profiles()
	if err != nil || len(profiles) != 3 || profiles[2].Name != "nl2" || !slices.Equal(warnings, want) {
		t.Errorf("Profiles = %+v, %q, %v", profiles, warnings, err)
	}
}

func TestDeleteProfile(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)
	if err := h.d.DeleteProfile("eu"); !errors.Is(err, store.ErrInUse) {
		t.Errorf("deleting a used profile: %v", err)
	}
	if err := h.d.DeleteProfile("nl"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.DeleteProfile("nl"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a missing profile: %v", err)
	}
}
