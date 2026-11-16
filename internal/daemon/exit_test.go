package daemon

import (
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/proxy"
	"github.com/anesthetised/lotsman/internal/store"
)

// fakeHelper stands in for a proxy upstream run in a helper process: the
// test makes it exit and decides whether it may start again.
type fakeHelper struct {
	mu     sync.Mutex
	tun    tun.Device
	spec   proxy.Spec
	exited func(error)
	starts int
	upErr  error
}

func (f *fakeHelper) run(t tun.Device, spec proxy.Spec, _ *slog.Logger, exited func(error)) (proxy.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tun, f.spec, f.exited = t, spec, exited
	return f, nil
}

func (f *fakeHelper) Up() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upErr != nil {
		return f.upErr
	}
	f.starts++
	return nil
}

func (f *fakeHelper) SetServer(string, netip.AddrPort) error { return nil }
func (f *fakeHelper) SetMaxFlows(int) error                  { return nil }
func (f *fakeHelper) Stats() (proxy.Stats, error)            { return proxy.Stats{}, nil }
func (f *fakeHelper) Close()                                 { f.tun.Close() }

func (f *fakeHelper) setUpErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upErr = err
}

func (f *fakeHelper) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

// crash reports an exit, as the helper runner does from its own goroutine.
func (f *fakeHelper) crash() {
	f.mu.Lock()
	exited := f.exited
	f.mu.Unlock()
	exited(errors.New("proxy helper exited: signal: killed"))
}

func TestProxyLinkExit(t *testing.T) {
	h, _ := newProxyHarness(t)
	f := &fakeHelper{}
	h.d.runProxy = f.run
	h.st.CreateUser("alice")
	peer, _ := h.st.AddPeer("alice", store.DefaultDevice, "px", h.cfg.Subnet)
	stop := h.run(t)
	routedVia := func(want string) func() bool {
		return func() bool { got, _ := h.dp.RouteOf(peer.IP); return got == want }
	}
	eventually(t, "routed via ol", routedVia("lm-up-ol"))
	if f.started() != 1 || f.spec.Server != netip.MustParseAddrPort("192.0.2.10:8388") || f.spec.Key == "" {
		t.Fatalf("starts %d, spec server %v", f.started(), f.spec.Server)
	}
	restarts := func() float64 {
		v, _ := metric(h.d.Metrics(), "lotsman_proxy_restarts_total", metrics.Label{Name: "upstream", Value: "ol"})
		return v
	}
	if _, ok := metric(h.d.Metrics(), "lotsman_proxy_restarts_total", metrics.Label{Name: "upstream", Value: "ol"}); !ok {
		t.Fatal("restarts_total{ol} missing before any restart")
	}
	if _, ok := metric(h.d.Metrics(), "lotsman_proxy_restarts_total", metrics.Label{Name: "upstream", Value: "nl"}); ok {
		t.Error("an AWG upstream reports proxy restarts")
	}

	// The helper dies while its probes still pass, so nothing but the exit
	// can take ol down, and by the time the report returns it has: ol is
	// down, its pins are gone and the peer has moved, not down_after
	// rounds later. The restart is held back for now.
	f.setUpErr(errors.New("held back"))
	f.crash()
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" {
		t.Errorf("peer routed via %q right after the exit, want lm-up-nl", got)
	}
	if h.dp.PinnedTo(peer.IP, "lm-up-ol") {
		t.Error("old flows stay pinned to the dead upstream")
	}
	if st := h.d.Status().Upstreams[1]; st.State != health.Down {
		t.Errorf("ol state right after the exit = %v", st.State)
	}
	if got := restarts(); got != 1 {
		t.Errorf("restarts_total{ol} = %v, want 1", got)
	}

	// It is started again once it can be, and probes bring it back.
	f.setUpErr(nil)
	eventually(t, "restarted", func() bool { return f.started() == 2 })
	eventually(t, "back via ol", routedVia("lm-up-ol"))

	// An exit that arrives while the daemon stops changes nothing.
	stop()
	f.crash()
	if got := restarts(); got != 1 {
		t.Errorf("restarts_total{ol} after stop = %v, want 1", got)
	}
}
