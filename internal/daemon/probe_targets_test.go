package daemon

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
)

// targetProbe answers per target and records every call and every call the
// round cancelled. A target missing from answers fails; a target in block
// waits for the round to cancel it.
type targetProbe struct {
	mu        sync.Mutex
	answers   map[string]time.Duration
	block     map[string]bool
	called    map[string]int
	cancelled map[string]bool
}

func newTargetProbe(answers map[string]time.Duration, block ...string) *targetProbe {
	p := &targetProbe{answers: answers, block: map[string]bool{}, called: map[string]int{}, cancelled: map[string]bool{}}
	for _, t := range block {
		p.block[t] = true
	}
	return p
}

func (p *targetProbe) probe(ctx context.Context, _ string, _ netip.Addr, target netip.AddrPort, _ time.Duration) (time.Duration, error) {
	key := target.String()
	p.mu.Lock()
	p.called[key]++
	latency, ok := p.answers[key]
	block := p.block[key]
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		p.mu.Lock()
		p.cancelled[key] = true
		p.mu.Unlock()
		return 0, ctx.Err()
	}
	if !ok {
		return 0, errors.New("unreachable " + key)
	}
	return latency, nil
}

func (p *targetProbe) calls(target string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.called[target]
}

func targets(t *testing.T, addrs ...string) config.ProbeTargets {
	t.Helper()
	var out config.ProbeTargets
	for _, a := range addrs {
		out = append(out, netip.MustParseAddrPort(a))
	}
	return out
}

func startedHarness(t *testing.T, p *targetProbe, probe config.ProbeTargets) *harness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Health.Probe = probe
	h.d.probe = p.probe
	if err := h.d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	return h
}

func TestProbeSecondTargetKeepsUpstreamUp(t *testing.T) {
	p := newTargetProbe(map[string]time.Duration{"8.8.8.8:443": 40 * time.Millisecond, "9.9.9.9:443": 7 * time.Millisecond})
	h := startedHarness(t, p, targets(t, "1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"))
	h.d.probeAll(context.Background())
	for _, u := range h.d.ups {
		if u.Tracker.State() != health.Up {
			t.Errorf("upstream %s state = %v, want up", u.Name, u.Tracker.State())
		}
		if got := u.Tracker.Latency(); got != 7*time.Millisecond {
			t.Errorf("upstream %s latency = %v, want the fastest answer, 7ms", u.Name, got)
		}
		// One round is one probe result, however many targets it tried.
		if c := h.d.probes[u.Name]; c == nil || c[0] != 1 || c[1] != 0 {
			t.Errorf("upstream %s probe counts = %v, want {1 0}", u.Name, c)
		}
	}
}

func TestProbeAllTargetsFailingTakesUpstreamDown(t *testing.T) {
	p := newTargetProbe(nil)
	h := startedHarness(t, p, targets(t, "1.1.1.1:443", "8.8.8.8:443"))
	for range 2 { // down_after is 2
		h.d.probeAll(context.Background())
	}
	for _, u := range h.d.ups {
		if u.Tracker.State() != health.Down {
			t.Errorf("upstream %s state = %v, want down", u.Name, u.Tracker.State())
		}
	}
	want := 2 * len(h.d.ups)
	if p.calls("1.1.1.1:443") != want || p.calls("8.8.8.8:443") != want {
		t.Errorf("every target must be tried every round, want %d calls each, got %v", want, p.called)
	}
}

// The first answer cancels targets still waiting, and the round does not
// return before they have stopped.
func TestProbeAnswerCancelsSlowTargets(t *testing.T) {
	p := newTargetProbe(map[string]time.Duration{"8.8.8.8:443": time.Millisecond}, "1.1.1.1:443")
	h := startedHarness(t, p, targets(t, "1.1.1.1:443", "8.8.8.8:443"))
	done := make(chan struct{})
	go func() {
		h.d.probeAll(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("probeAll waited for the blocked target instead of cancelling it")
	}
	p.mu.Lock()
	cancelled := p.cancelled["1.1.1.1:443"]
	p.mu.Unlock()
	if !cancelled {
		t.Error("the blocked target was not cancelled before probeAll returned")
	}
	for _, u := range h.d.ups {
		if u.Tracker.State() != health.Up {
			t.Errorf("upstream %s state = %v, want up", u.Name, u.Tracker.State())
		}
	}
}

// health.probe is not restart-only: a reload swaps it and the next round uses it.
func TestReloadSwapsProbeTargets(t *testing.T) {
	p := newTargetProbe(map[string]time.Duration{"1.1.1.1:443": time.Millisecond, "9.9.9.9:443": time.Millisecond})
	h := startedHarness(t, p, targets(t, "1.1.1.1:443"))
	h.d.probeAll(context.Background())
	if p.calls("9.9.9.9:443") != 0 {
		t.Fatalf("calls before reload = %v", p.called)
	}
	next := *h.cfg
	next.Health.Probe = targets(t, "1.1.1.1:443", "9.9.9.9:443")
	if err := h.d.Reload(context.Background(), &next); err != nil {
		t.Fatal(err)
	}
	h.d.probeAll(context.Background())
	if p.calls("9.9.9.9:443") == 0 {
		t.Errorf("reloaded target was not probed, calls = %v", p.called)
	}
}
