package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// Upstream is one provider: its config, the link that carries its traffic
// and its health.
type Upstream struct {
	config.Upstream
	Conf     *awgconf.Config // kind awg: the provider config, its endpoint resolved by Resolve
	Key      proxy.Key       // proxy kinds: the access key
	Source   string          // the provider file as loaded, to detect edits on reload
	Endpoint string          // the server as written, possibly a hostname
	Resolved string          // Endpoint's current address, set by Resolve
	Addr     netip.Prefix    // the TUN's IPv4 address, also the probe source
	MTU      int
	Tracker  *health.Tracker
	link     link // set by open
}

// link carries an upstream's traffic on its TUN: an AmneziaWG device or a
// proxy stack.
type link interface {
	Up() error
	Close()
	// SetEndpoint points the link at a new address of its server.
	SetEndpoint(resolved string) error
	Stats() linkStats
}

// linkStats holds what a link knows; each field is nil when it does not apply.
type linkStats struct {
	Peer  *tunnel.PeerStatus // AWG: the provider peer
	Flows *proxy.Stats       // proxy: flow counters
}

// LoadUpstream reads and validates a provider config or access key without
// touching the network: the endpoint stays as written until Resolve. The
// link is started later by open.
func LoadUpstream(u config.Upstream, h config.Health) (*Upstream, error) {
	source, err := os.ReadFile(u.Conf)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	var up *Upstream
	if u.Proxy() {
		up, err = loadProxy(source)
	} else {
		up, err = loadAWG(source)
	}
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	up.Upstream = u
	up.Source = string(source)
	up.Tracker = health.NewTracker(h.DownAfter, h.UpAfter)
	return up, nil
}

// loadProxy reads an access key. The key is a secret, so errors never quote
// the file.
func loadProxy(source []byte) (*Upstream, error) {
	key, err := proxy.ParseKey(string(source))
	if err != nil {
		return nil, err
	}
	// Nothing on the TUN adds overhead: the stack terminates every flow.
	return &Upstream{Key: key, Endpoint: key.Server(), Addr: config.ProxyAddress, MTU: mtu.Ethernet}, nil
}

func loadAWG(source []byte) (*Upstream, error) {
	conf, err := awgconf.Parse(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	if len(conf.Peers) != 1 {
		return nil, fmt.Errorf("want exactly one [Peer], got %d", len(conf.Peers))
	}
	peer := &conf.Peers[0]
	if !routesEverything(peer.AllowedIPs) {
		return nil, fmt.Errorf("[Peer] AllowedIPs must include 0.0.0.0/0")
	}
	if _, _, err := splitEndpoint(peer.Endpoint); err != nil {
		return nil, err
	}
	// Ordinary UDP payload limits, also used by the pinned AWG StdNetBind.
	// Hostnames resolve to IPv4 only, so only a literal IPv6 endpoint gets
	// the larger limit.
	payloadMax := 65507
	if remote, err := netip.ParseAddrPort(peer.Endpoint); err == nil && remote.Addr().Is6() && !remote.Addr().Is4In6() {
		payloadMax = 65527
	}
	if p := conf.Interface.Params; p.Jc > 0 {
		// Parse has already checked ordering; the upper bound is exclusive
		// except for a fixed-size range.
		largest := p.Jmax
		if p.Jmin < p.Jmax {
			largest--
		}
		if largest > uint32(payloadMax) {
			return nil, fmt.Errorf("[Interface] junk packet size must not exceed %d for endpoint %s", payloadMax, peer.Endpoint)
		}
	}
	if err := conf.Interface.Params.CheckWireSizes(payloadMax); err != nil {
		return nil, fmt.Errorf("%w for endpoint %s", err, peer.Endpoint)
	}
	addr, ok := firstIPv4(conf.Interface.Addresses)
	if !ok {
		return nil, fmt.Errorf("[Interface] Address has no IPv4")
	}
	m := conf.Interface.MTU
	if m == 0 {
		m = mtu.DefaultWireGuard
	}
	// Linux TUN needs at least 68 bytes, including for the client MTU after
	// subtracting S4 and ContentPaddingAddition. Compare thresholds to avoid
	// overflowing on a huge MTU.
	s4 := int(conf.Interface.S4)
	padding := conf.Interface.ContentPaddingAddition
	if padding.IsZero() && m < 68+s4 {
		return nil, fmt.Errorf("[Interface] MTU minus S4 must be at least 68 (MTU %d, S4 %d)", m, s4)
	}
	if int64(m) < 68+int64(s4)+int64(padding.Hi) {
		return nil, fmt.Errorf("[Interface] MTU minus S4 and ContentPaddingAddition must be at least 68 (MTU %d, S4 %d, ContentPaddingAddition %s)",
			m, s4, padding)
	}
	// AWG reserves a 16-byte transport header and 16-byte authentication tag.
	// Its default content padding clamps to the positive TUN MTU, without
	// rounding past it, so no additional alignment allowance is needed.
	// ContentPaddingAddition and RandomTrailers keep a message below the
	// runtime's UDP window: 500 bytes or the largest datagram already
	// exchanged with the provider, which fits payloadMax.
	if m > payloadMax-32-s4 {
		return nil, fmt.Errorf("[Interface] MTU plus S4 and transport overhead exceeds UDP payload limit %d for endpoint %s", payloadMax, peer.Endpoint)
	}
	return &Upstream{Conf: conf, Endpoint: peer.Endpoint, Addr: addr, MTU: m}, nil
}

// MTULimit is what the upstream contributes to the client MTU.
func (u *Upstream) MTULimit() mtu.Upstream {
	if u.Conf == nil {
		return mtu.Upstream{MTU: u.MTU}
	}
	iface := u.Conf.Interface
	return mtu.Upstream{MTU: u.MTU, S4: iface.S4, ContentPadding: iface.ContentPaddingAddition.Hi}
}

func routesEverything(prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Addr().Is4() && p.Bits() == 0 {
			return true
		}
	}
	return false
}

