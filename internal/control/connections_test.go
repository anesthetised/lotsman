package control

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/flows"
)

var (
	aliceAddr = netip.MustParseAddr("10.77.0.2")
	bobAddr   = netip.MustParseAddr("10.77.0.3")
)

// fakeRoutes is a ready daemon with alice (p_7, via nl) and bob (p_12,
// blocked) in 10.77.0.0/24.
type fakeRoutes struct{}

func (fakeRoutes) Ready() <-chan struct{} {
	ready := make(chan struct{})
	close(ready)
	return ready
}
func (fakeRoutes) Subnet() netip.Prefix { return netip.MustParsePrefix("10.77.0.0/24") }
func (fakeRoutes) PeerRoutes() map[netip.Addr]daemon.PeerRoute {
	return map[netip.Addr]daemon.PeerRoute{
		aliceAddr: {ID: 7, User: "alice", Device: "phone", Upstream: "nl"},
		bobAddr:   {ID: 12, User: "bob", Device: "default"},
	}
}

// fakeConntrack is a conntrack table the test changes.
type fakeConntrack struct {
	mu      sync.Mutex
	entries []flows.Entry
	err     error
}

func (f *fakeConntrack) Entries() ([]flows.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries, f.err
}

func (f *fakeConntrack) set(entries ...flows.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = entries
}

func tcpEntry(src netip.Addr, orig, reply uint64) flows.Entry {
	return flows.Entry{Protocol: flows.TCP, Source: netip.AddrPortFrom(src, 40000), Destination: netip.MustParseAddrPort("1.1.1.1:443"),
		OrigBytes: orig, ReplyBytes: reply}
}

// connectionServer is the connection service on an in-process server whose
// handler reports, on done, how each Watch call ended.
func connectionServer(src ConnectionSource, ct flows.Source) (lotsmanv1connect.ConnectionServiceClient, <-chan error) {
	done := make(chan error, 1)
	s := connect.NewServer(func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(ctx, spec, stream)
			done <- err
			return err
		}
	})
	lotsmanv1connect.RegisterConnectionServiceHandler(s, connectionService{src, ct})
	return lotsmanv1connect.NewConnectionServiceClient(connect.NewClient(connectinprocess.New(s))), done
}

// watchConnections opens a stream and returns its first snapshot, or the
// error the stream failed with.
func watchConnections(t *testing.T, client lotsmanv1connect.ConnectionServiceClient, req *lotsmanv1.WatchConnectionsRequest) (*lotsmanv1.ConnectionSnapshot, error) {
	t.Helper()
	stream, err := client.Watch(t.Context(), req)
	if err != nil {
		return nil, err
	}
	return stream.Receive()
}

func TestConnectionSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Minute)
	s := flows.Snapshot{Time: now, Truncated: true, Flows: []flows.Flow{
		// Every traffic field differs, so a swapped direction shows.
		{PeerID: "p_7", Protocol: flows.TCP, Source: netip.MustParseAddrPort("10.77.0.2:40000"), Destination: netip.MustParseAddrPort("1.1.1.1:443"),
			Upstream: "nl", Started: started, DownBytes: 1, UpBytes: 2, DownRate: 3, UpRate: 4},
		{PeerID: "p_7", Protocol: flows.UDP, Source: netip.MustParseAddrPort("10.77.0.2:5353"), Destination: netip.MustParseAddrPort("8.8.8.8:53"), Upstream: "nl"},
		// ICMP has no ports: bare addresses.
		{PeerID: "p_12", Protocol: flows.ICMP, Source: netip.AddrPortFrom(bobAddr, 0), Destination: netip.MustParseAddrPort("9.9.9.9:0")},
		{PeerID: "p_12", Protocol: 47, Source: netip.AddrPortFrom(bobAddr, 0), Destination: netip.MustParseAddrPort("9.9.9.9:0")},
	}}
	want := &lotsmanv1.ConnectionSnapshot{Time: timestamppb.New(now), Truncated: true, Connections: []*lotsmanv1.Connection{
		{PeerId: "p_7", Protocol: lotsmanv1.Protocol_PROTOCOL_TCP, Source: "10.77.0.2:40000", Destination: "1.1.1.1:443", Upstream: "nl",
			Started: timestamppb.New(started), Traffic: &lotsmanv1.Traffic{DownBytes: 1, UpBytes: 2, DownRate: 3, UpRate: 4}},
		// Start unknown: unset.
		{PeerId: "p_7", Protocol: lotsmanv1.Protocol_PROTOCOL_UDP, Source: "10.77.0.2:5353", Destination: "8.8.8.8:53", Upstream: "nl",
			Traffic: &lotsmanv1.Traffic{}},
		{PeerId: "p_12", Protocol: lotsmanv1.Protocol_PROTOCOL_ICMP, Source: "10.77.0.3", Destination: "9.9.9.9", Traffic: &lotsmanv1.Traffic{}},
		{PeerId: "p_12", Protocol: lotsmanv1.Protocol_PROTOCOL_UNSPECIFIED, Source: "10.77.0.3", Destination: "9.9.9.9", Traffic: &lotsmanv1.Traffic{}},
	}}
	if diff := cmp.Diff(want, connectionSnapshot(s), protocmp.Transform()); diff != "" {
		t.Errorf("snapshot (-want +got):\n%s", diff)
	}
}

