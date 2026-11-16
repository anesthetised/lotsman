package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/store"
)

// lockedBuffer is a log sink the test reads while the daemon writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// watchHarness is a running daemon whose health round never comes again, so
// only the firewall watch can repair the firewall, with two routed peers.
type watchHarness struct {
	*harness
	logs  *lockedBuffer
	peers []store.Peer
	stop  func()
}

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	h := newHarness(t)
	h.cfg.Health.Interval = time.Hour
	logs := &lockedBuffer{}
	h.d.log = slog.New(slog.NewTextHandler(logs, nil))
	if _, err := h.st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	var peers []store.Peer
	for _, profile := range []string{"nl", "eu"} {
		p, err := h.st.AddPeer("alice", store.DefaultDevice, profile, h.cfg.Subnet)
		if err != nil {
			t.Fatal(err)
		}
		peers = append(peers, p)
	}
	w := &watchHarness{harness: h, logs: logs, peers: peers}
	w.stop = h.run(t)
	select {
	case <-h.d.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("daemon not ready") // Run failed, such as on a busy listen port
	}
	w.wantAllowed(t, "after start")
	return w
}

func (h *watchHarness) wantAllowed(t *testing.T, when string) {
	t.Helper()
	eventually(t, "every peer admitted "+when, func() bool {
		for _, p := range h.peers {
			if !h.dp.IsAllowed(p.IP) {
				return false
			}
		}
		return true
	})
}

// settled waits until the daemon has stopped calling Verify and returns the
// number of calls.
func (h *watchHarness) settled(t *testing.T) int {
	t.Helper()
	n := h.dp.Verifies()
	for range 50 {
		time.Sleep(5 * firewallSettle)
		m := h.dp.Verifies()
		if m == n {
			return n
		}
		n = m
	}
	t.Fatalf("Verify keeps being called: %d calls", n)
	return 0
}

// A firewall another tool removes is repaired as soon as the change is
// reported, not at the next health round, an hour away here (issue #81).
func TestFirewallRepairedOnChange(t *testing.T) {
	h := newWatchHarness(t)
	before := h.settled(t)

	h.dp.FlushFirewall()
	h.wantAllowed(t, "after the flush")
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs = %v, want 1", got)
	}
	// The repair is itself reported as a change, which costs one more check
	// that finds the firewall intact, and nothing after that.
	if got := h.settled(t) - before; got != 2 {
		t.Errorf("Verify called %d times for one flush, want 2 (the repair and its echo)", got)
	}
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs after settling = %v, want 1", got)
	}
}

// A burst of changes, such as a firewall reload made of several
// transactions, costs one repair and one check.
func TestFirewallChangeBurstRepairsOnce(t *testing.T) {
	h := newWatchHarness(t)
	before := h.settled(t)

	start := time.Now()
	h.dp.FlushFirewall()
	for range 10 {
		time.Sleep(firewallSettle / 40)
		h.dp.Notify(nil)
	}
	burst := time.Since(start)
	h.wantAllowed(t, "after the burst")
	// The repair and its echo. Without settling it is about 10.
	got := h.settled(t) - before
	switch {
	case burst >= firewallSettle/2:
		t.Logf("a loaded runner stretched the burst to %s; %d Verify calls not checked", burst, got)
	case got != 2:
		t.Errorf("Verify called %d times for one burst, want 2", got)
	}
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs = %v, want 1", got)
	}
}

// A failed watch is logged once and checked at once, since changes may have
// been missed; the health round remains the fallback.
func TestFirewallWatchFailure(t *testing.T) {
	h := newWatchHarness(t)
	before := h.settled(t)

	for range 2 {
		h.dp.Notify(errors.New("monitor overrun"))
		eventually(t, "a check after the failure", func() bool { return h.dp.Verifies() > before })
		before = h.settled(t)
	}
	if got := strings.Count(h.logs.String(), "level=ERROR msg=\"watching the firewall for changes failed"); got != 1 {
		t.Errorf("watch failure logged %d times, want once:\n%s", got, h.logs)
	}
}

// After shutdown the watch is gone: a change no longer reaches the daemon.
func TestFirewallWatchStops(t *testing.T) {
	h := newWatchHarness(t)
	h.stop()
	n := h.dp.Verifies()
	h.dp.FlushFirewall()
	time.Sleep(5 * firewallSettle)
	if got := h.dp.Verifies(); got != n {
		t.Errorf("Verify called %d times after shutdown", got-n)
	}
}

func (h *watchHarness) repairs(t *testing.T) float64 {
	t.Helper()
	v, ok := metric(h.d.Metrics(), "lotsman_firewall_repairs_total")
	if !ok {
		t.Fatal("lotsman_firewall_repairs_total missing")
	}
	return v
}
