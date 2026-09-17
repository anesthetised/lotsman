//go:build linux && integration

package linux

import (
	"bytes"
	"net"
	"net/netip"
	"os/exec"
	"reflect"
	"sort"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

func dummy(t *testing.T, name string) {
	t.Helper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	netlink.LinkDel(link)
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("add dummy %s: %v (run as root with NET_ADMIN)", name, err)
	}
	t.Cleanup(func() { netlink.LinkDel(link) })
}

func peerRules(t *testing.T, peer netip.Addr) []netlink.Rule {
	t.Helper()
	rules, err := rulesFor(peer)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func rulesAt(t *testing.T, priority int) []netlink.Rule {
	t.Helper()
	all, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	var out []netlink.Rule
	for _, r := range all {
		if r.Priority == priority {
			out = append(out, r)
		}
	}
	return out
}

func TestDataplane(t *testing.T) {
	for _, n := range []string{"lm0", "lm-up-a", "lm-up-b"} {
		dummy(t, n)
	}
	down := dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/16")}
	ups := []dataplane.Interface{
		{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/32")},
		{Name: "lm-up-b", Addr: netip.MustParsePrefix("10.9.0.2/32")},
	}
	d := New()
	t.Cleanup(func() { d.Teardown() })

	for round := 1; round <= 2; round++ { // twice: Setup must be idempotent
		if err := d.Setup(down, ups, 1380); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		link, _ := netlink.LinkByName("lm0")
		if link.Attrs().MTU != 1380 || link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("lm0 mtu=%d flags=%v", link.Attrs().MTU, link.Attrs().Flags)
		}
		for i, up := range ups {
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: tableBase + i}, netlink.RT_FILTER_TABLE)
			if err != nil || len(routes) != 1 {
				t.Fatalf("table %d routes = %v, %v", tableBase+i, routes, err)
			}
			upLink, _ := netlink.LinkByName(up.Name)
			if routes[0].LinkIndex != upLink.Attrs().Index || routes[0].Dst != nil && routes[0].Dst.IP.String() != "0.0.0.0" {
				t.Errorf("table %d route = %+v", tableBase+i, routes[0])
			}
		}
		if got := rulesAt(t, probeRulePriority); len(got) != 2 {
			t.Errorf("round %d: probe rules = %v", round, got)
		}
	}

	c := &nftables.Conn{}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"forward": 5, "postrouting": 1}
	for _, ch := range chains {
		if ch.Table.Name != nftTable {
			continue
		}
		rules, err := c.GetRules(ch.Table, ch)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != want[ch.Name] {
			t.Errorf("chain %s has %d rules, want %d", ch.Name, len(rules), want[ch.Name])
		}
		delete(want, ch.Name)
	}
	if len(want) != 0 {
		t.Errorf("chains missing: %v", want)
	}
	set, err := c.GetSetByName(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable}, "upstreams")
	if err != nil {
		t.Fatal(err)
	}
	elems, err := c.GetSetElements(set)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range elems {
		names = append(names, string(bytes.TrimRight(e.Key, "\x00")))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"lm-up-a", "lm-up-b"}) {
		t.Errorf("upstreams set = %q", names)
	}
	if out, err := exec.Command("nft", "list", "table", "inet", nftTable).CombinedOutput(); err == nil {
		t.Logf("nft:\n%s", out)
	}

	peer := netip.MustParseAddr("10.77.0.5")
	if err := d.Route(peer, "nope"); err == nil {
		t.Error("unknown upstream accepted")
	}
	if err := d.Route(peer, "lm-up-b"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, peer); len(got) != 1 || got[0].Table != tableBase+1 {
		t.Fatalf("after Route b: %v", got)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, peer); len(got) != 1 || got[0].Table != tableBase {
		t.Fatalf("after Route a: %v", got)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, peer); len(got) != 1 {
		t.Fatalf("Route to same upstream duplicated rules: %v", got)
	}
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, peer); len(got) != 0 {
		t.Fatalf("after Unroute: %v", got)
	}

	if err := d.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got := rulesAt(t, probeRulePriority); len(got) != 0 {
		t.Errorf("probe rules survived teardown: %v", got)
	}
	routes, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: tableBase}, netlink.RT_FILTER_TABLE)
	if len(routes) != 0 {
		t.Errorf("routes survived teardown: %v", routes)
	}
	tables, _ := c.ListTablesOfFamily(nftables.TableFamilyINet)
	for _, tb := range tables {
		if tb.Name == nftTable {
			t.Error("nftables table survived teardown")
		}
	}
	if err := d.Teardown(); err != nil {
		t.Errorf("second teardown: %v", err)
	}
}
