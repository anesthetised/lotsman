package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane/fake"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/store"
)

var errUnroute = errors.New("temporary unroute failure")

type failingUnroute struct {
	*fake.Dataplane
	failures int
}

func (f *failingUnroute) Unroute(ip netip.Addr) error {
	if f.failures > 0 {
		f.failures--
		return errUnroute
	}
	return f.Dataplane.Unroute(ip)
}

func TestRouteCleanupRetry(t *testing.T) {
	for _, reason := range []string{"no healthy upstream", "upstream recovers", "peer removed", "upstream removed", "upstream replaced", "profile removed"} {
		failureCounts := []int{1, 3}
		if reason == "profile removed" {
			failureCounts = []int{0, 1, 3}
		}
		for _, failures := range failureCounts {
			t.Run(fmt.Sprintf("%s/%d failures", reason, failures), func(t *testing.T) {
				h := newHarness(t)
				if _, err := h.st.CreateUser("alice"); err != nil {
					t.Fatal(err)
				}
				peer, err := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				// Drive reconciliations explicitly so injected failures are deterministic.
				t.Cleanup(h.d.stop)
				if err := h.d.start(ctx); err != nil {
					t.Fatal(err)
				}
				h.d.probeAll(ctx)
				if err := h.d.Reconcile(); err != nil {
					t.Fatal(err)
				}
				// Leave an old flow pinned to the other, still healthy upstream.
				for _, upstream := range []string{"lm-up-de", "lm-up-nl"} {
					if err := h.dp.Route(peer.IP, upstream); err != nil {
						t.Fatal(err)
					}
				}
				if !h.dp.PinnedTo(peer.IP, "lm-up-de") {
					t.Fatal("setup did not create a flow pin")
				}
				h.d.dp = &failingUnroute{Dataplane: h.dp, failures: failures}
				switch reason {
				case "no healthy upstream", "upstream recovers":
					h.setHealthy("lm-up-nl", false)
					h.d.probeAll(ctx)
					h.d.probeAll(ctx)
				case "peer removed":
					if err := h.st.DeletePeer("alice", store.DefaultDevice, "nl"); err != nil {
						t.Fatal(err)
					}
				case "upstream removed", "upstream replaced", "profile removed":
					cfg, err := config.Parse([]byte(fmt.Sprintf(harnessConfig, h.dir, h.dir, h.dir)))
					if err != nil {
						t.Fatal(err)
					}
					if reason == "upstream removed" {
						cfg.Upstreams = cfg.Upstreams[1:]
					} else if reason == "profile removed" {
						cfg.Profiles = cfg.Profiles[1:]
					} else {
						providerConf(t, h.dir, "nl", "10.8.0.2/32", 1300)
					}
					if err := h.d.Reload(ctx, cfg); err != nil {
						t.Fatal(err)
					}
					// The replacement can recover before cleanup is retried.
					h.d.probeAll(ctx)
					if reason == "profile removed" {
						st := h.d.Status()
						if len(st.Upstreams) != 2 || st.Upstreams[0].State != health.Up || st.Upstreams[1].State != health.Up {
							t.Fatalf("profile removal must retain both healthy upstreams: %+v", st.Upstreams)
						}
					}
				}
				// Reload already attempted the first cleanup in these two cases.
				first := 0
				if reason == "upstream removed" || reason == "upstream replaced" {
					first = 1
				}
				assertPending := func() {
					t.Helper()
					if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" || !h.dp.PinnedTo(peer.IP, "lm-up-de") {
						t.Fatal("injected failure did not preserve the stale route and pin")
					}
					if st := h.d.Status(); len(st.Peers) != 1 || st.Peers[0].Upstream != "nl" {
						t.Fatalf("cleanup reported complete while forwarding state remains: %+v", st.Peers)
					}
				}
				if first == 1 {
					assertPending()
				}
				for i := first; i < failures; i++ {
					if err := h.d.Reconcile(); !errors.Is(err, errUnroute) {
						t.Fatalf("failed cleanup %d: %v", i+1, err)
					}
					assertPending()
					if reason == "upstream recovers" && i == 0 {
						h.setHealthy("lm-up-nl", true)
						h.d.probeAll(ctx)
					}
					if reason == "peer removed" {
						peers, err := h.d.down.Peers()
						if err != nil || len(peers) != 0 {
							t.Fatalf("removed peer still authorized on downstream: %d peers, %v", len(peers), err)
						}
					}
				}
				if err := h.d.Reconcile(); reason == "profile removed" {
					if err == nil || !strings.Contains(err.Error(), `profile "nl" is not in the config`) || errors.Is(err, errUnroute) {
						t.Fatalf("expected only a missing-profile error after cleanup, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				want := ""
				wantUpstream := ""
				if reason == "upstream replaced" || reason == "upstream recovers" {
					want = "lm-up-nl"
					wantUpstream = "nl"
				}
				if got, _ := h.dp.RouteOf(peer.IP); got != want {
					t.Errorf("route after recovery = %q, want %q", got, want)
				}
				if h.dp.PinnedTo(peer.IP, "lm-up-de") {
					t.Error("stale flow pin survived successful cleanup")
				}
				st := h.d.Status()
				if reason == "peer removed" {
					if len(st.Peers) != 0 {
						t.Errorf("removed peer still reported after cleanup: %+v", st.Peers)
					}
				} else if len(st.Peers) != 1 || st.Peers[0].Upstream != wantUpstream {
					t.Errorf("status after cleanup: %+v", st.Peers)
				}
			})
		}
	}
}

// replacePeer starts a daemon with alice's peer routed via nl, then swaps it for
// bob's peer, which the allocator gives the same IP, with no reconcile between.
func replacePeer(t *testing.T) (h *harness, oldPeer, newPeer store.Peer) {
	t.Helper()
	h = newHarness(t)
	for _, u := range []string{"alice", "bob"} {
		if _, err := h.st.CreateUser(u); err != nil {
			t.Fatal(err)
		}
	}
	oldPeer, err := h.st.AddPeer("alice", store.DefaultDevice, "nl", h.cfg.Subnet)
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
	if err := h.st.DeletePeer("alice", store.DefaultDevice, "nl"); err != nil {
		t.Fatal(err)
	}
	newPeer, err = h.st.AddPeer("bob", store.DefaultDevice, "nl", h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	if newPeer.IP != oldPeer.IP || newPeer.PublicKey == oldPeer.PublicKey {
		t.Fatalf("setup: allocator did not reuse the IP for a new key: old %v new %v", oldPeer, newPeer)
	}
	return h, oldPeer, newPeer
}

func deviceKeys(t *testing.T, h *harness) []awgconf.Key {
	t.Helper()
	peers, err := h.d.down.Peers()
	if err != nil {
		t.Fatal(err)
	}
	var keys []awgconf.Key
	for _, p := range peers {
		keys = append(keys, p.PublicKey)
	}
	return keys
}

func TestReconcileReusedPeerIP(t *testing.T) {
	h, old, peer := replacePeer(t)
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" {
		t.Errorf("new peer route = %q, want lm-up-nl", got)
	}
	if keys := deviceKeys(t, h); !slices.Equal(keys, []awgconf.Key{peer.PublicKey}) {
		t.Errorf("device peers = %v, want only the new key (old %v)", keys, old.PublicKey)
	}
	if st := h.d.Status(); len(st.Peers) != 1 || st.Peers[0].User != "bob" || st.Peers[0].Upstream != "nl" {
		t.Errorf("status = %+v", st.Peers)
	}
}

// While the old peer's cleanup fails, the new peer must neither be authorized
// nor routed over the unresolved state; the next successful pass converges.
func TestReconcileReusedPeerIPCleanupRetry(t *testing.T) {
	h, old, peer := replacePeer(t)
	h.d.dp = &failingUnroute{Dataplane: h.dp, failures: 2}
	for i := 0; i < 2; i++ {
		if err := h.d.Reconcile(); !errors.Is(err, errUnroute) {
			t.Fatalf("pass %d: %v", i+1, err)
		}
		if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" {
			t.Fatalf("pass %d: old route must stay until cleanup succeeds, got %q", i+1, got)
		}
		if keys := deviceKeys(t, h); slices.Contains(keys, peer.PublicKey) {
			t.Fatalf("pass %d: new key authorized over unresolved old state", i+1)
		}
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" {
		t.Errorf("new peer route = %q, want lm-up-nl", got)
	}
	if keys := deviceKeys(t, h); !slices.Equal(keys, []awgconf.Key{peer.PublicKey}) {
		t.Errorf("device peers = %v, want only the new key (old %v)", keys, old.PublicKey)
	}
}
