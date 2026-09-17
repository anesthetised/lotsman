package daemon

import (
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
	Conf    *awgconf.Config
	Addr    netip.Prefix // the tunnel's IPv4 address, also the probe source
	MTU     int
	Device  *tunnel.Device
	Tracker *health.Tracker
}

// LoadUpstream reads a provider config and resolves its endpoint. The device
// is attached later by whoever creates the TUN.
func LoadUpstream(ctx context.Context, u config.Upstream, h config.Health) (*Upstream, error) {
	f, err := os.Open(u.Conf)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	conf, err := awgconf.Parse(f)
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
	if peer.Endpoint, err = resolveEndpoint(ctx, peer.Endpoint); err != nil {
		return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
	}
	addr, ok := firstIPv4(conf.Interface.Addresses)
	if !ok {
		return nil, fmt.Errorf("upstream %s: [Interface] Address has no IPv4", u.Name)
	}
	m := conf.Interface.MTU
	if m == 0 {
		m = mtu.DefaultWireGuard
	}
	return &Upstream{
		Upstream: u,
		Conf:     conf,
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
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint %q: %w", endpoint, err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("resolve endpoint %q: no IPv4 address", endpoint)
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), uint16(portNum)).String(), nil
}
