package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// Upstream is one provider tunnel: its config, its device and its health.
type Upstream struct {
	config.Upstream
	Conf     *awgconf.Config
	Source   string       // the provider file as loaded, to detect edits on reload
	Endpoint string       // the peer endpoint as written, possibly a hostname
	Addr     netip.Prefix // the tunnel's IPv4 address, also the probe source
	MTU      int
	Device   *tunnel.Device
	Tracker  *health.Tracker
}

// LoadUpstream reads a provider config and resolves its endpoint. The device
// is attached later by whoever creates the TUN.
func LoadUpstream(ctx context.Context, u config.Upstream, h config.Health) (*Upstream, error) {
	source, err := os.ReadFile(u.Conf)
	if err != nil {
		return nil, err
	}
	conf, err := awgconf.Parse(bytes.NewReader(source))
	if err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	if len(conf.Peers) != 1 {
		return nil, fmt.Errorf("upstream %s: want exactly one [Peer], got %d", u.Name, len(conf.Peers))
	}
	peer := &conf.Peers[0]
	if !routesEverything(peer.AllowedIPs) {
		return nil, fmt.Errorf("upstream %s: [Peer] AllowedIPs must include 0.0.0.0/0", u.Name)
	}
	endpoint := peer.Endpoint
	if peer.Endpoint, err = resolveEndpoint(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	remote, err := netip.ParseAddrPort(peer.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("upstream %s: resolved endpoint: %w", u.Name, err)
	}
	// Ordinary UDP payload limits, also used by the pinned AWG StdNetBind.
	payloadMax := 65507
	if remote.Addr().Is6() && !remote.Addr().Is4In6() {
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
			return nil, fmt.Errorf("upstream %s: [Interface] junk packet size must not exceed %d for endpoint %s", u.Name, payloadMax, peer.Endpoint)
		}
	}
	addr, ok := firstIPv4(conf.Interface.Addresses)
	if !ok {
		return nil, fmt.Errorf("upstream %s: [Interface] Address has no IPv4", u.Name)
	}
	m := conf.Interface.MTU
	if m == 0 {
		m = mtu.DefaultWireGuard
	}
	// Linux TUN needs at least 68 bytes, including for the client MTU after
	// subtracting S4. Compare thresholds to avoid overflowing on a huge MTU.
	s4 := int(conf.Interface.S4)
	if m < 68+s4 {
		return nil, fmt.Errorf("upstream %s: [Interface] MTU minus S4 must be at least 68 (MTU %d, S4 %d)", u.Name, m, s4)
	}
	// AWG reserves a 16-byte transport header and 16-byte authentication tag.
	// Its content padding clamps to the positive TUN MTU, without rounding
	// past it, so no additional alignment allowance is needed.
	if m > payloadMax-32-s4 {
		return nil, fmt.Errorf("upstream %s: [Interface] MTU plus S4 and transport overhead exceeds UDP payload limit %d for endpoint %s", u.Name, payloadMax, peer.Endpoint)
	}
	return &Upstream{
		Upstream: u,
		Conf:     conf,
		Source:   string(source),
		Endpoint: endpoint,
		Addr:     addr,
		MTU:      m,
		Tracker:  health.NewTracker(h.DownAfter, h.UpAfter),
	}, nil
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

// RefreshEndpoint re-resolves a hostname endpoint and, if the provider moved
// to another address, points the running device at it. Returns whether the
// endpoint changed. Literal IP endpoints never change.
func (u *Upstream) RefreshEndpoint(ctx context.Context) (bool, error) {
	if _, err := netip.ParseAddrPort(u.Endpoint); err == nil {
		return false, nil
	}
	resolved, err := resolveEndpoint(ctx, u.Endpoint)
	if err != nil || resolved == u.Conf.Peers[0].Endpoint {
		return false, err
	}
	peer := u.Conf.Peers[0]
	if err := u.Device.Configure("public_key=" + peer.PublicKey.Hex() + "\nupdate_only=true\nendpoint=" + resolved + "\n"); err != nil {
		return false, err
	}
	u.Conf.Peers[0].Endpoint = resolved
	return true, nil
}

// lookupNetIP is swapped in tests.
var lookupNetIP = net.DefaultResolver.LookupNetIP

// resolveEndpoint turns host:port into ip:port; amneziawg-go's UAPI takes
// addresses only. Providers usually hand out IPs, so this is mostly a no-op.
func resolveEndpoint(ctx context.Context, endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("[Peer] Endpoint is required")
	}
	if _, err := netip.ParseAddrPort(endpoint); err == nil {
		return endpoint, nil
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("endpoint %q: %w", endpoint, err)
	}
	portNum, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return "", fmt.Errorf("endpoint %q: bad port", endpoint)
	}
	addrs, err := lookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint %q: %w", endpoint, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve endpoint %q: no IPv4 address", endpoint)
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), uint16(portNum)).String(), nil
}
