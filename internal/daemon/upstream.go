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
	"strings"
	"sync"

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
	Key      proxy.Key       // proxy kinds: the access key; for a dynamic key, set by Fetch
	Location *proxy.Location // a dynamic key's URL, a secret like the key; nil for a static key
	Source   string          // the provider file as loaded, to detect edits on reload
	Endpoint string          // the server as written, possibly a hostname
	Resolved string          // Endpoint's current address, set by Resolve
	Addr     netip.Prefix    // the TUN's IPv4 address, also the probe source
	MTU      int
	Tracker  *health.Tracker
	// Keep an immutable copy of the bind's send mode so reload can compare
	// it outside d.mu while updating the embedded settings.
	disableUDPGSO bool
	// keyText is what the link parses the key from (proxy.ParseConfig): a
	// static key file's contents, or a dynamic key's response as fetched,
	// which also detects a changed key.
	keyText string
	link    link // set by open
	// exits counts the times link stopped on its own; d.mu guards it.
	exits uint64
	// errs counts what an AWG link's device logs as errors, and sendErrors
	// judges that a minute at a time; d.mu guards sendErrors.
	errs       deviceErrors
	sendErrors sendErrorWindow
	// serverMu keeps the server changes of a running upstream in order
	// while its link is told without d.mu (Daemon.applyServer).
	serverMu sync.Mutex
}

// link carries an upstream's traffic on its TUN: an AmneziaWG device or a
// proxy stack.
type link interface {
	Up() error
	Close()
	// SetServer points the link at its server's current address and, for a
	// proxy link, at its current key, as text (Upstream.keyText); an AWG
	// link ignores the key.
	SetServer(keyText, resolved string) error
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
	switch {
	case u.Kind == config.KindVLESS:
		up, err = loadVLESS(source)
	case u.Proxy():
		up, err = loadProxy(source)
	default:
		up, err = loadAWG(source)
	}
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	up.Upstream = u
	up.Source = string(source)
	up.disableUDPGSO = u.DisableUDPGSO
	up.Tracker = health.NewTracker(h.DownAfter, h.UpAfter)
	return up, nil
}

func (u *Upstream) sameLinkConfig(other *Upstream) bool {
	return other != nil && u.Source == other.Source && u.disableUDPGSO == other.disableUDPGSO
}

// loadProxy reads an access key, or the URL of a dynamic one, which Fetch
// downloads. Both are secrets, so errors never quote the file.
func loadProxy(source []byte) (*Upstream, error) {
	// Nothing on the TUN adds overhead: the stack terminates every flow.
	up := &Upstream{Addr: config.ProxyAddress, MTU: mtu.Ethernet}
	if proxy.IsDynamic(string(source)) {
		loc, err := proxy.ParseLocation(string(source))
		if err != nil {
			return nil, err
		}
		up.Location = loc
		return up, nil
	}
	// The link parses the same text again: an ss:// key, or an Outline
	// config such as one with WebSocket transports.
	text := strings.TrimSpace(string(source))
	key, err := proxy.ParseConfig(text)
	if err != nil {
		return nil, err
	}
	up.Key, up.keyText, up.Endpoint = key, text, key.Server()
	return up, nil
}

// loadVLESS reads a vless:// link, a secret like an access key. VLESS keys
// are static only.
func loadVLESS(source []byte) (*Upstream, error) {
	key, err := proxy.ParseVLESS(string(source))
	if err != nil {
		return nil, err
	}
	return &Upstream{Addr: config.ProxyAddress, MTU: mtu.Ethernet,
		Key: key, keyText: strings.TrimSpace(string(source)), Endpoint: key.Server()}, nil
}

// fetchClient fetches dynamic keys; swapped in tests.
var fetchClient = proxy.NewFetchClient(nil)

// dynamicKey is a fetched key with the response it was read from.
type dynamicKey struct {
	key  proxy.Key
	text string
}

// fetchKey downloads a dynamic key over the host's network. It takes no lock
// and changes nothing.
func fetchKey(ctx context.Context, loc *proxy.Location) (dynamicKey, error) {
	key, text, err := proxy.FetchKey(ctx, fetchClient, loc)
	return dynamicKey{key, text}, err
}

