package history

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

var start = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// snap builds a traffic sample from totals; a peer or upstream maps to
// {down, up}.
func snap(at time.Time, peers map[int64][2]uint64, upstreams map[string][2]uint64) traffic.Snapshot {
	s := traffic.Snapshot{Time: at, Peers: map[int64]traffic.Traffic{}, Upstreams: map[string]traffic.Traffic{}}
	for id, c := range peers {
		s.Peers[id] = traffic.Traffic{DownBytes: c[0], UpBytes: c[1]}
	}
	for name, c := range upstreams {
		s.Upstreams[name] = traffic.Traffic{DownBytes: c[0], UpBytes: c[1]}
	}
	return s
}

func up(latency time.Duration) UpstreamHealth { return UpstreamHealth{Up: true, Latency: latency} }

var down = UpstreamHealth{}

// A minute's batch carries the growth of the totals inside it and the share
// of readings that found the upstream up.
func TestRecorderMinute(t *testing.T) {
	var r Recorder
	h := map[string]UpstreamHealth{"nl": up(20 * time.Millisecond)}
	// Readings at 12:00:00, :20 and :40, then 12:01:00.
	r.Observe(start, h, snap(start, map[int64][2]uint64{1: {100, 10}}, map[string][2]uint64{"nl": {1000, 100}}))
	h["nl"] = down
	r.Observe(start.Add(20*time.Second), h, snap(start, map[int64][2]uint64{1: {150, 12}}, map[string][2]uint64{"nl": {1100, 110}}))
	h["nl"] = up(40 * time.Millisecond)
	if _, ok := r.Observe(start.Add(40*time.Second), h, snap(start, map[int64][2]uint64{1: {250, 20}}, map[string][2]uint64{"nl": {1300, 130}})); ok {
		t.Fatal("a batch before the minute ended")
	}
	b, ok := r.Observe(start.Add(time.Minute), h, snap(start, map[int64][2]uint64{1: {250, 20}}, map[string][2]uint64{"nl": {1300, 130}}))
	if !ok {
		t.Fatal("no batch after the minute ended")
	}
	want := Batch{
		Time: start,
		// Up in 2 of 3 readings; the latency is the mean of those two.
		Upstreams: []store.UpstreamSample{{Name: "nl", UpRatio: 2.0 / 3, RTT: 30 * time.Millisecond, Down: 300, Up: 30}},
		// The first sample is the baseline: 100 and 10 were there before.
		Peers: []store.PeerSample{{PeerID: 1, Down: 150, Up: 10}},
	}
	if diff := cmp.Diff(want, b); diff != "" {
		t.Errorf("batch (-want +got):\n%s", diff)
	}
}

// The first sample sets the baseline, so a recorder that starts against a
// sampler that has counted for hours does not write them into its first
// minute, and a daemon restart, whose sampler starts again from zero, does
// not make the next minute's delta negative.
func TestRecorderRestart(t *testing.T) {
	var r Recorder
	big := snap(start, map[int64][2]uint64{1: {5e9, 1e9}}, map[string][2]uint64{"nl": {6e9, 2e9}})
	h := map[string]UpstreamHealth{"nl": up(time.Millisecond)}
	r.Observe(start, h, big)
	b, _ := r.Observe(start.Add(time.Minute), h, snap(start, map[int64][2]uint64{1: {5e9 + 7, 1e9 + 1}}, map[string][2]uint64{"nl": {6e9 + 9, 2e9 + 2}}))
	if got := b.Peers[0]; got.Down != 0 || got.Up != 0 {
		t.Errorf("first minute counted the baseline: %+v", got)
	}
	b, _ = r.Observe(start.Add(2*time.Minute), h, snap(start, map[int64][2]uint64{1: {5e9 + 10, 1e9 + 3}}, map[string][2]uint64{"nl": {6e9 + 9, 2e9 + 2}}))
	if want := (store.PeerSample{PeerID: 1, Down: 7, Up: 1}); b.Peers[0] != want {
		t.Errorf("second minute = %+v, want %+v", b.Peers[0], want)
	}

	// A new process: a fresh recorder over a sampler that restarted at zero
	// reads small totals, and what follows is a plain delta. The reading that
	// enters a minute belongs to it.
	var r2 Recorder
	r2.Observe(start, h, snap(start, map[int64][2]uint64{1: {40, 4}}, nil))
	r2.Observe(start.Add(time.Minute), h, snap(start, map[int64][2]uint64{1: {100, 5}}, nil))
	b, _ = r2.Observe(start.Add(2*time.Minute), h, snap(start, map[int64][2]uint64{1: {100, 5}}, nil))
	if b.Peers[0].Down != 60 || b.Peers[0].Up != 1 {
		t.Errorf("after a restart: %+v", b.Peers[0])
	}

	// A total that goes back inside one process counts as restarted: all it
	// holds is new, never an underflow. The peer also gained 3 down and 2 up
	// at 12:02:00, before it went back.
	r.Observe(start.Add(2*time.Minute+10*time.Second), h, snap(start, map[int64][2]uint64{1: {30, 3}}, map[string][2]uint64{"nl": {20, 2}}))
	b, _ = r.Observe(start.Add(3*time.Minute), h, snap(start, map[int64][2]uint64{1: {30, 3}}, map[string][2]uint64{"nl": {20, 2}}))
	if want := (store.PeerSample{PeerID: 1, Down: 33, Up: 5}); b.Peers[0] != want {
		t.Errorf("a total that went back: %+v, want %+v", b.Peers[0], want)
	}
	if want := (store.UpstreamSample{Name: "nl", UpRatio: 1, RTT: time.Millisecond, Down: 20, Up: 2}); b.Upstreams[0] != want {
		t.Errorf("an upstream total that went back: %+v, want %+v", b.Upstreams[0], want)
	}
}

