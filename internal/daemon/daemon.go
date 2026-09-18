// Package daemon ties the pieces together: it configures the devices, keeps
// the downstream peer list in sync with the store, probes upstreams and
// routes every peer to the upstream its profile selects.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/tun"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// DownstreamInterface is the TUN users' tunnels terminate on.
const DownstreamInterface = "lm0"

type ProbeFunc func(ctx context.Context, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error)

// TUNFunc creates the interface a device runs on: a kernel TUN in production,
// a netstack one in tests.
type TUNFunc func(name string, mtu int) (tun.Device, error)

type Deps struct {
	Config    *config.Config
	Identity  Identity
	Store     *store.Store
	Dataplane dataplane.Dataplane
	NewTUN    TUNFunc
	Probe     ProbeFunc
	Log       *slog.Logger
}

type Daemon struct {
	id     Identity
	store  *store.Store
	dp     dataplane.Dataplane
	newTUN TUNFunc
	probe  ProbeFunc
	log    *slog.Logger

	mu          sync.Mutex
	cfg         *config.Config
	down        *tunnel.Device
	ups         []*Upstream
	assignments map[netip.Addr]string      // peer address → upstream name
	devicePeers map[awgconf.Key]netip.Addr // peers currently added to the downstream device
	wake        chan struct{}              // nudges Run to reprobe and reconcile now
}

func New(d Deps) *Daemon {
	return &Daemon{
		cfg: d.Config, id: d.Identity, store: d.Store, dp: d.Dataplane, newTUN: d.NewTUN, probe: d.Probe, log: d.Log,
		assignments: map[netip.Addr]string{},
		devicePeers: map[awgconf.Key]netip.Addr{},
		wake:        make(chan struct{}, 1),
	}
}

// ClientMTU is what every issued client config carries.
func (d *Daemon) ClientMTU() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clientMTU()
}

func (d *Daemon) clientMTU() int {
	var ups []mtu.Upstream
	for _, u := range d.ups {
		ups = append(ups, mtu.Upstream{MTU: u.MTU, S4: u.Conf.Interface.S4})
	}
	return mtu.Client(d.id.Params.S4, ups)
}

// Gateway is the downstream interface's own address.
func Gateway(cfg *config.Config) netip.Prefix {
	return netip.PrefixFrom(cfg.Subnet.Addr().Next(), cfg.Subnet.Bits())
}

// Run brings everything up, then probes and reconciles until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.start(ctx); err != nil {
		d.stop()
		return err
	}
	defer d.stop()

	for {
		d.probeAll(ctx)
		if err := d.Reconcile(); err != nil {
			d.log.Error("reconcile failed", "err", err)
		}
		d.mu.Lock()
		interval := d.cfg.Health.Interval
		d.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		case <-d.wake:
		}
	}
}

func (d *Daemon) start(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	listen, err := netip.ParseAddrPort(d.cfg.Listen)
	if err != nil {
		return err
	}
	downTUN, err := d.newTUN(DownstreamInterface, mtu.DefaultWireGuard)
	if err != nil {
		return fmt.Errorf("create %s: %w", DownstreamInterface, err)
	}
	d.down = tunnel.New(DownstreamInterface, downTUN, d.log)
	downCfg := awgconf.Config{Interface: awgconf.Interface{
		PrivateKey: d.id.PrivateKey, ListenPort: listen.Port(), Params: d.id.Params,
	}}
	if err := d.down.Configure(downCfg.UAPI()); err != nil {
		return err
	}
	for _, u := range d.cfg.Upstreams {
		up, err := d.openUpstream(ctx, u)
		if err != nil {
			return err
		}
		d.ups = append(d.ups, up)
	}
	if err := d.dp.Setup(dataplane.Interface{Name: d.down.Name(), Addr: Gateway(d.cfg)}, d.interfaces(), d.clientMTU()); err != nil {
		return fmt.Errorf("dataplane setup: %w", err)
	}
	for _, u := range d.ups {
		if err := u.Device.Up(); err != nil {
			return err
		}
	}
	return d.down.Up()
}

// openUpstream loads the provider config and starts its device (still down).
func (d *Daemon) openUpstream(ctx context.Context, u config.Upstream) (*Upstream, error) {
	up, err := LoadUpstream(ctx, u, d.cfg.Health)
	if err != nil {
		return nil, err
	}
	t, err := d.newTUN(u.InterfaceName(), up.MTU)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", u.InterfaceName(), err)
	}
	up.Device = tunnel.New(u.InterfaceName(), t, d.log)
	if err := up.Device.Configure(up.Conf.UAPI()); err != nil {
		up.Device.Close()
		return nil, err
	}
	return up, nil
}

