// Package history records each upstream's availability and each peer's and
// upstream's traffic a minute at a time, keeps them downsampled for as long
// as the retention says, and plans range queries over them.
//
// Directions are from the client's side, as in package traffic: down is
// towards the client.
package history

import (
	"cmp"
	"slices"
	"time"

	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// Resolution is the period of the finest samples.
const Resolution = time.Minute

// UpstreamHealth is what the daemon knows of an upstream at one moment.
type UpstreamHealth struct {
	// Up is true for a healthy upstream, and false while it is down, not
	// probed yet, or degraded: reachable but failing its transfer probes. A
	// degraded upstream does not carry traffic, so it is not available; the
	// transfer probe exists to catch exactly that.
	Up      bool
	Latency time.Duration // the latest successful probe's time
}

// Batch is one finished minute, ready to be stored.
type Batch struct {
	Time      time.Time // the minute's start
	Upstreams []store.UpstreamSample
	Peers     []store.PeerSample
}

// Recorder folds readings into minutes. It is pure: the caller supplies the
// time and the readings, once a second or so, and gets a Batch back when a
// reading falls in a later minute than the one being folded. Observe is
// called by one goroutine.
type Recorder struct {
	minute time.Time // the minute being folded; zero before the first reading
	synced bool      // a traffic sample has been seen, so the baselines are set

	// The totals as of the latest reading, kept for series that leave the
	// samples and come back: their delta is against what they held, not
	// against zero.
	peerTotals     map[int64]traffic.Counter
	upstreamTotals map[string]traffic.Counter

	peers     map[int64]*traffic.Counter
	upstreams map[string]*upstreamMinute
}

// upstreamMinute is what one upstream did so far in the minute.
type upstreamMinute struct {
	ticks, up int // readings, and those that found it up
	rttSum    time.Duration
	traffic   traffic.Counter
}

// Observe folds one reading in. health is every upstream the daemon runs and
// tr its latest traffic sample. The result is the previous minute's batch,
// when now is in another minute than the last reading; the reading that
// enters a minute belongs to that minute.
//
// Bytes are the growth of the sampler's totals between readings, so a
// reading the sampler had no new sample for adds nothing and a late one adds
// both seconds. The first sample of the recorder sets the baseline: bytes
// counted before it are not this recorder's, and its first minute would
// show a spike for them. A series that appears later counts in full, as the
// sampler does, a total that went back counts as restarted, and one that is
// missing from a sample keeps its baseline.
func (r *Recorder) Observe(now time.Time, health map[string]UpstreamHealth, tr traffic.Snapshot) (Batch, bool) {
	minute := now.Truncate(Resolution)
	var done Batch
	var finished bool
	if !r.minute.IsZero() && !minute.Equal(r.minute) {
		done, finished = r.finish(), true
	}
	if r.peers == nil {
		r.peerTotals, r.upstreamTotals = map[int64]traffic.Counter{}, map[string]traffic.Counter{}
		r.peers, r.upstreams = map[int64]*traffic.Counter{}, map[string]*upstreamMinute{}
	}
	r.minute = minute

	for name, h := range health {
		u := r.upstream(name)
		u.ticks++
		if h.Up {
			u.up++
			u.rttSum += h.Latency
		}
	}
	if !tr.Time.IsZero() {
		first := !r.synced
		for id, t := range tr.Peers {
			c := r.peers[id]
			if c == nil {
				c = &traffic.Counter{}
				r.peers[id] = c
			}
			g := gained(r.peerTotals, id, counter(t), first)
			c.Down += g.Down
			c.Up += g.Up
		}
		for name, t := range tr.Upstreams {
			g := gained(r.upstreamTotals, name, counter(t), first)
			u := r.upstream(name)
			u.traffic.Down += g.Down
			u.traffic.Up += g.Up
		}
		r.synced = true
	}
	return done, finished
}

func (r *Recorder) upstream(name string) *upstreamMinute {
	u := r.upstreams[name]
	if u == nil {
		u = &upstreamMinute{}
		r.upstreams[name] = u
	}
	return u
}

// finish returns the folded minute and starts a new one. Every series seen
// in the minute gets a row, idle or not, so a missing minute means the daemon
// was not running. An upstream's row follows its health readings; traffic
// without any, which a configured upstream never has, gets no row.
func (r *Recorder) finish() Batch {
	b := Batch{Time: r.minute}
	for name, u := range r.upstreams {
		if u.ticks == 0 {
			continue
		}
		s := store.UpstreamSample{Name: name, UpRatio: float64(u.up) / float64(u.ticks), Down: u.traffic.Down, Up: u.traffic.Up}
		if u.up > 0 {
			s.RTT = u.rttSum / time.Duration(u.up)
		}
		b.Upstreams = append(b.Upstreams, s)
	}
	for id, c := range r.peers {
		b.Peers = append(b.Peers, store.PeerSample{PeerID: id, Down: c.Down, Up: c.Up})
	}
	slices.SortFunc(b.Upstreams, func(a, b store.UpstreamSample) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(b.Peers, func(a, b store.PeerSample) int { return cmp.Compare(a.PeerID, b.PeerID) })
	r.peers, r.upstreams = map[int64]*traffic.Counter{}, map[string]*upstreamMinute{}
	return b
}

func counter(t traffic.Traffic) traffic.Counter {
	return traffic.Counter{Down: t.DownBytes, Up: t.UpBytes}
}

// gained is how much a series grew since the previous reading, and records
// the new total. first is the recorder's first sample.
func gained[K comparable](totals map[K]traffic.Counter, key K, cur traffic.Counter, first bool) traffic.Counter {
	prev, known := totals[key]
	totals[key] = cur
	switch {
	case !known && first:
		return traffic.Counter{}
	case !known, cur.Down < prev.Down || cur.Up < prev.Up:
		return cur
	}
	return traffic.Counter{Down: cur.Down - prev.Down, Up: cur.Up - prev.Up}
}
