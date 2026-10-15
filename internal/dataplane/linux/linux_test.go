//go:build linux && integration

package linux

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"reflect"
	"sort"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/userdata"
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

func peerRules(t *testing.T, d *Dataplane, peer netip.Addr) []netlink.Rule {
	t.Helper()
	rules, err := d.rulesFor(peer)
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

// foreignRules counts the rules Lotsman inserted into chains it does not own.
func foreignRules(t *testing.T) int {
	t.Helper()
	c := &nftables.Conn{}
	chains, err := foreignForwardChains(c)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ch := range chains {
		rules, err := c.GetRules(ch.Table, ch)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rules {
			if comment, ok := userdata.GetString(r.UserData, userdata.TypeComment); ok && comment == foreignRuleComment {
				n++
			}
		}
	}
	return n
}

func TestDataplane(t *testing.T) {
	for _, n := range []string{"lm0", "lm-up-a", "lm-up-b", "lm-up-c"} {
		dummy(t, n)
	}
	// A Docker-style host firewall: iptables-nft FORWARD chain with policy drop.
	if err := exec.Command("iptables", "-P", "FORWARD", "DROP").Run(); err != nil {
		t.Logf("iptables not usable, skipping foreign chain checks: %v", err)
	} else {
		t.Cleanup(func() { exec.Command("iptables", "-P", "FORWARD", "ACCEPT").Run() })
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
		if chains, _ := foreignForwardChains(&nftables.Conn{}); len(chains) > 0 {
			if got := foreignRules(t); got != 2*len(chains) {
				t.Errorf("round %d: %d foreign accept rules for %d chains", round, got, len(chains))
			}
		}
	}

	c := &nftables.Conn{}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"forward": 5, "postrouting": 1, "restore_mark": 1, "stamp_mark": 1}
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

	pinMap, err := c.GetSetByName(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable}, pinMapName)
	if err != nil {
		t.Fatal(err)
	}
	pins, _ := c.GetSetElements(pinMap)
	marks := map[string]uint32{}
	for _, e := range pins {
		marks[string(bytes.TrimRight(e.Key, "\x00"))] = binaryutil.NativeEndian.Uint32(e.Val)
	}
	if marks["lm-up-a"] != uint32(d.tables["lm-up-a"]) || marks["lm-up-b"] != uint32(d.tables["lm-up-b"]) {
		t.Errorf("pinmark map = %v, tables = %v", marks, d.tables)
	}

	peer := netip.MustParseAddr("10.77.0.5")
	if err := d.Route(peer, "nope"); err == nil {
		t.Error("unknown upstream accepted")
	}
	if err := d.Route(peer, "lm-up-b"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, d, peer); len(got) != 1 || got[0].Table != tableBase+1 {
		t.Fatalf("after Route b: %v", got)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, d, peer); len(got) != 1 || got[0].Table != tableBase {
		t.Fatalf("after Route a: %v", got)
	}
	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, d, peer); len(got) != 1 {
		t.Fatalf("Route to same upstream duplicated rules: %v", got)
	}
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, d, peer); len(got) != 0 {
		t.Fatalf("after Unroute: %v", got)
	}

	// Flow pinning: moving off an alive upstream leaves a fwmark rule for the old flows.
	pinsTo := func(table int) []netlink.Rule {
		var out []netlink.Rule
		for _, r := range peerRules(t, d, peer) {
			if r.Priority == pinRulePriority && r.Table == table {
				out = append(out, r)
			}
		}
		return out
	}
	tableA, tableB := d.tables["lm-up-a"], d.tables["lm-up-b"]
	d.Route(peer, "lm-up-b")
	d.Route(peer, "lm-up-a") // b was never marked alive: no pin
	if got := pinsTo(tableB); len(got) != 0 {
		t.Errorf("pinned to an upstream never marked alive: %v", got)
	}
	d.SetAlive("lm-up-a", true)
	d.Route(peer, "lm-up-b")
	got := pinsTo(tableA)
	if len(got) != 1 || got[0].Mark != uint32(tableA) || got[0].Mask == nil || *got[0].Mask != 0xffffffff {
		t.Fatalf("pin rule after moving off a: %+v", got)
	}
	if plain := peerRules(t, d, peer); len(plain) != 2 {
		t.Errorf("expected plain rule + pin rule, got %v", plain)
	}
	d.Route(peer, "lm-up-a") // moving back drops the now-redundant pin to a
	if got := pinsTo(tableA); len(got) != 0 {
		t.Errorf("pin to the current upstream survived: %v", got)
	}
	d.SetAlive("lm-up-b", true)
	d.Route(peer, "lm-up-b")
	d.SetAlive("lm-up-a", false)
	if got := pinsTo(tableA); len(got) != 0 {
		t.Errorf("pins to a dead upstream survived: %v", got)
	}
	d.Route(peer, "lm-up-a")
	if got := pinsTo(tableB); len(got) != 1 {
		t.Errorf("expected a pin to b: %v", got)
	}
	d.Unroute(peer)
	if got := peerRules(t, d, peer); len(got) != 0 {
		t.Fatalf("pins survived Unroute: %v", got)
	}

	// Reload: a leaves, c arrives, b keeps its table; a peer on a is unrouted first.
	d.Route(peer, "lm-up-a")
	d.Unroute(peer)
	tableB = d.tables["lm-up-b"]
	newUps := []dataplane.Interface{ups[1], {Name: "lm-up-c", Addr: netip.MustParsePrefix("10.10.0.2/32")}}
	if err := d.SetUpstreams(newUps, 1300); err != nil {
		t.Fatal(err)
	}
	if d.tables["lm-up-b"] != tableB {
		t.Errorf("table of a kept upstream changed: %d -> %d", tableB, d.tables["lm-up-b"])
	}
	if _, gone := d.tables["lm-up-a"]; gone {
		t.Error("removed upstream still has a table")
	}
	if routes, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: tableBase}, netlink.RT_FILTER_TABLE); len(routes) != 0 {
		t.Errorf("routes of removed upstream survived: %v", routes)
	}
	if got := rulesAt(t, probeRulePriority); len(got) != 2 {
		t.Errorf("probe rules after reload = %v", got)
	}
	if err := d.Route(peer, "lm-up-c"); err != nil {
		t.Fatal(err)
	}
	if got := peerRules(t, d, peer); len(got) != 1 || got[0].Table != d.tables["lm-up-c"] {
		t.Errorf("route via new upstream: %v", got)
	}
	if err := d.Route(peer, "lm-up-a"); err == nil {
		t.Error("removed upstream still routable")
	}
	link, _ := netlink.LinkByName("lm0")
	if link.Attrs().MTU != 1300 {
		t.Errorf("mtu after reload = %d", link.Attrs().MTU)
	}
	elems, _ = c.GetSetElements(set)
	names = nil
	for _, e := range elems {
		names = append(names, string(bytes.TrimRight(e.Key, "\x00")))
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"lm-up-b", "lm-up-c"}) {
		t.Errorf("upstreams set after reload = %q", names)
	}
	d.Unroute(peer)

	// An upstream whose interface was recreated (daemon replaced its device)
	// gets its address, route and rule back, and keeps its table.
	tableC := d.tables["lm-up-c"]
	cLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "lm-up-c"}}
	netlink.LinkDel(cLink)
	if err := netlink.LinkAdd(cLink); err != nil {
		t.Fatal(err)
	}
	if err := d.SetUpstreams(newUps, 1300); err != nil {
		t.Fatal(err)
	}
	if d.tables["lm-up-c"] != tableC {
		t.Errorf("table changed after recreation: %d -> %d", tableC, d.tables["lm-up-c"])
	}
	cl, _ := netlink.LinkByName("lm-up-c")
	addrs, _ := netlink.AddrList(cl, netlink.FAMILY_V4)
	cRoutes, _ := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: tableC}, netlink.RT_FILTER_TABLE)
	if len(addrs) != 1 || cl.Attrs().Flags&net.FlagUp == 0 || len(cRoutes) != 1 || cRoutes[0].LinkIndex != cl.Attrs().Index {
		t.Errorf("recreated upstream not restored: addrs=%v up=%v routes=%v", addrs, cl.Attrs().Flags&net.FlagUp != 0, cRoutes)
	}
	if got := rulesAt(t, probeRulePriority); len(got) != 2 {
		t.Errorf("probe rules after recreation = %v", got)
	}

	if err := d.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got := foreignRules(t); got != 0 {
		t.Errorf("%d foreign rules survived teardown", got)
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

// foreignRule installs a rule that belongs to someone else and removes it when the test ends.
func foreignRule(t *testing.T, priority int, src string, table int, edit ...func(*netlink.Rule)) {
	t.Helper()
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	r.Src = hostNet(netip.MustParseAddr(src))
	r.Table, r.Priority = table, priority
	for _, e := range edit {
		e(r)
	}
	if err := netlink.RuleAdd(r); err != nil {
		t.Fatalf("foreign rule %v: %v", r, err)
	}
	t.Cleanup(func() { netlink.RuleDel(r) })
}

// foreignRoute installs a route over the named interface into a table and removes it when the test ends.
func foreignRoute(t *testing.T, dev string, table int, dst string) {
	t.Helper()
	link, err := netlink.LinkByName(dev)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	_, ipnet, err := net.ParseCIDR(dst)
	if err != nil {
		t.Fatal(err)
	}
	r := &netlink.Route{LinkIndex: link.Attrs().Index, Table: table, Dst: ipnet, Scope: netlink.SCOPE_LINK}
	if err := netlink.RouteAdd(r); err != nil {
		t.Fatalf("route %s dev %s table %d: %v", dst, dev, table, err)
	}
	t.Cleanup(func() { netlink.RouteDel(r) })
}

// routingState lists every IPv4 rule and every route of the upstream table range, one line each.
func routingState(t *testing.T) []string {
	t.Helper()
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := rangeRoutes(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rules {
		out = append(out, fmt.Sprintf("rule %v mark=%d", r, r.Mark))
	}
	for _, r := range routes {
		out = append(out, fmt.Sprintf("route table=%d dst=%v link=%d", r.Table, r.Dst, r.LinkIndex))
	}
	sort.Strings(out)
	return out
}

// lost returns the lines of want that are missing from now.
func lost(want, now []string) []string {
	have := map[string]bool{}
	for _, l := range now {
		have[l] = true
	}
	var out []string
	for _, l := range want {
		if !have[l] {
			out = append(out, l)
		}
	}
	return out
}

func TestPreservesForeignState(t *testing.T) {
	for _, n := range []string{"lm0", "lm-up-a", "lm-up-b", "lm-up-c", "fgn0"} {
		dummy(t, n)
	}

	// Foreign state on the priorities and tables Lotsman uses, including rules
	// of exactly our shape that point at a table with foreign routes.
	foreignRule(t, probeRulePriority, "192.0.2.10", 12000)
	foreignRule(t, pinRulePriority, "192.0.2.11", 12001, func(r *netlink.Rule) { r.Mark = 7 })
	foreignRule(t, peerRulePriority, "192.0.2.12", 12002)
	foreignRule(t, peerRulePriority, "192.0.2.13", 1005, func(r *netlink.Rule) { r.IifName = "fgn0" })
	foreignRule(t, peerRulePriority, "192.0.2.14", 1000)
	foreignRule(t, peerRulePriority, "192.0.2.15", 1030)
	foreignRule(t, 6000, "192.0.2.16", 1002) // holds table 1002 without being a rule of ours
	foreignRoute(t, "fgn0", 1000, "0.0.0.0/0")
	foreignRoute(t, "fgn0", 1001, "10.99.0.0/16")
	foreignRoute(t, "fgn0", 1030, "0.0.0.0/0")
	foreign := routingState(t)

	// Leftovers of a crashed run: exact shape, empty or lm-up- tables.
	mask := uint32(0xffffffff)
	foreignRule(t, peerRulePriority, "10.77.0.7", 1020)
	foreignRule(t, probeRulePriority, "10.8.9.9", 1021)
	foreignRule(t, pinRulePriority, "10.77.0.7", 1020, func(r *netlink.Rule) { r.Mark, r.Mask = 1020, &mask })
	foreignRule(t, peerRulePriority, "10.77.0.8", 1022)
	foreignRoute(t, "lm-up-b", 1022, "0.0.0.0/0")

	down := dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/16")}
	ups := []dataplane.Interface{
		{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/32")},
		{Name: "lm-up-b", Addr: netip.MustParsePrefix("10.9.0.2/32")},
	}
	d := New()
	t.Cleanup(func() { d.Teardown() })
	peer := netip.MustParseAddr("10.77.0.5")

	for round := 1; round <= 2; round++ { // twice: Setup must be idempotent
		if err := d.Setup(down, ups, 1380); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// Occupied: 1000, 1001 (routes), 1002 (rule), 1030 (route); the leftovers' tables were cleaned.
		if d.tables["lm-up-a"] != 1003 || d.tables["lm-up-b"] != 1004 {
			t.Errorf("round %d: tables = %v, want 1003 and 1004", round, d.tables)
		}
		if got := lost(foreign, routingState(t)); len(got) != 0 {
			t.Errorf("round %d: setup destroyed foreign state: %v", round, got)
		}
		if got := routingState(t); len(got) != len(foreign)+4 { // 2 probe rules, 2 default routes
			t.Errorf("round %d: state after setup:\n%v\nforeign:\n%v", round, got, foreign)
		}
	}

	if err := d.Route(peer, "lm-up-a"); err != nil {
		t.Fatal(err)
	}
	d.SetAlive("lm-up-a", true)
	if err := d.Route(peer, "lm-up-b"); err != nil { // leaves a pin rule behind
		t.Fatal(err)
	}

	// Reload: a leaves, c arrives.
	newUps := []dataplane.Interface{ups[1], {Name: "lm-up-c", Addr: netip.MustParsePrefix("10.10.0.2/32")}}
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	if err := d.SetUpstreams(newUps, 1300); err != nil {
		t.Fatal(err)
	}
	if got := lost(foreign, routingState(t)); len(got) != 0 {
		t.Errorf("reload destroyed foreign state: %v", got)
	}
	if err := d.SetUpstreams(newUps, 1300); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := d.Route(peer, "lm-up-c"); err != nil {
		t.Fatal(err)
	}
	if err := d.Unroute(peer); err != nil {
		t.Fatal(err)
	}
	if got := lost(foreign, routingState(t)); len(got) != 0 {
		t.Errorf("route/unroute destroyed foreign state: %v", got)
	}

	for i := 1; i <= 2; i++ { // twice: Teardown must be idempotent
		if err := d.Teardown(); err != nil {
			t.Fatalf("teardown %d: %v", i, err)
		}
		if got := routingState(t); !reflect.DeepEqual(got, foreign) {
			t.Errorf("teardown %d: state differs from the foreign state\ngot:  %v\nwant: %v", i, got, foreign)
		}
	}
}

func TestSetupRefusesWhenNoTableIsFree(t *testing.T) {
	for _, n := range []string{"lm0", "lm-up-a"} {
		dummy(t, n)
	}
	for table := tableBase; table < tableLimit; table++ {
		foreignRule(t, 6000, "192.0.2.1", table)
	}
	foreignRule(t, probeRulePriority, "192.0.2.2", 12000)
	before := routingState(t)

	d := New()
	t.Cleanup(func() { d.Teardown() })
	down := dataplane.Interface{Name: "lm0", Addr: netip.MustParsePrefix("10.77.0.1/16")}
	ups := []dataplane.Interface{{Name: "lm-up-a", Addr: netip.MustParsePrefix("10.8.0.2/32")}}
	if err := d.Setup(down, ups, 1380); err == nil {
		t.Fatal("setup succeeded with every table occupied")
	}
	if err := d.Teardown(); err != nil {
		t.Fatal(err)
	}
	if got := routingState(t); !reflect.DeepEqual(got, before) {
		t.Errorf("failed setup changed routing state\ngot:  %v\nwant: %v", got, before)
	}
}
