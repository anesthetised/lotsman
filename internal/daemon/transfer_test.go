package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/store"
)

// fakeTransfer is a transfer prober whose results the test sets per
// upstream interface.
type fakeTransfer struct {
	mu       sync.Mutex
	failing  map[string]bool
	calls    map[string]int
	inFlight map[string]int
	most     int           // the most probes in flight through one upstream at once
	hold     chan struct{} // when set, probes wait for it to close or for ctx
	urls     map[string]bool
}

func newFakeTransfer() *fakeTransfer {
	return &fakeTransfer{failing: map[string]bool{}, calls: map[string]int{}, inFlight: map[string]int{}, urls: map[string]bool{}}
}

func (f *fakeTransfer) probe(ctx context.Context, iface string, _ netip.Addr, url string, _ int64, _ time.Duration) (time.Duration, error) {
	f.mu.Lock()
	f.calls[iface]++
	f.inFlight[iface]++
	f.most = max(f.most, f.inFlight[iface])
	f.urls[url] = true
	hold, failing := f.hold, f.failing[iface]
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight[iface]--
		f.mu.Unlock()
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if failing {
		return 0, errors.New("16384 of 65536 bytes: context deadline exceeded")
	}
	return 20 * time.Millisecond, nil
}

func (f *fakeTransfer) set(iface string, failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[iface] = failing
}

func (f *fakeTransfer) callsTo(iface string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[iface]
}

// transferHarness is a started daemon driven round by round by the test:
// round makes every transfer probe due first, sched keeps the schedule.
func transferHarness(t *testing.T, upAfter int) (h *harness, f *fakeTransfer, round, sched func()) {
	t.Helper()
	h = newHarness(t)
	f = newFakeTransfer()
	h.d.transfer = f.probe
	h.cfg.Health.UpAfter = upAfter
	h.cfg.Health.Transfer = &config.Transfer{URL: config.TransferURLs{"https://a.test/down", "https://b.test/down"},
		MinBytes: 65536, Timeout: time.Second, Interval: time.Nanosecond}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		h.d.transferRuns.Wait()
		h.d.stop()
	})
	if err := h.d.start(ctx); err != nil {
		t.Fatal(err)
	}
	sched = func() {
		h.d.probeAll(ctx)
		h.d.transferRuns.Wait()
		if err := h.d.Reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	round = func() {
		h.d.mu.Lock()
		for _, st := range h.d.transfers {
			st.next = time.Time{}
		}
		h.d.mu.Unlock()
		sched()
	}
	return h, f, round, sched
}

// nextProbe is how long until the next transfer probe through upstream i.
func nextProbe(h *harness, i int) time.Duration {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return time.Until(h.d.transfers[h.d.ups[i]].next)
}

func degradedOf(h *harness, name string) bool {
	for _, u := range h.d.Status().Upstreams {
		if u.Name == name {
			return u.Degraded
		}
	}
	panic("no upstream " + name)
}

func TestTransferDegradesAndRecovers(t *testing.T) {
	h, f, round, _ := transferHarness(t, 2) // the harness has down_after 2
	h.st.CreateUser("alice")
	euPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)
	routeOf := func(p store.Peer) string { r, _ := h.dp.RouteOf(p.IP); return r }
	nl := metrics.Label{Name: "upstream", Value: "nl"}

	round()
	if routeOf(euPeer) != "lm-up-nl" || degradedOf(h, "nl") {
		t.Fatalf("start: eu via %q, nl degraded %v", routeOf(euPeer), degradedOf(h, "nl"))
	}
	if v, ok := metric(h.d.Metrics(), "lotsman_upstream_transfer_seconds", nl); !ok || v != 0.02 {
		t.Errorf("transfer_seconds{nl} = %v, %v", v, ok)
	}
	if !f.urls["https://a.test/down"] && !f.urls["https://b.test/down"] {
		t.Errorf("probed URLs %v", f.urls)
	}

	// NL's path freezes: one failure is not enough, down_after failures are.
	f.set("lm-up-nl", true)
	round()
	if degradedOf(h, "nl") || routeOf(euPeer) != "lm-up-nl" {
		t.Fatal("degraded after a single failed transfer probe")
	}
	round()
	if !degradedOf(h, "nl") {
		t.Fatal("not degraded after down_after failed transfer probes")
	}
	st := h.d.Status().Upstreams[0]
	if st.State.String() != "up" {
		t.Errorf("reachability state = %v, want up: only throughput failed", st.State)
	}
	// With an alternative, degraded is as good as down: eu fails over to DE,
	// and nl, now nobody's route, is not alive, so no flow stays pinned to it.
	if routeOf(euPeer) != "lm-up-de" {
		t.Errorf("degraded nl: eu via %q", routeOf(euPeer))
	}
	if h.dp.Alive["lm-up-nl"] || h.dp.PinnedTo(euPeer.IP, "lm-up-nl") {
		t.Errorf("degraded nl with an alternative: alive %v, eu pinned to it %v", h.dp.Alive["lm-up-nl"], h.dp.PinnedTo(euPeer.IP, "lm-up-nl"))
	}
	m := h.d.Metrics()
	if v, _ := metric(m, "lotsman_upstream_degraded", nl); v != 1 {
		t.Errorf("degraded{nl} = %v", v)
	}
	if v, _ := metric(m, "lotsman_upstream_degraded", metrics.Label{Name: "upstream", Value: "de"}); v != 0 {
		t.Errorf("degraded{de} = %v", v)
	}
	if v, _ := metric(m, "lotsman_upstream_up", nl); v != 1 {
		t.Errorf("up{nl} = %v", v)
	}
	if v, _ := metric(m, "lotsman_upstream_transfer_probes_total", nl, metrics.Label{Name: "result", Value: "fail"}); v != 2 {
		t.Errorf("transfer_probes_total{nl,fail} = %v", v)
	}

	// Recovery takes up_after passing probes.
	f.set("lm-up-nl", false)
	round()
	if !degradedOf(h, "nl") || routeOf(euPeer) != "lm-up-de" {
		t.Fatal("recovered after one passing transfer probe; up_after is 2")
	}
	round()
	if degradedOf(h, "nl") || routeOf(euPeer) != "lm-up-nl" || !h.dp.Alive["lm-up-nl"] {
		t.Fatalf("after recovery: degraded %v, eu via %q", degradedOf(h, "nl"), routeOf(euPeer))
	}
}

