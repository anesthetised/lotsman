package flows

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

type fakeSource struct {
	entries []Entry
	err     error
}

func (f *fakeSource) Entries() ([]Entry, error) { return f.entries, f.err }

type fakePeers map[netip.Addr]Peer

func (f fakePeers) Peers() map[netip.Addr]Peer { return f }

// addrs compares netip values, which cmp cannot look into.
var addrs = cmpopts.EquateComparable(netip.AddrPort{})

var (
	subnet = netip.MustParsePrefix("10.77.0.0/24")
	alice  = netip.MustParseAddr("10.77.0.2")
	bob    = netip.MustParseAddr("10.77.0.3")
	peers  = fakePeers{alice: {ID: "alice", Upstream: "nl"}, bob: {ID: "bob", Upstream: ""}}
	t0     = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
)

func tcp(src netip.Addr, sport uint16, dst string, orig, reply uint64) Entry {
	return Entry{Protocol: TCP, Source: netip.AddrPortFrom(src, sport), Destination: netip.MustParseAddrPort(dst), OrigBytes: orig, ReplyBytes: reply}
}

// meter returns a Meter over src whose clock the test advances.
func meter(src Source) (*Meter, *time.Time) {
	now := t0
	return &Meter{Source: src, Peers: peers, Subnet: subnet, Now: func() time.Time { return now }}, &now
}

