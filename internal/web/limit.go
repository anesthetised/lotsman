package web

import (
	"net/netip"
	"sync"
	"time"
)

// limiter gives each client a burst of attempts, refilled at one per
// interval: a token bucket per client. An IPv6 client counts by its /64,
// which one host usually holds whole.
type limiter struct {
	burst    float64
	interval time.Duration
	// max bounds the clients remembered. Past it, full buckets are
	// forgotten, and if that is not enough, every bucket: a flood from that
	// many addresses gets that many bursts anyway.
	max int

	mu      sync.Mutex
	clients map[netip.Prefix]bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newLimiter(burst int, interval time.Duration) *limiter {
	return &limiter{burst: float64(burst), interval: interval, max: 10000, clients: make(map[netip.Prefix]bucket)}
}

// allow takes one attempt from a's bucket, if it has one.
func (l *limiter) allow(a netip.Addr, now time.Time) bool {
	key := clientPrefix(a)
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.clients[key]
	if !ok {
		if len(l.clients) >= l.max {
			l.prune(now)
		}
		b = bucket{tokens: l.burst, at: now}
	}
	b = l.refill(b, now)
	ok = b.tokens >= 1
	if ok {
		b.tokens--
	}
	l.clients[key] = b
	return ok
}

func (l *limiter) refill(b bucket, now time.Time) bucket {
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed.Seconds()/l.interval.Seconds())
		b.at = now
	}
	return b
}

func (l *limiter) prune(now time.Time) {
	for key, b := range l.clients {
		if l.refill(b, now).tokens >= l.burst {
			delete(l.clients, key)
		}
	}
	if len(l.clients) >= l.max {
		clear(l.clients)
	}
}

func clientPrefix(a netip.Addr) netip.Prefix {
	bits := a.BitLen()
	if a.Is6() {
		bits = 64
	}
	p, _ := a.Prefix(bits)
	return p
}
