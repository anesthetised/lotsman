package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/history"
	"github.com/anesthetised/lotsman/internal/store"
)

// UpstreamHealth is each upstream's availability and latest speed test right
// now, for the history writer. It implements history.Source with Traffic and Retention.
func (d *Daemon) UpstreamHealth() map[string]history.UpstreamHealth {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]history.UpstreamHealth, len(d.ups))
	for _, u := range d.ups {
		out[u.Name] = history.UpstreamHealth{Up: u.Tracker.State() == health.Up && !d.degraded(u), Latency: u.Tracker.Latency(), Speed: d.speedOf(u)}
	}
	return out
}

// Retention is how long history is kept, as of the latest reload.
func (d *Daemon) Retention() config.History { return d.config().History }

// recordHistory writes the history every minute until the returned function
// stops it. A minute still being folded when it stops is not written.
func (d *Daemon) recordHistory(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	w := &history.Writer{Source: d, Sink: d.store, Log: d.log}
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// UpstreamHistory is the history of an upstream the daemon runs, planned by
// history.Resolve: history.ErrRange for a range it cannot answer,
// store.ErrNotFound for an upstream it does not run, one that was removed
// included.
func (d *Daemon) UpstreamHistory(name string, r history.Range) (history.Result, error) {
	plan, err := history.Resolve(r, time.Now(), d.Retention().MinuteRetention)
	if err != nil {
		return history.Result{}, err
	}
	d.mu.Lock()
	known := false
	for _, u := range d.ups {
		known = known || u.Name == name
	}
	d.mu.Unlock()
	if !known {
		return history.Result{}, store.ErrNotFound
	}
	samples, err := d.store.UpstreamSamples(name, plan.From, plan.To, plan.Step)
	if err != nil {
		return history.Result{}, fmt.Errorf("upstream history: %w", err)
	}
	return history.Result{Step: plan.Step, Samples: samples}, nil
}

// PeerHistory is UpstreamHistory for the peer with this row id, known while
// the store has it. The history of a removed peer stays until it expires but
// is no longer served.
func (d *Daemon) PeerHistory(id int64, r history.Range) (history.Result, error) {
	plan, err := history.Resolve(r, time.Now(), d.Retention().MinuteRetention)
	if err != nil {
		return history.Result{}, err
	}
	switch known, err := d.store.HasPeer(id); {
	case err != nil:
		return history.Result{}, fmt.Errorf("peer history: %w", err)
	case !known:
		return history.Result{}, store.ErrNotFound
	}
	samples, err := d.store.PeerSamples(id, plan.From, plan.To, plan.Step)
	if err != nil {
		return history.Result{}, fmt.Errorf("peer history: %w", err)
	}
	return history.Result{Step: plan.Step, Samples: samples}, nil
}
