package control

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/health"
	"github.com/anesthetised/lotsman/internal/traffic"
)

// fakeSource is a daemon with a fixed status and the traffic the test sets.
type fakeSource struct {
	ready chan struct{}
	st    daemon.Status

	mu sync.Mutex
	tr traffic.Snapshot
}

func newFakeSource(st daemon.Status) *fakeSource {
	f := &fakeSource{ready: make(chan struct{}), st: st}
	close(f.ready)
	return f
}

func (f *fakeSource) Ready() <-chan struct{} { return f.ready }
func (f *fakeSource) ClientMTU() int         { return 1280 }

func (f *fakeSource) Status() daemon.Status { return f.st }

func (f *fakeSource) Traffic() traffic.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tr
}

func (f *fakeSource) setTraffic(tr traffic.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tr = tr
}

func TestSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	handshake := now.Add(-time.Minute)
	st := daemon.Status{
		Upstreams: []daemon.UpstreamStatus{
			{Name: "nl", Kind: "awg", Geo: "NL", Provider: "acme", Tags: []string{"eu", "fast"}, State: health.Up,
				Latency: 25 * time.Millisecond, LastHandshake: handshake, Routed: 2, Active: 1},
			{Name: "de", Kind: "outline", State: health.Up, Degraded: true, Latency: time.Second},
			{Name: "fi", Kind: "vless", State: health.Down},
			{Name: "se", Kind: "awg", State: health.Unknown},
			// A Down upstream is down, whatever its last transfer probe said.
			{Name: "pl", Kind: "other", State: health.Down, Degraded: true},
		},
		Peers: []daemon.PeerStatus{
			{ID: 7, User: "alice", Device: "phone", Profile: "eu", Address: netip.MustParseAddr("10.77.0.2"),
				Upstream: "nl", LastHandshake: handshake},
			{ID: 12, User: "bob", Device: "default", Profile: "eu", Address: netip.MustParseAddr("10.77.0.3")},
		},
	}
	// Every field differs, so a swapped direction shows. bob's peer and
	// every upstream but nl were not sampled yet.
	tr := traffic.Snapshot{
		Time:      now.Add(-time.Second),
		Peers:     map[int64]traffic.Traffic{7: {DownBytes: 1, UpBytes: 2, DownRate: 3, UpRate: 4}},
		Upstreams: map[string]traffic.Traffic{"nl": {DownBytes: 5, UpBytes: 6, DownRate: 7, UpRate: 8}},
		Total:     traffic.Traffic{DownBytes: 9, UpBytes: 10, DownRate: 11, UpRate: 12},
	}
	want := &lotsmanv1.StatusSnapshot{
		Time: timestamppb.New(now), ClientMtu: 1280,
		Total: &lotsmanv1.Traffic{DownBytes: 9, UpBytes: 10, DownRate: 11, UpRate: 12},
		Upstreams: []*lotsmanv1.UpstreamStatus{
			{Name: "nl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "NL", Provider: "acme", Tags: []string{"eu", "fast"},
				State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP, Latency: durationpb.New(25 * time.Millisecond),
				LastHandshake: timestamppb.New(handshake), Routed: 2, Active: 1,
				Traffic: &lotsmanv1.Traffic{DownBytes: 5, UpBytes: 6, DownRate: 7, UpRate: 8}},
			{Name: "de", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_OUTLINE, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DEGRADED,
				Latency: durationpb.New(time.Second)},
			{Name: "fi", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_VLESS, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DOWN},
			{Name: "se", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UNKNOWN},
			{Name: "pl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_UNSPECIFIED, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DOWN},
		},
		Peers: []*lotsmanv1.PeerStatus{
			{PeerId: "p_7", User: "alice", Device: "phone", Profile: "eu", Address: "10.77.0.2", Upstream: "nl",
				LastHandshake: timestamppb.New(handshake), Traffic: &lotsmanv1.Traffic{DownBytes: 1, UpBytes: 2, DownRate: 3, UpRate: 4}},
			// Blocked and never connected: no upstream, no handshake.
			{PeerId: "p_12", User: "bob", Device: "default", Profile: "eu", Address: "10.77.0.3"},
		},
	}
	if diff := cmp.Diff(want, snapshot(now, 1280, st, tr), protocmp.Transform()); diff != "" {
		t.Errorf("snapshot (-want +got):\n%s", diff)
	}

	// Before the first sample there is no traffic at all.
	if got := snapshot(now, 1280, st, traffic.Snapshot{}); got.Total != nil || got.Upstreams[0].Traffic != nil || got.Peers[0].Traffic != nil {
		t.Errorf("traffic before the first sample: total %v, nl %v, alice %v", got.Total, got.Upstreams[0].Traffic, got.Peers[0].Traffic)
	}
}

// watchedServer is the status service on an in-process server whose
// handler reports, on done, how each Watch call ended.
func watchedServer(src Source) (lotsmanv1connect.StatusServiceClient, <-chan error) {
	done := make(chan error, 1)
	s := connect.NewServer(func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			err := next(ctx, spec, stream)
			done <- err
			return err
		}
	})
	lotsmanv1connect.RegisterStatusServiceHandler(s, statusService{src})
	return lotsmanv1connect.NewStatusServiceClient(connect.NewClient(connectinprocess.New(s))), done
}

