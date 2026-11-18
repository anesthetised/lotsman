package daemon

import (
	"context"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/health"
)

// Run with -race: retained upstream metadata changes during reload, while
// probes must use a consistent interface/source snapshot outside the mutex.
func TestProbeConcurrentReloadSnapshot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.d.start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	configs := make([]config.Config, 2)
	for i := range configs {
		configs[i] = *h.cfg
		configs[i].Upstreams = append([]config.Upstream(nil), h.cfg.Upstreams...)
		configs[i].Upstreams[0].Provider = []string{"before", "after"}[i]
	}
	start := make(chan struct{})
	errs := make(chan error, 4)
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			<-start
			for i := range 25 {
				if err := h.d.Reload(ctx, &configs[i%2]); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	workers.Go(func() {
		<-start
		for range 100 {
			runtime.Gosched()
			h.d.probeAll(ctx)
		}
	})
	close(start)
	workers.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
	for _, u := range h.d.ups {
		if u.Tracker.State() != health.Up {
			t.Errorf("upstream %s state = %v, want up", u.Name, u.Tracker.State())
		}
	}
}

// stallingResolver holds every lookup until released, so a test can act while
// DNS is slow. Each lookup reports whether its context was bounded by limit.
type stallingResolver struct {
	limit   time.Duration
	started chan bool
	release chan struct{}
	once    sync.Once
}

func useStallingResolver(t *testing.T, limit time.Duration) *stallingResolver {
	r := &stallingResolver{limit: limit, started: make(chan bool, 8), release: make(chan struct{})}
	old := lookupNetIP
	lookupNetIP = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		deadline, ok := ctx.Deadline()
		r.started <- ok && time.Until(deadline) <= r.limit
		<-r.release
		return []netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil
	}
	t.Cleanup(func() { lookupNetIP = old })
	return r
}

func (r *stallingResolver) unblock() { r.once.Do(func() { close(r.release) }) }

// await waits for the stalled lookup and checks its timeout.
func (r *stallingResolver) await(t *testing.T) {
	t.Helper()
	select {
	case bounded := <-r.started:
		if !bounded {
			t.Errorf("endpoint lookup has no deadline within the probe timeout %v", r.limit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no endpoint lookup started")
	}
}

// background runs op; cleanup releases DNS and waits for op before the daemon
// stops, even when the test fails early.
func background(t *testing.T, r *stallingResolver, op func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	t.Cleanup(func() {
		r.unblock()
		<-done
	})
	return done
}

// assertResponsive fails fast if Status, Reconcile or Metrics wait for DNS or
// a helper.
func assertResponsive(t *testing.T, h *harness) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.d.Status()
		h.d.Reconcile()
		h.d.Metrics()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Status, Reconcile or Metrics blocked behind an endpoint lookup or a helper request")
	}
}

// A slow resolver must not hold the daemon mutex: not at startup, not when a
// failing upstream re-resolves in a probe round, and not when a reload
// resolves a changed provider.
func TestEndpointLookupDoesNotBlockDaemon(t *testing.T) {
	limit := time.Second // the harness probe timeout: max(30ms/2, 1s)
	start := func(t *testing.T) *harness {
		h := newHarness(t)
		if err := h.d.start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.d.stop)
		return h
	}

	t.Run("startup", func(t *testing.T) {
		h := newHarness(t)
		dns := useStallingResolver(t, limit)
		t.Cleanup(h.d.stop) // registered first, so it runs after DNS is released
		var err error
		done := background(t, dns, func() { err = h.d.start(context.Background()) })
		dns.await(t)
		// Reconcile needs a started daemon; serve delivers SIGHUP only after Ready.
		responsive := make(chan struct{})
		go func() {
			defer close(responsive)
			h.d.Status()
			h.d.Metrics()
		}()
		select {
		case <-responsive:
		case <-time.After(2 * time.Second):
			t.Fatal("Status or Metrics blocked behind a startup endpoint lookup")
		}
		dns.unblock()
		<-done
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("probe round", func(t *testing.T) {
		h := start(t)
		dns := useStallingResolver(t, limit)
		h.setHealthy("lm-up-de", false)
		done := background(t, dns, func() { h.d.probeAll(context.Background()) })
		dns.await(t)
		assertResponsive(t, h)
		dns.unblock()
		<-done
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		if got := h.d.ups[1].Conf.Peers[0].Endpoint; got != "127.0.0.2:51820" {
			t.Errorf("de endpoint = %q, want the re-resolved 127.0.0.2:51820", got)
		}
	})

	t.Run("reload", func(t *testing.T) {
		h := start(t)
		dns := useStallingResolver(t, limit)
		providerConf(t, h.dir, "de", "10.9.0.3/32", 0) // changed, so it resolves
		var err error
		done := background(t, dns, func() { err = h.d.Reload(context.Background(), h.cfg) })
		dns.await(t)
		assertResponsive(t, h)
		dns.unblock()
		<-done
		if err != nil {
			t.Fatal(err)
		}
		h.d.mu.Lock()
		defer h.d.mu.Unlock()
		if got := h.d.ups[1].Addr.String(); got != "10.9.0.3/32" {
			t.Errorf("de address = %s, want the replacement's 10.9.0.3/32", got)
		}
	})

	t.Run("reload raced by a removal", func(t *testing.T) {
		h := start(t)
		dns := useStallingResolver(t, limit)
		providerConf(t, h.dir, "de", "10.9.0.3/32", 0)
		var err error
		done := background(t, dns, func() { err = h.d.Reload(context.Background(), h.cfg) })
		dns.await(t)
		assertResponsive(t, h)
		without := *h.cfg
		without.Upstreams = h.cfg.Upstreams[:1]
		if err := h.d.Reload(context.Background(), &without); err != nil {
			t.Fatal(err)
		}
		dns.unblock()
		<-done
		if err == nil || !strings.Contains(err.Error(), "upstream de: changed by a concurrent reload") {
			t.Errorf("stale reload error = %v", err)
		}
		if st := h.d.Status(); len(st.Upstreams) != 1 || st.Upstreams[0].Name != "nl" {
			t.Errorf("stale reload re-added de: %+v", st.Upstreams)
		}
	})
}