// A stream for one peer sends its flows at once, then every interval with
// the rates since the previous snapshot, until the client goes away.
func TestConnectionsWatch(t *testing.T) {
	ct := &fakeConntrack{}
	ct.set(tcpEntry(aliceAddr, 100, 1000), tcpEntry(bobAddr, 1, 1))
	client, done := connectionServer(fakeRoutes{}, ct)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	// Below the minimum, so the stream runs at one snapshot a second.
	stream, err := client.Watch(ctx, &lotsmanv1.WatchConnectionsRequest{PeerId: "p_7", Interval: durationpb.New(time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > MinInterval/2 {
		t.Errorf("first snapshot after %s, want it at once", took)
	}
	// Only alice's flow, under her API id and upstream; no rates yet.
	want := []*lotsmanv1.Connection{{PeerId: "p_7", Protocol: lotsmanv1.Protocol_PROTOCOL_TCP, Source: "10.77.0.2:40000", Destination: "1.1.1.1:443",
		Upstream: "nl", Traffic: &lotsmanv1.Traffic{DownBytes: 1000, UpBytes: 100}}}
	if diff := cmp.Diff(want, first.Connections, protocmp.Transform()); diff != "" {
		t.Errorf("first snapshot (-want +got):\n%s", diff)
	}

	ct.set(tcpEntry(aliceAddr, 200, 3000), tcpEntry(bobAddr, 1, 1))
	second, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	gap := second.Time.AsTime().Sub(first.Time.AsTime())
	if gap < MinInterval-50*time.Millisecond {
		t.Errorf("snapshots %s apart, want the minimum interval %s", gap, MinInterval)
	}
	if c := second.Connections; len(c) != 1 || c[0].Traffic.DownBytes != 3000 || c[0].Traffic.DownRate == 0 || c[0].Traffic.DownRate <= c[0].Traffic.UpRate {
		t.Errorf("second snapshot = %v, want down outrunning up", second)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("handler ended with %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler still running after the client went away")
	}
}

func TestConnectionsWatchEveryPeer(t *testing.T) {
	ct := &fakeConntrack{}
	ct.set(tcpEntry(aliceAddr, 100, 1000), tcpEntry(bobAddr, 1, 1), tcpEntry(netip.MustParseAddr("10.77.0.9"), 5, 5))
	client, _ := connectionServer(fakeRoutes{}, ct)
	got, err := watchConnections(t, client, &lotsmanv1.WatchConnectionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// An address no peer holds is not shown.
	var peers []string
	for _, c := range got.Connections {
		peers = append(peers, c.PeerId+" via "+c.Upstream)
	}
	if diff := cmp.Diff([]string{"p_7 via nl", "p_12 via "}, peers); diff != "" {
		t.Errorf("peers (-want +got):\n%s", diff)
	}
}

func TestConnectionsWatchErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *lotsmanv1.WatchConnectionsRequest
		err  error // from conntrack
		code connect.Code
	}{
		{"invalid interval", &lotsmanv1.WatchConnectionsRequest{Interval: &durationpb.Duration{Seconds: 1, Nanos: -1}}, nil, connect.CodeInvalidArgument},
		// The store id alone is not a peer id.
		{"unknown peer", &lotsmanv1.WatchConnectionsRequest{PeerId: "7"}, nil, connect.CodeNotFound},
		{"no conntrack", &lotsmanv1.WatchConnectionsRequest{}, fmt.Errorf("conntrack needs Linux: %w", errors.ErrUnsupported), connect.CodeFailedPrecondition},
		{"conntrack fails", &lotsmanv1.WatchConnectionsRequest{}, errors.New("netlink: operation not permitted"), connect.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := connectionServer(fakeRoutes{}, &fakeConntrack{err: tc.err})
			_, err := watchConnections(t, client, tc.req)
			if connect.CodeOf(err) != tc.code {
				t.Errorf("err = %v, want %v", err, tc.code)
			}
			if strings.Contains(fmt.Sprint(err), "netlink") {
				t.Errorf("kernel error leaked: %v", err)
			}
		})
	}
}

// The non-Linux build has no conntrack: the real server refuses cleanly.
func TestConnectionsWithoutConntrack(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux has conntrack")
	}
	src := newFakeSource(daemon.Status{})
	src.ConnectionSource = fakeRoutes{}
	client := lotsmanv1connect.NewConnectionServiceClient(connect.NewClient(connectinprocess.New(NewServer(src))))
	if _, err := watchConnections(t, client, &lotsmanv1.WatchConnectionsRequest{}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("err = %v, want failed_precondition", err)
	}
}
