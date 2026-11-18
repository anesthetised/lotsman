package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/proxy"
)

// stuckHelper runs proxy links in this process whose requests of the kinds
// the test sticks wait until it frees them, as a stuck helper's requests
// wait until they time out.
type stuckHelper struct {
	mu      sync.Mutex
	gates   map[string]chan struct{} // request → closed to let it through
	exited  func(error)              // the last link's
	waiting chan string              // requests that started waiting
}

func newStuckHelper(h *harness) *stuckHelper {
	s := &stuckHelper{gates: map[string]chan struct{}{}, waiting: make(chan string, 16)}
	h.d.runProxy = s.run
	return s
}

func (s *stuckHelper) run(t tun.Device, spec proxy.Spec, log *slog.Logger, exited func(error)) (proxy.Link, error) {
	l, err := proxy.InProcess(t, spec, log, nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exited = exited
	return stuckLink{l, s}, nil
}

// stick makes requests of kind op wait from now on.
func (s *stuckHelper) stick(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gates[op] = make(chan struct{})
}

// free lets requests of kind op through, the waiting ones too.
func (s *stuckHelper) free(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.gates[op]; g != nil {
		close(g)
		delete(s.gates, op)
	}
}

func (s *stuckHelper) freeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op, g := range s.gates {
		close(g)
		delete(s.gates, op)
	}
}

func (s *stuckHelper) wait(op string) {
	s.mu.Lock()
	g := s.gates[op]
	s.mu.Unlock()
	if g == nil {
		return
	}
	select {
	case s.waiting <- op:
	default:
	}
	<-g
}

