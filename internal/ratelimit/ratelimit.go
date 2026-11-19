// Package ratelimit limits events per key, such as a user or a client
// address, with a token bucket for each.
package ratelimit

import (
	"sync"
	"time"
)

// pruneAt is the number of keys past which full buckets are forgotten. A
// full bucket is the same as none, so forgetting it changes nothing.
const pruneAt = 1024

// Limiter allows burst events at once per key, and then one more every
// interval. Time is passed in, so callers and tests choose the clock.
type Limiter[K comparable] struct {
	every time.Duration
	burst float64

	mu      sync.Mutex
	buckets map[K]bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func New[K comparable](every time.Duration, burst int) *Limiter[K] {
	return &Limiter[K]{every: every, burst: float64(burst), buckets: map[K]bucket{}}
}

// Allow takes one event's token for key and reports whether there was one.
func (l *Limiter[K]) Allow(key K, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	if len(l.buckets) >= pruneAt {
		l.prune(now)
	}
	l.buckets[key] = b
	return true
}

// Exhausted reports whether key has no token left, without taking one.
func (l *Limiter[K]) Exhausted(key K, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key, now).tokens < 1
}

func (l *Limiter[K]) refill(key K, now time.Time) bucket {
	b, ok := l.buckets[key]
	if !ok {
		return bucket{tokens: l.burst, at: now}
	}
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+float64(elapsed)/float64(l.every))
		b.at = now
	}
	return b
}

func (l *Limiter[K]) prune(now time.Time) {
	for key := range l.buckets {
		if l.refill(key, now).tokens >= l.burst {
			delete(l.buckets, key)
		}
	}
}
