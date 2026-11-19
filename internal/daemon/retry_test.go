package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/dataplane"
	"github.com/anesthetised/lotsman/internal/dataplane/fake"
	"github.com/anesthetised/lotsman/internal/store"
)

// retryHarness is a started daemon with one peer that only the fr upstream
// can serve. fr is configured but not yet part of the running daemon.
type retryHarness struct {
	*harness
	logs   *bytes.Buffer
	peer   store.Peer
	withFR *config.Config // the harness config plus fr
}

func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()
	h := newHarness(t)
	logs := &bytes.Buffer{}
	h.d.log = slog.New(slog.NewTextHandler(logs, nil))
	h.setHealthy("lm-up-fr", true)
	providerConf(t, h.dir, "fr", "10.12.0.2/32", 0)
	text := fmt.Sprintf(harnessConfig, h.dir, h.dir, h.dir)
	text = strings.Replace(text, "  - {name: de,", "  - {name: fr, conf: "+h.dir+"/fr.conf, geo: FR}\n  - {name: de,", 1)
	withFR, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.d.stop)
	if err := h.d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// As `lotsman profile add` stores it once fr is in the config file but
	// before the reload; the start would refuse a profile nothing running matches.
	if err := h.st.AddProfile(config.Profile{Name: "fr", Prefer: []config.Match{{Geo: "FR"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	peer, err := h.st.AddPeer("alice", store.DefaultDevice, "fr", h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	return &retryHarness{harness: h, logs: logs, peer: peer, withFR: withFR}
}

// round is one health round as Run performs it.
func (h *retryHarness) round(t *testing.T) {
	t.Helper()
	h.d.probeAll(context.Background())
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

func (h *retryHarness) running() string {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	var names []string
	for _, u := range h.d.ups {
		names = append(names, u.Name)
	}
	return strings.Join(names, ",")
}

func (h *retryHarness) devicesDown() int {
	h.d.mu.Lock()
	defer h.d.mu.Unlock()
	return len(h.d.notUp)
}

// An upstream that fails to open in a reload is retried by the health rounds
// until it opens, and then serves its peers.
func TestReloadOpenFailureRetried(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(t *testing.T, h *retryHarness, failTUN *atomic.Bool)
		fix     func(t *testing.T, h *retryHarness, failTUN *atomic.Bool)
	}{
		{
			"TUN creation fails",
			func(t *testing.T, h *retryHarness, failTUN *atomic.Bool) { failTUN.Store(true) },
			func(t *testing.T, h *retryHarness, failTUN *atomic.Bool) { failTUN.Store(false) },
		},
		{
			"provider file missing",
			func(t *testing.T, h *retryHarness, _ *atomic.Bool) {
				if err := os.Rename(filepath.Join(h.dir, "fr.conf"), filepath.Join(h.dir, "fr.conf.off")); err != nil {
					t.Fatal(err)
				}
			},
			func(t *testing.T, h *retryHarness, _ *atomic.Bool) {
				if err := os.Rename(filepath.Join(h.dir, "fr.conf.off"), filepath.Join(h.dir, "fr.conf")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRetryHarness(t)
			var failTUN atomic.Bool
			newTUN := h.d.newTUN
			h.d.newTUN = func(name string, mtu int) (tun.Device, error) {
				if name == "lm-up-fr" && failTUN.Load() {
					return nil, errors.New("tun busy")
				}
				return newTUN(name, mtu)
			}
			tc.breakIt(t, h, &failTUN)
			if err := h.d.Reload(context.Background(), h.withFR); err == nil || !strings.Contains(err.Error(), "fr") {
				t.Fatalf("Reload error = %v, want the fr failure", err)
			}
			if got := h.running(); got != "nl,de" {
				t.Fatalf("running upstreams = %s, want nl,de", got)
			}
			if got := strings.Count(h.logs.String(), "upstream is not running, will retry"); got != 1 {
				t.Errorf("reload logged the open failure %d times, want once:\n%s", got, h.logs)
			}

			// Still failing: no change, and the same error is not logged again.
			h.round(t)
			h.round(t)
			if got := h.running(); got != "nl,de" {
				t.Fatalf("running upstreams = %s while still failing", got)
			}
			if got := strings.Count(h.logs.String(), "upstream is not running, will retry"); got != 1 {
				t.Errorf("repeated failure logged %d times, want once:\n%s", got, h.logs)
			}
			if _, ok := h.dp.RouteOf(h.peer.IP); ok {
				t.Fatal("peer routed before its upstream opened")
			}

			// Fixed: the next round opens it and routes the peer through it.
			tc.fix(t, h, &failTUN)
			h.round(t)
			if got := h.running(); got != "nl,fr,de" {
				t.Fatalf("running upstreams = %s, want nl,fr,de", got)
			}
			if got, _ := h.dp.RouteOf(h.peer.IP); got != "lm-up-fr" {
				t.Errorf("peer routed via %q, want lm-up-fr", got)
			}
			if len(h.dp.Ups) != 3 || !h.dp.Alive["lm-up-fr"] {
				t.Errorf("dataplane upstreams = %v, alive = %v", h.dp.Ups, h.dp.Alive)
			}
			if n := h.devicesDown(); n != 0 {
				t.Errorf("%d devices still down", n)
			}
			if got := strings.Count(h.logs.String(), "upstream is not running, will retry"); got != 1 {
				t.Errorf("failure logged %d times in total, want once:\n%s", got, h.logs)
			}
		})
	}
}

// A retry never opens an upstream the config no longer lists.
func TestRetryIgnoresRemovedUpstream(t *testing.T) {
	h := newRetryHarness(t)
	if err := os.Remove(filepath.Join(h.dir, "fr.conf")); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), h.withFR); err == nil {
		t.Fatal("Reload accepted a missing provider file")
	}
	// Dropping fr for good: a profile nothing else matches goes first.
	if err := h.st.DeletePeer("alice", store.DefaultDevice, "fr"); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeleteProfile("fr"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), h.harness.cfg); err != nil {
		t.Fatal(err)
	}
	providerConf(t, h.dir, "fr", "10.12.0.2/32", 0)
	h.d.probeAll(context.Background())
	if got := h.running(); got != "nl,de" {
		t.Errorf("running upstreams = %s, want nl,de", got)
	}
	h.d.mu.Lock()
	left := len(h.d.failures)
	h.d.mu.Unlock()
	if left != 0 {
		t.Errorf("%d failure records outlived their upstream", left)
	}
}

// failingSetUpstreams fails the first SetUpstreams calls, then delegates.
type failingSetUpstreams struct {
	*fake.Dataplane
	failures int
}

func (f *failingSetUpstreams) SetUpstreams(ups []dataplane.Interface, mtu int) error {
	if f.failures > 0 {
		f.failures--
		return errors.New("temporary set-upstreams failure")
	}
	return f.Dataplane.SetUpstreams(ups, mtu)
}

// When the dataplane rejects the new upstream set, the reload reports it and
// the health rounds push the set again until it is accepted.
func TestSetUpstreamsFailureRetried(t *testing.T) {
	h := newRetryHarness(t)
	h.d.dp = &failingSetUpstreams{Dataplane: h.dp, failures: 3}
	if err := h.d.Reload(context.Background(), h.withFR); err == nil || !strings.Contains(err.Error(), "dataplane") {
		t.Fatalf("Reload error = %v, want the dataplane failure", err)
	}
	if n := h.devicesDown(); n != 1 {
		t.Fatalf("%d devices waiting to come up, want only fr's", n)
	}
	if len(h.dp.Ups) != 2 {
		t.Fatalf("dataplane upstreams = %v before the push succeeded", h.dp.Ups)
	}

	h.round(t) // fails again
	h.round(t) // fails again
	if _, ok := h.dp.RouteOf(h.peer.IP); ok {
		t.Fatal("peer routed through an upstream the dataplane does not know")
	}
	if got := strings.Count(h.logs.String(), "dataplane upstreams not applied"); got != 1 {
		t.Errorf("identical failure logged %d times, want once:\n%s", got, h.logs)
	}

	h.round(t) // succeeds
	if got, _ := h.dp.RouteOf(h.peer.IP); got != "lm-up-fr" {
		t.Errorf("peer routed via %q, want lm-up-fr", got)
	}
	if len(h.dp.Ups) != 3 || !h.dp.Alive["lm-up-fr"] {
		t.Errorf("dataplane upstreams = %v, alive = %v", h.dp.Ups, h.dp.Alive)
	}
	if n := h.devicesDown(); n != 0 {
		t.Errorf("%d devices still down", n)
	}
	if !strings.Contains(h.logs.String(), "dataplane upstreams applied") {
		t.Errorf("recovery not logged:\n%s", h.logs)
	}

	// Converged: further rounds push nothing.
	n := len(h.dp.Log)
	h.round(t)
	for _, line := range h.dp.Log[n:] {
		if strings.HasPrefix(line, "set-upstreams") {
			t.Errorf("converged round pushed the upstream set again: %s", line)
		}
	}
}
