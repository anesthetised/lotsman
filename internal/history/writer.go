package history

import (
	"context"
	"log/slog"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/store"
	"github.com/anesthetised/lotsman/internal/traffic"
)

const (
	// Interval is how often the writer reads its source. The minute's
	// samples are written when a reading enters the next minute, so a
	// minute's data is in the database within Interval of its end.
	Interval = time.Second
	// CompactInterval is how often history is downsampled and expired. The
	// scan is one pass over the minute tables, and nothing depends on it
	// being prompt: a query reads minute and hour rows alike.
	CompactInterval = time.Hour
)

// Source is the daemon.
type Source interface {
	UpstreamHealth() map[string]UpstreamHealth
	Traffic() traffic.Snapshot
	// Retention is read at every compaction, so a reload applies.
	Retention() config.History
}

// Sink is the store.
type Sink interface {
	AddSamples(ts time.Time, ups []store.UpstreamSample, peers []store.PeerSample) error
	Compact(now time.Time, minuteKeep, hourKeep time.Duration) error
}

// Writer feeds a Recorder from the source and writes each finished minute
// in one transaction. A write that fails is logged and its minute is lost:
// the next minute does not depend on it.
type Writer struct {
	Source Source
	Sink   Sink
	Now    func() time.Time // time.Now when nil
	Log    *slog.Logger

	rec         Recorder
	nextCompact time.Time // zero: compact at the first Tick, which expires what a stopped daemon left
}

// Tick reads the source once, writes the minute it finished, if any, and
// compacts when it is due.
func (w *Writer) Tick() {
	now := time.Now()
	if w.Now != nil {
		now = w.Now()
	}
	if b, ok := w.rec.Observe(now, w.Source.UpstreamHealth(), w.Source.Traffic()); ok {
		if err := w.Sink.AddSamples(b.Time, b.Upstreams, b.Peers); err != nil {
			w.Log.Error("history: write failed, the minute is lost", "minute", b.Time, "err", err)
		}
	}
	if !now.Before(w.nextCompact) {
		keep := w.Source.Retention()
		if err := w.Sink.Compact(now, keep.MinuteRetention, keep.HourRetention); err != nil {
			w.Log.Error("history: compaction failed, trying again in an hour", "err", err)
		}
		w.nextCompact = now.Add(CompactInterval)
	}
}

// Run ticks every Interval until ctx is done.
func (w *Writer) Run(ctx context.Context) {
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	for {
		w.Tick()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
