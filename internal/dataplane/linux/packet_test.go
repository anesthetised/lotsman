//go:build linux && integration

package linux

import (
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

// packetLinks puts both ends of the gateway in disposable namespaces. The
// upstream /24 deliberately creates the main-table route behind issue #14.
func packetLinks(t *testing.T) {
	t.Helper()
	for _, ns := range []string{"lm-client", "lm-provider"} {
		packetCommand(t, "ip", "netns", "add", ns)
		t.Cleanup(func() { packetCommand(t, "ip", "netns", "del", ns) })
	}
	for _, link := range []struct{ name, peer, ns, addr string }{
		{"lm0", "client0", "lm-client", "10.77.0.2/24"},
		{"lm-up-a", "provider0", "lm-provider", "10.8.0.1/24"},
	} {
		packetCommand(t, "ip", "link", "add", link.name, "type", "veth", "peer", "name", link.peer)
		t.Cleanup(func() { packetCommand(t, "ip", "link", "del", link.name) })
		packetCommand(t, "ip", "link", "set", link.peer, "netns", link.ns)
		packetCommand(t, "ip", "-n", link.ns, "addr", "add", link.addr, "dev", link.peer)
		packetCommand(t, "ip", "-n", link.ns, "link", "set", link.peer, "up")
	}
	packetCommand(t, "ip", "-n", "lm-client", "route", "add", "default", "via", "10.77.0.1")
	// Count requests at the provider, not just missing replies at the client.
	nft := exec.Command("ip", "netns", "exec", "lm-provider", "nft", "-f", "-")
	nft.Stdin = strings.NewReader(`table ip observe {
 counter requests { }
 chain input {
  type filter hook input priority 0;
  ip protocol icmp icmp type echo-request counter name requests
 }
}
`)
	if out, err := nft.CombinedOutput(); err != nil {
		t.Fatalf("provider counter: %v: %s", err, out)
	}
}

func packetNetwork(t *testing.T) *Dataplane {
	t.Helper()
	packetLinks(t)
	d := New()
	t.Cleanup(func() {
		if err := d.Teardown(); err != nil {
			t.Errorf("teardown: %v", err)
		}
	})
	if err := d.Setup(
		dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/24")},
		[]dataplane.Interface{{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/24")}}, 1380,
	); err != nil {
		t.Fatal(err)
	}
	return d
}

func packetCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
	return string(out)
}

func checkPeerPackets(t *testing.T, allowed bool) {
	t.Helper()
	checkPeerPacketsTo(t, "10.8.0.1", allowed)
}

func checkPeerPacketsTo(t *testing.T, destination string, allowed bool) {
	t.Helper()
	packetCommand(t, "ip", "netns", "exec", "lm-provider", "nft", "reset", "counter", "ip", "observe", "requests")
	out, err := exec.Command("ip", "netns", "exec", "lm-client", "ping", "-n", "-c", "1", "-W", "1", destination).CombinedOutput()
	if allowed && err != nil {
		t.Fatalf("authorized packet failed: %v: %s", err, out)
	}
	if !allowed && err == nil {
		t.Error("blocked peer received a reply")
	}
	counter := packetCommand(t, "ip", "netns", "exec", "lm-provider", "nft", "list", "counter", "ip", "observe", "requests")
	want := "packets 0 "
	if allowed {
		want = "packets 1 "
	}
	if !strings.Contains(counter, want) {
		t.Errorf("provider must see %q: %s", want, counter)
	}
}

func TestUnroutedPeerPackets(t *testing.T) {
	d := packetNetwork(t)
	peer := netip.MustParseAddr("10.77.0.2")
	t.Run("never assigned", func(t *testing.T) { checkPeerPackets(t, false) })
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	t.Run("authorized", func(t *testing.T) { checkPeerPackets(t, true) })
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	t.Run("revoked", func(t *testing.T) { checkPeerPackets(t, false) })
	if err := d.Unroute(peer); err != nil { // repeated revocation is harmless
		t.Fatal(err)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	t.Run("reauthorized", func(t *testing.T) { checkPeerPackets(t, true) })
	cleanupErr := errors.New("injected rule deletion failure")
	for attempt := 1; attempt <= 2; attempt++ {
		if err := d.unroute(peer, func(*netlink.Rule) error { return cleanupErr }); !errors.Is(err, cleanupErr) {
			t.Fatalf("cleanup attempt %d: %v", attempt, err)
		}
		if len(peerRules(t, d, peer)) == 0 {
			t.Fatal("failure injection did not leave the routing rule behind")
		}
		checkPeerPackets(t, false)
	}
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	if len(peerRules(t, d, peer)) != 0 {
		t.Fatal("cleanup retry left a routing rule behind")
	}
	// An existing rule after a failed authorization must still be authorizable
	// on retry; Route must not return early solely because the rule exists.
	if err := addRule(peer, d.tables["lm-up-a"], peerRulePriority); err != nil {
		t.Fatal(err)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	t.Run("existing rule retry", func(t *testing.T) { checkPeerPackets(t, true) })
}
