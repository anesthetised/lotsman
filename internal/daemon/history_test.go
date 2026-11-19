package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/history"
	"github.com/anesthetised/lotsman/internal/store"
)

func TestUpstreamHealth(t *testing.T) {
	h := newHarness(t)
	h.run(t)
	<-h.d.Ready()
	got := h.d.UpstreamHealth()
	if len(got) != 2 || !got["nl"].Up || !got["de"].Up || got["nl"].Latency != time.Millisecond {
		t.Fatalf("healthy: %+v", got)
	}
	h.setHealthy("lm-up-nl", false)
	eventually(t, "nl down", func() bool { return !h.d.UpstreamHealth()["nl"].Up })
	if !h.d.UpstreamHealth()["de"].Up {
		t.Error("de went down with nl")
	}
}

// A degraded upstream is reachable and carries nothing, so history does not
// count it as up.
func TestUpstreamHealthDegradedIsNotUp(t *testing.T) {
	h, f, round, _ := transferHarness(t, 2)
	round()
	if !h.d.UpstreamHealth()["nl"].Up {
		t.Fatal("nl is not up at the start")
	}
	f.set("lm-up-nl", true)
	round()
	round()
	if !degradedOf(h, "nl") {
		t.Fatal("nl is not degraded")
	}
	if h.d.Status().Upstreams[0].State.String() != "up" || h.d.UpstreamHealth()["nl"].Up {
		t.Errorf("degraded nl: state %v, history up %v; want up, false", h.d.Status().Upstreams[0].State, h.d.UpstreamHealth()["nl"].Up)
	}
}

// The daemon is a history.Source: its health, traffic and retention reach
// the store through the writer, and come back through the queries.
func TestHistoryEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	peer, err := h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	h.d.interfaceCounters = func(iface string) (rx, tx uint64, err error) { return 40, 4, nil }
	h.run(t)
	<-h.d.Ready()
	eventually(t, "a traffic sample of both upstreams and the peer", func() bool {
		tr := h.d.Traffic()
		_, ok := tr.Peers[peer.ID]
		return len(tr.Upstreams) == 2 && ok
	})

	// A fake clock, in the past, so the real writer's minutes stay out of it.
	minute := time.Now().Truncate(time.Hour).Add(-3 * time.Hour)
	clock := minute
	w := &history.Writer{Source: h.d, Sink: h.st, Now: func() time.Time { return clock }, Log: quiet}
	w.Tick()
	clock = clock.Add(time.Minute)
	w.Tick()

	res, err := h.d.UpstreamHistory("nl", history.Range{From: minute.Add(-time.Minute), To: minute.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != time.Minute || len(res.Samples) != 1 || res.Samples[0].Time != minute.UTC() || res.Samples[0].UpRatio != 1 || res.Samples[0].Latency != time.Millisecond {
		t.Errorf("nl = %+v", res)
	}
	pres, err := h.d.PeerHistory(peer.ID, history.Range{From: minute.Add(-time.Minute), To: minute.Add(5 * time.Minute)})
	if err != nil || len(pres.Samples) != 1 || pres.Samples[0].Time != minute.UTC() {
		t.Errorf("peer = %+v, %v", pres, err)
	}
}

func TestHistoryQueries(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	peer, err := h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := h.st.AddPeer("alice", "old", nlRoute, h.cfg.Subnet)
	if err != nil {
		t.Fatal(err)
	}
	h.run(t)
	<-h.d.Ready()

	ts := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	if err := h.st.AddSamples(ts, []store.UpstreamSample{{Name: "nl", UpRatio: 1, Down: 10, Up: 1}, {Name: "removed", UpRatio: 1}},
		[]store.PeerSample{{PeerID: peer.ID, Down: 5, Up: 2}, {PeerID: gone.ID, Down: 9}}); err != nil {
		t.Fatal(err)
	}
	r := history.Range{From: ts.Add(-time.Hour), To: ts.Add(time.Hour)}

	if res, err := h.d.UpstreamHistory("nl", r); err != nil || len(res.Samples) != 1 || res.Samples[0].Down != 10 {
		t.Errorf("nl = %+v, %v", res, err)
	}
	if res, err := h.d.PeerHistory(peer.ID, r); err != nil || len(res.Samples) != 1 || res.Samples[0].Down != 5 || res.Samples[0].Up != 2 {
		t.Errorf("peer = %+v, %v", res, err)
	}
	// A configured upstream with no samples is known, and answers empty.
	if res, err := h.d.UpstreamHistory("de", r); err != nil || len(res.Samples) != 0 || res.Step != time.Minute {
		t.Errorf("de = %+v, %v", res, err)
	}

	// An upstream that is not configured, though its rows are stored, and a
	// peer that was deleted, are unknown.
	if _, err := h.d.UpstreamHistory("removed", r); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removed upstream: %v", err)
	}
	if err := h.st.DeletePeer("alice", "old"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{gone.ID, 999} {
		if _, err := h.d.PeerHistory(id, r); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("peer %d: %v", id, err)
		}
	}

	// A bad range is refused before the name is looked up.
	bad := history.Range{From: ts, To: ts.Add(-time.Minute)}
	if _, err := h.d.UpstreamHistory("nl", bad); !errors.Is(err, history.ErrRange) {
		t.Errorf("bad range: %v", err)
	}
	if _, err := h.d.PeerHistory(peer.ID, bad); !errors.Is(err, history.ErrRange) {
		t.Errorf("bad range, peer: %v", err)
	}
	if _, err := h.d.UpstreamHistory("nope", bad); !errors.Is(err, history.ErrRange) {
		t.Errorf("bad range, unknown name: %v", err)
	}
}
