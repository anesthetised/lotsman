package daemon

import (
	"errors"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
)

type RoutingBlock uint8

const (
	RoutingAllowed RoutingBlock = iota
	SubscriptionExpired
	QuotaExhausted
	RoutingUnavailable // public status; the precise reason is operator-only
)

// ApplyOutlineUsage saves the panel's last confirmed provider readings and
// enforces the change immediately. The caller waits for Ready.
func (d *Daemon) ApplyOutlineUsage(readings map[string]store.OutlineUsage) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return errors.New("daemon is stopping")
	}
	if err := d.store.SaveOutlineUsage(readings); err != nil {
		return err
	}
	return d.reconcile()
}

// loadSubscriptions preserves the cached settings if a database read fails.
// Callers hold d.mu.
func (d *Daemon) loadSubscriptions() error {
	expiries, expiryErr := d.store.UpstreamExpiries()
	if expiryErr == nil {
		d.expiries = expiries
	}
	outline, outlineErr := d.store.OutlineUsage()
	if outlineErr == nil {
		d.outlineUsage = outline
	}
	return errors.Join(expiryErr, outlineErr)
}

func (d *Daemon) subscriptionExpiry(u config.Upstream) time.Time {
	if u.Kind == config.KindOutline {
		if expiry := d.outlineUsage[u.Name].ExpiresAt; !expiry.IsZero() {
			return expiry
		}
	}
	return d.expiries[u.Name]
}

func (d *Daemon) subscriptionBlock(u config.Upstream, now time.Time) RoutingBlock {
	if expiry := d.subscriptionExpiry(u); !expiry.IsZero() && !expiry.After(now) {
		return SubscriptionExpired
	}
	if u.Kind == config.KindOutline && d.outlineUsage[u.Name].Exhausted {
		return QuotaExhausted
	}
	return RoutingAllowed
}

func (d *Daemon) subscriptionBlockByName(name string, now time.Time) RoutingBlock {
	for _, u := range d.ups {
		if u.Name == name {
			return d.subscriptionBlock(u.Upstream, now)
		}
	}
	for _, u := range d.cfg.Upstreams {
		if u.Name == name {
			return d.subscriptionBlock(u, now)
		}
	}
	return RoutingAllowed
}

// Schedule the nearest deadline independently of the probe interval. A
// concurrent callback always reconciles against the latest stored values.
// Callers hold d.mu.
func (d *Daemon) scheduleSubscriptionExpiry(now time.Time) {
	if d.subscriptionTimer != nil {
		d.subscriptionTimer.Stop()
		d.subscriptionTimer = nil
	}
	var next time.Time
	for _, u := range d.ups {
		expiry := d.subscriptionExpiry(u.Upstream)
		if expiry.After(now) && (next.IsZero() || expiry.Before(next)) {
			next = expiry
		}
	}
	if !next.IsZero() {
		d.subscriptionTimer = time.AfterFunc(time.Until(next), func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if !d.stopped {
				if err := d.reconcile(); err != nil {
					d.log.Error("reconcile after subscription expiry", "err", err)
				}
			}
		})
	}
}