// Peers and upstreams come and go: one that appears counts in full from
// then on, one that leaves keeps the bytes it moved and stops getting rows,
// and one that returns is measured against what it held.
func TestRecorderSeriesAppearAndDisappear(t *testing.T) {
	var r Recorder
	h := map[string]UpstreamHealth{"nl": up(time.Millisecond)}
	r.Observe(start, h, snap(start, map[int64][2]uint64{1: {10, 1}}, map[string][2]uint64{"nl": {10, 1}}))

	// Minute 0: peer 2 and upstream "de" appear mid-minute.
	h["de"] = up(2 * time.Millisecond)
	r.Observe(start.Add(30*time.Second), h, snap(start, map[int64][2]uint64{1: {20, 1}, 2: {500, 50}}, map[string][2]uint64{"nl": {10, 1}, "de": {300, 30}}))
	// Peer 1 leaves, and "nl" with it, after moving 5 more bytes.
	r.Observe(start.Add(45*time.Second), map[string]UpstreamHealth{"de": up(2 * time.Millisecond)},
		snap(start, map[int64][2]uint64{1: {25, 1}, 2: {600, 60}}, map[string][2]uint64{"nl": {10, 1}, "de": {400, 40}}))
	r.Observe(start.Add(50*time.Second), map[string]UpstreamHealth{"de": up(2 * time.Millisecond)},
		snap(start, map[int64][2]uint64{2: {600, 60}}, map[string][2]uint64{"de": {400, 40}}))
	b, ok := r.Observe(start.Add(time.Minute), map[string]UpstreamHealth{"de": up(2 * time.Millisecond)},
		snap(start, map[int64][2]uint64{2: {700, 70}}, map[string][2]uint64{"de": {400, 40}}))
	if !ok {
		t.Fatal("no batch")
	}
	wantPeers := []store.PeerSample{{PeerID: 1, Down: 15, Up: 0}, {PeerID: 2, Down: 600, Up: 60}}
	if diff := cmp.Diff(wantPeers, b.Peers); diff != "" {
		t.Errorf("peers of minute 0 (-want +got):\n%s", diff)
	}
	// "nl" was up in both its readings; "de" in all three of its.
	wantUps := []store.UpstreamSample{
		{Name: "de", UpRatio: 1, RTT: 2 * time.Millisecond, Down: 400, Up: 40},
		{Name: "nl", UpRatio: 1, RTT: time.Millisecond},
	}
	if diff := cmp.Diff(wantUps, b.Upstreams); diff != "" {
		t.Errorf("upstreams of minute 0 (-want +got):\n%s", diff)
	}

	// Minute 1: the reading at 12:01:00 is peer 2's 100 more. Peer 1 and "nl"
	// come back at 12:01:30 holding 10 and 1 more than when they left.
	r.Observe(start.Add(90*time.Second), map[string]UpstreamHealth{"de": up(time.Millisecond), "nl": down},
		snap(start, map[int64][2]uint64{2: {700, 70}, 1: {35, 2}}, map[string][2]uint64{"de": {400, 40}, "nl": {20, 3}}))
	b, _ = r.Observe(start.Add(2*time.Minute), map[string]UpstreamHealth{"de": up(time.Millisecond), "nl": down},
		snap(start, map[int64][2]uint64{2: {700, 70}, 1: {35, 2}}, map[string][2]uint64{"de": {400, 40}, "nl": {20, 3}}))
	wantPeers = []store.PeerSample{{PeerID: 1, Down: 10, Up: 1}, {PeerID: 2, Down: 100, Up: 10}}
	if diff := cmp.Diff(wantPeers, b.Peers); diff != "" {
		t.Errorf("peers of minute 1 (-want +got):\n%s", diff)
	}
	if got := b.Upstreams[1]; got.Name != "nl" || got.Down != 10 || got.Up != 2 {
		t.Errorf("upstream that came back: %+v, want 10 down and 2 up, not its whole total", got)
	}
}

