package traffic

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeSource returns whatever the test set last.
type fakeSource struct {
	mu sync.Mutex
	r  Reading
}

func (f *fakeSource) set(r Reading) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.r = r
}

func (f *fakeSource) Counters() Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.r
}

// fakeClock moves only when the test says so.
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time      { return c.now }
func (c *fakeClock) add(d time.Duration) { c.now = c.now.Add(d) }

func newSampler(src Source, c *fakeClock) *Sampler { return &Sampler{Source: src, Now: c.Now} }

func peers(m map[int64]Counter) Reading { return Reading{Peers: m} }

func TestSamplerDeltas(t *testing.T) {
	src, clock := &fakeSource{}, newFakeClock()
	s := newSampler(src, clock)

	// The first sample counts what the counters hold but has no interval
	// for a rate.
	src.set(Reading{
		Peers:     map[int64]Counter{1: {Down: 1000, Up: 100}},
		Upstreams: map[string]Counter{"nl": {Down: 2000, Up: 200}},
	})
	got := s.Sample()
	want := Snapshot{
		Time:      clock.now,
		Peers:     map[int64]Traffic{1: {DownBytes: 1000, UpBytes: 100}},
		Upstreams: map[string]Traffic{"nl": {DownBytes: 2000, UpBytes: 200}},
		Total:     Traffic{DownBytes: 1000, UpBytes: 100},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("first sample (-want +got):\n%s", diff)
	}

	clock.add(2 * time.Second)
	src.set(Reading{
		Peers:     map[int64]Counter{1: {Down: 5000, Up: 300}},
		Upstreams: map[string]Counter{"nl": {Down: 6000, Up: 600}},
	})
	got = s.Sample()
	want = Snapshot{
		Time:      clock.now,
		Peers:     map[int64]Traffic{1: {DownBytes: 5000, UpBytes: 300, DownRate: 2000, UpRate: 100}},
		Upstreams: map[string]Traffic{"nl": {DownBytes: 6000, UpBytes: 600, DownRate: 2000, UpRate: 200}},
		Total:     Traffic{DownBytes: 5000, UpBytes: 300, DownRate: 2000, UpRate: 100},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("second sample (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want, s.Latest()); diff != "" {
		t.Errorf("Latest (-want +got):\n%s", diff)
	}

	// Nothing moved: the totals stay, the rates drop to zero.
	clock.add(time.Second)
	got = s.Sample()
	if p := got.Peers[1]; p != (Traffic{DownBytes: 5000, UpBytes: 300}) {
		t.Errorf("idle peer = %+v", p)
	}
}

// A counter that goes back restarted, as when an interface is recreated or
// a peer re-added: what it holds is new, and the total keeps growing.
func TestSamplerReset(t *testing.T) {
	src, clock := &fakeSource{}, newFakeClock()
	s := newSampler(src, clock)
	src.set(peers(map[int64]Counter{1: {Down: 10_000, Up: 1000}}))
	s.Sample()

	clock.add(time.Second)
	src.set(peers(map[int64]Counter{1: {Down: 300, Up: 2000}})) // only down went back: still a restart
	got := s.Sample()
	want := Traffic{DownBytes: 10_300, UpBytes: 3000, DownRate: 300, UpRate: 2000}
	if got.Peers[1] != want {
		t.Errorf("after reset = %+v, want %+v", got.Peers[1], want)
	}
	if got.Total.DownBytes != 10_300 || got.Total.UpBytes != 3000 {
		t.Errorf("total after reset = %+v", got.Total)
	}

	clock.add(time.Second)
	src.set(peers(map[int64]Counter{1: {Down: 800, Up: 2500}}))
	if got := s.Sample().Peers[1]; got != (Traffic{DownBytes: 10_800, UpBytes: 3500, DownRate: 500, UpRate: 500}) {
		t.Errorf("after reset, counting again = %+v", got)
	}
}

func TestSamplerSeriesComeAndGo(t *testing.T) {
	src, clock := &fakeSource{}, newFakeClock()
	s := newSampler(src, clock)
	src.set(peers(map[int64]Counter{1: {Down: 100, Up: 10}}))
	s.Sample()

	// A peer added since the previous sample gained all its bytes in this
	// interval.
	clock.add(time.Second)
	src.set(peers(map[int64]Counter{1: {Down: 100, Up: 10}, 2: {Down: 4000, Up: 400}}))
	got := s.Sample()
	if p := got.Peers[2]; p != (Traffic{DownBytes: 4000, UpBytes: 400, DownRate: 4000, UpRate: 400}) {
		t.Errorf("new peer = %+v", p)
	}
	if got.Total != (Traffic{DownBytes: 4100, UpBytes: 410, DownRate: 4000, UpRate: 400}) {
		t.Errorf("total with a new peer = %+v", got.Total)
	}

	// A removed peer leaves the snapshot and the total rate; its bytes stay
	// in the total.
	clock.add(time.Second)
	src.set(peers(map[int64]Counter{2: {Down: 5000, Up: 500}}))
	got = s.Sample()
	if _, ok := got.Peers[1]; ok || len(got.Peers) != 1 {
		t.Errorf("peers after removal = %+v", got.Peers)
	}
	if got.Total != (Traffic{DownBytes: 5100, UpBytes: 510, DownRate: 1000, UpRate: 100}) {
		t.Errorf("total after removal = %+v", got.Total)
	}

	// An upstream missing from a reading, such as while its interface is
	// recreated, keeps its total when it comes back, and its rate covers
	// the whole time it was away.
	src.set(Reading{Upstreams: map[string]Counter{"nl": {Down: 1000, Up: 100}}})
	s.Sample()
	clock.add(time.Second)
	src.set(Reading{})
	if got := s.Sample(); len(got.Upstreams) != 0 {
		t.Errorf("upstreams while missing = %+v", got.Upstreams)
	}
	clock.add(time.Second)
	src.set(Reading{Upstreams: map[string]Counter{"nl": {Down: 3000, Up: 300}}})
	if got := s.Sample().Upstreams["nl"]; got != (Traffic{DownBytes: 3000, UpBytes: 300, DownRate: 1000, UpRate: 100}) {
		t.Errorf("upstream back = %+v", got)
	}
	clock.add(time.Second)
	src.set(Reading{Upstreams: map[string]Counter{"nl": {Down: 50, Up: 5}}}) // a new interface
	if got := s.Sample().Upstreams["nl"]; got != (Traffic{DownBytes: 3050, UpBytes: 305, DownRate: 50, UpRate: 5}) {
		t.Errorf("upstream on a new interface = %+v", got)
	}
}

// Rates use the clock's monotonic reading (time.Now's), so a wall clock
// step changes nothing. A clock that does not advance, or goes back, gives
// no rate rather than a huge or negative one, and the bytes still count.
func TestSamplerClock(t *testing.T) {
	src, clock := &fakeSource{}, newFakeClock()
	s := newSampler(src, clock)
	src.set(peers(map[int64]Counter{1: {Down: 100}}))
	s.Sample()
	src.set(peers(map[int64]Counter{1: {Down: 200}}))
	if got := s.Sample().Peers[1]; got != (Traffic{DownBytes: 200}) {
		t.Errorf("no time passed = %+v", got)
	}
	clock.add(-time.Second)
	src.set(peers(map[int64]Counter{1: {Down: 300}}))
	if got := s.Sample().Peers[1]; got != (Traffic{DownBytes: 300}) {
		t.Errorf("clock went back = %+v", got)
	}
}

func TestSamplerRun(t *testing.T) {
	src := &fakeSource{}
	src.set(peers(map[int64]Counter{1: {Down: 1}}))
	s := &Sampler{Source: src}
	if !s.Latest().Time.IsZero() {
		t.Error("Latest before the first sample has a time")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx, 10*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for s.Latest().Peers[1].DownBytes != 2 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not pick up the new counter")
		}
		src.set(peers(map[int64]Counter{1: {Down: 2}}))
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}