// A degraded upstream is the last resort of a profile with nothing else, and
// stays alive while it is anyone's route, so those clients keep working.
func TestTransferLastResort(t *testing.T) {
	h, f, round, _ := transferHarness(t, 1)
	h.st.CreateUser("alice")
	nlPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
	euPeer, _ := h.st.AddPeer("alice", store.DefaultDevice, "eu", h.cfg.Subnet)
	routeOf := func(p store.Peer) string { r, _ := h.dp.RouteOf(p.IP); return r }
	round()
	f.set("lm-up-nl", true)
	for range 4 {
		round()
	}
	if !degradedOf(h, "nl") {
		t.Fatal("nl not degraded")
	}
	// eu has DE; the nl profile has only nl and stays on it.
	if routeOf(euPeer) != "lm-up-de" || routeOf(nlPeer) != "lm-up-nl" {
		t.Errorf("eu via %q, nl via %q", routeOf(euPeer), routeOf(nlPeer))
	}
	if !h.dp.Alive["lm-up-nl"] || !h.dp.PinnedTo(euPeer.IP, "lm-up-nl") {
		t.Errorf("degraded last resort: alive %v, eu's old flows pinned %v; want both", h.dp.Alive["lm-up-nl"], h.dp.PinnedTo(euPeer.IP, "lm-up-nl"))
	}
	for _, line := range h.dp.Log {
		if line == "alive lm-up-nl=false" {
			t.Error("a last-resort upstream was marked dead, cutting its clients' flows")
		}
	}

	// Its last client leaves: degraded with nobody on it, nl is no longer alive.
	h.st.DeletePeer("alice", store.DefaultDevice, "nl")
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if h.dp.Alive["lm-up-nl"] || h.dp.PinnedTo(euPeer.IP, "lm-up-nl") {
		t.Errorf("degraded, nobody's route: alive %v, eu pinned %v; want neither", h.dp.Alive["lm-up-nl"], h.dp.PinnedTo(euPeer.IP, "lm-up-nl"))
	}
}

func TestTransferOffOnReload(t *testing.T) {
	h, f, round, _ := transferHarness(t, 1)
	f.set("lm-up-nl", true)
	round()
	round()
	if !degradedOf(h, "nl") || h.dp.Alive["lm-up-nl"] {
		t.Fatal("nl not degraded")
	}
	off, err := config.Parse(fmt.Appendf(nil, harnessConfig, h.dir, h.dir, h.dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), off); err != nil {
		t.Fatal(err)
	}
	if degradedOf(h, "nl") || !h.dp.Alive["lm-up-nl"] {
		t.Errorf("transfer turned off: nl degraded %v, alive %v", degradedOf(h, "nl"), h.dp.Alive["lm-up-nl"])
	}
	calls := f.callsTo("lm-up-nl")
	round()
	if f.callsTo("lm-up-nl") != calls {
		t.Error("transfer probe ran after it was turned off")
	}
}

