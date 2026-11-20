package daemon

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
)

// speedState is an upstream's speed tests. Callers hold d.mu.
type speedState struct {
	next    time.Time // the next test is due then; zero means at once
	running bool
	last    health.Speed // the latest test that passed; zero before one
}

// speedTesting reports whether a speed test runs through u. Callers hold
// d.mu.
func (d *Daemon) speedTesting(u *Upstream) bool {
	st := d.speeds[u]
	return st != nil && st.running
}

// startSpeedTests launches the speed test of the first upstream that is up
// and due, unless one already runs: tests through several upstreams at once
// would share the gateway's own link and measure that. Nor does one run
// beside the upstream's transfer probe. A test runs without d.mu and
// outlives the health round; Run waits for it before it tears down.
func (d *Daemon) startSpeedTests(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cfg.Health.Speed == nil || d.transfer == nil || d.upload == nil {
		return
	}
	for _, st := range d.speeds {
		if st.running {
			return
		}
	}
	s := *d.cfg.Health.Speed
	now := time.Now()
	for _, u := range d.ups {
		if u.Tracker.State() != health.Up || d.notUp[u] {
			continue
		}
		st := d.speeds[u]
		if st == nil {
			st = &speedState{}
			d.speeds[u] = st
		}
		if now.Before(st.next) {
			continue
		}
		if tr := d.transfers[u]; tr != nil && tr.running {
			continue
		}
		st.running = true
		iface, source := u.InterfaceName(), u.Addr.Addr()
		d.transferRuns.Go(func() {
			speed, err := d.speedTest(ctx, iface, source, s)
			d.finishSpeedTest(ctx, u, st, speed, err, s.Interval)
		})
		return
	}
}

// speedTest downloads s.Bytes, then uploads as many, and measures both
// rates. A rate counts the whole exchange, connecting included, as a client
// fetching that much would see it. d.mu is not held.
func (d *Daemon) speedTest(ctx context.Context, iface string, source netip.Addr, s config.Speed) (health.Speed, error) {
	down, err := d.transfer(ctx, iface, source, s.DownURL, s.Bytes, s.Timeout)
	if err != nil {
		return health.Speed{}, fmt.Errorf("download: %w", err)
	}
	up, err := d.upload(ctx, iface, source, s.UpURL, s.Bytes, s.Timeout)
	if err != nil {
		return health.Speed{}, fmt.Errorf("upload: %w", err)
	}
	return health.Speed{Down: rate(s.Bytes, down), Up: rate(s.Bytes, up), Time: time.Now()}, nil
}

// rate is bytes per second.
func rate(bytes int64, took time.Duration) uint64 {
	if took <= 0 {
		return 0
	}
	return uint64(float64(bytes) / took.Seconds())
}

// finishSpeedTest keeps a test's result unless it no longer applies: the
// daemon is stopping, the upstream was removed or replaced, or tests were
// turned off. A failed test changes nothing but the schedule; it only
// informs, the transfer probe judges.
func (d *Daemon) finishSpeedTest(ctx context.Context, u *Upstream, st *speedState, speed health.Speed, err error, interval time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.running = false
	st.next = time.Now().Add(jitter(interval))
	if ctx.Err() != nil || d.speeds[u] != st || d.cfg.Health.Speed == nil {
		return
	}
	if err != nil {
		d.log.Info("speed test failed", "upstream", u.Name, "err", err)
		return
	}
	st.last = speed
	d.log.Debug("speed test", "upstream", u.Name, "down_bytes_per_second", speed.Down, "up_bytes_per_second", speed.Up)
}

// speedOf is u's latest speed test; zero before one. Callers hold d.mu.
func (d *Daemon) speedOf(u *Upstream) health.Speed {
	if st := d.speeds[u]; st != nil {
		return st.last
	}
	return health.Speed{}
}