func TestWatchStreams(t *testing.T) {
	src := newFakeSource(daemon.Status{Upstreams: []daemon.UpstreamStatus{{Name: "nl", Kind: "awg", State: health.Up}}})
	client, done := watchedServer(src)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	// Below the minimum, so the stream runs at one snapshot a second.
	stream, err := client.Watch(ctx, &lotsmanv1.WatchStatusRequest{Interval: durationpb.New(time.Millisecond)})
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
	if len(first.Upstreams) != 1 || first.Upstreams[0].Name != "nl" {
		t.Errorf("first snapshot = %v", first)
	}
	second, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if gap := second.Time.AsTime().Sub(first.Time.AsTime()); gap < MinInterval-50*time.Millisecond {
		t.Errorf("snapshots %s apart, want the minimum interval %s", gap, MinInterval)
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
	if _, err := stream.Receive(); err == nil {
		t.Error("stream still delivering after cancel")
	}
}

// A snapshot of a daemon that has not finished its first round would show
// nothing, so Watch waits for it.
func TestWatchWaitsForReady(t *testing.T) {
	src := &fakeSource{ready: make(chan struct{})}
	client, done := watchedServer(src)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := client.Watch(ctx, &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan error, 1)
	go func() {
		_, err := stream.Receive()
		received <- err
	}()
	select {
	case err := <-received:
		t.Fatalf("snapshot before ready: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(src.ready)
	if err := <-received; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
}

// Every snapshot carries the sampler's latest traffic: streams share the
// daemon's samples instead of measuring on their own.
func TestWatchTraffic(t *testing.T) {
	src := newFakeSource(daemon.Status{
		Upstreams: []daemon.UpstreamStatus{{Name: "ol", Kind: "outline", State: health.Up}},
		Peers:     []daemon.PeerStatus{{ID: 3, User: "alice", Device: "default", Profile: "p", Upstream: "ol"}},
	})
	sample := func(down, up uint64) traffic.Snapshot {
		tr := traffic.Traffic{DownBytes: down, UpBytes: up, DownRate: down / 2, UpRate: up / 2}
		return traffic.Snapshot{Time: time.Now(), Peers: map[int64]traffic.Traffic{3: tr}, Upstreams: map[string]traffic.Traffic{"ol": tr}, Total: tr}
	}
	src.setTraffic(sample(1000, 100))
	client, _ := watchedServer(src)
	stream, err := client.Watch(t.Context(), &lotsmanv1.WatchStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []*lotsmanv1.Traffic{
		{DownBytes: 1000, UpBytes: 100, DownRate: 500, UpRate: 50},
		{DownBytes: 4000, UpBytes: 300, DownRate: 2000, UpRate: 150},
	} {
		got, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		for what, tr := range map[string]*lotsmanv1.Traffic{"total": got.Total, "upstream": got.Upstreams[0].Traffic, "peer": got.Peers[0].Traffic} {
			if !proto.Equal(tr, want) {
				t.Errorf("%s traffic = %v, want %v", what, tr, want)
			}
		}
		src.setTraffic(sample(4000, 300))
	}
}

func TestWatchRejectsInvalidInterval(t *testing.T) {
	client, _ := watchedServer(newFakeSource(daemon.Status{}))
	stream, err := client.Watch(t.Context(), &lotsmanv1.WatchStatusRequest{Interval: &durationpb.Duration{Seconds: 1, Nanos: -1}})
	if err == nil {
		_, err = stream.Receive()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("err = %v, want invalid_argument", err)
	}
}

// Every RPC of the package is registered, and all but the implemented ones
// answer Unimplemented. A new RPC is covered without touching this test.
func TestUnimplemented(t *testing.T) {
	implemented := []string{lotsmanv1connect.StatusServiceWatchProcedure}
	s := NewServer(newFakeSource(daemon.Status{}))
	var registered []string
	for spec := range s.Specs() {
		registered = append(registered, spec.Procedure)
	}
	var declared []string
	protoregistry.GlobalFiles.RangeFilesByPackage("lotsman.v1", func(f protoreflect.FileDescriptor) bool {
		for i := range f.Services().Len() {
			svc := f.Services().Get(i)
			for j := range svc.Methods().Len() {
				declared = append(declared, "/"+string(svc.FullName())+"/"+string(svc.Methods().Get(j).Name()))
			}
		}
		return true
	})
	slices.Sort(registered)
	slices.Sort(declared)
	if diff := cmp.Diff(declared, registered); diff != "" {
		t.Fatalf("registered procedures (-declared +registered):\n%s", diff)
	}

	client := connect.NewClient(connectinprocess.New(s))
	for spec := range s.Specs() {
		if slices.Contains(implemented, spec.Procedure) {
			continue
		}
		t.Run(spec.Procedure, func(t *testing.T) {
			md := spec.Schema.(protoreflect.MethodDescriptor)
			req, res := newMessage(t, md.Input()), newMessage(t, md.Output())
			var err error
			switch spec.StreamType {
			case connect.StreamTypeUnary:
				err = client.CallUnary(t.Context(), spec, req, res)
			case connect.StreamTypeServer:
				var stream connect.ClientStream
				if stream, err = client.CallServerStream(t.Context(), spec, req); err == nil {
					err = stream.Receive(res)
					stream.Close()
				}
			default:
				t.Fatalf("stream type %v: extend the test", spec.StreamType)
			}
			if connect.CodeOf(err) != connect.CodeUnimplemented {
				t.Errorf("err = %v, want unimplemented", err)
			}
		})
	}
}

func newMessage(t *testing.T, md protoreflect.MessageDescriptor) proto.Message {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		t.Fatal(err)
	}
	return mt.New().Interface()
}
