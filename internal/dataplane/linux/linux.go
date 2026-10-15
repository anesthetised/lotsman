//go:build linux

// Package linux programs the kernel with policy routing and nftables.
//
// Every upstream gets its own routing table whose only route is a default
// route over that upstream's TUN. A peer is sent through an upstream by an
// "ip rule from <peer>/32 lookup <table>". An nftables table masquerades what
// leaves through an upstream, clamps TCP MSS to the route MTU, and drops any
// forwarded packet from the downstream interface unless its peer is authorized
// and it is going into an upstream. Authorization is revoked before route
// cleanup, so connected routes in main and stale policy rules cannot bypass it.
//
// Lotsman shares the host's rule and route space with other software, so it
// only touches what it can prove it created: a rule of exactly the shape it
// installs (see ours) pointing at a table in [tableBase, tableLimit) whose
// routes are all default routes over lm-up- interfaces (see ownedTable). A
// table that holds anything else is never used or cleaned. Rules and routes
// of that shape left by a crash are removed at Setup.
package linux

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

const (
	tableBase         = 1000 // routing tables of upstreams start here; a table id doubles as the flow mark
	tableLimit        = 1100 // upstream tables are picked from [tableBase, tableLimit)
	pinRulePriority   = 3000 // "from <peer>/32 fwmark <table> lookup <table>", flows opened before a move
	probeRulePriority = 4000 // "from <upstream addr> lookup <table>", for health probes
	peerRulePriority  = 5000 // "from <peer>/32 lookup <table>"
	nftTable          = "lotsman"
	upstreamSetName   = "upstreams"
	pinMapName        = "pinmark" // upstream interface → mark stamped on new flows leaving through it
	peerSetName       = "authorized_peers"
	upstreamPrefix    = "lm-up-" // every upstream interface starts with this
)

type Dataplane struct {
	down    string
	tables  map[string]int      // upstream interface → routing table
	alive   map[string]bool     // upstream interface → flows may stay pinned to it
	allowed map[netip.Addr]bool // peers admitted by the firewall, independent of routes
}

func New() *Dataplane {
	return &Dataplane{tables: map[string]int{}, alive: map[string]bool{}}
}

func (d *Dataplane) Setup(down dataplane.Interface, ups []dataplane.Interface, mtu int) error {
	if err := sysctl("net/ipv4/ip_forward", "1"); err != nil {
		return err
	}
	if err := configureLink(down, mtu); err != nil {
		return err
	}
	d.down = down.Name
	d.tables = map[string]int{}
	d.alive = map[string]bool{}
	if err := removeLeftovers(); err != nil {
		return err
	}
	for _, up := range ups {
		if err := d.addUpstream(up); err != nil {
			return err
		}
	}
	return d.installFirewall(down.Name, ups)
}

func (d *Dataplane) SetUpstreams(ups []dataplane.Interface, mtu int) error {
	link, err := netlink.LinkByName(d.down)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("mtu on %s: %w", d.down, err)
	}
	keep := map[string]bool{}
	for _, up := range ups {
		keep[up.Name] = true
		// Re-applied even for known names: the interface may have been
		// recreated, which loses its address and the route that pointed at it.
		if err := d.addUpstream(up); err != nil {
			return err
		}
	}
	for name := range d.tables {
		if !keep[name] {
			if err := d.removeUpstream(name); err != nil {
				return err
			}
		}
	}
	return d.setUpstreamSets(ups)
}

// addUpstream gives the interface an address, a routing table with a default
// route over it, and the rule that sends probes from its address into that
// table. It is idempotent and keeps the table of an interface it already knows.
func (d *Dataplane) addUpstream(up dataplane.Interface) error {
	if err := configureLink(up, 0); err != nil {
		return err
	}
	table, known := d.tables[up.Name]
	if !known {
		var err error
		if table, err = d.allocateTable(); err != nil {
			return fmt.Errorf("table for %s: %w", up.Name, err)
		}
		d.tables[up.Name] = table
	}
	link, err := netlink.LinkByName(up.Name)
	if err != nil {
		return err
	}
	if err := addDefaultRoute(link.Attrs().Index, table); err != nil {
		return fmt.Errorf("default route for %s in table %d: %w", up.Name, table, err)
	}
	return addRule(up.Addr.Addr(), table, probeRulePriority)
}

