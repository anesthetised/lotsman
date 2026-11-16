//go:build linux

package linux

import (
	"net"
	"net/netip"
	"reflect"
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

// TestOursRejectsEveryExtraSelector sets, one at a time, each selector of
// netlink.Rule that Lotsman never uses, on each rule shape it installs.
func TestOursRejectsEveryExtraSelector(t *testing.T) {
	full := uint32(0xffffffff)
	shapes := []struct {
		name string
		rule func(edit ...func(*netlink.Rule)) netlink.Rule
	}{
		{"peer", func(edit ...func(*netlink.Rule)) netlink.Rule {
			return shapeRule(peerRulePriority, "10.77.0.5", 1003, edit...)
		}},
		{"probe", func(edit ...func(*netlink.Rule)) netlink.Rule {
			return shapeRule(probeRulePriority, "10.8.0.2", 1003, edit...)
		}},
		{"pin", func(edit ...func(*netlink.Rule)) netlink.Rule {
			pin := func(r *netlink.Rule) { r.Mark, r.Mask = 1003, &full }
			return shapeRule(pinRulePriority, "10.77.0.5", 1003, append([]func(*netlink.Rule){pin}, edit...)...)
		}},
	}
	extras := []struct {
		name string
		edit func(*netlink.Rule)
	}{
		{"dst", func(r *netlink.Rule) { r.Dst = &net.IPNet{IP: net.IPv4(1, 1, 1, 1).To4(), Mask: net.CIDRMask(32, 32)} }},
		{"iifname", func(r *netlink.Rule) { r.IifName = "eth0" }},
		{"tos", func(r *netlink.Rule) { r.Tos = 4 }},
		{"tun id", func(r *netlink.Rule) { r.TunID = 7 }},
		{"goto", func(r *netlink.Rule) { r.Goto = 6000 }},
		{"goto 0", func(r *netlink.Rule) { r.Goto = 0 }},
		{"flow", func(r *netlink.Rule) { r.Flow = 5 }},
		{"suppress ifgroup", func(r *netlink.Rule) { r.SuppressIfgroup = 1 }},
		{"suppress prefixlen", func(r *netlink.Rule) { r.SuppressPrefixlen = 0 }},
		{"invert", func(r *netlink.Rule) { r.Invert = true }},
		{"dport", func(r *netlink.Rule) { r.Dport = netlink.NewRulePortRange(443, 443) }},
		{"sport", func(r *netlink.Rule) { r.Sport = netlink.NewRulePortRange(1024, 2048) }},
		{"ip proto", func(r *netlink.Rule) { r.IPProto = 17 }},
		{"uid range", func(r *netlink.Rule) { r.UIDRange = netlink.NewRuleUIDRange(1000, 1000) }},
		{"protocol", func(r *netlink.Rule) { r.Protocol = 12 }},
		{"type", func(r *netlink.Rule) { r.Type = 6 }},
		{"family", func(r *netlink.Rule) { r.Family = netlink.FAMILY_V6 }},
	}
	for _, s := range shapes {
		if !ours(s.rule()) {
			t.Fatalf("%s: the plain rule is not ours", s.name)
		}
		for _, e := range extras {
			if ours(s.rule(e.edit)) {
				t.Errorf("%s with %s: ours = true, want false", s.name, e.name)
			}
		}
	}
	for _, s := range shapes[:2] {
		if ours(s.rule(func(r *netlink.Rule) { r.Mark = 1003 })) {
			t.Errorf("%s with a mark: ours = true, want false", s.name)
		}
	}
	if ours(shapes[2].rule(func(r *netlink.Rule) { r.OifName = "lm-up-a" })) {
		t.Error("pin with oif: ours = true, want false")
	}
}

// TestRuleFieldsAreAccountedFor fails when a netlink upgrade adds a Rule field:
// ours and noOtherSelectors must then decide whether Lotsman installs it.
func TestRuleFieldsAreAccountedFor(t *testing.T) {
	const accounted = 22 // 7 compared by ours + 15 rejected by noOtherSelectors
	if got := reflect.TypeOf(netlink.Rule{}).NumField(); got != accounted {
		t.Errorf("netlink.Rule has %d fields, ours accounts for %d: review ours and noOtherSelectors", got, accounted)
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
