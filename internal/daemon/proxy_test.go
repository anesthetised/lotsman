package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/store"
)

const proxyConfig = `
endpoint: vpn.example.org:51820
state_dir: %[1]s
upstreams:
  - {name: nl, conf: %[1]s/nl.conf, geo: NL}
  - {name: ol, kind: outline, conf: %[1]s/ol.key, geo: OL%[2]s}
health: {interval: 30ms, probe: 1.1.1.1:443, dns: 9.9.9.9:53, down_after: 2, up_after: 1}
`

const ssSecret = "do-not-print-me"

// newProxyHarness runs the harness with an AWG upstream, nl, and an Outline
// upstream, ol, whose server is a hostname. Nothing goes through the stack:
// the probes are fakes, as for AWG upstreams.
func newProxyHarness(t *testing.T) (*harness, *fakeResolver) {
	t.Helper()
	h := newHarness(t)
	dns := &fakeResolver{hosts: map[string]string{"localhost": "127.0.0.1", "ss.provider.test": "192.0.2.10"}}
	useResolver(t, dns)
	userinfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:" + ssSecret))
	key := "ss://" + userinfo + "@ss.provider.test:8388/?outline=1#Outline\n"
	if err := os.WriteFile(filepath.Join(h.dir, "ol.key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfg = proxyHarnessConfig(t, h.dir, "")
	h.d.cfg = h.cfg
	if err := h.st.AddProfile(config.Profile{Name: "px", Prefer: []config.Match{{Geo: "OL"}, {Geo: "NL"}}}); err != nil {
		t.Fatal(err)
	}
	h.setHealthy("lm-up-ol", true)
	return h, dns
}

func proxyHarnessConfig(t *testing.T, dir, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf(proxyConfig, dir, extra)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (h *harness) setDNSDown(iface, network string, down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dnsDown[iface+" "+network] = down
}

func (h *harness) probes(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probed[key]
}

func TestLoadProxyUpstream(t *testing.T) {
	h, _ := newProxyHarness(t)
	u, err := LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health)
	if err != nil {
		t.Fatal(err)
	}
	if u.Key == nil || u.Conf != nil || u.Endpoint != "ss.provider.test:8388" || u.Addr != config.ProxyAddress || u.MTU != 1500 {
		t.Errorf("upstream = %+v", u)
	}
	if got := u.Key.String(); got != "shadowsocks chacha20-ietf-poly1305" {
		t.Errorf("key description = %q", got)
	}
	if err := u.Resolve(context.Background()); err != nil || u.Resolved != "192.0.2.10:8388" {
		t.Errorf("Resolve: %v, %q", err, u.Resolved)
	}

	// A broken key names the upstream but never quotes the file.
	path := filepath.Join(h.dir, "ol.key")
	os.WriteFile(path, []byte("ss://rc4-md5:"+ssSecret+"@ss.provider.test:8388\n"), 0o600)
	_, err = LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health)
	if err == nil || !strings.HasPrefix(err.Error(), "upstream ol: ") || strings.Contains(err.Error(), ssSecret) {
		t.Errorf("broken key: %v", err)
	}
}

// A key file may hold an Outline config with WebSocket transports: the
// URL's host is the server, resolved up front like any other.
func TestLoadProxyUpstreamWebSocket(t *testing.T) {
	h, dns := newProxyHarness(t)
	dns.set("ws.provider.test", "192.0.2.20")
	path := filepath.Join(h.dir, "ol.key")
	side := func(path string) string {
		return `{$type: shadowsocks, endpoint: {$type: websocket, url: "wss://ws.provider.test/` + ssSecret + path + `"}, cipher: chacha20-ietf-poly1305, secret: ` + ssSecret + `}`
	}
	os.WriteFile(path, []byte("transport:\n  $type: tcpudp\n  tcp: "+side("/tcp")+"\n  udp: "+side("/udp")+"\n"), 0o600)
	u, err := LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health)
	if err != nil {
		t.Fatal(err)
	}
	if u.Endpoint != "ws.provider.test:443" || u.Key.String() != "shadowsocks chacha20-ietf-poly1305 over WebSocket" {
		t.Errorf("endpoint %q, key %q", u.Endpoint, u.Key)
	}
	if err := u.Resolve(context.Background()); err != nil || u.Resolved != "192.0.2.20:443" {
		t.Errorf("Resolve: %v, %q", err, u.Resolved)
	}

	// An unsupported transport names the upstream but never quotes the file.
	os.WriteFile(path, []byte("transport: {$type: tcpudp, tcp: {$type: iptable, secret: "+ssSecret+"}}\n"), 0o600)
	_, err = LoadUpstream(h.cfg.Upstreams[1], h.cfg.Health)
	if err == nil || !strings.Contains(err.Error(), `upstream ol: access key: tcp: dialer "iptable" is not supported`) || strings.Contains(err.Error(), ssSecret) {
		t.Errorf("unsupported transport: %v", err)
	}
}