// addDefaultRoute adds the table's only route. It never replaces: an existing
// route is accepted only if it is already exactly this one.
func addDefaultRoute(linkIndex, table int) error {
	err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: linkIndex,
		Table:     table,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Scope:     netlink.SCOPE_LINK,
	})
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	routes, listErr := rangeRoutes(table)
	if listErr != nil {
		return listErr
	}
	if len(routes) == 1 && routes[0].LinkIndex == linkIndex && isDefault(routes[0]) {
		return nil
	}
	return fmt.Errorf("table already holds a different route: %w", err)
}

func (d *Dataplane) removeUpstream(name string) error {
	table := d.tables[name]
	rules, err := d.ownRules()
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Table == table {
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("delete rule for %s: %w", name, err)
			}
		}
	}
	if err := flushTable(table); err != nil {
		return err
	}
	// Forgotten only once clean, so a failed removal is retried by the next reload.
	delete(d.tables, name)
	delete(d.alive, name)
	return nil
}

// allocateTable returns the lowest table id in range that no one uses: it has
// no route and no rule of any kind, so nothing foreign is ever adopted. Ids
// stay stable for upstreams that remain across a reload.
func (d *Dataplane) allocateTable() (int, error) {
	routes, err := rangeRoutes(0)
	if err != nil {
		return 0, err
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return 0, err
	}
	for t := tableBase; t < tableLimit; t++ {
		if interfaceOfTable(d.tables, t) == "" && !tableBusy(t, routes, rules) {
			return t, nil
		}
	}
	return 0, fmt.Errorf("no free routing table in %d-%d: all hold other routes or rules", tableBase, tableLimit-1)
}

// tableBusy reports whether any route or rule refers to the table.
func tableBusy(table int, routes []netlink.Route, rules []netlink.Rule) bool {
	for _, r := range routes {
		if r.Table == table {
			return true
		}
	}
	for _, r := range rules {
		if r.Table == table {
			return true
		}
	}
	return false
}

// rangeRoutes lists IPv4 routes of the tables in [tableBase, tableLimit), or
// only of one table when it is nonzero.
func rangeRoutes(table int) ([]netlink.Route, error) {
	var out []netlink.Route
	err := netlink.RouteListFilteredIter(netlink.FAMILY_V4, &netlink.Route{}, netlink.RT_FILTER_TABLE, func(r netlink.Route) bool {
		if r.Table >= tableBase && r.Table < tableLimit && (table == 0 || r.Table == table) {
			out = append(out, r)
		}
		return true
	})
	return out, err
}

func linkNames() (map[int]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(links))
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	return names, nil
}

func isDefault(r netlink.Route) bool {
	if r.Dst == nil {
		return true
	}
	ones, _ := r.Dst.Mask.Size()
	return ones == 0
}

// ourRoute reports whether r has the shape of the route addDefaultRoute installs:
// a default route with no gateway over an lm-up- interface.
func ourRoute(r netlink.Route, names map[int]string) bool {
	return isDefault(r) && len(r.Gw) == 0 && strings.HasPrefix(names[r.LinkIndex], upstreamPrefix)
}

// ownedTable reports whether table is one Lotsman may clean: in range, and
// every route in it is ours (an empty table qualifies, e.g. after a crash took
// the interface and with it the route).
func ownedTable(table int, routes []netlink.Route, names map[int]string) bool {
	if table < tableBase || table >= tableLimit {
		return false
	}
	for _, r := range routes {
		if r.Table == table && !ourRoute(r, names) {
			return false
		}
	}
	return true
}

// ours reports whether r has exactly the shape of a rule Lotsman installs, in a
// table of its range. Any extra selector (iif, dport, goto, ...) makes it foreign.
//
//	pin:   priority 3000, from <peer>/32 fwmark <table>/0xffffffff lookup <table>
//	probe: priority 4000, from <upstream addr>/32 lookup <table>
//	peer:  priority 5000, from <peer>/32 lookup <table>
func ours(r netlink.Rule) bool {
	if r.Table < tableBase || r.Table >= tableLimit || r.Src == nil {
		return false
	}
	if ones, bits := r.Src.Mask.Size(); bits != 32 || ones != 32 {
		return false
	}
	want := netlink.NewRule()
	want.Family, want.Src, want.Table, want.Priority = netlink.FAMILY_V4, r.Src, r.Table, r.Priority
	switch r.Priority {
	case pinRulePriority:
		mask := uint32(0xffffffff)
		want.Mark, want.Mask = uint32(r.Table), &mask
	case probeRulePriority, peerRulePriority:
	default:
		return false
	}
	return reflect.DeepEqual(r, *want)
}

