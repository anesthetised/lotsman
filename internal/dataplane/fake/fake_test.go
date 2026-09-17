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
	dp.Teardown()
	if !f.TornDown || len(f.Log) != 5 {
		t.Errorf("log = %v", f.Log)
	}
}