func TestProxyUpstream(t *testing.T) {
	h, dns := newProxyHarness(t)
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "px", h.cfg.Subnet)
	h.run(t)
	routedVia := func(want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == want }
	}
	eventually(t, "routed via ol", routedVia("lm-up-ol"))

	// The proxy TUN gets the shared proxy address and adds no MTU overhead.
	if !h.dp.HasUpstream(dataplane.Interface{Name: "lm-up-ol", Addr: netip.MustParsePrefix("192.0.0.8/32")}) {
		t.Errorf("dataplane upstreams = %+v", h.dp.Ups)
	}
	if got := h.d.ClientMTU(); got != 1392 { // min(1500-80-S4, 1400-8), as without ol
		t.Errorf("ClientMTU = %d", got)
	}

	// ol is checked by DNS over UDP and TCP from its address, nl by TCP connect.
	for _, key := range []string{"lm-up-ol dns-udp 192.0.0.8 9.9.9.9:53", "lm-up-ol dns-tcp 192.0.0.8 9.9.9.9:53", "lm-up-nl tcp-connect"} {
		if h.probes(key) == 0 {
			t.Errorf("no %q probe", key)
		}
	}
	if h.probes("lm-up-ol tcp-connect") != 0 || h.probes("lm-up-nl dns-udp 10.8.0.2 9.9.9.9:53") != 0 {
		t.Errorf("probes of the other kind ran: %v", h.probed)
	}

	st := h.d.Status()
	if ol := st.Upstreams[1]; ol.Kind != config.KindOutline || ol.Interface != "lm-up-ol" || !ol.LastHandshake.IsZero() || ol.Routed != 1 {
		t.Errorf("status = %+v", ol)
	}
	m := h.d.Metrics()
	ol := metrics.Label{Name: "upstream", Value: "ol"}
	if v, ok := metric(m, "lotsman_upstream_info", ol, metrics.Label{Name: "kind", Value: "outline"}, metrics.Label{Name: "interface", Value: "lm-up-ol"}); !ok || v != 1 {
		t.Errorf("upstream_info{ol} = %v, %v", v, ok)
	}
	if _, ok := metric(m, "lotsman_upstream_info", metrics.Label{Name: "upstream", Value: "nl"}, metrics.Label{Name: "kind", Value: "awg"}); !ok {
		t.Error("upstream_info{nl, awg} missing")
	}
	if v, ok := metric(m, "lotsman_upstream_flows", ol); !ok || v != 0 {
		t.Errorf("upstream_flows{ol} = %v, %v", v, ok)
	}
	if _, ok := metric(m, "lotsman_upstream_flows_refused_total", ol); !ok {
		t.Error("upstream_flows_refused_total{ol} missing")
	}
	if _, ok := metric(m, "lotsman_upstream_last_handshake_timestamp_seconds", ol); ok {
		t.Error("a proxy upstream reports a handshake")
	}

	// Either protocol failing takes ol down, and the profile fails over.
	for _, network := range []string{"udp", "tcp"} {
		h.setDNSDown("lm-up-ol", network, true)
		eventually(t, "failover to nl without dns over "+network, routedVia("lm-up-nl"))
		h.setDNSDown("lm-up-ol", network, false)
		eventually(t, "back to ol", routedVia("lm-up-ol"))
	}

	// A failing proxy re-resolves its server, and new flows go to the new address.
	dns.set("ss.provider.test", "192.0.2.11")
	h.setHealthy("lm-up-ol", false)
	eventually(t, "server re-resolved", func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return h.d.ups[1].Resolved == "192.0.2.11:8388" && h.d.ups[1].Tracker.State() == health.Down
	})
	h.setHealthy("lm-up-ol", true)
	eventually(t, "ol back", routedVia("lm-up-ol"))
}