// ownRules lists our rules that point at the tables this instance allocated.
func (d *Dataplane) ownRules() ([]netlink.Rule, error) {
	all, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []netlink.Rule
	for _, r := range all {
		if ours(r) && interfaceOfTable(d.tables, r.Table) != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

// removeLeftovers deletes what an earlier run left behind: rules of our shape
// and routes in tables that pass ownedTable. Everything else stays.
func removeLeftovers() error {
	routes, err := rangeRoutes(0)
	if err != nil {
		return err
	}
	names, err := linkNames()
	if err != nil {
		return err
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if ours(r) && ownedTable(r.Table, routes, names) {
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("delete leftover %v: %w", r, err)
			}
		}
	}
	for _, r := range routes {
		if ownedTable(r.Table, routes, names) {
			if err := netlink.RouteDel(&r); err != nil {
				return fmt.Errorf("delete leftover route in table %d: %w", r.Table, err)
			}
		}
	}
	return nil
}

// flushTable deletes the routes of the table that are ours.
func flushTable(table int) error {
	routes, err := rangeRoutes(table)
	if err != nil {
		return err
	}
	names, err := linkNames()
	if err != nil {
		return err
	}
	for _, r := range routes {
		if ourRoute(r, names) {
			if err := netlink.RouteDel(&r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dataplane) Route(peer netip.Addr, upstream string) error {
	table, ok := d.tables[upstream]
	if !ok {
		return fmt.Errorf("unknown upstream %q", upstream)
	}
	rules, err := d.rulesFor(peer)
	if err != nil {
		return err
	}
	var old []netlink.Rule
	for _, r := range rules {
		switch {
		case r.Priority == peerRulePriority && r.Table == table:
			return d.authorize(peer, true)
		case r.Priority == peerRulePriority:
			old = append(old, r)
		case r.Priority == pinRulePriority && r.Table == table:
			// Flows pinned to the upstream we are moving back to no longer need a pin.
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("remove pin for %s: %w", peer, err)
			}
		}
	}
	for _, r := range old {
		if d.alive[interfaceOfTable(d.tables, r.Table)] {
			if err := addPinRule(peer, r.Table); err != nil {
				return err
			}
		}
	}
	if err := addRule(peer, table, peerRulePriority); err != nil {
		return err
	}
	for _, r := range old {
		if err := netlink.RuleDel(&r); err != nil {
			return fmt.Errorf("remove old rule for %s: %w", peer, err)
		}
	}
	return d.authorize(peer, true)
}

func (d *Dataplane) Unroute(peer netip.Addr) error {
	return d.unroute(peer, netlink.RuleDel)
}

func (d *Dataplane) unroute(peer netip.Addr, deleteRule func(*netlink.Rule) error) error {
	// Revoke before touching routes: even a failed cleanup must not leave a
	// peer authorized by an old policy rule or a connected route in main.
	if err := d.authorize(peer, false); err != nil {
		return err
	}
	rules, err := d.rulesFor(peer)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if err := deleteRule(&r); err != nil {
			return fmt.Errorf("remove rule for %s: %w", peer, err)
		}
	}
	return nil
}

func (d *Dataplane) authorize(peer netip.Addr, allowed bool) error {
	if d.allowed[peer] == allowed {
		return nil
	}
	c := &nftables.Conn{}
	set := &nftables.Set{Table: &nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable}, Name: peerSetName}
	elements := []nftables.SetElement{{Key: peer.AsSlice()}}
	var err error
	if allowed {
		err = c.SetAddElements(set, elements)
	} else {
		err = c.SetDeleteElements(set, elements)
	}
	if err == nil {
		err = c.Flush()
	}
	if err != nil {
		return fmt.Errorf("authorize %s=%v: %w", peer, allowed, err)
	}
	if allowed {
		d.allowed[peer] = true
	} else {
		delete(d.allowed, peer)
	}
	return nil
}

func (d *Dataplane) SetAlive(upstream string, alive bool) error {
	table, ok := d.tables[upstream]
	if !ok {
		return fmt.Errorf("unknown upstream %q", upstream)
	}
	d.alive[upstream] = alive
	if alive {
		return nil
	}
	all, err := d.ownRules()
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Priority == pinRulePriority && r.Table == table {
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("remove pin to %s: %w", upstream, err)
			}
		}
	}
	return nil
}

