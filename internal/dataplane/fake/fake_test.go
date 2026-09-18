package fake

import (
	"net/netip"
	"testing"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

func TestFake(t *testing.T) {
	f := New()
	var dp dataplane.Dataplane = f
	ups := []dataplane.Interface{{Name: "a"}, {Name: "b"}}
	if err := dp.Setup(dataplane.Interface{Name: "lm0"}, ups, 1400); err != nil {
		t.Fatal(err)
	}
	peer := netip.MustParseAddr("10.77.0.2")
	if err := dp.Route(peer, "nope"); err == nil {
		t.Error("routing via unknown upstream should fail")
	}
	if err := dp.Route(peer, "a"); err != nil {
		t.Fatal(err)
	}
	if u, ok := f.RouteOf(peer); !ok || u != "a" {
		t.Errorf("RouteOf = %q, %v", u, ok)
	}
	dp.Route(peer, "b")
	if u, _ := f.RouteOf(peer); u != "b" {
		t.Errorf("route not replaced: %q", u)
	}
	dp.Unroute(peer)
	if _, ok := f.RouteOf(peer); ok {
		t.Error("route survived Unroute")
	}
	// Pinning: moving off an alive upstream keeps old flows there; a dead one does not.
	dp.SetAlive("b", true)
	dp.Route(peer, "b")
	dp.Route(peer, "a")
	if !f.PinnedTo(peer, "b") {
		t.Error("flows not pinned to the alive previous upstream")
	}
	dp.SetAlive("b", false)
	if f.PinnedTo(peer, "b") {
		t.Error("pins to a dead upstream survived")
	}
	dp.Route(peer, "b")
	if f.PinnedTo(peer, "a") {
		t.Error("pinned to an upstream never marked alive")
	}
	dp.Unroute(peer)
	if len(f.Pins[peer]) != 0 {
		t.Error("pins survived Unroute")
	}
	dp.SetUpstreams([]dataplane.Interface{{Name: "c"}}, 1300)
	if err := dp.Route(peer, "a"); err == nil {
		t.Error("removed upstream still routable")
	}
	if err := dp.Route(peer, "c"); err != nil {
		t.Error(err)
	}
	dp.Unroute(peer)
	dp.Teardown()
	if !f.TornDown || f.MTU != 1300 || len(f.Log) != 14 {
		t.Errorf("log = %v", f.Log)
	}
}