func firstIPv4(prefixes []netip.Prefix) (netip.Prefix, bool) {
	for _, p := range prefixes {
		if p.Addr().Is4() {
			return p, true
		}
	}
	return netip.Prefix{}, false
}

// Resolve finds the endpoint's current address: amneziawg-go's UAPI takes
// addresses only, and a proxy stack must not look up its server per flow.
func (u *Upstream) Resolve(ctx context.Context) error {
	resolved, err := resolveEndpoint(ctx, u.Endpoint)
	if err != nil {
		return fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	u.setResolved(resolved)
	return nil
}

func (u *Upstream) setResolved(resolved string) {
	u.Resolved = resolved
	if u.Conf != nil {
		u.Conf.Peers[0].Endpoint = resolved
	}
}

// open starts the upstream's link on t, which it owns from then on. The
// link stays down until Up. The endpoint must be resolved.
func (u *Upstream) open(t tun.Device, log *slog.Logger) error {
	if u.Key == nil {
		dev := tunnel.New(u.InterfaceName(), t, log)
		if err := dev.Configure(u.Conf.UAPI()); err != nil {
			dev.Close()
			return err
		}
		u.link = &awgLink{dev: dev, peer: u.Conf.Peers[0].PublicKey}
		return nil
	}
	l := &proxyLink{key: u.Key}
	d, err := l.dialers(u.Resolved)
	if err != nil {
		t.Close()
		return fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	// New owns t and d from here on, also when it fails.
	l.stack, err = proxy.New(t, d, proxy.Options{MaxFlows: u.MaxFlows}, log.With("upstream", u.Name))
	if err != nil {
		return fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	u.link = l
	return nil
}

// SetEndpoint points the running link at a freshly resolved endpoint if the
// server moved to another address, and returns whether it changed. The
// caller resolves u.Endpoint first; it is fixed at load, so that needs no
// lock. Literal IP endpoints resolve to themselves and never change.
func (u *Upstream) SetEndpoint(resolved string) (bool, error) {
	if resolved == u.Resolved {
		return false, nil
	}
	if err := u.link.SetEndpoint(resolved); err != nil {
		return false, err
	}
	u.setResolved(resolved)
	return true, nil
}

// awgLink is an AmneziaWG device with one peer, the provider.
type awgLink struct {
	dev  *tunnel.Device
	peer awgconf.Key
}

func (l *awgLink) Up() error { return l.dev.Up() }
func (l *awgLink) Close()    { l.dev.Close() }

func (l *awgLink) SetEndpoint(resolved string) error {
	return l.dev.Configure("public_key=" + l.peer.Hex() + "\nupdate_only=true\nendpoint=" + resolved + "\n")
}

func (l *awgLink) Stats() linkStats {
	if peers, err := l.dev.Peers(); err == nil && len(peers) == 1 {
		return linkStats{Peer: &peers[0]}
	}
	return linkStats{}
}

// proxyLink is a proxy stack. It carries traffic from the start: the TUN
// passes nothing before the dataplane sets it up and routes to it.
type proxyLink struct {
	key   proxy.Key
	stack *proxy.Stack
}

func (l *proxyLink) Up() error { return nil }
func (l *proxyLink) Close()    { l.stack.Close() }

func (l *proxyLink) dialers(resolved string) (proxy.Dialers, error) {
	addr, err := netip.ParseAddrPort(resolved)
	if err != nil {
		return proxy.Dialers{}, fmt.Errorf("server address %q: %w", resolved, err)
	}
	return l.key.Dialers(addr)
}

// SetEndpoint swaps the dialers at once: new flows reach the new address,
// open ones stay where they are.
func (l *proxyLink) SetEndpoint(resolved string) error {
	d, err := l.dialers(resolved)
	if err != nil {
		return err
	}
	l.stack.SetDialers(d)
	return nil
}

func (l *proxyLink) Stats() linkStats {
	st := l.stack.Stats()
	return linkStats{Flows: &st}
}

// lookupNetIP is swapped in tests.
var lookupNetIP = net.DefaultResolver.LookupNetIP

// resolveEndpoint turns host:port into ip:port. Providers usually hand out
// IPs, so this is mostly a no-op.
func resolveEndpoint(ctx context.Context, endpoint string) (string, error) {
	if _, err := netip.ParseAddrPort(endpoint); err == nil {
		return endpoint, nil
	}
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	addrs, err := lookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint %q: %w", endpoint, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve endpoint %q: no IPv4 address", endpoint)
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), port).String(), nil
}

// splitEndpoint checks the host:port syntax without resolving anything.
func splitEndpoint(endpoint string) (string, uint16, error) {
	if endpoint == "" {
		return "", 0, fmt.Errorf("[Peer] Endpoint is required")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("endpoint %q: bad port", endpoint)
	}
	return host, uint16(portNum), nil
}