// await waits until a request of kind op is stuck.
func (s *stuckHelper) await(t *testing.T, op string) {
	t.Helper()
	select {
	case got := <-s.waiting:
		if got != op {
			t.Fatalf("stuck request %q, want %q", got, op)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s request started", op)
	}
}

// exit reports that the last link stopped on its own.
func (s *stuckHelper) exit() {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	exited(errors.New("proxy helper exited: signal: killed"))
}

type stuckLink struct {
	proxy.Link
	s *stuckHelper
}

func (l stuckLink) Up() error {
	l.s.wait("up")
	return l.Link.Up()
}

func (l stuckLink) SetServer(key string, addr netip.AddrPort) error {
	l.s.wait("server")
	return l.Link.SetServer(key, addr)
}

func (l stuckLink) SetMaxFlows(n int) error {
	l.s.wait("max_flows")
	return l.Link.SetMaxFlows(n)
}

func (l stuckLink) Stats() (proxy.Stats, error) {
	l.s.wait("stats")
	return l.Link.Stats()
}

func (l stuckLink) Close() {
	l.s.wait("close")
	l.Link.Close()
}

// inBackground runs op; cleanup frees every stuck request and waits for op,
// even when the test fails early.
func inBackground(t *testing.T, s *stuckHelper, op func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	t.Cleanup(func() {
		s.freeAll()
		<-done
	})
	return done
}

// A stuck helper must not hold the daemon mutex: Status, Metrics and
// Reconcile answer while a request waits for it, whichever request it is.
func TestHelperRequestsDoNotBlockDaemon(t *testing.T) {
	setup := func(t *testing.T) (*harness, *stuckHelper, *fakeResolver) {
		h, dns := newProxyHarness(t)
		return h, newStuckHelper(h), dns
	}
	start := func(t *testing.T) (*harness, *stuckHelper, *fakeResolver) {
		h, s, dns := setup(t)
		if err := h.d.start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.d.stop)
		t.Cleanup(s.freeAll) // runs first, so stop never waits
		return h, s, dns
	}
	ol := metrics.Label{Name: "upstream", Value: "ol"}

	t.Run("startup", func(t *testing.T) {
		h, s, _ := setup(t)
		t.Cleanup(h.d.stop)
		s.stick("up")
		var err error
		done := inBackground(t, s, func() { err = h.d.start(context.Background()) })
		s.await(t, "up")
		assertResponsive(t, h)
		s.free("up")
		<-done
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("health round stats", func(t *testing.T) {
		h, s, _ := start(t)
		if _, ok := metric(h.d.Metrics(), "lotsman_upstream_flows", ol); ok {
			t.Error("flows reported before a health round asked the helper")
		}
		s.stick("stats")
		done := inBackground(t, s, func() { h.d.probeAll(context.Background()) })
		s.await(t, "stats")
		assertResponsive(t, h)
		s.free("stats")
		<-done
		if _, ok := metric(h.d.Metrics(), "lotsman_upstream_flows", ol); !ok {
			t.Error("flows the health round fetched are not reported")
		}
	})

	t.Run("endpoint change", func(t *testing.T) {
		h, s, dns := start(t)
		dns.set("ss.provider.test", "192.0.2.11")
		h.setHealthy("lm-up-ol", false)
		s.stick("server")
		done := inBackground(t, s, func() { h.d.probeAll(context.Background()) })
		s.await(t, "server")
		assertResponsive(t, h)
		s.free("server")
		<-done
		if u := h.upstream("ol"); u.Resolved != "192.0.2.11:8388" {
			t.Errorf("ol server = %s, want the re-resolved 192.0.2.11:8388", u.Resolved)
		}
	})

	t.Run("endpoint change raced by a removal", func(t *testing.T) {
		h, s, dns := start(t)
		u := h.upstream("ol")
		dns.set("ss.provider.test", "192.0.2.11")
		h.setHealthy("lm-up-ol", false)
		s.stick("server")
		done := inBackground(t, s, func() { h.d.probeAll(context.Background()) })
		s.await(t, "server")
		without := *h.cfg
		without.Upstreams = h.cfg.Upstreams[:1]
		if err := h.d.Reload(context.Background(), &without); err != nil {
			t.Fatal(err)
		}
		s.free("server")
		<-done
		if u.Resolved != "192.0.2.10:8388" {
			t.Errorf("removed upstream's server = %s, want it left at 192.0.2.10:8388", u.Resolved)
		}
	})

	t.Run("reload max_flows", func(t *testing.T) {
		h, s, _ := start(t)
		s.stick("max_flows")
		var err error
		done := inBackground(t, s, func() { err = h.d.Reload(context.Background(), proxyHarnessConfig(t, h.dir, ", max_flows: 7")) })
		s.await(t, "max_flows")
		assertResponsive(t, h)
		s.free("max_flows")
		<-done
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reload replacing the upstream", func(t *testing.T) {
		h, s, _ := start(t)
		old := h.upstream("ol")
		key := mustRead(t, filepath.Join(h.dir, "ol.key"))
		os.WriteFile(filepath.Join(h.dir, "ol.key"), []byte(key[:len(key)-1]+"2\n"), 0o600) // another name, so the key changed
		s.stick("close")
		s.stick("up")
		var err error
		done := inBackground(t, s, func() { err = h.d.Reload(context.Background(), h.cfg) })
		s.await(t, "close")
		assertResponsive(t, h)
		s.free("close")
		s.await(t, "up")
		assertResponsive(t, h)
		s.free("up")
		<-done
		if err != nil {
			t.Fatal(err)
		}
		u := h.upstream("ol")
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		if u == nil || u == old || h.d.notUp[u] {
			t.Errorf("ol after reload: replaced %v, up %v", u != old, u != nil && !h.d.notUp[u])
		}
	})

	t.Run("restart after an exit", func(t *testing.T) {
		h, s, _ := start(t)
		s.exit()
		s.stick("up")
		done := inBackground(t, s, func() { h.d.probeAll(context.Background()) })
		s.await(t, "up")
		assertResponsive(t, h)
		s.free("up")
		<-done
		if h.notUp("ol") {
			t.Error("ol not up again after its restart")
		}
	})

	t.Run("exit while coming up", func(t *testing.T) {
		h, s, _ := start(t)
		s.exit()
		s.stick("up")
		done := inBackground(t, s, func() { h.d.probeAll(context.Background()) })
		s.await(t, "up")
		s.exit() // the restarted helper dies before the daemon records it as up
		s.free("up")
		<-done
		if !h.notUp("ol") {
			t.Fatal("ol counted as up although it exited again")
		}
		h.d.probeAll(context.Background())
		if h.notUp("ol") {
			t.Error("ol not brought up by the next round")
		}
	})

	t.Run("stop", func(t *testing.T) {
		h, s, _ := setup(t)
		if err := h.d.start(context.Background()); err != nil {
			t.Fatal(err)
		}
		s.stick("close")
		done := inBackground(t, s, h.d.stop)
		s.await(t, "close")
		responsive := make(chan struct{})
		go func() {
			defer close(responsive)
			h.d.Status()
			h.d.Metrics()
		}()
		select {
		case <-responsive:
		case <-time.After(2 * time.Second):
			t.Fatal("Status or Metrics blocked behind closing a helper")
		}
		s.free("close")
		<-done
	})
}

// notUp reports whether the running upstream called name waits to come up.
func (h *harness) notUp(name string) bool {
	u := h.upstream(name)
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return h.d.notUp[u]
}
