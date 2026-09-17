package health

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestTracker(t *testing.T) {
	type step struct {
		ok      bool
		want    State
		changed bool
	}
	cases := []struct {
		name               string
		downAfter, upAfter int
		steps              []step
	}{
		{"first success goes up immediately", 3, 2, []step{
			{true, Up, true},
		}},
		{"failures from unknown need downAfter", 3, 2, []step{
			{false, Unknown, false},
			{false, Unknown, false},
			{false, Down, true},
		}},
		{"up survives fewer than downAfter failures", 3, 2, []step{
			{true, Up, true},
			{false, Up, false},
			{false, Up, false},
			{true, Up, false},
			{false, Up, false},
			{false, Up, false},
			{false, Down, true},
		}},
		{"down needs upAfter successes", 3, 2, []step{
			{false, Unknown, false}, {false, Unknown, false}, {false, Down, true},
			{true, Down, false},
			{false, Down, false},
			{true, Down, false},
			{true, Up, true},
		}},
		{"upAfter one recovers on first success", 1, 1, []step{
			{false, Down, true},
			{true, Up, true},
			{false, Down, true},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTracker(c.downAfter, c.upAfter)
			for i, s := range c.steps {
				changed := tr.Observe(s.ok, time.Millisecond)
				if tr.State() != s.want || changed != s.changed {
					t.Fatalf("step %d (ok=%v): state=%v changed=%v, want state=%v changed=%v",
						i, s.ok, tr.State(), changed, s.want, s.changed)
				}
			}
		})
	}
}

func TestTrackerLatencyKeepsLastSuccess(t *testing.T) {
	tr := NewTracker(2, 2)
	tr.Observe(true, 10*time.Millisecond)
	tr.Observe(false, 0)
	if tr.Latency() != 10*time.Millisecond {
		t.Errorf("latency = %v", tr.Latency())
	}
}

func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	target := netip.MustParseAddrPort(ln.Addr().String())
	source := netip.MustParseAddr("127.0.0.1")

	latency, err := Probe(context.Background(), source, target, time.Second)
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	if latency <= 0 {
		t.Errorf("latency = %v", latency)
	}

	ln.Close()
	if _, err := Probe(context.Background(), source, target, time.Second); err == nil {
		t.Error("expected failure against closed listener")
	}
}
