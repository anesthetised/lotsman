// Package health decides whether an upstream is usable from a stream of probe results.
package health

import "time"

type State int

const (
	Unknown State = iota
	Up
	Down
)

func (s State) String() string {
	switch s {
	case Up:
		return "up"
	case Down:
		return "down"
	}
	return "unknown"
}

// Tracker applies hysteresis: it takes downAfter consecutive failures to leave Up
// and upAfter consecutive successes to leave Down. From Unknown a single success
// is enough, so a freshly started daemon routes traffic as soon as it can.
type Tracker struct {
	downAfter, upAfter int
	state              State
	fails, oks         int
	latency            time.Duration
}

func NewTracker(downAfter, upAfter int) *Tracker {
	return &Tracker{downAfter: downAfter, upAfter: upAfter}
}

// Observe records one probe result and reports whether the state changed.
func (t *Tracker) Observe(ok bool, latency time.Duration) bool {
	before := t.state
	if ok {
		t.fails = 0
		t.oks++
		t.latency = latency
		if t.state == Unknown || t.oks >= t.upAfter {
			t.state = Up
		}
	} else {
		t.oks = 0
		t.fails++
		if t.fails >= t.downAfter {
			t.state = Down
		}
	}
	return t.state != before
}

func (t *Tracker) State() State { return t.state }

// Latency is the most recent successful probe time.
func (t *Tracker) Latency() time.Duration { return t.latency }
