// Package daemon ties the pieces together: it configures the devices, keeps
// the downstream peer list in sync with the store, probes upstreams and
// routes every peer to the upstream its device's route selects.
package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/mtu"
	"github.com/anesthetised/lotsman/internal/policy"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
	"github.com/anesthetised/lotsman/internal/tunnel"
)

// DownstreamInterface is the TUN users' tunnels terminate on.
const DownstreamInterface = "lm0"

// ProbeFunc checks one target through an upstream by a TCP handshake, as
// health.ProbeOnInterface does.
type ProbeFunc func(ctx context.Context, iface string, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error)

// DNSProbeFunc queries one resolver through an upstream over network, "udp"
// or "tcp", as health.DNSOnInterface does.
type DNSProbeFunc func(ctx context.Context, network, iface string, source netip.Addr, target netip.AddrPort, timeout time.Duration) (time.Duration, error)

// TUNFunc creates the interface a device runs on: a kernel TUN in production,
// a netstack one in tests.
type TUNFunc func(name string, mtu int) (tun.Device, error)

type Deps struct {
	Version   string
	Config    *config.Config
	Identity  Identity
	Store     *store.Store
	Dataplane dataplane.Dataplane
	NewTUN    TUNFunc
	Probe     ProbeFunc    // AWG upstreams
	ProbeDNS  DNSProbeFunc // proxy upstreams
	Transfer  TransferFunc // every upstream, when health.transfer or health.speed is set
	Upload    UploadFunc   // every upstream, when health.transfer.upload_url or health.speed is set
	Counters  CountersFunc // upstream interfaces' byte counters; none when nil
	// Proxy runs proxy upstreams' stacks: proxy.InProcess when nil, as in
	// tests on netstack TUNs, which have no descriptor to hand to a helper
	// process; serve runs each in a helper (internal/proxy/helper).
	Proxy proxy.Runner
	Log   *slog.Logger
}

type Daemon struct {
	version  string
	id       Identity
	store    *store.Store
	dp       dataplane.Dataplane
	newTUN   TUNFunc
	probe    ProbeFunc
	probeDNS DNSProbeFunc
	transfer TransferFunc
	upload   UploadFunc
	runProxy proxy.Runner
	log      *slog.Logger

	interfaceCounters CountersFunc
	sampler           traffic.Sampler // the traffic of peers and upstreams; Run starts it

	transferRuns sync.WaitGroup // transfer probes and speed tests in flight; Run waits for them

	// applyMu serializes the changes to d.ups that open, close or bring up
	// links: Reload, reopenMissing and stop. They release d.mu while a link
	// waits for a helper, which may take seconds, so Status, Metrics and
	// Reconcile still answer; applyMu keeps d.ups as they left it
	// meanwhile. It is taken before d.mu.
	applyMu sync.Mutex

	mu             sync.Mutex
	cfg            *config.Config
	down           *tunnel.Device
	ups            []*Upstream
	assignments    map[netip.Addr]string      // peer address → upstream name
	unroutePending map[netip.Addr]bool        // cleanup must finish before routing this address again
	devicePeers    map[awgconf.Key]store.Peer // added peers, retained until device and route cleanup succeed
	aliveFailed    map[string]string          // upstream → last SetAlive error; the kernel is stale until it succeeds
	appliedAlive   map[*Upstream]bool         // the last successful SetAlive for each link
	dpStale        bool                       // d.ups is not yet pushed to the dataplane; retried every round
	dpSetUp        bool                       // dataplane Setup was called, so stop must tear it down
	notUp          map[*Upstream]bool         // opened upstreams whose device still has to come up
	reauthorize    bool                       // the firewall was reinstalled; routed peers must be routed again
	stopped        bool                       // stop has run; a late link exit changes nothing
	failures       map[failure]string         // retried failure → last logged error, so repeats stay quiet
	fetched        map[string]time.Time       // upstream → last dynamic key fetch, to space refetches on failed probes
	wake           chan struct{}              // nudges Run to reprobe and reconcile now
	ready          chan struct{}              // closed after the first reconcile
	rounds         chan struct{}              // one pending token once a round has finished since Rounds was last read

	transfers         map[*Upstream]*transferState // throughput health while health.transfer is set
	speeds            map[*Upstream]*speedState    // speed tests while health.speed is set
	incidents         map[incidentKey]*openIncident
	expiries          map[string]time.Time
	outlineUsage      map[string]store.OutlineUsage
	subscriptionTimer *time.Timer

	// Monotonic counters for metrics.
	probes   map[string]*[2]uint64 // upstream → {ok, fail}
	reroutes map[string]uint64     // destination upstream → moves
	reloads  [2]uint64             // {ok, error}
	repairs  uint64                // firewall reinstalls after another tool removed it
	restarts map[string]uint64     // proxy upstream → links that stopped on their own

	transferCounts map[transferCount]*[2]uint64 // upstream and direction → transfer probes {ok, fail}
}

func New(d Deps) *Daemon {
	runProxy := d.Proxy
	if runProxy == nil {
		runProxy = proxy.InProcess
	}
	return &Daemon{
		version: d.Version, cfg: d.Config, id: d.Identity, store: d.Store, dp: d.Dataplane, newTUN: d.NewTUN,
		probe: d.Probe, probeDNS: d.ProbeDNS, transfer: d.Transfer, upload: d.Upload, runProxy: runProxy, log: d.Log,
		interfaceCounters: d.Counters,

		assignments:    map[netip.Addr]string{},
		unroutePending: map[netip.Addr]bool{},
		devicePeers:    map[awgconf.Key]store.Peer{},
		aliveFailed:    map[string]string{},
		appliedAlive:   map[*Upstream]bool{},
		notUp:          map[*Upstream]bool{},
		failures:       map[failure]string{},
		fetched:        map[string]time.Time{},
		transfers:      map[*Upstream]*transferState{},
		speeds:         map[*Upstream]*speedState{},
		incidents:      map[incidentKey]*openIncident{},
		wake:           make(chan struct{}, 1),
		ready:          make(chan struct{}),
		rounds:         make(chan struct{}, 1),
		probes:         map[string]*[2]uint64{},
		transferCounts: map[transferCount]*[2]uint64{},
		reroutes:       map[string]uint64{},
		restarts:       map[string]uint64{},
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
		ups = append(ups, u.MTULimit())
	}
	return mtu.Client(d.id.Params.S4, ups)
}

