package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/anesthetised/lotsman/internal/dataplane/fake"
	"github.com/anesthetised/lotsman/internal/store"
)

// flakyDataplane fails the first Verify, Route and Unroute calls it is told
// to, then delegates.
type flakyDataplane struct {
	*fake.Dataplane
	verifyFailures, routeFailures, unrouteFailures int
}

func (f *flakyDataplane) Verify() (bool, error) {
	if f.verifyFailures > 0 {
		f.verifyFailures--
		return false, errors.New("temporary verify failure")
	}
	return f.Dataplane.Verify()
}

func (f *flakyDataplane) Route(peer netip.Addr, upstream string) error {
	if f.routeFailures > 0 {
		f.routeFailures--
		return errors.New("temporary route failure")
	}
	return f.Dataplane.Route(peer, upstream)
}

func (f *flakyDataplane) Unroute(peer netip.Addr) error {
	if f.unrouteFailures > 0 {
		f.unrouteFailures--
		return errors.New("temporary unroute failure")
	}
	return f.Dataplane.Unroute(peer)
}

// repairHarness is a started daemon with two routed peers and its log.
type repairHarness struct {
	*harness
	logs  *bytes.Buffer
	flaky *flakyDataplane
	peers []store.Peer
}

func newRepairHarness(t *testing.T) *repairHarness {
	t.Helper()
	h := newHarness(t)
	logs := &bytes.Buffer{}
	h.d.log = slog.New(slog.NewTextHandler(logs, nil))
	flaky := &flakyDataplane{Dataplane: h.dp}
	h.d.dp = flaky
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
	t.Cleanup(h.d.stop)
	if err := h.d.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &repairHarness{harness: h, logs: logs, flaky: flaky, peers: peers}
	r.round(t)
	r.wantAllowed(t, "before the flush", true)
	return r
}

// round is one health round as Run performs it.
func (h *repairHarness) round(t *testing.T) {
	t.Helper()
	h.d.probeAll(context.Background())
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
}

func (h *repairHarness) wantAllowed(t *testing.T, when string, want bool) {
	t.Helper()
	for _, p := range h.peers {
		if got := h.dp.IsAllowed(p.IP); got != want {
			t.Errorf("%s: peer %s allowed = %v, want %v", when, p.IP, got, want)
		}
	}
}

func (h *repairHarness) repairs(t *testing.T) float64 {
	t.Helper()
	v, ok := metric(h.d.Metrics(), "lotsman_firewall_repairs_total")
	if !ok {
		t.Fatal("lotsman_firewall_repairs_total missing")
	}
	return v
}

// A firewall another tool removed is reinstalled in the next health round,
// every routed peer is admitted again, and the incident is logged once as an
// error and counted (issue #47).
func TestFirewallRepairedAfterFlush(t *testing.T) {
	h := newRepairHarness(t)
	if got := h.repairs(t); got != 0 {
		t.Fatalf("repairs before any flush = %v", got)
	}
	h.dp.FlushFirewall()
	h.wantAllowed(t, "after the flush", false)

	h.round(t)
	h.wantAllowed(t, "after the repair round", true)
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs = %v, want 1", got)
	}
	if got := strings.Count(h.logs.String(), "level=ERROR msg=\"firewall rules were removed by another tool"); got != 1 {
		t.Errorf("repair logged %d times as an error, want once:\n%s", got, h.logs)
	}

	// Converged: no more repairs, logs or re-routing.
	n := len(h.dp.Log)
	h.round(t)
	h.round(t)
	for _, line := range h.dp.Log[n:] {
		if strings.HasPrefix(line, "route ") || strings.HasPrefix(line, "verify") {
			t.Errorf("converged round touched the dataplane: %s", line)
		}
	}
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs after converging = %v, want 1", got)
	}
	if got := strings.Count(h.logs.String(), "firewall rules were removed by another tool"); got != 1 {
		t.Errorf("repair logged %d times in total, want once:\n%s", got, h.logs)
	}

	// A second flush is a second incident.
	h.dp.FlushFirewall()
	h.round(t)
	h.wantAllowed(t, "after the second repair", true)
	if got := h.repairs(t); got != 2 {
		t.Errorf("repairs after a second flush = %v, want 2", got)
	}
}

// Failed checks and failed re-routing are retried every round and logged once.
func TestFirewallRepairRetried(t *testing.T) {
	h := newRepairHarness(t)
	h.dp.FlushFirewall()
	h.flaky.verifyFailures = 2
	h.round(t)
	h.round(t)
	h.wantAllowed(t, "while Verify fails", false)
	if got := strings.Count(h.logs.String(), "firewall repair failed"); got != 1 {
		t.Errorf("identical failure logged %d times, want once:\n%s", got, h.logs)
	}

	// Verify repairs, but the first re-route fails: the next round retries it.
	h.flaky.routeFailures = 1
	h.round(t)
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs = %v, want 1", got)
	}
	h.round(t)
	h.wantAllowed(t, "after the retry", true)
	if got := h.repairs(t); got != 1 {
		t.Errorf("repairs after the retry = %v, want 1", got)
	}
	if !strings.Contains(h.logs.String(), "firewall repair completed") {
		t.Errorf("recovery not logged:\n%s", h.logs)
	}
}

// A peer whose unroute is still pending stays revoked through a repair.
func TestFirewallRepairKeepsRevokedPeersOut(t *testing.T) {
	h := newRepairHarness(t)
	gone := h.peers[0]
	if err := h.st.DeletePeer(gone.User, gone.Device, gone.Profile); err != nil {
		t.Fatal(err)
	}
	h.flaky.unrouteFailures = 3
	if err := h.d.Reconcile(); err == nil {
		t.Fatal("Reconcile hid the unroute failure")
	}
	h.dp.FlushFirewall()
	h.d.probeAll(context.Background())
	if h.dp.IsAllowed(gone.IP) {
		t.Error("repair re-admitted a peer whose unroute is pending")
	}
	if !h.dp.IsAllowed(h.peers[1].IP) {
		t.Error("repair did not re-admit the remaining peer")
	}
}