func snapshot(t *testing.T, m *Meter) Snapshot {
	t.Helper()
	s, err := m.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRates(t *testing.T) {
	src := &fakeSource{entries: []Entry{tcp(alice, 40000, "1.1.1.1:443", 100, 1000)}}
	m, now := meter(src)

	// The first snapshot has counters but no rates and no start time.
	first := snapshot(t, m)
	want := []Flow{{PeerID: "alice", Protocol: TCP, Source: netip.MustParseAddrPort("10.77.0.2:40000"), Destination: netip.MustParseAddrPort("1.1.1.1:443"),
		Upstream: "nl", UpBytes: 100, DownBytes: 1000}}
	if diff := cmp.Diff(Snapshot{Time: t0, Flows: want}, first, addrs); diff != "" {
		t.Errorf("first snapshot (-want +got):\n%s", diff)
	}

	// Replies count as down, what the client sent as up.
	*now = now.Add(2 * time.Second)
	src.entries = []Entry{tcp(alice, 40000, "1.1.1.1:443", 300, 5000)}
	want[0].UpBytes, want[0].DownBytes, want[0].UpRate, want[0].DownRate = 300, 5000, 100, 2000
	want[0].LastTraffic = *now
	if diff := cmp.Diff(want, snapshot(t, m).Flows, addrs); diff != "" {
		t.Errorf("second snapshot (-want +got):\n%s", diff)
	}

	// A flow that appeared since the previous snapshot sent all of its
	// bytes in this interval; it started about now.
	*now = now.Add(time.Second)
	src.entries = append(src.entries, Entry{Protocol: UDP, Source: netip.MustParseAddrPort("10.77.0.2:5353"), Destination: netip.MustParseAddrPort("8.8.8.8:53"), OrigBytes: 60, ReplyBytes: 120})
	// The fastest flow comes first.
	flows := snapshot(t, m).Flows
	if len(flows) != 2 {
		t.Fatalf("flows = %+v", flows)
	}
	if f := flows[0]; f.Protocol != UDP || f.UpRate != 60 || f.DownRate != 120 || !f.Started.Equal(*now) || !f.LastTraffic.Equal(*now) {
		t.Errorf("new flow = %+v", f)
	}
	if f := flows[1]; f.UpRate != 0 || f.DownRate != 0 || !f.Started.IsZero() || !f.LastTraffic.Equal(want[0].LastTraffic) {
		t.Errorf("idle flow = %+v, want no rate, an unknown start and the previous activity time", f)
	}
}

func TestFirstViewIncludesTrafficObservedBeforeItOpened(t *testing.T) {
	src := &fakeSource{entries: []Entry{tcp(alice, 40000, "1.1.1.1:443", 100, 1000)}}
	sampler := NewSampler(src, peers, subnet)
	sampler.sample(t0)
	before, _ := meter(sampler)
	if f := snapshot(t, before).Flows[0]; !f.LastTraffic.IsZero() {
		t.Fatalf("baseline with existing bytes inferred activity at %s", f.LastTraffic)
	}
	src.entries = []Entry{tcp(alice, 40000, "1.1.1.1:443", 200, 1000)}
	last := t0.Add(time.Second)
	sampler.sample(last)
	sampler.sample(last.Add(time.Second))

	for range 2 {
		m, _ := meter(sampler)
		got := snapshot(t, m).Flows
		if len(got) != 1 || !got[0].LastTraffic.Equal(last) || got[0].UpRate != 0 || got[0].DownRate != 0 {
			t.Fatalf("first view = %+v, want previously observed traffic and no rates", got)
		}
	}
}

func TestFlowsComeAndGo(t *testing.T) {
	flow := tcp(alice, 40000, "1.1.1.1:443", 1000, 1000)
	src := &fakeSource{entries: []Entry{flow}}
	m, now := meter(src)
	snapshot(t, m)

	// Gone: not reported.
	*now = now.Add(time.Second)
	src.entries = nil
	if s := snapshot(t, m); len(s.Flows) != 0 {
		t.Errorf("closed flow still reported: %+v", s.Flows)
	}

	// The same tuple again is a new flow, counted from zero.
	*now = now.Add(time.Second)
	flow.OrigBytes, flow.ReplyBytes = 10, 20
	src.entries = []Entry{flow}
	if f := snapshot(t, m).Flows[0]; f.UpRate != 10 || f.DownRate != 20 || !f.Started.Equal(*now) {
		t.Errorf("reopened flow = %+v", f)
	}
}

func TestCounterReset(t *testing.T) {
	src := &fakeSource{entries: []Entry{tcp(alice, 40000, "1.1.1.1:443", 1000, 1000)}}
	m, now := meter(src)
	snapshot(t, m)

	// The tuple was reused between two snapshots: counters went back.
	*now = now.Add(time.Second)
	src.entries = []Entry{tcp(alice, 40000, "1.1.1.1:443", 50, 5000)}
	if f := snapshot(t, m).Flows[0]; f.UpRate != 50 || f.DownRate != 5000 || !f.Started.Equal(*now) {
		t.Errorf("after a reset = %+v, want the new flow's whole counters as its rate", f)
	}

	// With kernel timestamps, a moved start is a new flow even when its
	// counters are higher.
	start := t0.Add(-time.Hour)
	e := tcp(alice, 40000, "1.1.1.1:443", 100, 6000)
	e.Start = start
	*now = now.Add(time.Second)
	src.entries = []Entry{e}
	if f := snapshot(t, m).Flows[0]; f.UpRate != 100 || f.DownRate != 6000 || !f.Started.Equal(start) {
		t.Errorf("with a new start = %+v", f)
	}
	*now = now.Add(time.Second)
	e.OrigBytes, e.ReplyBytes = 200, 6100
	src.entries = []Entry{e}
	if f := snapshot(t, m).Flows[0]; f.UpRate != 100 || f.DownRate != 100 || !f.Started.Equal(start) {
		t.Errorf("same start = %+v", f)
	}
	*now = now.Add(time.Second)
	e.OrigBytes, e.ReplyBytes = 0, 0
	src.entries = []Entry{e}
	if f := snapshot(t, m).Flows[0]; !f.LastTraffic.IsZero() {
		t.Errorf("new flow without counted bytes inherited last traffic at %s", f.LastTraffic)
	}
}

func TestMapping(t *testing.T) {
	src := &fakeSource{entries: []Entry{
		tcp(alice, 1, "1.1.1.1:443", 1, 1),
		tcp(bob, 2, "1.1.1.1:443", 1, 1),
		tcp(netip.MustParseAddr("10.77.0.9"), 3, "1.1.1.1:443", 1, 1),  // no such peer
		tcp(netip.MustParseAddr("192.0.2.1"), 4, "1.1.1.1:443", 1, 1),  // the host's own flow
		tcp(alice, 5, "10.77.0.1:9100", 1, 1),                          // to the gateway
		tcp(netip.MustParseAddr("192.0.2.1"), 6, "10.77.0.2:22", 1, 1), // towards a client
	}}
	m, _ := meter(src)
	got := map[string]string{}
	for _, f := range snapshot(t, m).Flows {
		got[f.PeerID] = f.Upstream
	}
	if diff := cmp.Diff(map[string]string{"alice": "nl", "bob": ""}, got); diff != "" {
		t.Errorf("peer → upstream (-want +got):\n%s", diff)
	}

	m, _ = meter(src)
	m.PeerIDs = map[string]bool{"bob": true}
	if flows := snapshot(t, m).Flows; len(flows) != 1 || flows[0].PeerID != "bob" {
		t.Errorf("flows of bob = %+v", flows)
	}
}

func TestLimit(t *testing.T) {
	src := &fakeSource{entries: []Entry{
		tcp(alice, 1, "1.1.1.1:443", 0, 0),
		tcp(alice, 2, "1.1.1.1:443", 0, 0),
		tcp(alice, 3, "1.1.1.1:443", 0, 0),
	}}
	m, now := meter(src)
	m.Limit = 2
	if s := snapshot(t, m); !s.Truncated || len(s.Flows) != 2 {
		t.Errorf("first snapshot: truncated %v, %d flows", s.Truncated, len(s.Flows))
	}

	*now = now.Add(time.Second)
	src.entries = []Entry{
		tcp(alice, 1, "1.1.1.1:443", 10, 0),
		tcp(alice, 2, "1.1.1.1:443", 0, 0),
		tcp(alice, 3, "1.1.1.1:443", 0, 30),
	}
	s := snapshot(t, m)
	var ports []uint16
	for _, f := range s.Flows {
		ports = append(ports, f.Source.Port())
	}
	if diff := cmp.Diff([]uint16{3, 1}, ports); diff != "" || !s.Truncated {
		t.Errorf("top flows by rate (-want +got):\n%s, truncated %v", diff, s.Truncated)
	}

	// A flow left out keeps its reference: its next rate is a delta.
	*now = now.Add(time.Second)
	src.entries = []Entry{tcp(alice, 2, "1.1.1.1:443", 0, 50)}
	if s := snapshot(t, m); s.Truncated || len(s.Flows) != 1 || s.Flows[0].DownRate != 50 {
		t.Errorf("snapshot under the limit = %+v", s)
	}
	src.entries = []Entry{tcp(alice, 2, "1.1.1.1:443", 0, 50)}
	*now = now.Add(time.Second)
	if f := snapshot(t, m).Flows[0]; f.DownRate != 0 {
		t.Errorf("idle flow rate = %d", f.DownRate)
	}
}

// A failed read changes nothing: the next rates span both intervals.
func TestSourceError(t *testing.T) {
	src := &fakeSource{entries: []Entry{tcp(alice, 1, "1.1.1.1:443", 0, 0)}}
	m, now := meter(src)
	snapshot(t, m)
	*now = now.Add(time.Second)
	src.err = errors.New("netlink busy")
	if _, err := m.Snapshot(); !errors.Is(err, src.err) {
		t.Fatalf("err = %v", err)
	}
	*now = now.Add(time.Second)
	src.err = nil
	src.entries = []Entry{tcp(alice, 1, "1.1.1.1:443", 0, 400)}
	if f := snapshot(t, m).Flows[0]; f.DownRate != 200 {
		t.Errorf("rate after a failed read = %d, want 200", f.DownRate)
	}
}

func TestEnableAccounting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nf_conntrack_acct")

	if err := enableAccounting(path); err == nil || !strings.Contains(err.Error(), "nf_conntrack") {
		t.Errorf("without the module: %v", err)
	}

	os.WriteFile(path, []byte("0\n"), 0o600)
	if err := enableAccounting(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "1" {
		t.Errorf("sysctl = %q, want 1", got)
	}

	// Already on: nothing is written, so a read-only /proc/sys works.
	os.WriteFile(path, []byte("1\n"), 0o600)
	os.Chmod(path, 0o400)
	if err := enableAccounting(path); err != nil {
		t.Errorf("already enabled: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "1\n" {
		t.Errorf("sysctl was rewritten: %q", got)
	}
}
