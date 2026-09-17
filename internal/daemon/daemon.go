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
	"sync"
	"time"

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

type Daemon struct {
	cfg   *config.Config
	id    Identity
	store *store.Store
	dp    dataplane.Dataplane
	down  *tunnel.Device
	ups   []*Upstream
	probe ProbeFunc
	log   *slog.Logger

	mu          sync.Mutex
	assignments map[netip.Addr]string      // peer address → upstream name
	devicePeers map[awgconf.Key]netip.Addr // peers currently added to the downstream device
}

func New(cfg *config.Config, id Identity, st *store.Store, dp dataplane.Dataplane,
	down *tunnel.Device, ups []*Upstream, probe ProbeFunc, log *slog.Logger) *Daemon {
	return &Daemon{
		cfg: cfg, id: id, store: st, dp: dp, down: down, ups: ups, probe: probe, log: log,
		assignments: map[netip.Addr]string{},
		devicePeers: map[awgconf.Key]netip.Addr{},
	}
}

// ClientMTU is what every issued client config carries.
func (d *Daemon) ClientMTU() int {
	var ups []mtu.Upstream
	for _, u := range d.ups {
		ups = append(ups, mtu.Upstream{MTU: u.MTU, S4: u.Conf.Interface.S4})
	}
	return mtu.Client(d.id.Params.S4, ups)
}

// Gateway is the downstream interface's own address.
func (d *Daemon) Gateway() netip.Prefix {
	return netip.PrefixFrom(d.cfg.Subnet.Addr().Next(), d.cfg.Subnet.Bits())
}

// Run brings everything up, then probes and reconciles until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.start(); err != nil {
		d.stop()
		return err
	}
	defer d.stop()

	d.probeAll(ctx)
	if err := d.Reconcile(); err != nil {
		return err
	}
	ticker := time.NewTicker(d.cfg.Health.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.probeAll(ctx)
			if err := d.Reconcile(); err != nil {
				d.log.Error("reconcile failed", "err", err)
			}
		}
	}
}

func (d *Daemon) start() error {
	listen, err := netip.ParseAddrPort(d.cfg.Listen)
	if err != nil {
		return err
	}
	downCfg := awgconf.Config{Interface: awgconf.Interface{
		PrivateKey: d.id.PrivateKey, ListenPort: listen.Port(), Params: d.id.Params,
	}}
	if err := d.down.Configure(downCfg.UAPI()); err != nil {
		return err
	}
	var ifaces []dataplane.Interface
	for _, u := range d.ups {
		if err := u.Device.Configure(u.Conf.UAPI()); err != nil {
			return err
		}
		ifaces = append(ifaces, dataplane.Interface{Name: u.Device.Name(), Addr: u.Addr})
	}
	if err := d.dp.Setup(dataplane.Interface{Name: d.down.Name(), Addr: d.Gateway()}, ifaces, d.ClientMTU()); err != nil {
		return fmt.Errorf("dataplane setup: %w", err)
	}
	for _, u := range d.ups {
		if err := u.Device.Up(); err != nil {
			return err
		}
	}
	return d.down.Up()
}

func (d *Daemon) stop() {
	if err := d.dp.Teardown(); err != nil {
		d.log.Error("dataplane teardown", "err", err)
	}
	for _, u := range d.ups {
		u.Device.Close()
	}
	d.down.Close()
}

func (d *Daemon) probeAll(ctx context.Context) {
	target, _ := netip.ParseAddrPort(d.cfg.Health.Probe)
	timeout := max(d.cfg.Health.Interval/2, time.Second)
	var wg sync.WaitGroup
	for _, u := range d.ups {
		wg.Go(func() {
			latency, err := d.probe(ctx, u.Addr.Addr(), target, timeout)
			before := u.Tracker.State()
			if u.Tracker.Observe(err == nil, latency) {
				d.log.Info("upstream health changed", "upstream", u.Name, "from", before, "to", u.Tracker.State(), "err", err)
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
		return fmt.Errorf("peer %s/%s: profile %q is not in the config", p.User, p.Profile, p.Profile)
	}
	current := d.assignments[p.IP]
	want, ok := policy.Select(profile, d.candidates(), current)
	switch {
	case !ok && current != "":
		delete(d.assignments, p.IP)
		d.log.Warn("no healthy upstream, peer is now blocked", "peer", p.IP, "user", p.User, "profile", p.Profile)
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
	d.log.Info("peer routed", "peer", p.IP, "user", p.User, "profile", p.Profile, "from", current, "to", want)
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
