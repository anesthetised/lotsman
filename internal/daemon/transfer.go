package daemon

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/netip"
	"slices"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
)

// TransferFunc downloads at least minBytes of url through an upstream, as
// health.TransferOnInterface does.
type TransferFunc func(ctx context.Context, iface string, source netip.Addr, url string, minBytes int64, timeout time.Duration) (time.Duration, error)

// UploadFunc uploads size bytes to url through an upstream, as
// health.UploadOnInterface does.
type UploadFunc func(ctx context.Context, iface string, source netip.Addr, url string, size int64, timeout time.Duration) (time.Duration, error)

// The transfer probe runs both ways, each with its own health: it names
// them by the incident each one's failure opens.
const (
	probeDownload = store.IncidentDownload
	probeUpload   = store.IncidentUpload // only with health.transfer.upload_url
)

// transferState is an upstream's throughput health: per direction, its
// probe results with the same hysteresis as reachability. Down there means
// degraded. Callers hold d.mu.
type transferState struct {
	probes  map[store.IncidentKind]*transferProbe // probeDownload, and probeUpload while it is configured
	next    time.Time                             // the next probe is due then; zero means at once
	running bool
	alive   bool // what applyAlive last pushed to the dataplane
}

// transferProbe is one direction of an upstream's transfer probe.
type transferProbe struct {
	tracker *health.Tracker
	last    time.Duration // how long the last probe took, a failed one included
}

// transferResult is what one direction of a probe found.
type transferResult struct {
	took time.Duration
	err  error
}

// degraded reports whether the probe of either direction failed often
// enough to count.
func (st *transferState) degraded() bool {
	for _, p := range st.probes {
		if p.tracker.State() == health.Down {
			return true
		}
	}
	return false
}

// jitter spreads an interval by ±20%, so probes through several upstreams
// drift apart and form no fixed period.
func jitter(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64()))
}

// degraded reports whether u carries a handshake but failed its transfer
// probes. Callers hold d.mu.
func (d *Daemon) degraded(u *Upstream) bool {
	st := d.transfers[u]
	return d.cfg.Health.Transfer != nil && st != nil && st.degraded()
}

// alive decides whether flows may stay pinned to u, which the dataplane
// learns by SetAlive. A dead upstream releases its pins. So does a degraded
// one, once its clients have moved on; while it is still some client's route,
// its last resort, it stays alive, or that client's flows would be cut every
// round. Callers hold d.mu.
func (d *Daemon) alive(u *Upstream) bool {
	if u.Tracker.State() != health.Up {
		return false
	}
	if !d.degraded(u) {
		return true
	}
	for _, name := range d.assignments {
		if name == u.Name {
			return true
		}
	}
	return false
}

// syncAlive pushes alive again for upstreams with throughput health whose
// answer changed because clients moved, which the dataplane does not see by
// itself. Reconcile calls it after routing. Callers hold d.mu.
func (d *Daemon) syncAlive() {
	for u, st := range d.transfers {
		if d.alive(u) != st.alive && slices.Contains(d.ups, u) {
			_ = d.applyAlive(u) // failure is logged and retried next round
		}
	}
}

// startTransfers launches a transfer probe through every upstream that is up
// and due, at most one in flight per upstream and none beside its speed
// test. Probes run without d.mu and outlive the health round; Run waits for
// them before it tears down.
func (d *Daemon) startTransfers(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg.Health.Transfer == nil || d.transfer == nil {
		return
	}
	t := *d.cfg.Health.Transfer
	now := time.Now()
	for _, u := range d.ups {
		// A probe through an upstream that is down only proves it again.
		if u.Tracker.State() != health.Up || d.notUp[u] {
			continue
		}
		st := d.transfers[u]
		if st == nil {
			st = &transferState{probes: map[store.IncidentKind]*transferProbe{}, alive: true}
			d.transfers[u] = st
		}
		if st.running || now.Before(st.next) || d.speedTesting(u) {
			continue
		}
		st.running = true
		iface, source := u.InterfaceName(), u.Addr.Addr()
		d.transferRuns.Go(func() {
			// One way after the other, so neither slows the other down.
			results := map[store.IncidentKind]transferResult{probeDownload: d.transferAny(ctx, iface, source, t)}
			if t.UploadURL != "" && d.upload != nil {
				results[probeUpload] = timed(func() (time.Duration, error) {
					return d.upload(ctx, iface, source, t.UploadURL, t.UploadBytes, t.Timeout)
				})
			}
			d.finishTransfer(ctx, u, st, results, t.Interval)
		})
	}
}

