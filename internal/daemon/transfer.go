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
)

// TransferFunc downloads at least minBytes of url through an upstream, as
// health.TransferOnInterface does.
type TransferFunc func(ctx context.Context, iface string, source netip.Addr, url string, minBytes int64, timeout time.Duration) (time.Duration, error)

// transferState is an upstream's throughput health: its transfer probe
// results with the same hysteresis as reachability. Down there means
// degraded. Callers hold d.mu.
type transferState struct {
	tracker *health.Tracker
	next    time.Time // the next probe is due then; zero means at once
	running bool
	last    time.Duration // how long the last probe took, a failed one included
	ran     bool
	alive   bool // what applyAlive last pushed to the dataplane
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
	return d.cfg.Health.Transfer != nil && st != nil && st.tracker.State() == health.Down
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
// and due, at most one in flight per upstream. Probes run without d.mu and
// outlive the health round; Run waits for them before it tears down.
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
			st = &transferState{tracker: health.NewTracker(d.cfg.Health.DownAfter, d.cfg.Health.UpAfter), alive: true}
			d.transfers[u] = st
		}
		if st.running || now.Before(st.next) {
			continue
		}
		st.running = true
		iface, source := u.InterfaceName(), u.Addr.Addr()
		d.transferRuns.Go(func() {
			start := time.Now()
			took, err := d.transferAny(ctx, iface, source, t)
			if err != nil {
				took = time.Since(start)
			}
			d.finishTransfer(ctx, u, st, took, err, t.Interval)
		})
	}
}

// transferAny tries the URLs in random order until one passes. d.mu is not held.
func (d *Daemon) transferAny(ctx context.Context, iface string, source netip.Addr, t config.Transfer) (time.Duration, error) {
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
}

// finishTransfer records a probe result unless it no longer applies: the
// daemon is stopping, the upstream was removed or replaced, the probe was
// turned off, or the upstream went down meanwhile, which its reachability
// already reports. While probes fail, or the upstream is degraded, the next
// one comes after a fifth of the interval (at least a health interval), so
// a freeze is confirmed and a recovery noticed in minutes.
func (d *Daemon) finishTransfer(ctx context.Context, u *Upstream, st *transferState, took time.Duration, err error, interval time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.running = false
	st.next = time.Now().Add(jitter(interval))
	if ctx.Err() != nil || d.transfers[u] != st || d.cfg.Health.Transfer == nil || u.Tracker.State() != health.Up {
		return
	}
	st.last, st.ran = took, true
	countResult(d.transferCounts, u.Name, err == nil)
	before := d.degraded(u)
	st.tracker.SetThresholds(d.cfg.Health.DownAfter, d.cfg.Health.UpAfter)
	st.tracker.Observe(err == nil, took)
	after := d.degraded(u)
	if err != nil || after {
		st.next = time.Now().Add(max(jitter(interval/5), d.cfg.Health.Interval))
	}
	switch {
	case after && !before:
		d.log.Warn("upstream degraded: it is reachable but its transfer probes fail; clients use it only as a last resort", "upstream", u.Name, "err", err)
	case before && !after:
		d.log.Info("upstream no longer degraded", "upstream", u.Name, "took", took)
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

// stopTransfers forgets every upstream's throughput health once the probe is
// turned off, so none stays degraded. Callers hold d.mu.
func (d *Daemon) stopTransfers() {
	for u, st := range d.transfers {
		degraded := st.tracker.State() == health.Down
		delete(d.transfers, u)
		if degraded && slices.Contains(d.ups, u) {
			_ = d.applyAlive(u)
		}
	}
}