func TestTransferSkipsUpstreamsThatAreDown(t *testing.T) {
	h, f, round, _ := transferHarness(t, 1)
	h.setHealthy("lm-up-nl", false)
	for range 3 {
		round()
	}
	if f.callsTo("lm-up-nl") != 0 || f.callsTo("lm-up-de") != 3 {
		t.Errorf("transfer probes: nl %d (down), de %d (up)", f.callsTo("lm-up-nl"), f.callsTo("lm-up-de"))
	}
	if degradedOf(h, "nl") {
		t.Error("an upstream that is down reports degraded")
	}
}

func TestTransferOneInFlight(t *testing.T) {
	h, f, round, _ := transferHarness(t, 1)
	f.hold = make(chan struct{})
	ctx := context.Background()
	h.d.probeAll(ctx) // a health round never waits for transfer probes
	eventually(t, "a transfer probe through each upstream", func() bool {
		return f.callsTo("lm-up-nl") == 1 && f.callsTo("lm-up-de") == 1
	})
	h.d.probeAll(ctx)
	h.d.probeAll(ctx)
	time.Sleep(50 * time.Millisecond) // time for a second probe to show up if one started
	if f.callsTo("lm-up-nl") != 1 || f.callsTo("lm-up-de") != 1 {
		t.Errorf("probes started: nl %d, de %d; want one each", f.callsTo("lm-up-nl"), f.callsTo("lm-up-de"))
	}
	close(f.hold)
	h.d.transferRuns.Wait()
	round()
	if f.callsTo("lm-up-nl") != 2 || f.most != 1 {
		t.Errorf("after the first finished: %d probes, at most %d at once", f.callsTo("lm-up-nl"), f.most)
	}
}

func TestTransferInterval(t *testing.T) {
	h, f, round, sched := transferHarness(t, 2)
	h.cfg.Health.Interval = time.Minute // the floor of a retry; the fakes ignore it otherwise
	h.cfg.Health.Transfer.Interval = time.Hour
	within := func(what string, lo, hi time.Duration) {
		t.Helper()
		if next := nextProbe(h, 0); next < lo || next > hi {
			t.Errorf("%s: next probe in %v, want %v to %v", what, next, lo, hi)
		}
	}
	sched()
	sched()
	if f.callsTo("lm-up-nl") != 1 {
		t.Errorf("%d probes within the interval, want 1", f.callsTo("lm-up-nl"))
	}
	within("passing", 47*time.Minute, 72*time.Minute)

	// Failing: a fifth of the interval, ±20%, until it passes while not degraded.
	f.set("lm-up-nl", true)
	round()
	within("one failure", 9*time.Minute, 15*time.Minute)
	round()
	within("degraded", 9*time.Minute, 15*time.Minute)
	f.set("lm-up-nl", false)
	round()
	if !degradedOf(h, "nl") {
		t.Fatal("recovered after one pass; up_after is 2")
	}
	within("degraded, one pass", 9*time.Minute, 15*time.Minute)
	round()
	within("recovered", 47*time.Minute, 72*time.Minute)

	// A fifth of a short interval is still at least a health interval.
	h.cfg.Health.Transfer.Interval = 2 * time.Minute
	f.set("lm-up-nl", true)
	round()
	within("short interval", 55*time.Second, time.Minute)
}

func TestJitter(t *testing.T) {
	const interval = 5 * time.Minute
	seen := map[time.Duration]bool{}
	for range 1000 {
		j := jitter(interval)
		if j < interval*8/10 || j >= interval*12/10 {
			t.Fatalf("jitter(%v) = %v, outside ±20%%", interval, j)
		}
		seen[j] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct intervals in 1000", len(seen))
	}
}

// Run stops transfer probes and waits for them before it tears anything down.
func TestTransferStopsWithRun(t *testing.T) {
	h := newHarness(t)
	f := newFakeTransfer()
	f.hold = make(chan struct{}) // never closed: only ctx ends the probe
	torn := make(chan bool, 2)
	h.d.transfer = func(ctx context.Context, iface string, source netip.Addr, url string, n int64, timeout time.Duration) (time.Duration, error) {
		took, err := f.probe(ctx, iface, source, url, n, timeout)
		time.Sleep(50 * time.Millisecond) // a probe takes a moment to unwind
		torn <- h.dp.TornDown             // Teardown comes after Run waited for this probe
		return took, err
	}
	h.cfg.Health.Transfer = &config.Transfer{URL: config.TransferURLs{"https://a.test/"}, MinBytes: 1, Timeout: time.Second, Interval: time.Minute}
	stop := h.run(t)
	eventually(t, "transfer probes started", func() bool { return f.callsTo("lm-up-nl") == 1 && f.callsTo("lm-up-de") == 1 })
	stop()
	for range 2 {
		if <-torn {
			t.Error("a transfer probe was still running at teardown")
		}
	}
}
