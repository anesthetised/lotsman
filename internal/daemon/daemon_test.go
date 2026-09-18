package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/tun"
	"github.com/amnezia-vpn/amneziawg-go/tun/netstack"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane/fake"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// providerConf writes a provider-style config for an upstream whose tunnel address is addr.
func providerConf(t *testing.T, dir, name, addr string, mtu int) string {
	t.Helper()
	key, _ := awgconf.GeneratePrivateKey()
	peer, _ := awgconf.GeneratePrivateKey()
	// A hostname endpoint, as providers often hand out; LoadUpstream resolves it.
	conf := &awgconf.Config{
		Interface: awgconf.Interface{
			PrivateKey: key, Addresses: []netip.Prefix{netip.MustParsePrefix(addr)}, MTU: mtu,
			Params: awgconf.Params{S1: 5, S2: 6, S3: 7, S4: 8, H1: awgconf.Range{Lo: 10, Hi: 10}, H2: awgconf.Range{Lo: 20, Hi: 20}, H3: awgconf.Range{Lo: 30, Hi: 30}, H4: awgconf.Range{Lo: 40, Hi: 40}},
		},
		Peers: []awgconf.Peer{{PublicKey: peer.Public(), Endpoint: "localhost:51820", AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
	}
	path := filepath.Join(dir, name+".conf")
	if err := os.WriteFile(path, []byte(conf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadUpstream(t *testing.T) {
	dir := t.TempDir()
	h := config.Health{DownAfter: 2, UpAfter: 1}
	good := providerConf(t, dir, "good", "10.8.0.2/32", 0)
	u, err := LoadUpstream(context.Background(), config.Upstream{Name: "good", Conf: good}, h)
	if err != nil {
		t.Fatal(err)
	}
	if u.Addr.String() != "10.8.0.2/32" || u.MTU != 1420 || u.Conf.Peers[0].Endpoint != "127.0.0.1:51820" {
		t.Errorf("upstream = %+v", u)
	}
	if !strings.Contains(u.Source, "localhost:51820") {
		t.Errorf("Source should be the file as written, got %q", u.Source)
	}

	bad := map[string]string{
		"split tunnel": strings.Replace(mustRead(t, good), "0.0.0.0/0", "10.0.0.0/8", 1),
		"no peer":      strings.Split(mustRead(t, good), "\n[Peer]")[0],
		"no ipv4":      strings.Replace(mustRead(t, good), "10.8.0.2/32", "fd00::2/128", 1),
		"no endpoint":  strings.Replace(mustRead(t, good), "Endpoint = localhost:51820\n", "", 1),
	}
	for name, content := range bad {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "bad.conf")
			os.WriteFile(path, []byte(content), 0o600)
			if _, err := LoadUpstream(context.Background(), config.Upstream{Name: "bad", Conf: path}, h); err == nil {
				t.Error("expected error")
			}
		})
	}

	t.Run("ip endpoint is kept", func(t *testing.T) {
		content := strings.Replace(mustRead(t, good), "localhost:51820", "192.0.2.1:51820", 1)
		path := filepath.Join(dir, "ip.conf")
		os.WriteFile(path, []byte(content), 0o600)
		u, err := LoadUpstream(context.Background(), config.Upstream{Name: "ip", Conf: path}, h)
		if err != nil {
			t.Fatal(err)
		}
		if u.Conf.Peers[0].Endpoint != "192.0.2.1:51820" {
			t.Errorf("endpoint = %q", u.Conf.Peers[0].Endpoint)
		}
	})
}

// fakeResolver answers hostname lookups from a map the test controls.
type fakeResolver struct {
	mu    sync.Mutex
	hosts map[string]string
}

func (r *fakeResolver) set(host, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[host] = ip
}

func (r *fakeResolver) lookup(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ip, ok := r.hosts[host]; ok {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	return nil, errors.New("no such host")
}

func useResolver(t *testing.T, r *fakeResolver) {
	t.Helper()
	old := lookupNetIP
	lookupNetIP = r.lookup
	t.Cleanup(func() { lookupNetIP = old })
}

func TestRefreshEndpoint(t *testing.T) {
	dns := &fakeResolver{hosts: map[string]string{"vpn.provider.test": "192.0.2.1"}}
	useResolver(t, dns)
	dir := t.TempDir()
	path := providerConf(t, dir, "p", "10.8.0.2/32", 0)
	content := strings.Replace(mustRead(t, path), "localhost:51820", "vpn.provider.test:51820", 1)
	os.WriteFile(path, []byte(content), 0o600)
	h := config.Health{DownAfter: 2, UpAfter: 1}
	u, err := LoadUpstream(context.Background(), config.Upstream{Name: "p", Conf: path}, h)
	if err != nil {
		t.Fatal(err)
	}
	if u.Endpoint != "vpn.provider.test:51820" || u.Conf.Peers[0].Endpoint != "192.0.2.1:51820" {
		t.Fatalf("loaded endpoint = %q resolved %q", u.Endpoint, u.Conf.Peers[0].Endpoint)
	}
	tn, _, _ := netstack.CreateNetTUN([]netip.Addr{u.Addr.Addr()}, nil, 1400)
	u.Device = tunnel.New("p", tn, quiet)
	t.Cleanup(u.Device.Close)
	if err := u.Device.Configure(u.Conf.UAPI()); err != nil {
		t.Fatal(err)
	}

	if changed, err := u.RefreshEndpoint(context.Background()); err != nil || changed {
		t.Errorf("unchanged DNS: changed=%v err=%v", changed, err)
	}
	dns.set("vpn.provider.test", "192.0.2.2")
	if changed, err := u.RefreshEndpoint(context.Background()); err != nil || !changed {
		t.Fatalf("changed DNS: changed=%v err=%v", changed, err)
	}
	peers, _ := u.Device.Peers()
	if len(peers) != 1 || peers[0].Endpoint != "192.0.2.2:51820" {
		t.Errorf("device endpoint not updated: %+v", peers)
	}
	if u.Conf.Peers[0].Endpoint != "192.0.2.2:51820" {
		t.Errorf("Conf endpoint not updated: %q", u.Conf.Peers[0].Endpoint)
	}

	// A literal IP endpoint is never re-resolved.
	dns.set("vpn.provider.test", "192.0.2.3")
	u.Endpoint = "192.0.2.2:51820"
	if changed, err := u.RefreshEndpoint(context.Background()); err != nil || changed {
		t.Errorf("literal endpoint: changed=%v err=%v", changed, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "identity.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("identity file: %v, mode %v", err, info.Mode())
	}
	p := id.Params
	if p.S1 < 15 || p.S1 > 100 || p.S4 < 8 || p.S4 > 16 || p.Jc != 0 || p.I[0] != "" {
		t.Errorf("params out of range or client-only params set: %+v", p)
	}
	ranges := []awgconf.Range{p.H1, p.H2, p.H3, p.H4}
	for i := range ranges {
		for j := i + 1; j < len(ranges); j++ {
			if ranges[i].Lo <= ranges[j].Hi && ranges[j].Lo <= ranges[i].Hi {
				t.Errorf("H%d and H%d overlap: %v %v", i+1, j+1, ranges[i], ranges[j])
			}
		}
		if ranges[i].Lo <= 4 {
			t.Errorf("H%d collides with WireGuard message types: %v", i+1, ranges[i])
		}
	}

	again, err := LoadOrCreateIdentity(dir)
	if err != nil || again != id {
		t.Errorf("reload changed identity: %v\n%+v\n%+v", err, id, again)
	}

	os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{"), 0o600)
	if _, err := LoadOrCreateIdentity(dir); err == nil {
		t.Error("corrupt identity accepted")
	}
}

func TestClientConfig(t *testing.T) {
	cfg, err := config.Parse([]byte("endpoint: vpn.example.org:51820\nupstreams: [{name: a, conf: /x}]\nprofiles: [{name: p}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := newIdentity()
	peerKey, _ := awgconf.GeneratePrivateKey()
	peer := store.Peer{PrivateKey: peerKey, PublicKey: peerKey.Public(), IP: netip.MustParseAddr("10.77.0.9")}

	c := ClientConfig(cfg, id, peer, 1380)
	parsed, err := awgconf.Parse(strings.NewReader(c.String()))
	if err != nil {
		t.Fatalf("rendered client config does not parse: %v\n%s", err, c)
	}
	i := parsed.Interface
	if i.PrivateKey != peerKey || i.Addresses[0].String() != "10.77.0.9/32" || i.MTU != 1380 || i.DNS[0].String() != "1.1.1.1" {
		t.Errorf("interface = %+v", i)
	}
	if i.S1 != id.Params.S1 || i.S4 != id.Params.S4 || i.H1 != id.Params.H1 || i.H4 != id.Params.H4 {
		t.Errorf("shared params not copied: %+v", i.Params)
	}
	if i.Jc == 0 || i.I[0] == "" {
		t.Errorf("client junk missing: %+v", i.Params)
	}
	p := parsed.Peers[0]
	if p.PublicKey != id.PrivateKey.Public() || p.Endpoint != "vpn.example.org:51820" || p.AllowedIPs[0].String() != "0.0.0.0/0" || p.PersistentKeepalive != 25 {
		t.Errorf("peer = %+v", p)
	}
}

// harness runs a daemon on netstack devices with a fake dataplane and a probe
// whose results the test controls per upstream address.
type harness struct {
	d       *Daemon
	st      *store.Store
	dp      *fake.Dataplane
	cfg     *config.Config
	dir     string
	mu      sync.Mutex
	healthy map[netip.Addr]bool
}

const harnessConfig = `
endpoint: vpn.example.org:51820
state_dir: %s
upstreams:
  - {name: nl, conf: %s/nl.conf, geo: NL}
  - {name: de, conf: %s/de.conf, geo: DE}
profiles:
  - {name: nl, prefer: [{geo: NL}]}
  - {name: eu, prefer: [{geo: NL}, {geo: DE}]}
health: {interval: 30ms, probe: 1.1.1.1:443, down_after: 2, up_after: 1}
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	providerConf(t, dir, "nl", "10.8.0.2/32", 1400)
	providerConf(t, dir, "de", "10.9.0.2/32", 0)
	cfg, err := config.Parse([]byte(fmt.Sprintf(harnessConfig, dir, dir, dir)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "lotsman.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{st: st, dp: fake.New(), cfg: cfg, dir: dir, healthy: map[netip.Addr]bool{}}
	h.setHealthy("10.8.0.2", true)
	h.setHealthy("10.9.0.2", true)
	probe := func(_ context.Context, source netip.Addr, _ netip.AddrPort, _ time.Duration) (time.Duration, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.healthy[source] {
			return time.Millisecond, nil
		}
		return 0, errors.New("probe failed")
	}
	// Devices run on netstack; the address only matters for the downstream gateway.
	newTUN := func(name string, mtu int) (tun.Device, error) {
		addr := netip.MustParseAddr("10.77.0.1")
		if name != DownstreamInterface {
			addr = netip.MustParseAddr("10.0.0.1")
		}
		tn, _, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, mtu)
		return tn, err
	}
	h.d = New(Deps{Config: cfg, Identity: id, Store: st, Dataplane: h.dp, NewTUN: newTUN, Probe: probe, Log: quiet})
	return h
}

// run starts the daemon and makes the test wait for it to stop, so the
// listen port is free again before the next test starts its own daemon.
func (h *harness) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (h *harness) setHealthy(addr string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.healthy[netip.MustParseAddr(addr)] = ok
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDaemon(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	nlPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
	euPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)

	stop := h.run(t)

	routedVia := func(peer netip.Addr, want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(peer); return got == want }
	}
	eventually(t, "initial routing", func() bool {
		return routedVia(nlPeer.IP, "lm-up-nl")() && routedVia(euPeer.IP, "lm-up-nl")()
	})
	if h.dp.MTU != 1392 || h.dp.Down.Name != DownstreamInterface || len(h.dp.Ups) != 2 {
		t.Errorf("dataplane setup = %+v", h.dp)
	}
	if got := h.d.ClientMTU(); got != 1392 { // min(1500-60-S4, 1400-8, 1420-8)
		t.Errorf("ClientMTU = %d", got)
	}
	if peers, _ := h.d.down.Peers(); len(peers) != 2 {
		t.Errorf("downstream device has %d peers", len(peers))
	}

	// NL dies: the eu profile falls back to DE, the nl-only profile is blocked.
	h.setHealthy("10.8.0.2", false)
	eventually(t, "failover to de", routedVia(euPeer.IP, "lm-up-de"))
	eventually(t, "nl-only peer blocked", func() bool { _, ok := h.dp.RouteOf(nlPeer.IP); return !ok })
	st := h.d.Status()
	if st[0].State != health.Down || st[1].State != health.Up || st[1].Clients != 1 {
		t.Errorf("status = %+v", st)
	}

	// NL recovers: the preferred tier wins again.
	h.setHealthy("10.8.0.2", true)
	eventually(t, "return to nl", func() bool {
		return routedVia(euPeer.IP, "lm-up-nl")() && routedVia(nlPeer.IP, "lm-up-nl")()
	})

	// Removing a peer from the store removes it from the device and the kernel.
	h.st.DeletePeer("alice", store.DefaultDevice, "nl")
	eventually(t, "peer removal", func() bool {
		peers, _ := h.d.down.Peers()
		_, routed := h.dp.RouteOf(nlPeer.IP)
		return len(peers) == 1 && !routed
	})

	stop()
	if !h.dp.TornDown {
		t.Error("dataplane not torn down")
	}
}

func TestProbeFailureReresolvesEndpoint(t *testing.T) {
	dns := &fakeResolver{hosts: map[string]string{"localhost": "127.0.0.1"}}
	useResolver(t, dns)
	h := newHarness(t)
	h.run(t)
	eventually(t, "start", h.dp.Ready)

	dns.set("localhost", "127.0.0.2")
	h.setHealthy("10.8.0.2", false) // nl starts failing its probes
	eventually(t, "nl endpoint re-resolved", func() bool {
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		return h.d.ups[0].Conf.Peers[0].Endpoint == "127.0.0.2:51820"
	})
	h.d.mu.Lock()
	de := h.d.ups[1].Conf.Peers[0].Endpoint
	h.d.mu.Unlock()
	if de != "127.0.0.1:51820" {
		t.Errorf("healthy upstream was re-resolved: %q", de)
	}
}

func TestReconcileUnknownProfile(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("bob")
	h.st.AddPeer("bob", store.DefaultDevice, "gone", h.cfg.Subnet)
	h.run(t)
	eventually(t, "start", h.dp.Ready)
	if err := h.d.Reconcile(); err == nil || !strings.Contains(err.Error(), `profile "gone"`) {
		t.Errorf("expected profile error, got %v", err)
	}
}

func TestReload(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	euPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)
	h.run(t)
	ctx := context.Background()
	routedVia := func(want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(euPeer.IP); return got == want }
	}
	eventually(t, "initial routing", routedVia("lm-up-nl"))

	// Reloading an unchanged config must not replace anything, even when the
	// provider file uses a hostname that LoadUpstream resolves to an IP.
	h.st.CreateUser("carol")
	if _, err := os.Stat(filepath.Join(h.dir, "nl.conf")); err != nil {
		t.Fatal(err)
	}
	same, _ := config.Parse([]byte(fmt.Sprintf(harnessConfig, h.dir, h.dir, h.dir)))
	before := h.d.Status()
	h.d.mu.Lock()
	nlDevice := h.d.ups[0].Device
	h.d.mu.Unlock()
	if err := h.d.Reload(ctx, same); err != nil {
		t.Fatal(err)
	}
	h.d.mu.Lock()
	sameDevice := h.d.ups[0].Device == nlDevice
	h.d.mu.Unlock()
	if !sameDevice {
		t.Error("unchanged upstream was replaced on reload")
	}
	if got, _ := h.dp.RouteOf(euPeer.IP); got != "lm-up-nl" {
		t.Errorf("peer moved on a no-op reload: %q", got)
	}
	if after := h.d.Status(); after[0].State != before[0].State {
		t.Errorf("health state lost on no-op reload: %v -> %v", before[0].State, after[0].State)
	}

	// Settings that need a restart reject the reload as a whole.
	bad, _ := config.Parse([]byte(strings.Replace(fmt.Sprintf(harnessConfig, h.dir, h.dir, h.dir), "endpoint:", "listen: 0.0.0.0:1\nendpoint:", 1)))
	if err := h.d.Reload(ctx, bad); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("listen change accepted: %v", err)
	}

	// Drop NL from the config: its peers move to DE, its device is gone.
	providerConf(t, h.dir, "fi", "10.10.0.2/32", 0)
	h.setHealthy("10.10.0.2", true)
	cfg2, err := config.Parse([]byte(fmt.Sprintf(`
endpoint: vpn.example.org:51820
state_dir: %s
upstreams:
  - {name: de, conf: %s/de.conf, geo: DE}
  - {name: fi, conf: %s/fi.conf, geo: FI}
profiles:
  - {name: eu, prefer: [{geo: FI}, {geo: DE}]}
health: {interval: 30ms, probe: 1.1.1.1:443, down_after: 1, up_after: 1}
`, h.dir, h.dir, h.dir)))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(ctx, cfg2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "rerouted to the new preferred upstream", routedVia("lm-up-fi"))
	names := []string{}
	for _, u := range h.dp.Ups {
		names = append(names, u.Name)
	}
	if strings.Join(names, ",") != "lm-up-de,lm-up-fi" {
		t.Errorf("dataplane upstreams after reload = %v", names)
	}
	st := h.d.Status()
	if len(st) != 2 || st[0].Name != "de" || st[1].Name != "fi" || st[1].Clients != 1 {
		t.Errorf("status after reload = %+v", st)
	}
	if h.dp.MTU != 1412 { // de: 1420-8; fi: 1420-8; downstream 1500-60-S4 is larger
		t.Errorf("mtu after reload = %d", h.dp.MTU)
	}

	// Same upstream with a changed provider config is replaced, not kept.
	providerConf(t, h.dir, "fi", "10.11.0.2/32", 1300)
	h.setHealthy("10.11.0.2", true)
	if err := h.d.Reload(ctx, cfg2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "still routed after replacing fi", routedVia("lm-up-fi"))
	if h.dp.MTU != 1292 {
		t.Errorf("mtu after replacing fi = %d", h.dp.MTU)
	}

	// The peer's profile vanished from the config: it is reported, not routed blindly.
	cfg3, _ := config.Parse([]byte(fmt.Sprintf("endpoint: vpn.example.org:51820\nstate_dir: %s\nupstreams: [{name: de, conf: %s/de.conf, geo: DE}]\nprofiles: [{name: other}]\n", h.dir, h.dir)))
	if err := h.d.Reload(ctx, cfg3); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reconcile(); err == nil || !strings.Contains(err.Error(), `profile "eu"`) {
		t.Errorf("expected profile error after reload, got %v", err)
	}
}
