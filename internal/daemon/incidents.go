package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/anesthetised/lotsman/internal/store"
)

// An incident is one stretch of one kind of trouble with one upstream (see
// store.Incident). The daemon opens it where it notices the trouble start
// and closes it where it notices the trouble end: down by the reachability
// probes, download and upload by the transfer probe, send_errors by a
// minute's count of the AWG device's errors. It keeps what is open in
// d.incidents and writes every change to the store at once.

// incidentKey is one kind of trouble with one upstream.
type incidentKey struct {
	upstream string
	kind     store.IncidentKind
}

// openIncident is an incident the daemon has open.
type openIncident struct {
	id      int64
	started time.Time
	detail  string // as stored
}

// sendErrorsQuiet is how many minutes in a row an upstream's device must log
// at most the threshold for its send_errors incident to close.
const sendErrorsQuiet = 5

// closeLeftoverIncidents ends what an earlier run left open: this run cannot
// tell whether the trouble lasted. Real trouble opens a new one.
func (d *Daemon) closeLeftoverIncidents(now time.Time) {
	if err := d.store.CloseOpenIncidents(now, "closed when the daemon started"); err != nil {
		d.log.Error("closing the incidents left open", "err", err)
	}
}

// setIncident makes the incident of kind for upstream open while trouble
// holds and closed when it does not. An open incident keeps the latest
// detail that is not "". A store failure is logged once and leaves the
// incident as it was, so the next observation tries again. Callers hold
// d.mu.
func (d *Daemon) setIncident(upstream string, kind store.IncidentKind, trouble bool, detail string, now time.Time) {
	key := incidentKey{upstream, kind}
	in := d.incidents[key]
	if trouble && d.subscriptionBlockByName(upstream, now) != RoutingAllowed {
		return
	}
	var err error
	switch {
	case trouble && in == nil:
		devices := d.routedTo(upstream)
		var id int64
		if id, err = d.store.OpenIncident(upstream, kind, now, detail, devices); err == nil {
			d.incidents[key] = &openIncident{id: id, started: now, detail: detail}
			d.log.Warn("incident opened", "upstream", upstream, "kind", kind, "devices", devices, "detail", detail)
		}
	case trouble && detail != "" && detail != in.detail:
		if err = d.store.SetIncidentDetail(in.id, detail); err == nil {
			in.detail = detail
		}
	case !trouble && in != nil:
		if err = d.store.CloseIncident(in.id, now); err == nil {
			delete(d.incidents, key)
			d.log.Info("incident closed", "upstream", upstream, "kind", kind, "lasted", now.Sub(in.started).Round(time.Second))
		}
	}
	failed := failure{"incident", ""}
	if err != nil {
		d.fail(failed, "incident not recorded, will retry", err, "upstream", upstream, "kind", kind)
	} else {
		delete(d.failures, failed)
	}
}

// closeIncidents closes every incident of an upstream that is removed or
// replaced. Callers hold d.mu.
func (d *Daemon) closeIncidents(upstream string, now time.Time) {
	for _, kind := range store.IncidentKinds {
		d.setIncident(upstream, kind, false, "", now)
	}
}

// openKinds lists the kinds of the upstream's open incidents, in the order
// of store.IncidentKinds. Callers hold d.mu.
func (d *Daemon) openKinds(upstream string) []store.IncidentKind {
	var out []store.IncidentKind
	for _, kind := range store.IncidentKinds {
		if d.incidents[incidentKey{upstream, kind}] != nil {
			out = append(out, kind)
		}
	}
	return out
}

// routedTo counts the devices routed through the upstream. Callers hold
// d.mu.
func (d *Daemon) routedTo(upstream string) int {
	n := 0
	for _, name := range d.assignments {
		if name == upstream {
			n++
		}
	}
	return n
}

// IncidentsPage lists up to limit incidents of the upstream, or of every
// upstream when it is "": the open ones first, then the newest.
func (d *Daemon) IncidentsPage(upstream string, limit, offset int) ([]store.Incident, error) {
	return d.store.IncidentsPage(upstream, limit, offset)
}

// deviceErrors counts the errors an AWG upstream's device logs, such as
// "Failed to send data packets". The device reports them from its own
// goroutines.
type deviceErrors struct {
	mu    sync.Mutex
	count uint64
	last  string
}

func (e *deviceErrors) add(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.count++
	e.last = msg
}

// read returns the count since the device opened and the latest error.
func (e *deviceErrors) read() (uint64, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count, e.last
}

// sendErrorWindow judges an upstream's device errors a minute at a time:
// more than the threshold in a minute is trouble, which ends after
// sendErrorsQuiet minutes in a row at or below it.
type sendErrorWindow struct {
	seen    uint64 // the device's count at the previous check
	quiet   int    // minutes in a row at or below the threshold, while in trouble
	trouble bool
}

// observe takes the device's count at the end of a minute and returns the
// minute's errors and whether the upstream is in trouble.
func (w *sendErrorWindow) observe(count uint64, threshold int) (uint64, bool) {
	n := count - w.seen
	w.seen = count
	switch {
	case n > uint64(threshold):
		w.trouble, w.quiet = true, 0
	case w.trouble:
		w.quiet++
		w.trouble = w.quiet < sendErrorsQuiet
	}
	return n, w.trouble
}

// checkSendErrors closes the minute that ends at now for every AWG
// upstream: proxy upstreams have no device of their own. The detail names
// the minute's count and the latest error.
func (d *Daemon) checkSendErrors(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	threshold := d.cfg.Incidents.SendErrorsPerMinute
	for _, u := range d.ups {
		if u.Key != nil {
			continue
		}
		count, last := u.errs.read()
		n, trouble := u.sendErrors.observe(count, threshold)
		detail := ""
		if n > uint64(threshold) {
			detail = fmt.Sprintf("%d errors in a minute, the latest: %s", n, last)
		}
		d.setIncident(u.Name, store.IncidentSendErrors, trouble, detail, now)
	}
}

// watchSendErrors checks the send errors every minute until the returned
// function stops it.
func (d *Daemon) watchSendErrors(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				d.checkSendErrors(now)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