// Health without a traffic sample yet still counts: before the first sample
// the sampler's time is zero.
func TestRecorderBeforeTheFirstTrafficSample(t *testing.T) {
	var r Recorder
	h := map[string]UpstreamHealth{"nl": down}
	r.Observe(start, h, traffic.Snapshot{})
	r.Observe(start.Add(10*time.Second), h, snap(start, map[int64][2]uint64{1: {900, 90}}, nil))
	r.Observe(start.Add(time.Minute), h, snap(start, map[int64][2]uint64{1: {950, 95}}, nil))
	b, _ := r.Observe(start.Add(2*time.Minute), h, snap(start, map[int64][2]uint64{1: {950, 95}}, nil))
	want := Batch{Time: start.Add(time.Minute),
		Upstreams: []store.UpstreamSample{{Name: "nl", UpRatio: 0}},
		Peers:     []store.PeerSample{{PeerID: 1, Down: 50, Up: 5}}}
	if diff := cmp.Diff(want, b); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// A minute with no upstream reading and no traffic has nothing to write, and
// an upstream seen only through its traffic gets no row.
func TestRecorderEmptyAndTrafficOnly(t *testing.T) {
	var r Recorder
	r.Observe(start, nil, snap(start, nil, map[string][2]uint64{"ghost": {1, 1}}))
	b, ok := r.Observe(start.Add(time.Minute), nil, snap(start, nil, map[string][2]uint64{"ghost": {9, 9}}))
	if !ok || len(b.Upstreams) != 0 || len(b.Peers) != 0 {
		t.Errorf("batch = %+v, %v", b, ok)
	}
}

// Readings may skip a minute entirely, as when the process was suspended:
// the finished minute is written under its own time and the gap stays empty.
func TestRecorderGap(t *testing.T) {
	var r Recorder
	h := map[string]UpstreamHealth{"nl": up(time.Millisecond)}
	r.Observe(start, h, snap(start, map[int64][2]uint64{1: {0, 0}}, nil))
	b, ok := r.Observe(start.Add(10*time.Minute+5*time.Second), h, snap(start, map[int64][2]uint64{1: {50, 5}}, nil))
	if !ok || !b.Time.Equal(start) {
		t.Fatalf("batch = %+v, %v", b, ok)
	}
	b, ok = r.Observe(start.Add(11*time.Minute), h, snap(start, map[int64][2]uint64{1: {50, 5}}, nil))
	if !ok || !b.Time.Equal(start.Add(10*time.Minute)) {
		t.Errorf("next batch = %+v, %v", b, ok)
	}
}

// A speed test counts once, in the minute of the first reading that shows
// it, though every reading after shows it until the next test.
func TestRecorderSpeedTests(t *testing.T) {
	var r Recorder
	first := health.Speed{Down: 1000, Up: 100, Time: start.Add(10 * time.Second)}
	second := health.Speed{Down: 3000, Up: 300, Time: start.Add(50 * time.Second)}
	read := func(at time.Duration, sp health.Speed) (Batch, bool) {
		h := up(time.Millisecond)
		h.Speed = sp
		return r.Observe(start.Add(at), map[string]UpstreamHealth{"nl": h, "de": up(time.Millisecond)}, traffic.Snapshot{})
	}
	read(0, health.Speed{})
	read(20*time.Second, first)
	read(40*time.Second, first)
	read(55*time.Second, second)
	b, ok := read(time.Minute, second)
	if !ok {
		t.Fatal("no batch after the minute ended")
	}
	want := []store.UpstreamSample{
		{Name: "de", UpRatio: 1, RTT: time.Millisecond},
		{Name: "nl", UpRatio: 1, RTT: time.Millisecond, SpeedTests: 2, DownRateSum: 4000, UpRateSum: 400},
	}
	if diff := cmp.Diff(want, b.Upstreams); diff != "" {
		t.Errorf("first minute (-want +got):\n%s", diff)
	}
	// The next minute still shows the second test, and does not count it again.
	b, _ = read(2*time.Minute, second)
	if got := b.Upstreams[1]; got.SpeedTests != 0 || got.DownRateSum != 0 {
		t.Errorf("second minute = %+v, want no speed test", got)
	}
}
