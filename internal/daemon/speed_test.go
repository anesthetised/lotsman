package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/history"
)

// speedHarness is transferHarness with the speed test on instead of the
// transfer probe, due every round. Each way takes the fake's 20 ms, so
// 2000 bytes make 100000 bytes per second.
func speedHarness(t *testing.T) (h *harness, f *fakeTransfer, round func()) {
	t.Helper()
	h, f, round, _ = transferHarness(t, 1)
	h.cfg.Health.Transfer = nil
	h.cfg.Health.Speed = &config.Speed{DownURL: "https://a.test/down", UpURL: "https://a.test/up", Bytes: 2000, Timeout: time.Second, Interval: time.Nanosecond}
	return h, f, round
}

// One speed test at a time, through the upstreams in turn: tests at once
// would share the gateway's link. The result shows in the status and in
// what the history writer reads.
func TestSpeedTests(t *testing.T) {
	h, f, round := speedHarness(t)
	round()
	if f.callsTo("lm-up-nl") != 1 || f.callsTo("lm-up-nl up") != 1 || f.callsTo("lm-up-de") != 0 {
		t.Fatalf("first round: nl %d down %d up, de %d; want one test, through nl", f.callsTo("lm-up-nl"), f.callsTo("lm-up-nl up"), f.callsTo("lm-up-de"))
	}
	sp := statusOf(h, "nl").Speed
	if sp.Down != 100000 || sp.Up != 100000 || time.Since(sp.Time) > time.Minute {
		t.Errorf("nl speed = %+v", sp)
	}
	if !statusOf(h, "de").Speed.Time.IsZero() {
		t.Error("de has a speed before its test")
	}
	if got := h.d.UpstreamHealth()["nl"].Speed; got != sp {
		t.Errorf("history reads %+v, want %+v", got, sp)
	}
	// nl's next test is not due yet: it waits a jittered interval.
	h.d.mu.Lock()
	h.d.speeds[h.d.ups[0]].next = time.Now().Add(time.Hour)
	h.d.mu.Unlock()
	round()
	if f.callsTo("lm-up-de") != 1 || statusOf(h, "de").Speed.Down != 100000 {
		t.Errorf("second round: de %d tests, speed %+v", f.callsTo("lm-up-de"), statusOf(h, "de").Speed)
	}
	if f.callsTo("lm-up-nl") != 1 {
		t.Error("nl tested again before it was due")
	}
}

// A failed test changes no state and keeps the last result; it only informs.
func TestSpeedTestFailureChangesNothing(t *testing.T) {
	h, f, round := speedHarness(t)
	round()
	last := statusOf(h, "nl").Speed
	f.set("lm-up-nl up", true)
	h.d.mu.Lock()
	h.d.speeds[h.d.ups[1]] = &speedState{next: time.Now().Add(time.Hour)} // keep de out of the way
	h.d.mu.Unlock()
	for range 3 {
		round()
	}
	st := statusOf(h, "nl")
	if st.Speed != last || st.State != health.Up || st.Degraded || len(st.Incidents) != 0 {
		t.Errorf("after failed tests: %+v; want the earlier speed %+v and nothing else changed", st, last)
	}
}

// Turning the speed test off by a reload clears the results.
func TestSpeedOffOnReload(t *testing.T) {
	h, f, round := speedHarness(t)
	round()
	off, err := config.Parse(fmt.Appendf(nil, harnessConfig, h.dir, h.dir, h.dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	if !statusOf(h, "nl").Speed.Time.IsZero() {
		t.Error("speed kept after the test was turned off")
	}
	calls := f.callsTo("lm-up-nl") + f.callsTo("lm-up-de")
	round()
	if f.callsTo("lm-up-nl")+f.callsTo("lm-up-de") != calls {
		t.Error("speed test ran after it was turned off")
	}
}

// A speed test reaches the stored history: its minute carries the rates.
func TestSpeedHistory(t *testing.T) {
	h, _, round := speedHarness(t)
	round()
	minute := time.Now().Truncate(time.Hour).Add(-3 * time.Hour) // in the past, away from the real writer
	clock := minute
	w := &history.Writer{Source: h.d, Sink: h.st, Now: func() time.Time { return clock }, Log: quiet}
	w.Tick()
	clock = clock.Add(time.Minute)
	w.Tick() // nl's test counted in the first minute only
	clock = clock.Add(time.Minute)
	w.Tick()
	res, err := h.d.UpstreamHistory("nl", history.Range{From: minute, To: minute.Add(5 * time.Minute)})
	if err != nil || len(res.Samples) != 2 {
		t.Fatalf("nl = %+v, %v", res, err)
	}
	if s := res.Samples[0]; s.DownRate != 100000 || s.UpRate != 100000 {
		t.Errorf("first minute = %+v, want the test's rates", s)
	}
	if s := res.Samples[1]; s.DownRate != 0 || s.UpRate != 0 {
		t.Errorf("second minute = %+v, want no test", s)
	}
}
