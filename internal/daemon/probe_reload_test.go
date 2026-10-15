package daemon

import (
	"context"
	"runtime"
	"sync"
	"testing"

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
