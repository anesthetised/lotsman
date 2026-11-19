package daemon

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestPeerRoutes(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	nl, _ := h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	eu, _ := h.st.AddPeer("alice", "eu", euRoute, h.cfg.Subnet)
	h.run(t)
	<-h.d.Ready()

	want := map[netip.Addr]PeerRoute{
		nl.IP: {ID: nl.ID, User: "alice", Device: "nl", Upstream: "nl"},
		eu.IP: {ID: eu.ID, User: "alice", Device: "eu", Upstream: "nl"},
	}
	if diff := cmp.Diff(want, h.d.PeerRoutes(), cmpopts.EquateComparable(netip.Addr{})); diff != "" {
		t.Errorf("routes (-want +got):\n%s", diff)
	}

	// NL dies: eu falls back to DE, the nl-only peer is blocked.
	h.setHealthy("lm-up-nl", false)
	want[nl.IP] = PeerRoute{ID: nl.ID, User: "alice", Device: "nl"}
	want[eu.IP] = PeerRoute{ID: eu.ID, User: "alice", Device: "eu", Upstream: "de"}
	eventually(t, "failover", func() bool { return cmp.Equal(want, h.d.PeerRoutes(), cmpopts.EquateComparable(netip.Addr{})) })
}
