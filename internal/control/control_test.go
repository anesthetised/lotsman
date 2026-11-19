package control

import (
	"context"
	"errors"
	"net/netip"
	"slices"
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
)

// fakeSource is a daemon with a fixed status.
type fakeSource struct {
	ready chan struct{}
	st    daemon.Status
}

func newFakeSource(st daemon.Status) *fakeSource {
	f := &fakeSource{ready: make(chan struct{}), st: st}
	close(f.ready)
	return f
}

func (f *fakeSource) Ready() <-chan struct{} { return f.ready }
func (f *fakeSource) ClientMTU() int         { return 1280 }

func (f *fakeSource) Status() daemon.Status { return f.st }

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
	want := &lotsmanv1.StatusSnapshot{
		Time: timestamppb.New(now), ClientMtu: 1280,
		Upstreams: []*lotsmanv1.UpstreamStatus{
			{Name: "nl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, Geo: "NL", Provider: "acme", Tags: []string{"eu", "fast"},
				State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UP, Latency: durationpb.New(25 * time.Millisecond),
				LastHandshake: timestamppb.New(handshake), Routed: 2, Active: 1},
			{Name: "de", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_OUTLINE, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DEGRADED,
				Latency: durationpb.New(time.Second)},
			{Name: "fi", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_VLESS, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DOWN},
			{Name: "se", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_AWG, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_UNKNOWN},
			{Name: "pl", Kind: lotsmanv1.UpstreamKind_UPSTREAM_KIND_UNSPECIFIED, State: lotsmanv1.UpstreamState_UPSTREAM_STATE_DOWN},
		},
		Peers: []*lotsmanv1.PeerStatus{
			{PeerId: "p_7", User: "alice", Device: "phone", Profile: "eu", Address: "10.77.0.2", Upstream: "nl",
				LastHandshake: timestamppb.New(handshake)},
			// Blocked and never connected: no upstream, no handshake.
			{PeerId: "p_12", User: "bob", Device: "default", Profile: "eu", Address: "10.77.0.3"},
		},
	}
	if diff := cmp.Diff(want, snapshot(now, 1280, st), protocmp.Transform()); diff != "" {
		t.Errorf("snapshot (-want +got):\n%s", diff)
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