func (d *Daemon) interfaces() []dataplane.Interface {
	var out []dataplane.Interface
	for _, u := range d.ups {
		out = append(out, dataplane.Interface{Name: u.Device.Name(), Addr: u.Addr})
	}
	return out
}

func (d *Daemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.dp.Teardown(); err != nil {
		d.log.Error("dataplane teardown", "err", err)
	}
	for _, u := range d.ups {
		u.Device.Close()
	}
	if d.down != nil {
		d.down.Close()
	}
}

// Reload applies a new configuration without dropping anyone's tunnel.
// Upstreams are matched by name; one whose provider config changed is
// replaced. Profiles and health settings are swapped in and every peer is
// re-evaluated. Settings that need a restart reject the whole reload.
func (d *Daemon) Reload(ctx context.Context, cfg *config.Config) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := restartRequired(d.cfg, cfg); err != nil {
		return err
	}
	current := map[string]*Upstream{}
	for _, u := range d.ups {
		current[u.Name] = u
	}
	var next []*Upstream
	var errs []error
	for _, u := range cfg.Upstreams {
		old, exists := current[u.Name]
		delete(current, u.Name)
		if exists && old.Source == fileContent(u.Conf) {
			old.Upstream = u
			old.Tracker.SetThresholds(cfg.Health.DownAfter, cfg.Health.UpAfter)
			next = append(next, old)
			continue
		}
		if exists {
			// The interface name is reused, so the old device must go first.
			d.dropUpstream(old)
			d.log.Info("upstream replaced", "upstream", u.Name)
		}
		up, err := d.openUpstream(ctx, u)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		next = append(next, up)
		if !exists {
			d.log.Info("upstream added", "upstream", u.Name)
		}
	}
	for name, old := range current {
		d.dropUpstream(old)
		d.log.Info("upstream removed", "upstream", name)
	}
	d.ups = next
	d.cfg = cfg
	if err := d.dp.SetUpstreams(d.interfaces(), d.clientMTU()); err != nil {
		return fmt.Errorf("dataplane: %w", err)
	}
	for _, u := range d.ups {
		if err := u.Device.Up(); err != nil {
			errs = append(errs, err)
		}
		// A replaced upstream starts unknown again; nothing may stay pinned to it.
		if err := d.dp.SetAlive(u.Device.Name(), u.Tracker.State() == health.Up); err != nil {
			errs = append(errs, err)
		}
	}
	d.log.Info("configuration reloaded", "upstreams", len(d.ups), "profiles", len(cfg.Profiles), "client_mtu", d.clientMTU())
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return errors.Join(errs...)
}

// dropUpstream takes an upstream out of service. Its peers are unrouted so
// the firewall blocks them until the next Reconcile finds them a new one.
func (d *Daemon) dropUpstream(u *Upstream) {
	for ip, name := range d.assignments {
		if name == u.Name {
			if err := d.dp.Unroute(ip); err != nil {
				d.log.Error("unroute", "peer", ip, "err", err)
			}
			delete(d.assignments, ip)
		}
	}
	u.Device.Close()
}