func interfaceOfTable(tables map[string]int, table int) string {
	for name, t := range tables {
		if t == table {
			return name
		}
	}
	return ""
}

func (d *Dataplane) Teardown() error {
	rules, err := d.ownRules()
	for _, r := range rules {
		if delErr := netlink.RuleDel(&r); delErr != nil {
			err = errors.Join(err, fmt.Errorf("delete rule %v: %w", r, delErr))
		}
	}
	for _, table := range d.tables {
		err = errors.Join(err, flushTable(table))
	}
	c := &nftables.Conn{}
	err = errors.Join(err, closeForeignForwardChains(c))
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable})
	if flushErr := c.Flush(); flushErr != nil && !errors.Is(flushErr, unix.ENOENT) {
		err = errors.Join(err, flushErr)
	}
	return err
}

func configureLink(iface dataplane.Interface, mtu int) error {
	link, err := netlink.LinkByName(iface.Name)
	if err != nil {
		return fmt.Errorf("interface %s: %w", iface.Name, err)
	}
	addr, err := netlink.ParseAddr(iface.Addr.String())
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("address %s on %s: %w", iface.Addr, iface.Name, err)
	}
	if mtu != 0 {
		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return fmt.Errorf("mtu on %s: %w", iface.Name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("up %s: %w", iface.Name, err)
	}
	// Replies from the internet arrive on an upstream TUN while the main table
	// would route their source via the host uplink; strict reverse-path
	// filtering would drop them. Loose mode accepts any reachable source.
	return sysctl("net/ipv4/conf/"+iface.Name+"/rp_filter", "2")
}

func addRule(src netip.Addr, table, priority int) error {
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	r.Src = hostNet(src)
	r.Table = table
	r.Priority = priority
	if err := netlink.RuleAdd(r); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("rule from %s lookup %d: %w", src, table, err)
	}
	return nil
}

// addPinRule keeps flows the peer opened through table's upstream on it:
// "from <peer> fwmark <table> lookup <table>", ahead of the peer's plain rule.
func addPinRule(peer netip.Addr, table int) error {
	r := netlink.NewRule()
	r.Family = netlink.FAMILY_V4
	r.Src = hostNet(peer)
	r.Mark = uint32(table)
	mask := uint32(0xffffffff)
	r.Mask = &mask
	r.Table = table
	r.Priority = pinRulePriority
	if err := netlink.RuleAdd(r); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("pin rule from %s fwmark %d: %w", peer, table, err)
	}
	return nil
}

// rulesFor lists the peer's plain and pin rules.
func (d *Dataplane) rulesFor(peer netip.Addr) ([]netlink.Rule, error) {
	all, err := d.ownRules()
	if err != nil {
		return nil, err
	}
	var out []netlink.Rule
	for _, r := range all {
		if (r.Priority == peerRulePriority || r.Priority == pinRulePriority) && r.Src.IP.Equal(peer.AsSlice()) {
			out = append(out, r)
		}
	}
	return out, nil
}

func hostNet(a netip.Addr) *net.IPNet {
	return &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(a.BitLen(), a.BitLen())}
}

// sysctl sets a kernel parameter unless it already has that value, so a
// container with a read-only /proc/sys still works when the operator presets it.
func sysctl(key, value string) error {
	path := filepath.Join("/proc/sys", key)
	if current, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(current)) == value {
		return nil
	}
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		return fmt.Errorf("sysctl %s=%s: %w", key, value, err)
	}
	return nil
}

