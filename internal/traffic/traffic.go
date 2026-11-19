// Package traffic turns byte counters into totals and rates per peer and per
// upstream. A Sampler reads every counter at once from a Source, typically
// once a second, and keeps the latest result for any number of readers.
//
// Directions are from the client's side everywhere: down is towards the
// client.
package traffic

import (
	"context"
	"sync"
	"time"
)

// Counter is one series' byte counters as its source keeps them. They only
// grow until the source restarts them, such as when an interface is
// recreated.
type Counter struct {
	Down, Up uint64
}

// Reading is every counter the source could read, by series. A series it
// cannot read now is left out.
type Reading struct {
	Peers     map[int64]Counter  // by the store's peer row
	Upstreams map[string]Counter // by upstream name
}

// Source reads the current counters. The daemon implements it.
type Source interface {
	Counters() Reading
}

// Traffic is a series' bytes since the sampler's first reading, counting
// what its counters held then, and its rates over the last interval.
type Traffic struct {
	DownBytes, UpBytes uint64
	DownRate, UpRate   uint64 // bytes per second
}

// Snapshot is the result of one sample. Its maps are never changed once
// Latest returns them.
type Snapshot struct {
	Time      time.Time // zero before the first sample
	Peers     map[int64]Traffic
	Upstreams map[string]Traffic
	Total     Traffic // every peer's bytes, removed peers' included; the rate is the current peers' sum
}

// Sampler keeps the totals across counter restarts and computes rates from
// the deltas between samples. Sample is called by one goroutine; Latest is
// safe for any number.
type Sampler struct {
	Source Source
	Now    func() time.Time // time.Now when nil; its monotonic reading makes rates immune to clock changes

	polled    time.Time // of the previous sample; zero before the first
	peers     map[int64]*series
	upstreams map[string]*series
	total     Traffic

	mu     sync.Mutex
	latest Snapshot
}

// series is what the sampler remembers of one counter. A series is kept
// after it disappears from the readings, so one that comes back, such as an
// upstream whose interface was missing for a moment, keeps its total. Peer
// rows and upstream names are few, so they are never forgotten.
type series struct {
	last  Counter   // the previous reading
	seen  time.Time // when it was read
	total Traffic
}

// Sample reads the counters once and publishes the result to Latest.
func (s *Sampler) Sample() Snapshot {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	r := s.Source.Counters()
	if s.peers == nil {
		s.peers, s.upstreams = map[int64]*series{}, map[string]*series{}
	}
	snap := Snapshot{Time: now, Peers: map[int64]Traffic{}, Upstreams: map[string]Traffic{}}
	for id, c := range r.Peers {
		t, delta := update(s.peers, id, c, now, s.polled)
		snap.Peers[id] = t
		s.total.DownBytes += delta.Down
		s.total.UpBytes += delta.Up
		snap.Total.DownRate += t.DownRate
		snap.Total.UpRate += t.UpRate
	}
	for name, c := range r.Upstreams {
		snap.Upstreams[name], _ = update(s.upstreams, name, c, now, s.polled)
	}
	snap.Total.DownBytes, snap.Total.UpBytes = s.total.DownBytes, s.total.UpBytes
	s.polled = now

	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = snap
	return snap
}

// Latest is the result of the last Sample.
func (s *Sampler) Latest() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// Run samples at once and then every interval until ctx is done.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		s.Sample()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// update adds a reading to its series and returns the series' traffic and
// the bytes it gained. polled is the previous sample's time.
func update[K comparable](all map[K]*series, key K, cur Counter, now, polled time.Time) (Traffic, Counter) {
	s, known := all[key]
	var delta Counter
	var since time.Time // the start of the interval delta covers; zero when unknown
	switch {
	case !known:
		// Every byte is new: counters start with their device, and the
		// daemon creates its devices. A series that appears after the
		// first sample gained them since the previous one.
		s = &series{}
		all[key] = s
		delta, since = cur, polled
	case cur.Down < s.last.Down || cur.Up < s.last.Up:
		// The counters restarted, as when an interface is recreated: all
		// they hold is new.
		delta, since = cur, s.seen
	default:
		delta, since = Counter{Down: cur.Down - s.last.Down, Up: cur.Up - s.last.Up}, s.seen
	}
	s.last, s.seen = cur, now
	s.total.DownBytes += delta.Down
	s.total.UpBytes += delta.Up
	s.total.DownRate, s.total.UpRate = 0, 0
	if !since.IsZero() {
		elapsed := now.Sub(since)
		s.total.DownRate, s.total.UpRate = rate(delta.Down, elapsed), rate(delta.Up, elapsed)
	}
	return s.total, delta
}

func rate(bytes uint64, elapsed time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	return uint64(float64(bytes) / elapsed.Seconds())
}