// fileContent is the provider file as it is now, or "" if unreadable; an
// unreadable file counts as changed so the reload reports the error.
func fileContent(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func restartRequired(old, cfg *config.Config) error {
	var changed []string
	if old.Listen != cfg.Listen {
		changed = append(changed, "listen")
	}
	if old.Subnet != cfg.Subnet {
		changed = append(changed, "subnet")
	}
	if old.StateDir != cfg.StateDir {
		changed = append(changed, "state_dir")
	}
	if len(changed) > 0 {
		return fmt.Errorf("reload rejected: %v changed, which needs a restart", changed)
	}
	return nil
}

func (d *Daemon) probeAll(ctx context.Context) {
	d.mu.Lock()
	target, _ := netip.ParseAddrPort(d.cfg.Health.Probe)
	timeout := max(d.cfg.Health.Interval/2, time.Second)
	ups := d.ups
	d.mu.Unlock()
	var wg sync.WaitGroup
	for _, u := range ups {
		wg.Go(func() {
			latency, err := d.probe(ctx, u.Addr.Addr(), target, timeout)
			d.mu.Lock()
			defer d.mu.Unlock()
			before := u.Tracker.State()
			if u.Tracker.Observe(err == nil, latency) {
				d.log.Info("upstream health changed", "upstream", u.Name, "from", before, "to", u.Tracker.State(), "err", err)
				if aerr := d.dp.SetAlive(u.Device.Name(), u.Tracker.State() == health.Up); aerr != nil {
					d.log.Error("set alive", "upstream", u.Name, "err", aerr)
				}
			}
			if err == nil {
				return
			}
			// A failing upstream may simply have moved; providers change IPs.
			if changed, rerr := u.RefreshEndpoint(ctx); rerr != nil {
				d.log.Warn("re-resolve endpoint", "upstream", u.Name, "err", rerr)
			} else if changed {
				d.log.Info("upstream endpoint changed", "upstream", u.Name, "endpoint", u.Conf.Peers[0].Endpoint)
			}
		})
	}
	wg.Wait()
}

// Reconcile makes the device peer list and the kernel routes match the store
// and the current health. It is safe to call from any goroutine.
func (d *Daemon) Reconcile() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	peers, err := d.store.ListPeers()
	if err != nil {
		return err
	}
	var errs []error
	wanted := map[awgconf.Key]bool{}
	for _, p := range peers {
		wanted[p.PublicKey] = true
		if _, ok := d.devicePeers[p.PublicKey]; !ok {
			err := d.down.AddPeer(awgconf.Peer{PublicKey: p.PublicKey, AllowedIPs: []netip.Prefix{netip.PrefixFrom(p.IP, 32)}})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			d.devicePeers[p.PublicKey] = p.IP
		}
		errs = append(errs, d.route(p))
	}
	for key, ip := range d.devicePeers {
		if wanted[key] {
			continue
		}
		errs = append(errs, d.down.RemovePeer(key), d.dp.Unroute(ip))
		delete(d.devicePeers, key)
		delete(d.assignments, ip)
		d.log.Info("peer removed", "peer", ip)
	}
	return errors.Join(errs...)
}

func (d *Daemon) route(p store.Peer) error {
	profile, ok := d.cfg.Profile(p.Profile)
	if !ok {
		return fmt.Errorf("peer %s/%s/%s: profile %q is not in the config", p.User, p.Device, p.Profile, p.Profile)
	}
	current := d.assignments[p.IP]
	want, ok := policy.Select(profile, d.candidates(), current)
	switch {
	case !ok && current != "":
		delete(d.assignments, p.IP)
		d.log.Warn("no healthy upstream, peer is now blocked", "peer", p.IP, "user", p.User, "device", p.Device, "profile", p.Profile)
		return d.dp.Unroute(p.IP)
	case !ok:
		return nil
	case want == current:
		return nil
	}
	if err := d.dp.Route(p.IP, d.interfaceOf(want)); err != nil {
		return err
	}
	d.assignments[p.IP] = want
	d.log.Info("peer routed", "peer", p.IP, "user", p.User, "device", p.Device, "profile", p.Profile, "from", current, "to", want)
	return nil
}

func (d *Daemon) interfaceOf(upstream string) string {
	for _, u := range d.ups {
		if u.Name == upstream {
			return u.Device.Name()
		}
	}
	return ""
}

func (d *Daemon) candidates() []policy.Upstream {
	out := make([]policy.Upstream, len(d.ups))
	for i, u := range d.ups {
		out[i] = policy.Upstream{
			Upstream: u.Upstream,
			Healthy:  u.Tracker.State() == health.Up,
			Latency:  u.Tracker.Latency(),
		}
	}
	return out
}

type UpstreamStatus struct {
	Name          string
	Interface     string
	State         health.State
	Latency       time.Duration
	LastHandshake time.Time
	Clients       int // Lotsman peers currently routed through this upstream
}

func (d *Daemon) Status() []UpstreamStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []UpstreamStatus
	for _, u := range d.ups {
		s := UpstreamStatus{Name: u.Name, Interface: u.Device.Name(), State: u.Tracker.State(), Latency: u.Tracker.Latency()}
		if peers, err := u.Device.Peers(); err == nil && len(peers) == 1 {
			s.LastHandshake = peers[0].LastHandshake
		}
		for _, name := range d.assignments {
			if name == u.Name {
				s.Clients++
			}
		}
		out = append(out, s)
	}
	return out
}