func (d *Dataplane) installFirewall(down string, ups []dataplane.Interface) error {
	c := &nftables.Conn{}
	if err := closeForeignForwardChains(c); err != nil {
		return err
	}
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable})
	if err := c.Flush(); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove old nftables table: %w", err)
	}

	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable})
	peers := &nftables.Set{Table: table, Name: peerSetName, KeyType: nftables.TypeIPAddr}
	if err := c.AddSet(peers, nil); err != nil {
		return err
	}
	upSet := &nftables.Set{Table: table, Name: upstreamSetName, KeyType: nftables.TypeIFName, KeyByteOrder: binaryutil.NativeEndian}
	if err := c.AddSet(upSet, d.upstreamElements(ups)); err != nil {
		return err
	}
	pinMap := &nftables.Set{Table: table, Name: pinMapName, IsMap: true,
		KeyType: nftables.TypeIFName, KeyByteOrder: binaryutil.NativeEndian, DataType: nftables.TypeMark}
	if err := c.AddSet(pinMap, d.pinElements(ups)); err != nil {
		return err
	}

	accept := nftables.ChainPolicyAccept
	forward := c.AddChain(&nftables.Chain{
		Name: "forward", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &accept,
	})
	postrouting := c.AddChain(&nftables.Chain{
		Name: "postrouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource, Policy: &accept,
	})
	// Flow pinning: a new flow is stamped with the table it first left
	// through; later packets carry that mark into routing, where a pin rule
	// may send them back to the same upstream after the peer has moved.
	restore := c.AddChain(&nftables.Chain{
		Name: "restore_mark", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityMangle, Policy: &accept,
	})
	stamp := c.AddChain(&nftables.Chain{
		Name: "stamp_mark", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityMangle, Policy: &accept,
	})

	iifIs := func(name string) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)},
		}
	}
	iifInUps := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Lookup{SourceRegister: 1, SetName: upSet.Name, SetID: upSet.ID},
	}
	oifIs := func(name string) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(name)},
		}
	}
	oifInUps := []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Lookup{SourceRegister: 1, SetName: upSet.Name, SetID: upSet.ID},
	}
	rule := func(chain *nftables.Chain, parts ...[]expr.Any) {
		var exprs []expr.Any
		for _, p := range parts {
			exprs = append(exprs, p...)
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: exprs})
	}

	rule(forward, iifIs(down), oifInUps, tcpSyn, clampMSS)
	peerInSet := func(offset uint32) []expr.Any {
		return []expr.Any{
			&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
			&expr.Lookup{SourceRegister: 1, SetName: peers.Name, SetID: peers.ID},
		}
	}
	rule(forward, iifIs(down), oifInUps, peerInSet(12), accepting)                // IPv4 source
	rule(forward, iifInUps, oifIs(down), peerInSet(16), ctEstablished, accepting) // IPv4 destination
	rule(forward, iifIs(down), dropping)
	rule(forward, iifInUps, dropping)
	rule(postrouting, oifInUps, []expr.Any{&expr.Masq{}})
	// iifname lm0 ct mark != 0 meta mark set ct mark
	rule(restore, iifIs(down), []expr.Any{
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Meta{Key: expr.MetaKeyMARK, Register: 1, SourceRegister: true},
	})
	// iifname lm0 ct mark 0 ct mark set oifname map @pinmark
	rule(stamp, iifIs(down), []expr.Any{
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0, 0, 0, 0}},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Lookup{SourceRegister: 1, DestRegister: 1, IsDestRegSet: true, SetName: pinMap.Name, SetID: pinMap.ID},
		&expr.Ct{Key: expr.CtKeyMARK, Register: 1, SourceRegister: true},
	})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("install nftables rules: %w", err)
	}
	d.allowed = map[netip.Addr]bool{}
	return openForeignForwardChains(c, down)
}

// setUpstreamSets replaces the members of the "upstreams" set and the
// "pinmark" map in one transaction; the rules referencing them are untouched.
func (d *Dataplane) setUpstreamSets(ups []dataplane.Interface) error {
	c := &nftables.Conn{}
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable}
	set, err := c.GetSetByName(table, upstreamSetName)
	if err != nil {
		return fmt.Errorf("upstreams set: %w", err)
	}
	pins, err := c.GetSetByName(table, pinMapName)
	if err != nil {
		return fmt.Errorf("pinmark map: %w", err)
	}
	c.FlushSet(set)
	c.FlushSet(pins)
	if len(ups) > 0 {
		if err := c.SetAddElements(set, d.upstreamElements(ups)); err != nil {
			return err
		}
		if err := c.SetAddElements(pins, d.pinElements(ups)); err != nil {
			return err
		}
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("update upstream sets: %w", err)
	}
	return nil
}