// timed runs a probe; a failed one took as long as it ran.
func timed(probe func() (time.Duration, error)) transferResult {
	start := time.Now()
	took, err := probe()
	if err != nil {
		took = time.Since(start)
	}
	return transferResult{took, err}
}

// transferAny tries the download URLs in random order until one passes.
// d.mu is not held.
func (d *Daemon) transferAny(ctx context.Context, iface string, source netip.Addr, t config.Transfer) transferResult {
	return timed(func() (time.Duration, error) {
		var errs []error
		for _, i := range rand.Perm(len(t.URL)) {
			took, err := d.transfer(ctx, iface, source, t.URL[i], t.MinBytes, t.Timeout)
			if err == nil {
				return took, nil
			}
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
		}
		return 0, errors.Join(errs...)
	})
}

// finishTransfer records a probe's results unless they no longer apply: the
// daemon is stopping, the upstream was removed or replaced, the probe was
// turned off, or the upstream went down meanwhile, which its reachability
// already reports. A direction whose probe is down opens its incident. While
// probes fail, or the upstream is degraded, the next one comes after a fifth
// of the interval (at least a health interval), so a freeze is confirmed and
// a recovery noticed in minutes.
func (d *Daemon) finishTransfer(ctx context.Context, u *Upstream, st *transferState, results map[store.IncidentKind]transferResult, interval time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.running = false
	st.next = time.Now().Add(jitter(interval))
	if ctx.Err() != nil || d.transfers[u] != st || d.cfg.Health.Transfer == nil || u.Tracker.State() != health.Up {
		return
	}
	before := d.degraded(u)
	var errs []error
	failing := []store.IncidentKind{}
	for _, kind := range []store.IncidentKind{probeDownload, probeUpload} {
		r, ok := results[kind]
		if !ok || (kind == probeUpload && d.cfg.Health.Transfer.UploadURL == "") {
			continue // not run, or turned off by a reload meanwhile
		}
		p := st.probes[kind]
		if p == nil {
			p = &transferProbe{tracker: health.NewTracker(d.cfg.Health.DownAfter, d.cfg.Health.UpAfter)}
			st.probes[kind] = p
		}
		p.last = r.took
		countResult(d.transferCounts, transferCount{u.Name, kind}, r.err == nil)
		p.tracker.SetThresholds(d.cfg.Health.DownAfter, d.cfg.Health.UpAfter)
		p.tracker.Observe(r.err == nil, r.took)
		down := p.tracker.State() == health.Down
		if down {
			failing = append(failing, kind)
		}
		d.setIncident(u.Name, kind, down, errText(r.err), time.Now())
		if r.err != nil {
			errs = append(errs, r.err)
		}
	}
	err := errors.Join(errs...)
	after := d.degraded(u)
	if err != nil || after {
		st.next = time.Now().Add(max(jitter(interval/5), d.cfg.Health.Interval))
	}
	switch {
	case after && !before:
		d.log.Warn("upstream degraded: it is reachable but its transfer probes fail; clients use it only as a last resort", "upstream", u.Name, "probes", failing, "err", err)
	case before && !after:
		d.log.Info("upstream no longer degraded", "upstream", u.Name, "took", results[probeDownload].took)
	case err != nil:
		d.log.Debug("transfer probe failed", "upstream", u.Name, "err", err)
	}
	if before == after {
		return
	}
	_ = d.applyAlive(u) // failure is logged and retried next round
	// Reconcile now rather than a health interval later; it moves clients
	// and then settles alive by where they went.
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// trimTransfers forgets the throughput health a reload turned off: every
// upstream's once health.transfer is gone, the upload's once its URL is.
// Their incidents close, and an upstream no longer degraded is alive again.
// Callers hold d.mu.
func (d *Daemon) trimTransfers() {
	t := d.cfg.Health.Transfer
	now := time.Now()
	for u, st := range d.transfers {
		degraded := st.degraded()
		switch {
		case t == nil:
			delete(d.transfers, u)
			d.setIncident(u.Name, probeDownload, false, "", now)
			d.setIncident(u.Name, probeUpload, false, "", now)
		case t.UploadURL == "":
			delete(st.probes, probeUpload)
			d.setIncident(u.Name, probeUpload, false, "", now)
		}
		if degraded != d.degraded(u) && slices.Contains(d.ups, u) {
			_ = d.applyAlive(u)
		}
	}
}

// transferCount keys the transfer probe counters: upstream and direction.
type transferCount struct {
	upstream string
	probe    store.IncidentKind
}

// errText is err's message, or "" for none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
