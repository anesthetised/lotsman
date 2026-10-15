package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/dataplane/fake"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
)

var errAlive = errors.New("temporary set-alive failure")

// failingAlive fails the first SetAlive calls, then delegates to the fake.
type failingAlive struct {
	*fake.Dataplane
	failures int
	calls    int
}

func (f *failingAlive) SetAlive(upstream string, alive bool) error {
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errAlive
	}
	return f.Dataplane.SetAlive(upstream, alive)
}

// aliveSetup returns a started daemon whose peer holds an old flow pinned to
// de, with SetAlive failing the given number of times.
func aliveSetup(t *testing.T, failures int) (*harness, *failingAlive, *bytes.Buffer, store.Peer) {
	t.Helper()
	h := newHarness(t)
	var logs bytes.Buffer
	h.d.log = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := h.st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	peer, err := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t.Cleanup(h.d.stop)
	if err := h.d.start(ctx); err != nil {
		t.Fatal(err)
	}
	h.d.probeAll(ctx)
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []string{"lm-up-de", "lm-up-nl"} {
		if err := h.dp.Route(peer.IP, upstream); err != nil {
			t.Fatal(err)
		}
	}
	if !h.dp.PinnedTo(peer.IP, "lm-up-de") {
		t.Fatal("setup did not create a flow pin")
	}
	fp := &failingAlive{Dataplane: h.dp, failures: failures}
	h.d.dp = fp
	return h, fp, &logs, peer
}

func TestSetAliveRetry(t *testing.T) {
	for _, failures := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d failures", failures), func(t *testing.T) {
			h, fp, logs, peer := aliveSetup(t, failures)
			ctx := context.Background()
			h.setHealthy("lm-up-de", false)
			h.d.probeAll(ctx)
			h.d.probeAll(ctx) // down_after: 2, so this round marks de down and fails once
			for i := 1; i < failures; i++ {
				// Observed health is kept while the kernel state is stale.
				if st := h.d.Status(); st.Upstreams[1].State != health.Down {
					t.Fatalf("observed state lost after failed SetAlive: %+v", st.Upstreams)
				}
				if !h.dp.PinnedTo(peer.IP, "lm-up-de") {
					t.Fatalf("pin removed before SetAlive succeeded (retry %d)", i)
				}
				h.d.probeAll(ctx)
			}
			if !h.dp.PinnedTo(peer.IP, "lm-up-de") {
				t.Fatal("pin removed before SetAlive succeeded")
			}
			h.d.probeAll(ctx) // first successful attempt
			if h.dp.PinnedTo(peer.IP, "lm-up-de") {
				t.Error("stale flow pin survived the retried SetAlive(false)")
			}
			if h.dp.Alive["lm-up-de"] {
				t.Error("de still marked alive in the dataplane")
			}
			if got := strings.Count(logs.String(), `msg="set alive"`); got != 1 {
				t.Errorf("identical failure logged %d times, want once:\n%s", got, logs)
			}
			// Converged: unchanged health makes no further dataplane calls.
			calls := fp.calls
			h.d.probeAll(ctx)
			h.d.probeAll(ctx)
			if fp.calls != calls {
				t.Errorf("SetAlive called %d extra times with unchanged health", fp.calls-calls)
			}
		})
	}
}

// A failed SetAlive(false) must not be retried with stale state once the
// upstream is healthy again.
func TestSetAliveRetryAppliesCurrentHealth(t *testing.T) {
	h, _, _, peer := aliveSetup(t, 1)
	ctx := context.Background()
	h.setHealthy("lm-up-de", false)
	h.d.probeAll(ctx)
	h.d.probeAll(ctx) // marks down; SetAlive(false) fails
	h.setHealthy("lm-up-de", true)
	h.d.probeAll(ctx) // up_after: 1, so de is up again
	if !h.dp.Alive["lm-up-de"] {
		t.Error("recovered upstream not marked alive")
	}
	if !h.dp.PinnedTo(peer.IP, "lm-up-de") {
		t.Error("pin to a recovered upstream must stay")
	}
}
