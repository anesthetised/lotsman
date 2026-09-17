package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	conf := &awgconf.Config{
		Interface: awgconf.Interface{
			PrivateKey: key, Addresses: []netip.Prefix{netip.MustParsePrefix(addr)}, MTU: mtu,
			Params: awgconf.Params{S1: 5, S2: 6, S3: 7, S4: 8, H1: awgconf.Range{Lo: 10, Hi: 10}, H2: awgconf.Range{Lo: 20, Hi: 20}, H3: awgconf.Range{Lo: 30, Hi: 30}, H4: awgconf.Range{Lo: 40, Hi: 40}},
		},
		Peers: []awgconf.Peer{{PublicKey: peer.Public(), Endpoint: "192.0.2.1:51820", AllowedIPs: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}},
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
	if u.Addr.String() != "10.8.0.2/32" || u.MTU != 1420 || u.Conf.Peers[0].Endpoint != "192.0.2.1:51820" {
		t.Errorf("upstream = %+v", u)
	}

	bad := map[string]string{
		"split tunnel": strings.Replace(mustRead(t, good), "0.0.0.0/0", "10.0.0.0/8", 1),
		"no peer":      strings.Split(mustRead(t, good), "\n[Peer]")[0],
		"no ipv4":      strings.Replace(mustRead(t, good), "10.8.0.2/32", "fd00::2/128", 1),
		"no endpoint":  strings.Replace(mustRead(t, good), "Endpoint = 192.0.2.1:51820\n", "", 1),
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

	t.Run("hostname endpoint is resolved", func(t *testing.T) {
		content := strings.Replace(mustRead(t, good), "192.0.2.1:51820", "localhost:51820", 1)
		path := filepath.Join(dir, "host.conf")
		os.WriteFile(path, []byte(content), 0o600)
		u, err := LoadUpstream(context.Background(), config.Upstream{Name: "host", Conf: path}, h)
		if err != nil {
			t.Fatal(err)
		}
		if u.Conf.Peers[0].Endpoint != "127.0.0.1:51820" {
			t.Errorf("endpoint = %q", u.Conf.Peers[0].Endpoint)
		}
	})
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
	mu      sync.Mutex
	healthy map[netip.Addr]bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	nl := providerConf(t, dir, "nl", "10.8.0.2/32", 1400)
	de := providerConf(t, dir, "de", "10.9.0.2/32", 0)
	cfg, err := config.Parse([]byte(`
endpoint: vpn.example.org:51820
state_dir: ` + dir + `
upstreams:
  - {name: nl, conf: ` + nl + `, geo: NL}
  - {name: de, conf: ` + de + `, geo: DE}
profiles:
  - {name: nl, prefer: [{geo: NL}]}
  - {name: eu, prefer: [{geo: NL}, {geo: DE}]}
health: {interval: 30ms, probe: 1.1.1.1:443, down_after: 2, up_after: 1}
`))
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

	newDev := func(name string, addr netip.Addr) *tunnel.Device {
		tn, _, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, 1400)
		if err != nil {
			t.Fatal(err)
		}
		return tunnel.New(name, tn, quiet)
	}
	var ups []*Upstream
	h := &harness{st: st, dp: fake.New(), cfg: cfg, healthy: map[netip.Addr]bool{}}
	for _, u := range cfg.Upstreams {
		up, err := LoadUpstream(context.Background(), u, cfg.Health)
		if err != nil {
			t.Fatal(err)
		}
		up.Device = newDev(u.InterfaceName(), up.Addr.Addr())
		h.healthy[up.Addr.Addr()] = true
		ups = append(ups, up)
	}
	probe := func(_ context.Context, source netip.Addr, _ netip.AddrPort, _ time.Duration) (time.Duration, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.healthy[source] {
			return time.Millisecond, nil
		}
		return 0, errors.New("probe failed")
	}
	down := newDev(DownstreamInterface, netip.MustParseAddr("10.77.0.1"))
	h.d = New(cfg, id, st, h.dp, down, ups, probe, quiet)
	return h
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
	nlPeer, _ := h.st.AddPeer("alice", "nl", h.cfg.Subnet)
	euPeer, _ := h.st.AddPeer("alice", "eu", h.cfg.Subnet)

	if got := h.d.ClientMTU(); got != 1392 { // min(1500-60-S4, 1400-8, 1420-8)
		t.Errorf("ClientMTU = %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.d.Run(ctx) }()

	routedVia := func(peer netip.Addr, want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(peer); return got == want }
	}
	eventually(t, "initial routing", func() bool {
		return routedVia(nlPeer.IP, "lm-up-nl")() && routedVia(euPeer.IP, "lm-up-nl")()
	})
	if h.dp.MTU != 1392 || h.dp.Down.Name != DownstreamInterface || len(h.dp.Ups) != 2 {
		t.Errorf("dataplane setup = %+v", h.dp)
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
	h.st.DeletePeer("alice", "nl")
	eventually(t, "peer removal", func() bool {
		peers, _ := h.d.down.Peers()
		_, routed := h.dp.RouteOf(nlPeer.IP)
		return len(peers) == 1 && !routed
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !h.dp.TornDown {
		t.Error("dataplane not torn down")
	}
}

func TestReconcileUnknownProfile(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("bob")
	h.st.AddPeer("bob", "gone", h.cfg.Subnet)
	if err := h.d.Reconcile(); err == nil || !strings.Contains(err.Error(), `profile "gone"`) {
		t.Errorf("expected profile error, got %v", err)
	}
}
