package history

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

type fakeSource struct {
	health    map[string]UpstreamHealth
	traffic   traffic.Snapshot
	retention config.History
}

func (f *fakeSource) UpstreamHealth() map[string]UpstreamHealth { return f.health }
func (f *fakeSource) Traffic() traffic.Snapshot                 { return f.traffic }
func (f *fakeSource) Retention() config.History                 { return f.retention }

type written struct {
	ts    time.Time
	ups   []store.UpstreamSample
	peers []store.PeerSample
}

type compaction struct {
	now                  time.Time
	minuteKeep, hourKeep time.Duration
}

type fakeSink struct {
	writes      []written
	compactions []compaction
	writeErr    error
	compactErr  error
}

func (f *fakeSink) AddSamples(ts time.Time, ups []store.UpstreamSample, peers []store.PeerSample) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, written{ts, ups, peers})
	return nil
}

func (f *fakeSink) Compact(now time.Time, minuteKeep, hourKeep time.Duration) error {
	f.compactions = append(f.compactions, compaction{now, minuteKeep, hourKeep})
	return f.compactErr
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newWriter() (*Writer, *fakeSource, *fakeSink, *fakeClock) {
	src := &fakeSource{
		health:    map[string]UpstreamHealth{"nl": up(10 * time.Millisecond)},
		traffic:   snap(start, map[int64][2]uint64{1: {0, 0}}, nil),
		retention: config.History{MinuteRetention: 7 * 24 * time.Hour, HourRetention: 90 * 24 * time.Hour},
	}
	sink, clock := &fakeSink{}, &fakeClock{now: start}
	return &Writer{Source: src, Sink: sink, Now: clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, src, sink, clock
}

// One transaction per minute, written when the first reading of the next
// minute arrives, however many readings the minute held.
func TestWriterWritesOncePerMinute(t *testing.T) {
	w, src, sink, clock := newWriter()
	for i := 0; i < 60; i++ { // 12:00:00 to 12:00:59
		src.traffic = snap(clock.now, map[int64][2]uint64{1: {uint64(i) * 10, uint64(i)}}, nil)
		w.Tick()
		clock.now = clock.now.Add(time.Second)
	}
	if len(sink.writes) != 0 {
		t.Fatalf("written before the minute ended: %+v", sink.writes)
	}
	w.Tick() // 12:01:00
	if len(sink.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(sink.writes))
	}
	want := written{ts: start,
		ups:   []store.UpstreamSample{{Name: "nl", UpRatio: 1, RTT: 10 * time.Millisecond}},
		peers: []store.PeerSample{{PeerID: 1, Down: 590, Up: 59}}} // the first reading is the baseline
	if diff := cmp.Diff(want, sink.writes[0], cmp.AllowUnexported(written{})); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// A failed write loses its minute and nothing else: the next one is written.
func TestWriterSurvivesAFailedWrite(t *testing.T) {
	w, _, sink, clock := newWriter()
	w.Tick()
	sink.writeErr = errors.New("database is locked")
	clock.now = clock.now.Add(time.Minute)
	w.Tick() // 12:01: the write of 12:00 fails
	sink.writeErr = nil
	clock.now = clock.now.Add(time.Minute)
	w.Tick() // 12:02: 12:01 is written
	if len(sink.writes) != 1 || !sink.writes[0].ts.Equal(start.Add(time.Minute)) {
		t.Errorf("writes = %+v", sink.writes)
	}
}

// Compaction runs at the first tick, which expires what a stopped daemon
// left, then once an hour with the retention the source has by then.
func TestWriterCompactsHourly(t *testing.T) {
	w, src, sink, clock := newWriter()
	w.Tick()
	if len(sink.compactions) != 1 || sink.compactions[0] != (compaction{start, 7 * 24 * time.Hour, 90 * 24 * time.Hour}) {
		t.Fatalf("first tick: %+v", sink.compactions)
	}
	clock.now = start.Add(59*time.Minute + 59*time.Second)
	w.Tick()
	if len(sink.compactions) != 1 {
		t.Errorf("compacted again within the hour: %+v", sink.compactions)
	}
	src.retention = config.History{MinuteRetention: 48 * time.Hour, HourRetention: 100 * 24 * time.Hour} // a reload
	clock.now = start.Add(time.Hour)
	w.Tick()
	if len(sink.compactions) != 2 || sink.compactions[1] != (compaction{start.Add(time.Hour), 48 * time.Hour, 100 * 24 * time.Hour}) {
		t.Errorf("an hour later: %+v", sink.compactions)
	}
}

// A compaction that fails is tried again an hour later, not every second.
func TestWriterDoesNotHammerAFailingCompaction(t *testing.T) {
	w, _, sink, clock := newWriter()
	sink.compactErr = errors.New("disk full")
	for i := 0; i < 5; i++ {
		w.Tick()
		clock.now = clock.now.Add(time.Second)
	}
	if len(sink.compactions) != 1 {
		t.Errorf("compactions = %d, want 1", len(sink.compactions))
	}
}
