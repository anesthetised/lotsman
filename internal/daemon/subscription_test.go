package daemon

import (
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/store"
)

func TestSubscriptionDeadlineRemovesRoutesAndPins(t *testing.T) {
	h := newHarness(t)
	h.cfg.Health.Interval = time.Hour
	if _, err := h.st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	move, err := h.st.AddPeer("alice", "move", route("NL", "DE"), h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	strict, err := h.st.AddPeer("alice", "strict", route("NL"), h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := h.st.AddPeer("alice", "pinned", route("NL"), h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	<-h.d.Ready()
	if _, err := h.st.SetRoute("alice", "pinned", route("DE")); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if !h.dp.PinnedTo(pinned.IP, "lm-up-nl") {
		t.Fatal("fixture did not pin the old flows")
	}
	if err := h.st.SetUpstreamExpiry("nl", time.Now().Add(600*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(move.IP); got != "lm-up-nl" {
		t.Fatalf("route before deadline = %q", got)
	}
	eventually(t, "subscription deadline", func() bool {
		moved, _ := h.dp.RouteOf(move.IP)
		_, allowed := h.dp.RouteOf(strict.IP)
		return moved == "lm-up-de" && !allowed && !h.dp.PinnedTo(pinned.IP, "lm-up-nl")
	})
	nl := h.d.Status().Upstreams[0]
	if nl.RoutingBlock != SubscriptionExpired || len(nl.Incidents) != 0 {
		t.Fatalf("status after expiry = %+v", nl)
	}
	if err := h.st.SetUpstreamExpiry("nl", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(strict.IP); got != "lm-up-nl" {
		t.Fatalf("strict route after renewal = %q", got)
	}
}

func TestOutlineExhaustionUsesLastConfirmedReading(t *testing.T) {
	h, _ := newProxyHarness(t)
	if _, err := h.st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	peer, err := h.st.AddPeer("alice", "laptop", pxRoute, h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	<-h.d.Ready()
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-ol" {
		t.Fatalf("route without provider data = %q", got)
	}
	key := []byte("outline-test-key")
	observed := time.Now()
	zero := store.OutlineUsage{KeyHash: key, Exhausted: true, ObservedAt: observed}
	old := store.OutlineUsage{KeyHash: key, ObservedAt: observed.Add(-time.Minute)}
	for _, reading := range []store.OutlineUsage{zero, {KeyHash: key}, old} {
		if err := h.d.ApplyOutlineUsage(map[string]store.OutlineUsage{"ol": reading}); err != nil {
			t.Fatal(err)
		}
		status := h.d.Status().Upstreams[1]
		if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" || status.RoutingBlock != QuotaExhausted {
			t.Fatalf("route=%q, status=%+v after reading %+v", got, status, reading)
		}
	}
	if err := h.d.ApplyOutlineUsage(map[string]store.OutlineUsage{
		"ol": {KeyHash: key, ObservedAt: observed.Add(time.Minute)},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-ol" {
		t.Fatalf("route after quota renews = %q", got)
	}
	zero.ObservedAt = observed.Add(2 * time.Minute)
	if err := h.d.ApplyOutlineUsage(map[string]store.OutlineUsage{"ol": zero}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" {
		t.Fatalf("route after another zero reading = %q", got)
	}
	if err := h.d.ApplyOutlineUsage(map[string]store.OutlineUsage{
		"ol": {KeyHash: []byte("a different provider key")},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-ol" {
		t.Fatalf("route with new key and unknown quota = %q", got)
	}
	// The provider date is authoritative when it confirms one for the key.
	if err := h.st.SetUpstreamExpiry("ol", observed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	expired := observed.Add(-time.Second)
	if err := h.d.ApplyOutlineUsage(map[string]store.OutlineUsage{
		"ol": {KeyHash: []byte("a different provider key"), ObservedAt: observed, ExpiresAt: expired},
	}); err != nil {
		t.Fatal(err)
	}
	status := h.d.Status().Upstreams[1]
	if got, _ := h.dp.RouteOf(peer.IP); got != "lm-up-nl" || status.RoutingBlock != SubscriptionExpired || !status.ExpiresAt.Equal(expired) {
		t.Fatalf("route=%q, status=%+v after provider expiry", got, status)
	}
}
