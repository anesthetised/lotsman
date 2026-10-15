//go:build linux

package linux

import (
	"net"
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink"
)

// shapeRule builds a rule the way the kernel lists it: the defaults of
// netlink.NewRule, IPv4, a host source.
func shapeRule(priority int, src string, table int, edit ...func(*netlink.Rule)) netlink.Rule {
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	r.Src = hostNet(netip.MustParseAddr(src))
	r.Table, r.Priority = table, priority
	for _, e := range edit {
		e(r)
	}
	return *r
}

func TestOursMatchesOnlyTheRulesWeInstall(t *testing.T) {
	full := uint32(0xffffffff)
	partial := uint32(0xff)
	pin := func(mark uint32, mask *uint32) func(*netlink.Rule) {
		return func(r *netlink.Rule) { r.Mark, r.Mask = mark, mask }
	}
	tests := []struct {
		name string
		rule netlink.Rule
		want bool
	}{
		{"peer", shapeRule(peerRulePriority, "10.77.0.5", 1000), true},
		{"probe", shapeRule(probeRulePriority, "10.8.0.2", 1099), true},
		{"qualified probe", shapeRule(probeRulePriority, "10.8.0.2", 1099, func(r *netlink.Rule) { r.OifName = "lm-up-a" }), true},
		{"foreign qualified probe", shapeRule(probeRulePriority, "10.8.0.2", 1099, func(r *netlink.Rule) { r.OifName = "eth0" }), false},
		{"pin", shapeRule(pinRulePriority, "10.77.0.5", 1003, pin(1003, &full)), true},
		{"pin with another mark", shapeRule(pinRulePriority, "10.77.0.5", 1003, pin(7, &full)), false},
		{"pin with partial mask", shapeRule(pinRulePriority, "10.77.0.5", 1003, pin(1003, &partial)), false},
		{"pin without mark", shapeRule(pinRulePriority, "10.77.0.5", 1003), false},
		{"peer with a mark", shapeRule(peerRulePriority, "10.77.0.5", 1003, pin(1003, &full)), false},
		{"other priority", shapeRule(6000, "10.77.0.5", 1000), false},
		{"table below range", shapeRule(peerRulePriority, "10.77.0.5", tableBase-1), false},
		{"table above range", shapeRule(peerRulePriority, "10.77.0.5", tableLimit), false},
		{"foreign table", shapeRule(probeRulePriority, "10.8.0.2", 12000), false},
		{"source not a host", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) {
			r.Src = &net.IPNet{IP: net.IPv4(10, 77, 0, 0).To4(), Mask: net.CIDRMask(24, 32)}
		}), false},
		{"no source", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.Src = nil }), false},
		{"with iif", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.IifName = "eth0" }), false},
		{"with oif", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.OifName = "eth0" }), false},
		{"with destination", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) {
			r.Dst = &net.IPNet{IP: net.IPv4(1, 1, 1, 1).To4(), Mask: net.CIDRMask(32, 32)}
		}), false},
		{"with dport", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) {
			r.Dport = netlink.NewRulePortRange(443, 443)
		}), false},
		{"with tos", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.Tos = 4 }), false},
		{"inverted", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.Invert = true }), false},
		{"with another originator", shapeRule(peerRulePriority, "10.77.0.5", 1000, func(r *netlink.Rule) { r.Protocol = 12 }), false},
	}
	for _, tt := range tests {
		if got := ours(tt.rule); got != tt.want {
			t.Errorf("%s: ours = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestOwnedTable(t *testing.T) {
	names := map[int]string{1: "lm-up-a", 2: "eth0"}
	def := &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	lan := &net.IPNet{IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)}
	via := net.IPv4(192, 0, 2, 1)
	tests := []struct {
		name   string
		table  int
		routes []netlink.Route
		want   bool
	}{
		{"empty table", 1000, nil, true},
		{"our default route", 1000, []netlink.Route{{Table: 1000, LinkIndex: 1, Dst: def}}, true},
		{"default route without destination", 1000, []netlink.Route{{Table: 1000, LinkIndex: 1}}, true},
		{"default route over a foreign interface", 1000, []netlink.Route{{Table: 1000, LinkIndex: 2, Dst: def}}, false},
		{"default route with a gateway", 1000, []netlink.Route{{Table: 1000, LinkIndex: 1, Dst: def, Gw: via}}, false},
		{"non-default route over our interface", 1000, []netlink.Route{{Table: 1000, LinkIndex: 1, Dst: lan}}, false},
		{"ours next to a foreign route", 1000, []netlink.Route{{Table: 1000, LinkIndex: 1, Dst: def}, {Table: 1000, LinkIndex: 2, Dst: lan}}, false},
		{"foreign route in another table", 1000, []netlink.Route{{Table: 1001, LinkIndex: 2, Dst: def}}, true},
		{"below range", tableBase - 1, nil, false},
		{"above range", tableLimit, nil, false},
	}
	for _, tt := range tests {
		if got := ownedTable(tt.table, tt.routes, names); got != tt.want {
			t.Errorf("%s: ownedTable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTableBusy(t *testing.T) {
	routes := []netlink.Route{{Table: 1001}}
	rules := []netlink.Rule{shapeRule(6000, "192.0.2.1", 1002)}
	for table, want := range map[int]bool{1000: false, 1001: true, 1002: true} {
		if got := tableBusy(table, routes, rules); got != want {
			t.Errorf("tableBusy(%d) = %v, want %v", table, got, want)
		}
	}
}