// Gateway is the downstream interface's own address.
func Gateway(cfg *config.Config) netip.Prefix {
	return netip.PrefixFrom(cfg.Subnet.Addr().Next(), cfg.Subnet.Bits())
}

// Ready is closed once the daemon has probed and reconciled for the first time.
func (d *Daemon) Ready() <-chan struct{} { return d.ready }

// Rounds receives a value after each health round that follows the first one
// (Ready covers that), however it was triggered: the interval elapsing, a
// reload or another wake-up. Notifications coalesce, so a slow reader sees
// that something finished, not how many rounds. There is one reader.
func (d *Daemon) Rounds() <-chan struct{} { return d.rounds }

// Run brings everything up, then probes and reconciles until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.start(ctx); err != nil {
		d.stop()
		return err
	}
	defer d.stop()
	// Transfer probes and speed tests end once ctx is done; none may outlive
	// the devices.
	defer d.transferRuns.Wait()
	defer d.watchSendErrors(ctx)()
	defer d.sampleTraffic(ctx)() // stopped before d.stop closes the devices
	defer d.recordHistory(ctx)() // reads the sampler and the trackers, so it stops first too
	// Stopped before d.stop, so no repair races the teardown.
	watchCtx, stopWatch := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		d.watchFirewall(d.dp.Watch(watchCtx))
	}()
	defer func() {
		stopWatch()
		<-watched
	}()

	first := true
	for {
		d.probeAll(ctx)
		if err := d.Reconcile(); err != nil {
			d.log.Error("reconcile failed", "err", err)
		}
		if first {
			close(d.ready)
			first = false
		} else {
			select {
			case d.rounds <- struct{}{}:
			default:
			}
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
	d.closeLeftoverIncidents(time.Now())
	d.mu.Lock()
	cfg := d.cfg
	if err := d.loadSubscriptions(); err != nil {
		d.log.Error("load subscription limits", "err", err)
	}
	d.mu.Unlock()
	// Provider configs are loaded and resolved before d.mu is taken, so slow
	// DNS never blocks Status or Metrics. The serve loop delivers SIGHUP only
	// after Ready, so no reload or reconcile races a half-started daemon.
	var ups []*Upstream
	noKey := map[string]error{}
	for _, u := range cfg.Upstreams {
		up, err := d.loadResolved(ctx, u, cfg.Health, cfg.StateDir, nil)
		if errors.Is(err, errNoKey) {
			// Down until reopenMissing gets a key, like an upstream a
			// reload failed to open.
			noKey[u.Name] = err
			continue
		}
		if err != nil {
			return err
		}
		ups = append(ups, up)
	}
	if err := d.setup(ups, noKey); err != nil {
		return err
	}
	// A helper takes a moment to start, and the metrics endpoint already
	// answers, so links come up without d.mu. Nothing else changes d.ups
	// before Ready.
	for _, u := range ups {
		if err := u.link.Up(); err != nil {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.down.Up()
}

// setup is start's part under d.mu: it opens the downstream device and the
// loaded upstreams' links (still down) and sets up the dataplane.
func (d *Daemon) setup(ups []*Upstream, noKey map[string]error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for name, err := range noKey {
		d.openFailed(name, err)
	}
	listen, err := netip.ParseAddrPort(d.cfg.Listen)
	if err != nil {
		return err
	}
	downTUN, err := d.newTUN(DownstreamInterface, mtu.DefaultWireGuard)
	if err != nil {
		return fmt.Errorf("create %s: %w", DownstreamInterface, err)
	}
	d.down = tunnel.NewBound(DownstreamInterface, downTUN, listen.Addr(), d.log)
	downCfg := awgconf.Config{Interface: awgconf.Interface{
		PrivateKey: d.id.PrivateKey, ListenPort: listen.Port(), Params: d.id.Shared(d.cfg.AWG),
	}}
	if err := d.down.Configure(downCfg.UAPI()); err != nil {
		return err
	}
	for _, up := range ups {
		if err := d.openDevice(up); err != nil {
			return err
		}
		d.ups = append(d.ups, up)
	}
	metricsAddr, _ := netip.ParseAddrPort(d.cfg.Metrics.Listen) // validated by config; zero when unset
	// Set before Setup: a failed Setup may have changed the host too.
	d.dpSetUp = true
	if err := d.dp.Setup(dataplane.Interface{Name: d.down.Name(), Addr: Gateway(d.cfg)}, d.interfaces(), d.clientMTU(), metricsAddr); err != nil {
		return fmt.Errorf("dataplane setup: %w", err)
	}
	return nil
}

// openDevice creates the TUN for a loaded and resolved upstream and starts
// its link (still down).
func (d *Daemon) openDevice(up *Upstream) error {
	t, err := d.newTUN(up.InterfaceName(), up.MTU)
	if err != nil {
		return fmt.Errorf("create %s: %w", up.InterfaceName(), err)
	}
	return up.open(t, d.log, d.runProxy, func(err error) { d.linkExited(up, err) })
}

// linkExited takes an upstream whose link stopped on its own, such as a
// proxy helper that crashed, out of service at once instead of after
// down_after failed probes: it is marked down and its peers move now. It
// stays not up until activate brings it up again, every health round (the
// link may hold that back after repeated quick exits). The wake makes the
// first attempt immediate. Once up, probes bring it back as usual.
func (d *Daemon) linkExited(u *Upstream, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || !slices.Contains(d.ups, u) {
		return // stopping, or replaced or removed by a reload
	}
	d.restarts[u.Name]++
	u.exits++
	d.notUp[u] = true
	before := u.Tracker.State()
	if u.Tracker.Fail() {
		d.log.Info("upstream health changed", "upstream", u.Name, "from", before, "to", u.Tracker.State(), "err", err)
	}
	d.setIncident(u.Name, store.IncidentDown, true, errText(err), time.Now())
	_ = d.applyAlive(u) // drops pins to it; a failure is logged and retried next round
	if err := d.reconcile(); err != nil {
		d.log.Error("reconcile after upstream exit", "err", err)
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *Daemon) interfaces() []dataplane.Interface {
	var out []dataplane.Interface
	for _, u := range d.ups {
		out = append(out, dataplane.Interface{Name: u.InterfaceName(), Addr: u.Addr})
	}
	return out
}

func (d *Daemon) stop() {
	// Waits for a reload that is opening or closing links.
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	for _, l := range d.teardown() {
		// A helper may take seconds to exit; Status and Metrics still answer.
		l.Close()
	}
}

// teardown is stop's part under d.mu. It returns the upstreams' links,
// which the caller closes without d.mu.
func (d *Daemon) teardown() []link {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	if d.subscriptionTimer != nil {
		d.subscriptionTimer.Stop()
	}
	// A start that failed before dataplane setup changed no rule, route or
	// firewall, so it removes none: the host's may not be its own.
	var err error
	if d.dpSetUp {
		err = d.dp.Teardown() // disables ingress before removing forwarding protection
		if err != nil {
			d.log.Error("dataplane teardown", "err", err)
		}
	}
	if d.down != nil {
		d.down.Close()
	}
	// Closing downstream also stops ingress if disabling it failed. Retry
	// cleanup before upstream TUNs disappear, while ownership is still known.
	if err != nil {
		if retryErr := d.dp.Teardown(); retryErr != nil {
			d.log.Error("dataplane teardown after downstream close", "err", retryErr)
		}
	}
	var links []link
	for _, u := range d.ups {
		links = append(links, u.link)
	}
	return links
}

// Reload applies a new configuration without dropping anyone's tunnel.
// Upstreams are matched by name; one whose link config changed is
// replaced once the new config loads and resolves, otherwise the running one
// stays and the error is returned. Health settings are swapped in and every
// peer is re-evaluated against its route. Settings that need a restart reject
// the whole reload. Removing upstreams never does: a route that named one
// simply matches less, and a device left without an exit is blocked.
func (d *Daemon) Reload(ctx context.Context, cfg *config.Config) error {
	d.mu.Lock()
	err := restartRequired(d.cfg, cfg)
	if err != nil {
		d.reloads[1]++
	}
	running := map[string]*Upstream{}
	for _, u := range d.ups {
		running[u.Name] = u
	}
	d.mu.Unlock()
	if err != nil {
		return err
	}

	// Provider configs are read, and changed ones resolved, without d.mu, so
	// slow DNS never blocks Status or Reconcile. A loaded upstream is private
	// until it is applied, and Source never changes after load.
	type loaded struct {
		up    *Upstream
		basis *Upstream // what ran under this name when it was loaded
		err   error
	}
	loads := make([]loaded, len(cfg.Upstreams))
	for i, u := range cfg.Upstreams {
		// Health settings are passed in because d.cfg switches to the
		// incoming configuration only when this reload applies.
		basis := running[u.Name]
		up, err := d.loadResolved(ctx, u, cfg.Health, cfg.StateDir, basis)
		loads[i] = loaded{up, basis, err}
	}

	// Restart-only settings never change live, so the check above still holds.
	// Closing a link and bringing one up wait for its helper, so they run
	// without d.mu; applyMu keeps other changes to d.ups out until this one
	// is done.
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return errors.New("reload rejected: the daemon is stopping")
	}
	current := map[string]*Upstream{}
	for _, u := range d.ups {
		current[u.Name] = u
	}
	// What runs under each name once applied: a running upstream that stays,
	// a new one still to open, or nil.
	type entry struct {
		up      *Upstream
		open    bool // new: open it
		replace bool // it replaces a running upstream of the same name
	}
	entries := make([]entry, len(cfg.Upstreams))
	// Changes to running upstreams that stay, told to their links without d.mu.
	type update struct {
		u        *Upstream
		name     string
		maxFlows int
		key      *dynamicKey // fetched again for this reload; nil for a static key
		resolved string
	}
	var updates []update
	var closing []link // links of dropped upstreams
	var kept []*Upstream
	var errs []error
	for i, u := range cfg.Upstreams {
		old, exists := current[u.Name]
		delete(current, u.Name)
		up, err := loads[i].up, loads[i].err
		unchanged := err == nil && up.sameLinkConfig(old)
		if err == nil && !unchanged && loads[i].basis != old {
			// A concurrent reload added, replaced or removed this upstream
			// after it was loaded; whatever runs now is kept.
			err = fmt.Errorf("upstream %s: changed by a concurrent reload, not applied", u.Name)
		}
		if exists && (unchanged || err != nil) {
			// A broken or unreadable replacement never takes the running
			// upstream down.
			if err != nil {
				errs = append(errs, err)
			}
			// Labels and limits apply live. Keep the settings of the
			// running link when a replacement fails.
			u.Kind = old.Kind
			u.DisableUDPGSO = old.disableUDPGSO
			old.Upstream = u
			old.Tracker.SetThresholds(cfg.Health.DownAfter, cfg.Health.UpAfter)
			w := update{u: old, name: u.Name, maxFlows: u.MaxFlows}
			if unchanged && up.Location != nil {
				// Fetched again for this reload: a rotated key applies now.
				w.key, w.resolved = &dynamicKey{up.Key, up.keyText}, up.Resolved
			}
			updates = append(updates, w)
			entries[i] = entry{up: old}
			kept = append(kept, old)
			continue
		}
		if err != nil {
			errs = append(errs, err)
			d.openFailed(u.Name, err)
			continue
		}
		if exists {
			// The interface name is reused, so the old link is closed before
			// the new one opens.
			closing = append(closing, d.dropUpstream(old))
			d.log.Info("upstream replaced", "upstream", u.Name)
			if old.Location != nil && up.Location == nil {
				d.forgetKey(cfg.StateDir, u.Name)
			}
		}
		entries[i] = entry{up: up, open: true, replace: exists}
	}
	for name, old := range current {
		closing = append(closing, d.dropUpstream(old))
		d.log.Info("upstream removed", "upstream", name)
		if old.Location != nil {
			d.forgetKey(cfg.StateDir, name)
		}
	}
	// Until the new upstreams open, they are out of service like ones that
	// are not up yet; routes and labels already match.
	d.ups = kept
	d.cfg = cfg
	d.trimTransfers()
	if cfg.Health.Speed == nil {
		clear(d.speeds)
	}
	d.mu.Unlock()

	for _, l := range closing {
		l.Close()
	}
	for _, w := range updates {
		if l, ok := w.u.link.(*proxyLink); ok {
			if err := l.SetMaxFlows(w.maxFlows); err != nil {
				// A helper that failed is restarted with the new limit.
				d.log.Warn("max_flows not applied to the running upstream", "upstream", w.name, "err", err)
			}
		}
		if w.key != nil {
			if err := d.applyServer(w.u, w.key, w.resolved, ""); err != nil {
				errs = append(errs, fmt.Errorf("upstream %s: %w", w.name, err))
			}
		}
	}

	d.mu.Lock()
	var next []*Upstream
	for i, e := range entries {
		name := cfg.Upstreams[i].Name
		if e.up == nil {
			continue
		}
		if e.open {
			if err := d.openDevice(e.up); err != nil {
				errs = append(errs, err)
				d.openFailed(name, err)
				continue
			}
			d.notUp[e.up] = true
			delete(d.failures, failure{"open", name})
			if !e.replace {
				d.log.Info("upstream added", "upstream", name)
			}
		}
		next = append(next, e.up)
	}
	d.ups = next
	d.dpStale = true
	d.mu.Unlock()

	errs = append(errs, d.activate()...)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.log.Info("configuration reloaded", "upstreams", len(d.ups), "client_mtu", d.clientMTU())
	select {
	case d.wake <- struct{}{}:
	default:
	}
	if len(errs) == 0 {
		d.reloads[0]++
	} else {
		d.reloads[1]++
	}
	return errors.Join(errs...)
}

// dropUpstream takes an upstream out of service. Failed peer cleanup stays
// pending until Reconcile can unroute it, before selecting another upstream.
// It returns u's link, which the caller closes without d.mu: a helper may
// take seconds to exit. Callers hold d.mu.
func (d *Daemon) dropUpstream(u *Upstream) link {
	for ip, name := range d.assignments {
		if name == u.Name {
			if err := d.unroute(ip); err != nil {
				d.log.Error("unroute", "peer", ip, "err", err)
			}
		}
	}
	delete(d.aliveFailed, u.Name)
	delete(d.appliedAlive, u)
	delete(d.notUp, u)
	delete(d.failures, failure{"up", u.Name})
	delete(d.transfers, u)
	delete(d.speeds, u)
	d.closeIncidents(u.Name, time.Now())
	return u.link
}

// loadResolved loads a provider config and, unless it matches the running
// upstream basis, resolves its endpoint within the probe timeout. A dynamic
// key is fetched in that time even when it matches, so a reload picks up a
// rotated key. It takes no lock; the returned upstream is private until
// applied.
func (d *Daemon) loadResolved(ctx context.Context, u config.Upstream, h config.Health, stateDir string, basis *Upstream) (*Upstream, error) {
	up, err := LoadUpstream(u, h)
	if err != nil {
		return nil, err
	}
	unchanged := up.sameLinkConfig(basis)
	if unchanged && up.Location == nil {
		return up, nil
	}
	if up.Location != nil {
		// A running upstream keeps its key when the fetch fails, so only a
		// new one falls back on the cache.
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout(probeTimeout(h)))
		err := d.acquireKey(fctx, up, stateDir, !unchanged)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout(h))
	defer cancel()
	if err := up.Resolve(ctx); err != nil {
		return nil, err
	}
	return up, nil
}

// keyRefreshInterval spaces the fetches of a dynamic key while its upstream
// fails its probes, so a dead server does not turn into a stream of requests
// to the key's URL.
const keyRefreshInterval = 5 * time.Minute

// errNoKey marks a dynamic upstream with neither a fetched nor a cached key.
// It does not stop the daemon: the upstream stays down and is retried every
// health round.
var errNoKey = errors.New("no usable cached key either")

// acquireKey fetches the dynamic key of an upstream that is not running yet
// and caches it. If the fetch fails and useCache is set, the last good key
// cached for the same URL is used instead. It takes no lock.
func (d *Daemon) acquireKey(ctx context.Context, up *Upstream, stateDir string, useCache bool) error {
	d.mu.Lock()
	d.fetched[up.Name] = time.Now()
	d.mu.Unlock()
	k, err := fetchKey(ctx, up.Location)
	if err == nil {
		up.setKey(k)
		d.cacheKey(stateDir, up.Name, up.Location, k)
		return nil
	}
	err = fmt.Errorf("upstream %s: %w", up.Name, err)
	if !useCache {
		return err
	}
	cached, cacheErr := loadCachedKey(stateDir, up.Name, up.Location)
	if cacheErr != nil {
		return fmt.Errorf("%w; %w", err, errNoKey)
	}
	up.setKey(cached)
	d.log.Warn("dynamic key fetch failed, using the cached key", "upstream", up.Name, "err", err)
	return nil
}

// fetchDue reports whether a running dynamic upstream may fetch its key
// again, and if so counts this as its fetch.
func (d *Daemon) fetchDue(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if time.Since(d.fetched[name]) < keyRefreshInterval {
		return false
	}
	d.fetched[name] = time.Now()
	return true
}

// cacheKey keeps k as the upstream's last good key; a failure only costs the
// fallback, so it is logged.
func (d *Daemon) cacheKey(stateDir, name string, loc *proxy.Location, k dynamicKey) {
	if err := saveCachedKey(stateDir, name, loc, k); err != nil {
		d.log.Warn("could not cache the dynamic key", "upstream", name, "err", err)
	}
}

// forgetKey deletes the cached key of an upstream that no longer has a
// dynamic key. Callers hold d.mu.
func (d *Daemon) forgetKey(stateDir, name string) {
	delete(d.fetched, name)
	if err := removeCachedKey(stateDir, name); err != nil {
		d.log.Warn("could not delete the cached dynamic key", "upstream", name, "err", err)
	}
}

// applyServer points a running upstream at a fetched key, if it is set and
// differs from the running one, and at resolved, and logs what changed. A
// new key is cached in cacheDir unless it is "". A helper link waits for
// its helper's answer, so the link is told without d.mu, and u.serverMu
// keeps the changes to one upstream in order. Nothing changes once a reload
// has replaced or removed u. Callers hold neither lock.
func (d *Daemon) applyServer(u *Upstream, k *dynamicKey, resolved, cacheDir string) error {
	u.serverMu.Lock()
	defer u.serverMu.Unlock()
	d.mu.Lock()
	running := slices.Contains(d.ups, u)
	text, newKey, changed := u.nextServer(k, resolved)
	d.mu.Unlock()
	if !running || !changed {
		return nil
	}
	if err := u.link.SetServer(text, resolved); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains(d.ups, u) {
		return nil // removed meanwhile, its link closed
	}
	u.commitServer(k, newKey, resolved)
	if !newKey {
		d.log.Info("upstream endpoint changed", "upstream", u.Name, "endpoint", resolved)
		return nil
	}
	d.log.Info("upstream key changed", "upstream", u.Name, "key", u.Key.String(), "endpoint", u.Resolved)
	if cacheDir != "" {
		// Local and small, so written under d.mu: the upstream cannot be
		// removed between applying the key and caching it.
		d.cacheKey(cacheDir, u.Name, u.Location, *k)
	}
	return nil
}

// failure keys an error that is retried every health round.
type failure struct{ kind, name string }

// fail logs err unless the same error was already logged for key.
func (d *Daemon) fail(key failure, msg string, err error, args ...any) {
	if d.failures[key] != err.Error() {
		d.log.Error(msg, append(args, "err", err)...)
	}
	d.failures[key] = err.Error()
}

// openFailed reports an upstream that is configured but could not be opened.
func (d *Daemon) openFailed(name string, err error) {
	d.fail(failure{"open", name}, "upstream is not running, will retry every health round", err, "upstream", name)
}

// activate pushes d.ups to the dataplane if that is pending, then brings up
// the devices that are not up, as start does. Nothing comes up while the
// dataplane is out of date. A failure leaves the work pending for the next
// health round and is returned for Reload to report; it is logged once.
// Bringing up a helper waits for it to start, so links come up without d.mu.
// Callers hold d.applyMu, not d.mu.
func (d *Daemon) activate() []error {
	d.mu.Lock()
	fresh := d.dpStale
	if fresh {
		key := failure{"dataplane", ""}
		if err := d.dp.SetUpstreams(d.interfaces(), d.clientMTU()); err != nil {
			d.fail(key, "dataplane upstreams not applied, new upstreams stay down and will be retried every health round", err)
			d.mu.Unlock()
			return []error{fmt.Errorf("dataplane: %w", err)}
		}
		if _, failed := d.failures[key]; failed {
			d.log.Info("dataplane upstreams applied")
			delete(d.failures, key)
		}
		d.dpStale = false
	}
	type starting struct {
		u     *Upstream
		exits uint64 // u.exits before Up
	}
	var start []starting
	var errs []error
	for _, u := range d.ups {
		if d.notUp[u] {
			start = append(start, starting{u, u.exits})
		} else if fresh {
			// A new or replaced upstream starts unknown; nothing may stay pinned to it.
			if err := d.applyAlive(u); err != nil {
				errs = append(errs, err)
			}
		}
	}
	d.mu.Unlock()

	upErrs := make([]error, len(start))
	for i, s := range start {
		upErrs[i] = s.u.link.Up()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for i, s := range start {
		u, key := s.u, failure{"up", s.u.Name}
		if err := upErrs[i]; err != nil {
			d.fail(key, "upstream device not up, will retry every health round", err, "upstream", u.Name)
			errs = append(errs, err)
			continue
		}
		if u.exits != s.exits {
			continue // it came up and stopped again; the next round brings it up
		}
		delete(d.notUp, u)
		delete(d.failures, key)
		if err := d.applyAlive(u); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// reopenMissing opens upstreams the config lists but d.ups lacks, such as
// ones whose open failed in a reload, and finishes any pending dataplane push
// and device Up. Like Reload, files are read and endpoints resolved without
// d.mu, and the result is applied under it only if nothing changed meanwhile.
func (d *Daemon) reopenMissing(ctx context.Context) {
	d.mu.Lock()
	cfg := d.cfg
	missing := map[string]bool{}
	for _, u := range cfg.Upstreams {
		missing[u.Name] = true
	}
	for _, u := range d.ups {
		delete(missing, u.Name)
	}
	for key := range d.failures {
		if key.kind == "open" && !slices.ContainsFunc(cfg.Upstreams, func(u config.Upstream) bool { return u.Name == key.name }) {
			delete(d.failures, key) // dropped from the config
		}
	}
	d.mu.Unlock()

	// One lookup bound for all of them, so a slow provider does not hold up
	// the probes for long.
	loads := make([]*Upstream, len(cfg.Upstreams))
	errs := make([]error, len(cfg.Upstreams))
	var wg sync.WaitGroup
	for i, u := range cfg.Upstreams {
		if missing[u.Name] {
			wg.Go(func() { loads[i], errs[i] = d.loadResolved(ctx, u, cfg.Health, cfg.StateDir, nil) })
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	// Bringing links up releases d.mu; applyMu keeps reloads out meanwhile.
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	if d.cfg == cfg { // otherwise a reload took over; the next round retries
		for i, u := range cfg.Upstreams {
			if !missing[u.Name] || slices.ContainsFunc(d.ups, func(r *Upstream) bool { return r.Name == u.Name }) {
				continue // running, or opened by a reload meanwhile
			}
			err := errs[i]
			if err == nil {
				err = d.openDevice(loads[i])
			}
			if err != nil {
				d.openFailed(u.Name, err)
				continue
			}
			d.ups = append(d.ups, loads[i])
			d.notUp[loads[i]] = true
			d.dpStale = true
			delete(d.failures, failure{"open", u.Name})
			d.log.Info("upstream added", "upstream", u.Name)
		}
		// Keep d.ups in configuration order, as Reload does.
		order := map[string]int{}
		for i, u := range cfg.Upstreams {
			order[u.Name] = i
		}
		slices.SortStableFunc(d.ups, func(a, b *Upstream) int { return order[a.Name] - order[b.Name] })
	}
	d.mu.Unlock()
	d.activate() // failures are logged and retried next round
}

func restartRequired(old, cfg *config.Config) error {
	var changed []string
	if old.Listen != cfg.Listen {
		changed = append(changed, "listen")
	}
	if old.AWG != cfg.AWG {
		changed = append(changed, "awg")
	}
	if old.Subnet != cfg.Subnet {
		changed = append(changed, "subnet")
	}
	if old.StateDir != cfg.StateDir {
		changed = append(changed, "state_dir")
	}
	if old.Metrics.Listen != cfg.Metrics.Listen {
		changed = append(changed, "metrics.listen")
	}
	if len(changed) > 0 {
		return fmt.Errorf("reload rejected: %v changed, which needs a restart", changed)
	}
	return nil
}

// probeTimeout bounds each probe and each endpoint lookup.
func probeTimeout(h config.Health) time.Duration {
	return max(h.Interval/2, time.Second)
}

// fetchTimeout bounds a dynamic key fetch: the probe timeout, but at least
// long enough for an HTTPS exchange over a slow or filtered path.
func fetchTimeout(probe time.Duration) time.Duration {
	return max(probe, 5*time.Second)
}

// probeAny tries every target at once and passes when any answers, reporting
// the fastest latency. The first answer cancels the others, and probeAny
// returns only after every attempt has, so none outlives the round. d.mu is
// not held.
func probeAny(ctx context.Context, targets []netip.AddrPort, probe func(context.Context, netip.AddrPort) (time.Duration, error)) (time.Duration, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		latency time.Duration
		err     error
	}
	results := make(chan result, len(targets))
	for _, target := range targets {
		go func() {
			latency, err := probe(ctx, target)
			results <- result{latency, err}
		}()
	}
	var best time.Duration
	var failures []string
	passed := false
	for range targets {
		r := <-results
		if r.err != nil {
			failures = append(failures, r.err.Error())
			continue
		}
		if !passed || r.latency < best {
			best, passed = r.latency, true
		}
		cancel()
	}
	if passed {
		return best, nil
	}
	return 0, errors.New(strings.Join(failures, "; "))
}

// probeUpstream runs one health round's probe through an upstream. An AWG
// upstream passes when a TCP handshake with any probe target completes. A
// proxy upstream completes TCP handshakes itself, and its server may accept
// a connection it will never serve (outline-ss-server does that with a wrong
// key), so it passes only when DNS queries get answers both over UDP, which
// needs the server's UDP relay, and over TCP. Its latency is that of UDP, one
// round trip to the resolver like the AWG handshake. d.mu is not held.
func (d *Daemon) probeUpstream(ctx context.Context, proxied bool, iface string, source netip.Addr, h config.Health) (time.Duration, error) {
	timeout := probeTimeout(h)
	if !proxied {
		return probeAny(ctx, h.Probe, func(ctx context.Context, target netip.AddrPort) (time.Duration, error) {
			return d.probe(ctx, iface, source, target, timeout)
		})
	}
	dns := func(network string) func(context.Context, netip.AddrPort) (time.Duration, error) {
		return func(ctx context.Context, target netip.AddrPort) (time.Duration, error) {
			return d.probeDNS(ctx, network, iface, source, target, timeout)
		}
	}
	var latency time.Duration
	var udpErr, tcpErr error
	var wg sync.WaitGroup
	wg.Go(func() { latency, udpErr = probeAny(ctx, h.DNS, dns("udp")) })
	wg.Go(func() { _, tcpErr = probeAny(ctx, h.DNS, dns("tcp")) })
	wg.Wait()
	if err := errors.Join(udpErr, tcpErr); err != nil {
		return 0, err
	}
	return latency, nil
}

func (d *Daemon) probeAll(ctx context.Context) {
	// First, so a pending dataplane push finds the firewall in place.
	d.verifyFirewall()
	// Before the probes, so an upstream that opens now is probed this round.
	d.reopenMissing(ctx)
	var wg sync.WaitGroup
	d.mu.Lock()
	h, stateDir := d.cfg.Health, d.cfg.StateDir
	timeout := probeTimeout(h)
	for _, u := range d.ups {
		if l, ok := u.link.(*proxyLink); ok {
			// For Status and Metrics, which never wait for a helper.
			wg.Go(l.refreshStats)
		}
		name, iface, source, endpoint, proxied := u.Name, u.InterfaceName(), u.Addr.Addr(), u.Endpoint, u.Key != nil
		wg.Go(func() {
			latency, err := d.probeUpstream(ctx, proxied, iface, source, h)
			d.mu.Lock()
			countResult(d.probes, u.Name, err == nil)
			before := u.Tracker.State()
			changed := u.Tracker.Observe(err == nil, latency)
			if changed {
				d.log.Info("upstream health changed", "upstream", u.Name, "from", before, "to", u.Tracker.State(), "err", err)
			}
			if slices.Contains(d.ups, u) { // not removed by a reload meanwhile
				d.setIncident(u.Name, store.IncidentDown, u.Tracker.State() == health.Down, errText(err), time.Now())
			}
			if _, stale := d.aliveFailed[u.Name]; changed || stale {
				_ = d.applyAlive(u) // failure is logged and retried next round
			}
			d.mu.Unlock()
			if err != nil {
				// A failing upstream may simply have moved; providers change
				// IPs, and rotate dynamic keys.
				d.refreshEndpoint(ctx, u, name, endpoint, stateDir, timeout)
			}
		})
	}
	d.mu.Unlock()
	wg.Wait()
	// After the round, so an upstream that just came up is measured at once.
	d.startTransfers(ctx)
	d.startSpeedTests(ctx)
}

// verifyFirewall repairs the firewall if another tool removed it, such as by
// "nft flush ruleset", and then routes every routed peer again: the repaired
// firewall admits a peer only once Route has run against it. A failure is
// retried every health round and logged once.
func (d *Daemon) verifyFirewall() {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := failure{"firewall", ""}
	repaired, err := d.dp.Verify()
	if repaired {
		d.repairs++
		d.reauthorize = true
		d.log.Error("firewall rules were removed by another tool and have been reinstalled", "routed_peers", len(d.assignments))
	}
	if d.reauthorize {
		var errs []error
		for ip, name := range d.assignments {
			// A peer with a pending unroute stays revoked; Reconcile finishes it.
			if iface := d.interfaceOf(name); iface != "" && !d.unroutePending[ip] {
				errs = append(errs, d.dp.Route(ip, iface))
			}
		}
		routeErr := errors.Join(errs...)
		d.reauthorize = routeErr != nil
		err = errors.Join(err, routeErr)
	}
	if err != nil {
		d.fail(key, "firewall repair failed, clients may stay blocked; will retry every health round", err)
		return
	}
	if _, failed := d.failures[key]; failed {
		d.log.Info("firewall repair completed")
		delete(d.failures, key)
	}
}

// firewallSettle is how long the daemon waits after a reported firewall
// change before it verifies, so a burst of changes (a firewall reload is
// often several transactions) costs one check and at most one repair.
const firewallSettle = 100 * time.Millisecond

// watchFirewall verifies the firewall shortly after the dataplane reports a
// change, so rules another tool removes are back within moments instead of
// at the next health round, which stays the fallback for missed changes. A
// repair is itself reported as a change; the check that follows finds the
// firewall intact and changes nothing, so repairs do not feed each other.
// It returns once changes is closed.
func (d *Daemon) watchFirewall(changes <-chan error) {
	for {
		err, ok := <-changes
		if !ok {
			return
		}
		d.watchFailed(err)
		settled := time.After(firewallSettle)
	burst:
		for {
			select {
			case err, ok := <-changes:
				if !ok {
					return
				}
				d.watchFailed(err)
			case <-settled:
				break burst
			}
		}
		d.verifyFirewall()
	}
}

// watchFailed logs a failed firewall watch once; the dataplane retries it.
func (d *Daemon) watchFailed(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail(failure{"firewall watch", ""}, "watching the firewall for changes failed, removed rules are repaired at the next health round until it recovers", err)
}

// refreshEndpoint re-resolves an upstream's endpoint without holding d.mu, so
// slow DNS never blocks Status or Reconcile, and applies the answer only if a
// reload has not replaced or removed u meanwhile. A dynamic key is fetched
// again first, at most every keyRefreshInterval; if that fails, the running
// key stays. u.Location never changes, so reading it needs no lock; name is
// u.Name when the round started.
func (d *Daemon) refreshEndpoint(ctx context.Context, u *Upstream, name, endpoint, stateDir string, timeout time.Duration) {
	var fetched *dynamicKey
	var fetchErr error
	if u.Location != nil && d.fetchDue(name) {
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout(timeout))
		k, err := fetchKey(fctx, u.Location)
		cancel()
		if err == nil {
			fetched, endpoint = &k, k.key.Server()
		}
		fetchErr = err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	resolved, err := resolveEndpoint(ctx, endpoint)
	cancel()
	d.mu.Lock()
	running := slices.Contains(d.ups, u)
	d.mu.Unlock()
	if !running {
		return
	}
	if fetchErr != nil {
		d.log.Warn("dynamic key fetch failed, keeping the running key", "upstream", name, "err", fetchErr)
	}
	if err == nil {
		err = d.applyServer(u, fetched, resolved, stateDir)
	}
	if err != nil {
		d.log.Warn("re-resolve endpoint", "upstream", name, "err", err)
	}
}

// applyAlive pushes alive(u) to the dataplane, which drops flow pins to an
// upstream that is not alive. A failure is remembered so the next probe round retries
// it, and logged only when the error changes; the observed health is untouched.
func (d *Daemon) applyAlive(u *Upstream) error {
	alive := d.alive(u)
	err := d.dp.SetAlive(u.InterfaceName(), alive)
	if err == nil {
		d.appliedAlive[u] = alive
		if _, stale := d.aliveFailed[u.Name]; stale {
			d.log.Info("set alive recovered", "upstream", u.Name)
			delete(d.aliveFailed, u.Name)
		}
		return nil
	}
	if d.aliveFailed[u.Name] != err.Error() {
		d.log.Error("set alive", "upstream", u.Name, "err", err)
	}
	d.aliveFailed[u.Name] = err.Error()
	return err
}

// Reconcile makes the device peer list and the kernel routes match the store
// and the current health. It is safe to call from any goroutine.
func (d *Daemon) Reconcile() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reconcile()
}

// reconcile is Reconcile for callers that hold d.mu.
func (d *Daemon) reconcile() error {
	// Routes and rights are read every pass, so a route or rights edited
	// in the store apply on the next reconcile, as a new peer does.
	peers, users, err := d.store.PeersAndUsers()
	if err != nil {
		return err
	}
	exits := map[string][]string{}
	for _, u := range users {
		exits[u.Name] = u.Rights.Exits
	}
	var errs []error
	if err := d.loadSubscriptions(); err != nil {
		errs = append(errs, fmt.Errorf("load subscription limits: %w", err))
	}
	d.scheduleSubscriptionExpiry(time.Now())
	wanted := map[awgconf.Key]bool{}
	for _, p := range peers {
		wanted[p.PublicKey] = true
	}
	// Revoke first: a new peer may reuse an old peer's IP, and removing the old
	// one afterwards would unroute the new one.
	for key, p := range d.devicePeers {
		if wanted[key] {
			continue
		}
		if err := errors.Join(d.down.RemovePeer(key), d.unroute(p.IP)); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(d.devicePeers, key)
		d.log.Info("peer removed", "peer", p.IP)
	}
	// Addresses whose old peer cleanup is unresolved stay unassigned.
	held := map[netip.Addr]bool{}
	for _, p := range d.devicePeers {
		held[p.IP] = true
	}
	for _, p := range peers {
		if _, ok := d.devicePeers[p.PublicKey]; !ok {
			if held[p.IP] {
				continue
			}
			err := d.down.AddPeer(awgconf.Peer{PublicKey: p.PublicKey, AllowedIPs: []netip.Prefix{netip.PrefixFrom(p.IP, 32)}})
			if err != nil {
				errs = append(errs, err)
				continue
			}
		}
		// Kept fresh, not only when added: a new route changes a peer that
		// is already on the device.
		d.devicePeers[p.PublicKey] = p
		errs = append(errs, d.route(p, exits[p.User]))
	}
	d.syncAlive()
	return errors.Join(errs...)
}

// route moves p to the upstream its route selects among the exits its user
// may use, or blocks it when there is none.
func (d *Daemon) route(p store.Peer, exits []string) error {
	if d.unroutePending[p.IP] {
		if err := d.unroute(p.IP); err != nil {
			return err
		}
	}
	current := d.assignments[p.IP]
	if current != "" && d.subscriptionBlockByName(current, time.Now()) != RoutingAllowed {
		// Revoke access before trying the replacement, so a failed Route
		// cannot leave new flows using the excluded upstream.
		if err := d.unroute(p.IP); err != nil {
			return err
		}
		current = ""
	}
	want, ok := policy.Select(p.Route.Tiers(), policy.Exits(d.candidates(), exits), current)
	switch {
	case !ok && current != "":
		if err := d.unroute(p.IP); err != nil {
			return err
		}
		d.log.Warn("no healthy upstream, peer is now blocked", "peer", p.IP, "user", p.User, "device", p.Device, "route", p.Route.String())
		return nil
	case !ok:
		return nil
	case want == current:
		return nil
	}
	if err := d.dp.Route(p.IP, d.interfaceOf(want)); err != nil {
		return err
	}
	d.assignments[p.IP] = want
	d.reroutes[want]++
	d.log.Info("peer routed", "peer", p.IP, "user", p.User, "device", p.Device, "route", p.Route.String(), "from", current, "to", want)
	return nil
}

// unroute keeps both the assignment and retry state until all routes and pins
// are removed. In particular, a reloaded upstream can reuse the same name.
func (d *Daemon) unroute(ip netip.Addr) error {
	if err := d.dp.Unroute(ip); err != nil {
		d.unroutePending[ip] = true
		return err
	}
	delete(d.unroutePending, ip)
	delete(d.assignments, ip)
	return nil
}

// countResult counts one probe result in m, upstream (or what else keys
// it) → {ok, fail}.
func countResult[K comparable](m map[K]*[2]uint64, key K, ok bool) {
	c := m[key]
	if c == nil {
		c = new([2]uint64)
		m[key] = c
	}
	if ok {
		c[0]++
	} else {
		c[1]++
	}
}

func (d *Daemon) interfaceOf(upstream string) string {
	for _, u := range d.ups {
		if u.Name == upstream {
			return u.InterfaceName()
		}
	}
	return ""
}

func (d *Daemon) candidates() []policy.Upstream {
	out := make([]policy.Upstream, 0, len(d.ups))
	now := time.Now()
	for _, u := range d.ups {
		if d.subscriptionBlock(u.Upstream, now) != RoutingAllowed {
			continue
		}
		out = append(out, policy.Upstream{
			Upstream: u.Upstream,
			Healthy:  u.Tracker.State() == health.Up && !d.notUp[u], // a device that is not up carries nothing
			Degraded: d.degraded(u),
			Latency:  u.Tracker.Latency(),
		})
	}
	return out
}

// ActiveWindow is how recent a handshake must be for a client to count as
// connected: WireGuard rekeys at least every 3 minutes while traffic flows.
const ActiveWindow = 3 * time.Minute

type UpstreamStatus struct {
	Name          string
	Kind          string
	Geo           string
	Provider      string
	ExpiresAt     time.Time `json:"-"` // only included in the live operator API
	RoutingBlock  RoutingBlock
	Interface     string
	State         health.State
	Degraded      bool // reachable, but failing its transfer probes
	Latency       time.Duration
	LastHandshake time.Time            // with the provider; zero if never, and for proxy upstreams
	Routed        int                  // clients whose traffic is directed through this upstream
	Active        int                  // of those, clients with a handshake inside ActiveWindow
	Speed         health.Speed         // the latest speed test; zero before one or with health.speed off
	Incidents     []store.IncidentKind // the kinds open now, in the order of store.IncidentKinds
}

type PeerStatus struct {
	ID            int64 // the store's peer row
	User, Device  string
	Address       netip.Addr // the client's tunnel address
	Upstream      string     // "" when blocked
	LastHandshake time.Time
}

type Status struct {
	Upstreams []UpstreamStatus
	Peers     []PeerStatus
}

func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	var st Status
	handshakes := d.downstreamHandshakes()
	routed, active := map[string]int{}, map[string]int{}
	for key, p := range d.devicePeers {
		ps := PeerStatus{ID: p.ID, User: p.User, Device: p.Device, Address: p.IP, Upstream: d.assignments[p.IP], LastHandshake: handshakes[key]}
		st.Peers = append(st.Peers, ps)
		if ps.Upstream == "" {
			continue
		}
		routed[ps.Upstream]++
		if now.Sub(ps.LastHandshake) < ActiveWindow {
			active[ps.Upstream]++
		}
	}
	slices.SortFunc(st.Peers, func(a, b PeerStatus) int {
		return cmp.Or(strings.Compare(a.User, b.User), strings.Compare(a.Device, b.Device))
	})
	for _, u := range d.ups {
		s := UpstreamStatus{Name: u.Name, Kind: u.Kind, Geo: u.Geo, Provider: u.Provider, ExpiresAt: d.subscriptionExpiry(u.Upstream), RoutingBlock: d.subscriptionBlock(u.Upstream, now), Interface: u.InterfaceName(), State: u.Tracker.State(), Degraded: d.degraded(u),
			Latency: u.Tracker.Latency(), Routed: routed[u.Name], Active: active[u.Name], Speed: d.speedOf(u), Incidents: d.openKinds(u.Name)}
		if peer := u.link.Stats().Peer; peer != nil {
			s.LastHandshake = peer.LastHandshake
		}
		st.Upstreams = append(st.Upstreams, s)
	}
	return st
}

// downstreamHandshakes maps each client key to its last handshake; callers hold d.mu.
func (d *Daemon) downstreamHandshakes() map[awgconf.Key]time.Time {
	out := map[awgconf.Key]time.Time{}
	if d.down == nil {
		return out
	}
	if peers, err := d.down.Peers(); err == nil {
		for _, p := range peers {
			out[p.PublicKey] = p.LastHandshake
		}
	}
	return out
}
