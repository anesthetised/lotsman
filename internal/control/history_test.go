package control

import (
	"errors"
	"strings"
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
	"github.com/anesthetised/lotsman/internal/history"
	"github.com/anesthetised/lotsman/internal/store"
)

// fakeHistory answers with what the test set and records what it was asked.
type fakeHistory struct {
	res  history.Result
	err  error
	name string // the upstream's name, or the peer's id as p_<id>
	got  history.Range
}

func (f *fakeHistory) UpstreamHistory(name string, r history.Range) (history.Result, error) {
	f.name, f.got = name, r
	return f.res, f.err
}

func (f *fakeHistory) PeerHistory(id int64, r history.Range) (history.Result, error) {
	f.name, f.got = PeerID(id), r
	return f.res, f.err
}

func historyClient(h HistorySource) lotsmanv1connect.HistoryServiceClient {
	src := newFakeSource(daemon.Status{})
	src.HistorySource = h
	return lotsmanv1connect.NewHistoryServiceClient(connect.NewClient(connectinprocess.New(NewServer(src))))
}

var (
	t0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func TestHistoryMapsTheRequestAndTheResponse(t *testing.T) {
	f := &fakeHistory{res: history.Result{Step: 2 * time.Minute, Samples: []store.Sample{
		{Time: t0, UpRatio: 0.75, Latency: 25 * time.Millisecond, Down: 1000, Up: 100},
		// An upstream that was never up has no latency, not a zero one.
		{Time: t0.Add(2 * time.Minute), UpRatio: 0, Down: 7, Up: 3},
	}}}
	client := historyClient(f)
	req := &lotsmanv1.HistoryRequest{Name: "nl", From: timestamppb.New(t0), To: timestamppb.New(t1), Step: durationpb.New(90 * time.Second)}
	want := &lotsmanv1.HistoryResponse{Step: durationpb.New(2 * time.Minute), Samples: []*lotsmanv1.Sample{
		{Time: timestamppb.New(t0), UpRatio: 0.75, Latency: durationpb.New(25 * time.Millisecond), DownBytes: 1000, UpBytes: 100},
		{Time: timestamppb.New(t0.Add(2 * time.Minute)), DownBytes: 7, UpBytes: 3},
	}}

	got, err := client.Upstream(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	if f.name != "nl" || !f.got.From.Equal(t0) || !f.got.To.Equal(t1) || f.got.Step != 90*time.Second {
		t.Errorf("asked for %q %+v", f.name, f.got)
	}

	// The peer is named by its API id.
	req.Name = "p_42"
	if _, err := client.Peer(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if f.name != "p_42" {
		t.Errorf("peer asked for %q", f.name)
	}

	// Unset times and step stay zero, for the planner to default or refuse.
	if _, err := client.Upstream(t.Context(), &lotsmanv1.HistoryRequest{Name: "nl", From: timestamppb.New(t0)}); err != nil {
		t.Fatal(err)
	}
	if !f.got.To.IsZero() || f.got.Step != 0 {
		t.Errorf("unset to and step arrived as %+v", f.got)
	}

	// No samples is an empty answer, with the step.
	f.res = history.Result{Step: time.Minute}
	got, err = client.Peer(t.Context(), req)
	if err != nil || len(got.Samples) != 0 || got.Step.AsDuration() != time.Minute {
		t.Errorf("empty history = %v, %v", got, err)
	}
}

func TestHistoryErrorCodes(t *testing.T) {
	valid := func(name string) *lotsmanv1.HistoryRequest {
		return &lotsmanv1.HistoryRequest{Name: name, From: timestamppb.New(t0), To: timestamppb.New(t1)}
	}
	cases := []struct {
		name string
		call func(c lotsmanv1connect.HistoryServiceClient) error
		src  error
		want connect.Code
	}{
		{"unknown upstream", func(c lotsmanv1connect.HistoryServiceClient) error {
			_, err := c.Upstream(t.Context(), valid("zz"))
			return err
		},
			store.ErrNotFound, connect.CodeNotFound},
		{"unknown peer", func(c lotsmanv1connect.HistoryServiceClient) error {
			_, err := c.Peer(t.Context(), valid("p_9"))
			return err
		},
			store.ErrNotFound, connect.CodeNotFound},
		{"a bad range, upstream", func(c lotsmanv1connect.HistoryServiceClient) error {
			_, err := c.Upstream(t.Context(), valid("nl"))
			return err
		},
			errors.Join(history.ErrRange, errors.New("to must be after from")), connect.CodeInvalidArgument},
		{"a bad range, peer", func(c lotsmanv1connect.HistoryServiceClient) error {
			_, err := c.Peer(t.Context(), valid("p_1"))
			return err
		},
			history.ErrRange, connect.CodeInvalidArgument},
		{"a failing database", func(c lotsmanv1connect.HistoryServiceClient) error {
			_, err := c.Upstream(t.Context(), valid("nl"))
			return err
		},
			errors.New("database is locked"), connect.CodeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(historyClient(&fakeHistory{err: c.src}))
			if got := connect.CodeOf(err); got != c.want {
				t.Errorf("code = %v, want %v (%v)", got, c.want, err)
			}
			// The database's own message stays on the daemon's side.
			if c.want == connect.CodeInternal && err != nil && strings.Contains(err.Error(), "locked") {
				t.Errorf("internal error leaks %q", err)
			}
		})
	}
}

// Requests that never reach the source: they are refused by what they say.
func TestHistoryRefusesBadRequests(t *testing.T) {
	f := &fakeHistory{}
	client := historyClient(f)
	for name, c := range map[string]struct {
		req  *lotsmanv1.HistoryRequest
		peer bool
		want connect.Code
	}{
		"no name":                 {&lotsmanv1.HistoryRequest{From: timestamppb.New(t0)}, false, connect.CodeInvalidArgument},
		"no peer name":            {&lotsmanv1.HistoryRequest{From: timestamppb.New(t0)}, true, connect.CodeInvalidArgument},
		"from out of range":       {&lotsmanv1.HistoryRequest{Name: "nl", From: &timestamppb.Timestamp{Seconds: 1 << 40}}, false, connect.CodeInvalidArgument},
		"to out of range":         {&lotsmanv1.HistoryRequest{Name: "nl", From: timestamppb.New(t0), To: &timestamppb.Timestamp{Seconds: -1 << 40}}, false, connect.CodeInvalidArgument},
		"step out of range":       {&lotsmanv1.HistoryRequest{Name: "nl", From: timestamppb.New(t0), Step: &durationpb.Duration{Seconds: 1, Nanos: -1}}, false, connect.CodeInvalidArgument},
		"a peer id without p_":    {&lotsmanv1.HistoryRequest{Name: "42", From: timestamppb.New(t0)}, true, connect.CodeNotFound},
		"a peer id, not a number": {&lotsmanv1.HistoryRequest{Name: "p_x", From: timestamppb.New(t0)}, true, connect.CodeNotFound},
		"a peer id, signed":       {&lotsmanv1.HistoryRequest{Name: "p_+4", From: timestamppb.New(t0)}, true, connect.CodeNotFound},
		"a peer id, padded":       {&lotsmanv1.HistoryRequest{Name: "p_04", From: timestamppb.New(t0)}, true, connect.CodeNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			f.name = ""
			var err error
			if c.peer {
				_, err = client.Peer(t.Context(), c.req)
			} else {
				_, err = client.Upstream(t.Context(), c.req)
			}
			if got := connect.CodeOf(err); got != c.want {
				t.Errorf("code = %v, want %v (%v)", got, c.want, err)
			}
			if f.name != "" {
				t.Errorf("the source was asked for %q", f.name)
			}
		})
	}
}

func TestParsePeerID(t *testing.T) {
	for _, id := range []int64{1, 7, 1234567890123} {
		if got, ok := ParsePeerID(PeerID(id)); !ok || got != id {
			t.Errorf("ParsePeerID(%q) = %d, %v", PeerID(id), got, ok)
		}
	}
	for _, bad := range []string{"", "p_", "p", "7", "P_7", "p_-7", "p_+7", "p_07", "p_7 ", "p_0x7", "p_99999999999999999999"} {
		if id, ok := ParsePeerID(bad); ok {
			t.Errorf("ParsePeerID(%q) = %d, true", bad, id)
		}
	}
}
