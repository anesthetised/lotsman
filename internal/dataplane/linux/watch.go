//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/nftables"
)

// A failed nftables monitor is restarted after watchRetryMin, doubling up to
// watchRetryMax; a monitor that ran longer than watchRetryMax starts over.
const (
	watchRetryMin = time.Second
	watchRetryMax = time.Minute
)

// Watch follows nftables change notifications. It reports a transaction that
// touches our table, deletes our accepts from a foreign chain or adds a
// foreign forward chain; Lotsman's own transactions included, which Verify
// then finds intact. A failed monitor is reported and restarted with backoff.
// It reads no Dataplane state, so it runs alongside every other method.
func (d *Dataplane) Watch(ctx context.Context) <-chan error {
	out := make(chan error, 1)
	go func() {
		defer close(out)
		delay := watchRetryMin
		for {
			started := time.Now()
			err := watchOnce(ctx, out)
			if ctx.Err() != nil {
				return
			}
			if time.Since(started) > watchRetryMax {
				delay = watchRetryMin
			}
			select {
			case out <- fmt.Errorf("nftables monitor: %w; restarting in %s", err, delay):
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
			delay = min(2*delay, watchRetryMax)
		}
	}()
	return out
}

// watchOnce runs one monitor until ctx is done (nil) or the monitor fails.
// It reports a change as soon as the monitor runs, because anything removed
// while no monitor ran went unseen.
func watchOnce(ctx context.Context, out chan<- error) error {
	// The default monitor receives every object type. Filtering is done here:
	// in v0.3.0 the library's own filter drops everything unless the mask
	// includes NFT_MSG_NEWTABLE.
	// A lasting connection, because Monitor.Close does not close the socket
	// once the monitor has stopped on an error; CloseLasting always does.
	c, err := nftables.New(nftables.AsLasting())
	if err != nil {
		return err
	}
	batches, err := c.AddGenerationalMonitor(nftables.NewMonitor())
	if err != nil {
		c.CloseLasting()
		return err
	}
	defer func() {
		// Closing the socket stops the monitor's goroutine, which exits only
		// after its last send is read.
		c.CloseLasting()
		for range batches {
		}
	}()
	changed(out)
	for {
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-batches:
			if !ok {
				return errors.New("closed")
			}
			if b.GeneratedBy != nil && b.GeneratedBy.Type == nftables.MonitorEventTypeOOB {
				return b.GeneratedBy.Error // ENOBUFS when events overran the socket
			}
			if slices.ContainsFunc(b.Changes, mayBreakFirewall) {
				changed(out)
			}
		}
	}
}

// changed reports a change unless one is already waiting to be read.
func changed(out chan<- error) {
	select {
	case out <- nil:
	default:
	}
}

// mayBreakFirewall reports whether a change could have removed something
// Verify repairs, or added a forward chain whose drop Verify must open. An
// object that did not parse counts as ours.
func mayBreakFirewall(ev *nftables.MonitorEvent) bool {
	var table *nftables.Table
	switch data := ev.Data.(type) {
	case *nftables.Table:
		table = data
	case *nftables.Chain:
		if data != nil {
			table = data.Table
		}
	case *nftables.Set:
		if data != nil {
			table = data.Table
		}
	case *nftables.Rule:
		if data != nil {
			table = data.Table
		}
	}
	ours := table == nil || table.Name == nftTable
	switch ev.Type {
	case nftables.MonitorEventTypeDelTable, nftables.MonitorEventTypeDelChain, nftables.MonitorEventTypeDelSet:
		return ours
	case nftables.MonitorEventTypeDelRule:
		r, _ := ev.Data.(*nftables.Rule)
		return ours || r == nil || isForeignAccept(r)
	case nftables.MonitorEventTypeNewChain:
		ch, _ := ev.Data.(*nftables.Chain)
		return isForeignForwardChain(ch)
	}
	return false
}
