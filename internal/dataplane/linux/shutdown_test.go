//go:build linux && integration

package linux

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

func TestTeardownStopsIngressPackets(t *testing.T) {
	for _, startup := range []string{"complete", "partial"} {
		t.Run(startup, func(t *testing.T) {
			packetLinks(t)
			// A separate plaintext host uplink reaches the same observer. After
			// policy routing is removed, main selects this link, not lm-up-a.
			packetCommand(t, "ip", "link", "add", "lm-host", "type", "veth", "peer", "name", "uplink0")
			t.Cleanup(func() { packetCommand(t, "ip", "link", "del", "lm-host") })
			packetCommand(t, "ip", "link", "set", "uplink0", "netns", "lm-provider")
			packetCommand(t, "ip", "addr", "add", "198.51.100.2/24", "dev", "lm-host")
			packetCommand(t, "ip", "link", "set", "lm-host", "up")
			packetCommand(t, "ip", "-n", "lm-provider", "addr", "add", "198.51.100.1/24", "dev", "uplink0")
			packetCommand(t, "ip", "-n", "lm-provider", "link", "set", "uplink0", "up")
			packetCommand(t, "ip", "-n", "lm-provider", "route", "add", "10.77.0.0/24", "via", "198.51.100.2")
			packetCommand(t, "iptables", "-P", "FORWARD", "ACCEPT")
			d := New()
			t.Cleanup(func() {
				if err := d.Teardown(); err != nil {
					t.Errorf("teardown retry: %v", err)
				}
			})
			ups := []dataplane.Interface{{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/24")}}
			if startup == "partial" {
				ups = append(ups, dataplane.Interface{Name: "lm-up-missing", Addr: netip.MustParsePrefix("10.9.0.2/32")})
			}
			err := d.Setup(dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/24")}, ups, 1380)
			if (err != nil) != (startup == "partial") {
				t.Fatalf("setup: %v", err)
			}
			if startup == "complete" {
				if err := d.Route(netip.MustParseAddr("10.77.0.2"), "lm-up-a"); err != nil {
					t.Fatal(err)
				}
			}
			checkPeerPacketsTo(t, "198.51.100.1", true)
			if err := d.Teardown(); err != nil {
				t.Fatal(err)
			}
			// This is the deterministic daemon cleanup boundary: Teardown has
			// returned but the downstream device has not been closed/deleted.
			link, err := netlink.LinkByName("lm0")
			if err != nil {
				t.Fatal(err)
			}
			if link.Attrs().Flags&net.FlagUp != 0 {
				t.Error("Teardown left downstream ingress up")
			}
			checkPeerPacketsTo(t, "198.51.100.1", false)
		})
	}
}

func TestTeardownPreservesProtectionOnDisableFailure(t *testing.T) {
	d := packetNetwork(t)
	if err := d.Route(netip.MustParseAddr("10.77.0.2"), "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	routes := routingState(t)
	firewall := packetCommand(t, "nft", "list", "table", "inet", nftTable)
	stopErr := errors.New("injected link-down failure")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := d.teardown(func(netlink.Link) error { return stopErr }); !errors.Is(err, stopErr) {
			t.Fatalf("disable attempt %d: %v", attempt, err)
		}
		if got := routingState(t); !reflect.DeepEqual(got, routes) {
			t.Fatalf("failed disable changed routes: %v", got)
		}
		if got := packetCommand(t, "nft", "list", "table", "inet", nftTable); got != firewall {
			t.Fatalf("failed disable changed firewall: %s", got)
		}
		checkPeerPackets(t, true)
	}
	if err := d.teardown(func(link netlink.Link) error {
		// Observe the exact boundary before ingress is disabled. Removing
		// even one rule earlier would fail this check, without a timing race.
		if got := routingState(t); !reflect.DeepEqual(got, routes) {
			t.Errorf("routes removed before ingress stopped: %v", got)
		}
		if got := packetCommand(t, "nft", "list", "table", "inet", nftTable); got != firewall {
			t.Errorf("firewall changed before ingress stopped: %s", got)
		}
		return netlink.LinkSetDown(link)
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := d.Teardown(); err != nil {
		t.Fatalf("repeated teardown: %v", err)
	}
	checkPeerPackets(t, false)
}

func TestTeardownAfterDownstreamRemoved(t *testing.T) {
	for _, name := range []string{"lm0", "lm-up-a"} {
		dummy(t, name)
	}
	foreign := routingState(t)
	d := New()
	t.Cleanup(func() { d.Teardown() })
	if err := d.Setup(
		dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/24")},
		[]dataplane.Interface{{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/32")}}, 1380,
	); err != nil {
		t.Fatal(err)
	}
	if err := d.Route(netip.MustParseAddr("10.77.0.2"), "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	packetCommand(t, "ip", "link", "del", "lm0")
	if err := d.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got := routingState(t); !reflect.DeepEqual(got, foreign) {
		t.Errorf("routing resources survived downstream close: %v", got)
	}
}