func TestProxyReloadAppliesMaxFlowsLive(t *testing.T) {
	h, _ := newProxyHarness(t)
	h.run(t)
	eventually(t, "start", h.dp.Ready)
	// What the stack runs with now, not what the health round cached.
	stats := func() (link, *proxy.Stats) {
		h.d.mu.Lock()
		l := h.d.ups[1].link
		h.d.mu.Unlock()
		st, err := l.(*proxyLink).Link.Stats()
		if err != nil {
			t.Fatal(err)
		}
		return l, &st
	}
	before, st := stats()
	if st.MaxFlows != proxy.DefaultMaxFlows {
		t.Fatalf("max flows = %d, want the default", st.MaxFlows)
	}
	if err := h.d.Reload(context.Background(), proxyHarnessConfig(t, h.dir, ", max_flows: 7")); err != nil {
		t.Fatal(err)
	}
	after, st := stats()
	if after != before || st.MaxFlows != 7 {
		t.Errorf("after reload: same link %v, max flows %d", after == before, st.MaxFlows)
	}

	// A changed kind whose file does not load as that kind keeps the running
	// upstream, and status keeps showing what runs.
	asAWG, err := config.Parse([]byte(strings.Replace(fmt.Sprintf(proxyConfig, h.dir, ""), "kind: outline", "kind: awg", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), asAWG); err == nil {
		t.Fatal("reload loaded an access key as an AWG config")
	}
	if after, _ := stats(); after != before {
		t.Error("broken key replaced the running upstream")
	}
	if st := h.d.Status().Upstreams[1]; st.Kind != config.KindOutline {
		t.Errorf("kind after failed reload = %q", st.Kind)
	}
}

// probeUpstream needs both protocols and reports the UDP latency.
func TestProbeUpstreamProxy(t *testing.T) {
	d := &Daemon{probeDNS: func(_ context.Context, network, _ string, _ netip.Addr, target netip.AddrPort, _ time.Duration) (time.Duration, error) {
		if network == "tcp" && target.Port() == 54 {
			return 0, fmt.Errorf("no tcp to %s", target)
		}
		if network == "udp" {
			return 3 * time.Millisecond, nil
		}
		return 9 * time.Millisecond, nil
	}}
	h := config.Health{Interval: time.Second}
	h.DNS = config.DNSTargets{netip.MustParseAddrPort("192.0.2.1:53")}
	if latency, err := d.probeUpstream(context.Background(), true, "lm-up-ol", config.ProxyAddress.Addr(), h); err != nil || latency != 3*time.Millisecond {
		t.Errorf("probe = %v, %v; want the UDP latency", latency, err)
	}
	h.DNS = config.DNSTargets{netip.MustParseAddrPort("192.0.2.1:54")}
	if _, err := d.probeUpstream(context.Background(), true, "lm-up-ol", config.ProxyAddress.Addr(), h); err == nil || !strings.Contains(err.Error(), "no tcp") {
		t.Errorf("probe with TCP failing = %v", err)
	}
}