// Fetch downloads a dynamic key and installs it on an upstream that is not
// running; for a static key it does nothing. Resolve comes after.
func (u *Upstream) Fetch(ctx context.Context) error {
	if u.Location == nil {
		return nil
	}
	k, err := fetchKey(ctx, u.Location)
	if err != nil {
		return fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	u.setKey(k)
	return nil
}

// setKey installs a dynamic key on an upstream that is not running.
func (u *Upstream) setKey(k dynamicKey) {
	u.Key, u.keyText, u.Endpoint = k.key, k.text, k.key.Server()
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
	if m < mtu.MinimumIPv4 {
		return nil, fmt.Errorf("[Interface] MTU must be at least %d (MTU %d)", mtu.MinimumIPv4, m)
	}
	s4 := int(conf.Interface.S4)
	up := &Upstream{Conf: conf, Endpoint: peer.Endpoint, Addr: addr, MTU: m}
	if up.MTULimit().PacketLimit() < mtu.MinimumIPv4 {
		return nil, fmt.Errorf("[Interface] S4 and ContentPaddingAddition leave less than %d bytes in the outer path budget (S4 %d, ContentPaddingAddition %s)",
			mtu.MinimumIPv4, s4, conf.Interface.ContentPaddingAddition)
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
	return up, nil
}

// MTULimit is what the upstream contributes to the client MTU.
func (u *Upstream) MTULimit() mtu.Upstream {
	if u.Conf == nil {
		return mtu.Upstream{MTU: u.MTU, Proxy: true}
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
	if u.Location != nil && u.Key == nil {
		return fmt.Errorf("upstream %s: dynamic key not fetched", u.Name)
	}
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
// link stays down until Up. The endpoint must be resolved. A proxy link runs
// through run; exited is called if it stops on its own.
func (u *Upstream) open(t tun.Device, log *slog.Logger, run proxy.Runner, exited func(error)) error {
	if u.Key == nil {
		dev := tunnel.NewUpstream(u.InterfaceName(), t, log, u.errs.add, u.disableUDPGSO)
		if err := dev.Configure(u.Conf.UAPI()); err != nil {
			dev.Close()
			return err
		}
		u.link = &awgLink{dev: dev, peer: u.Conf.Peers[0].PublicKey}
		return nil
	}
	server, err := netip.ParseAddrPort(u.Resolved)
	if err != nil {
		t.Close()
		return fmt.Errorf("upstream %s: server address %q: %w", u.Name, u.Resolved, err)
	}
	spec := proxy.Spec{Key: u.keyText, Server: server, MaxFlows: u.MaxFlows}
	l, err := run(t, spec, log.With("upstream", u.Name), exited)
	if err != nil {
		return fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	u.link = &proxyLink{Link: l, key: u.keyText}
	return nil
}

// SetEndpoint points the running link at a freshly resolved endpoint if the
// server moved to another address, and returns whether it changed. The
// caller resolves u.Endpoint first. Literal IP endpoints resolve to
// themselves and never change.
func (u *Upstream) SetEndpoint(resolved string) (bool, error) {
	text, _, changed := u.nextServer(nil, resolved)
	if !changed {
		return false, nil
	}
	if err := u.link.SetServer(text, resolved); err != nil {
		return false, err
	}
	u.commitServer(nil, false, resolved)
	return true, nil
}

// nextServer works out how to point the running link at resolved and, when
// k is set and differs from the running key, at k; resolved is then k's
// server resolved. It returns the key text to tell the link, whether k is a
// new key, and whether anything changes. New flows use them, open ones stay.
func (u *Upstream) nextServer(k *dynamicKey, resolved string) (text string, newKey, changed bool) {
	newKey = k != nil && k.text != u.keyText
	text = u.keyText
	if newKey {
		text = k.text
	}
	return text, newKey, newKey || resolved != u.Resolved
}

// commitServer records what nextServer worked out once the link took it.
func (u *Upstream) commitServer(k *dynamicKey, newKey bool, resolved string) {
	if newKey {
		u.Key, u.keyText, u.Endpoint = k.key, k.text, k.key.Server()
	}
	u.setResolved(resolved)
}

// awgLink is an AmneziaWG device with one peer, the provider.
type awgLink struct {
	dev  *tunnel.Device
	peer awgconf.Key
}

func (l *awgLink) Up() error { return l.dev.Up() }
func (l *awgLink) Close()    { l.dev.Close() }

func (l *awgLink) SetServer(_, resolved string) error {
	return l.dev.Configure("public_key=" + l.peer.Hex() + "\nupdate_only=true\nendpoint=" + resolved + "\n")
}

func (l *awgLink) Stats() linkStats {
	if peers, err := l.dev.Peers(); err == nil && len(peers) == 1 {
		return linkStats{Peer: &peers[0]}
	}
	return linkStats{}
}

// proxyLink is a proxy stack, in this process or in a helper. In process it
// carries traffic from the start: the TUN passes nothing before the
// dataplane sets it up and routes to it. A helper starts at Up. A request
// to a helper waits for its answer, so the daemon never makes one under
// d.mu; Stats serves what the health round last fetched instead of asking.
type proxyLink struct {
	proxy.Link

	mu    sync.Mutex
	key   string       // the key text it runs with
	flows *proxy.Stats // the last answer to refreshStats; nil before one, or when it failed
}

// SetServer makes new flows reach the new address with the given key; open
// ones stay where they are.
func (l *proxyLink) SetServer(keyText, resolved string) error {
	addr, err := netip.ParseAddrPort(resolved)
	if err != nil {
		return fmt.Errorf("server address %q: %w", resolved, err)
	}
	if err := l.Link.SetServer(keyText, addr); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.key = keyText
	return nil
}

func (l *proxyLink) runningKey() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.key
}

// Stats returns the flow counters refreshStats fetched last, without asking
// the stack.
func (l *proxyLink) Stats() linkStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return linkStats{Flows: l.flows}
}

// refreshStats asks the stack for its flow counters, which waits for a
// helper's answer. There are none while no helper runs.
func (l *proxyLink) refreshStats() {
	var flows *proxy.Stats
	if st, err := l.Link.Stats(); err == nil {
		flows = &st
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flows = flows
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