func (d *Dataplane) upstreamElements(ups []dataplane.Interface) []nftables.SetElement {
	var elems []nftables.SetElement
	for _, up := range ups {
		elems = append(elems, nftables.SetElement{Key: ifname(up.Name)})
	}
	return elems
}

func (d *Dataplane) pinElements(ups []dataplane.Interface) []nftables.SetElement {
	var elems []nftables.SetElement
	for _, up := range ups {
		elems = append(elems, nftables.SetElement{Key: ifname(up.Name), Val: binaryutil.NativeEndian.PutUint32(uint32(d.tables[up.Name]))})
	}
	return elems
}

// foreignRuleComment marks the rules Lotsman inserts into chains it does not own.
const foreignRuleComment = "lotsman"

// openForeignForwardChains inserts accept rules at the top of every other
// forward base chain. Docker sets "iptables -P FORWARD DROP" and ufw adds
// its own drops; a drop in any chain on the hook wins over our accept, so the
// forwarded traffic must be accepted there too. Interface names are matched
// by prefix because sets cannot be shared across tables.
func openForeignForwardChains(c *nftables.Conn, down string) error {
	chains, err := foreignForwardChains(c)
	if err != nil {
		return err
	}
	comment := userdata.AppendString(nil, userdata.TypeComment, foreignRuleComment)
	for _, ch := range chains {
		oifUp := []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte(upstreamPrefix)},
		}
		iifUp := []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte(upstreamPrefix)},
		}
		iifDown := []expr.Any{
			&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(down)},
		}
		oifDown := []expr.Any{
			&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(down)},
		}
		// Insert in reverse so the final order is: downstream→upstream, then replies.
		c.InsertRule(&nftables.Rule{Table: ch.Table, Chain: ch, UserData: comment,
			Exprs: concat(iifUp, oifDown, ctEstablished, accepting)})
		c.InsertRule(&nftables.Rule{Table: ch.Table, Chain: ch, UserData: comment,
			Exprs: concat(iifDown, oifUp, accepting)})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("open foreign forward chains: %w", err)
	}
	return nil
}

func closeForeignForwardChains(c *nftables.Conn) error {
	chains, err := foreignForwardChains(c)
	if err != nil {
		return err
	}
	for _, ch := range chains {
		rules, err := c.GetRules(ch.Table, ch)
		if err != nil {
			return err
		}
		for _, r := range rules {
			if comment, ok := userdata.GetString(r.UserData, userdata.TypeComment); ok && comment == foreignRuleComment {
				if err := c.DelRule(r); err != nil {
					return err
				}
			}
		}
	}
	return c.Flush()
}

// foreignForwardChains lists IPv4-capable base chains on the forward hook that Lotsman does not own.
func foreignForwardChains(c *nftables.Conn) ([]*nftables.Chain, error) {
	all, err := c.ListChains()
	if err != nil {
		return nil, fmt.Errorf("list nftables chains: %w", err)
	}
	var out []*nftables.Chain
	for _, ch := range all {
		if ch.Table.Name == nftTable || ch.Hooknum == nil || *ch.Hooknum != *nftables.ChainHookForward {
			continue
		}
		if ch.Table.Family == nftables.TableFamilyIPv4 || ch.Table.Family == nftables.TableFamilyINet {
			out = append(out, ch)
		}
	}
	return out, nil
}

func concat(parts ...[]expr.Any) []expr.Any {
	var out []expr.Any
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

var (
	accepting = []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	dropping  = []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}

	// tcp flags syn
	tcpSyn = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
		&expr.Bitwise{DestRegister: 1, SourceRegister: 1, Len: 1, Mask: []byte{0x02}, Xor: []byte{0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00}},
	}
	// tcp option maxseg size set rt mtu
	clampMSS = []expr.Any{
		&expr.Rt{Register: 1, Key: expr.RtTCPMSS},
		&expr.Byteorder{DestRegister: 1, SourceRegister: 1, Op: expr.ByteorderHton, Len: 2, Size: 2},
		&expr.Exthdr{SourceRegister: 1, Type: unix.TCPOPT_MAXSEG, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
	}
	// ct state established,related
	ctEstablished = []expr.Any{
		&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{
			DestRegister: 1, SourceRegister: 1, Len: 4,
			Mask: binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:  binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
	}
)

// ifname is an interface name as nftables stores it: zero-padded to IFNAMSIZ.
func ifname(n string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, n)
	return b
}
