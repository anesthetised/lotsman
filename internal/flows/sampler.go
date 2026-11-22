package flows

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// Sampler reads conntrack once per interval, independently of Watch clients.
// It keeps only the latest open client flows in memory.
type Sampler struct {
	source Source
	peers  Peers
	subnet netip.Prefix
	ready  chan struct{}

	seen map[key]record // only the sampling goroutine writes
	mu   sync.Mutex
	rows []Entry
	err  error
	once sync.Once
}

func NewSampler(source Source, peers Peers, subnet netip.Prefix) *Sampler {
	return &Sampler{source: source, peers: peers, subnet: subnet, ready: make(chan struct{})}
}

// Ready closes on the first attempt, including when conntrack is unavailable.
func (s *Sampler) Ready() <-chan struct{} { return s.ready }

// Entries returns the latest sample. Callers must not modify its entries.
func (s *Sampler) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, s.err
}

func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.sample(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sampler) sample(now time.Time) {
	entries, err := s.source.Entries()
	var rows []Entry
	if err == nil {
		peers := s.peers.Peers()
		seen := make(map[key]record, len(s.seen))
		for _, e := range entries {
			peer, ok := clientFlow(e, s.subnet, peers)
			if !ok {
				continue
			}
			k := key{peer.ID, e.Protocol, e.Source, e.Destination}
			prev, known := s.seen[k]
			cur, _, _ := observe(e, prev, known, s.seen != nil, now)
			e.sampled, e.started, e.lastTraffic = true, cur.started, cur.lastTraffic
			seen[k] = cur
			rows = append(rows, e)
		}
		s.seen = seen
	}
	s.mu.Lock()
	s.rows, s.err = rows, err
	s.mu.Unlock()
	s.once.Do(func() { close(s.ready) })
}
