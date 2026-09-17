//go:build linux

// Package linux programs the kernel with policy routing and nftables.
//
// Every upstream gets its own routing table whose only route is a default
// route over that upstream's TUN. A peer is sent through an upstream by an
// "ip rule from <peer>/32 lookup <table>". An nftables table masquerades what
// leaves through an upstream, clamps TCP MSS to the route MTU, and drops any
// forwarded packet from the downstream interface that is not going into an
// upstream, so a peer without a rule loses connectivity instead of leaking
// through the host's own uplink.
package linux

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
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
	tableBase         = 1000 // routing table of upstream i is tableBase+i
	probeRulePriority = 4000 // "from <upstream addr> lookup <table>", for health probes
	peerRulePriority  = 5000 // "from <peer>/32 lookup <table>"
	nftTable          = "lotsman"
	upstreamSetName   = "upstreams"
	upstreamPrefix    = "lm-up-" // every upstream interface starts with this
)

type Dataplane struct {
	down   string
	tables map[string]int // upstream interface → routing table
}

func New() *Dataplane {
	return &Dataplane{tables: map[string]int{}}
}

func (d *Dataplane) Setup(down dataplane.Interface, ups []dataplane.Interface, mtu int) error {
	if err := sysctl("net/ipv4/ip_forward", "1"); err != nil {
		return err
	}
	if err := configureLink(down, mtu); err != nil {
		return err
	}
	d.down = down.Name
	if err := deleteRules(probeRulePriority, peerRulePriority); err != nil {
		return err
	}
	d.tables = map[string]int{}
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
		if _, exists := d.tables[up.Name]; !exists {
			if err := d.addUpstream(up); err != nil {
				return err
			}
		}
	}
	for name := range d.tables {
		if !keep[name] {
			if err := d.removeUpstream(name); err != nil {
				return err
			}
		}
	}
	return d.setUpstreamSet(ups)
}

// addUpstream gives the interface an address, a routing table with a default
// route over it, and the rule that sends probes from its address into that table.
func (d *Dataplane) addUpstream(up dataplane.Interface) error {
	if err := configureLink(up, 0); err != nil {
		return err
	}
	table := d.allocateTable()
	d.tables[up.Name] = table
	link, err := netlink.LinkByName(up.Name)
	if err != nil {
		return err
	}
	err = netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Table:     table,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		Scope:     netlink.SCOPE_LINK,
	})
	if err != nil {
		return fmt.Errorf("default route for %s in table %d: %w", up.Name, table, err)
	}
	return addRule(up.Addr.Addr(), table, probeRulePriority)
}

func (d *Dataplane) removeUpstream(name string) error {
	table := d.tables[name]
	delete(d.tables, name)
	all, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Table == table && (r.Priority == probeRulePriority || r.Priority == peerRulePriority) {
			if err := netlink.RuleDel(&r); err != nil {
				return fmt.Errorf("delete rule for %s: %w", name, err)
			}
		}
	}
	return flushTable(table)
}

// allocateTable returns the lowest table id not in use, so ids are stable
// for upstreams that stay across a reload.
func (d *Dataplane) allocateTable() int {
	used := map[int]bool{}
	for _, t := range d.tables {
		used[t] = true
	}
	for t := tableBase; ; t++ {
		if !used[t] {
			return t
		}
	}
}

func flushTable(table int) error {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	for _, r := range routes {
		if err := netlink.RouteDel(&r); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dataplane) Route(peer netip.Addr, upstream string) error {
	table, ok := d.tables[upstream]
	if !ok {
		return fmt.Errorf("unknown upstream %q", upstream)
	}
	old, err := rulesFor(peer)
	if err != nil {
		return err
	}
	for _, r := range old {
		if r.Table == table {
			return nil
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
	return nil
}

func (d *Dataplane) Unroute(peer netip.Addr) error {
	rules, err := rulesFor(peer)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if err := netlink.RuleDel(&r); err != nil {
			return fmt.Errorf("remove rule for %s: %w", peer, err)
		}
	}
	return nil
}

func (d *Dataplane) Teardown() error {
	err := deleteRules(probeRulePriority, peerRulePriority)
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

func rulesFor(peer netip.Addr) ([]netlink.Rule, error) {
	all, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []netlink.Rule
	for _, r := range all {
		if r.Priority == peerRulePriority && r.Src != nil && r.Src.IP.Equal(peer.AsSlice()) {
			out = append(out, r)
		}
	}
	return out, nil
}

func deleteRules(priorities ...int) error {
	all, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range all {
		for _, p := range priorities {
			if r.Priority == p {
				if err := netlink.RuleDel(&r); err != nil {
					return fmt.Errorf("delete rule %v: %w", r, err)
				}
			}
		}
	}
	return nil
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
	upSet := &nftables.Set{Table: table, Name: upstreamSetName, KeyType: nftables.TypeIFName, KeyByteOrder: binaryutil.NativeEndian}
	var elems []nftables.SetElement
	for _, up := range ups {
		elems = append(elems, nftables.SetElement{Key: ifname(up.Name)})
	}
	if err := c.AddSet(upSet, elems); err != nil {
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
	rule(forward, iifIs(down), oifInUps, accepting)
	rule(forward, iifInUps, oifIs(down), ctEstablished, accepting)
	rule(forward, iifIs(down), dropping)
	rule(forward, iifInUps, dropping)
	rule(postrouting, oifInUps, []expr.Any{&expr.Masq{}})

	if err := c.Flush(); err != nil {
		return fmt.Errorf("install nftables rules: %w", err)
	}
	return openForeignForwardChains(c, down)
}

// setUpstreamSet replaces the members of the "upstreams" set in one
// transaction; the rules referencing the set are untouched.
func (d *Dataplane) setUpstreamSet(ups []dataplane.Interface) error {
	c := &nftables.Conn{}
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: nftTable}
	set, err := c.GetSetByName(table, upstreamSetName)
	if err != nil {
		return fmt.Errorf("upstreams set: %w", err)
	}
	c.FlushSet(set)
	var elems []nftables.SetElement
	for _, up := range ups {
		elems = append(elems, nftables.SetElement{Key: ifname(up.Name)})
	}
	if len(elems) > 0 {
		if err := c.SetAddElements(set, elems); err != nil {
			return err
		}
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("update upstreams set: %w", err)
	}
	return nil
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
