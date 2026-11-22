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
	"google.golang.org/protobuf/types/known/timestamppb"

	lotsmanv1 "github.com/anesthetised/lotsman/api/lotsman/v1"
	"github.com/anesthetised/lotsman/api/lotsman/v1/lotsmanv1connect"
	"github.com/anesthetised/lotsman/internal/daemon"
	"github.com/anesthetised/lotsman/internal/store"
)

// fakeIncidents answers with what the test set and records what it was asked.
type fakeIncidents struct {
	incidents []store.Incident
	err       error
	upstream  string
	limit     int
	offset    int
}

func (f *fakeIncidents) IncidentsPage(upstream string, limit, offset int) ([]store.Incident, error) {
	f.upstream, f.limit, f.offset = upstream, limit, offset
	return f.incidents, f.err
}

func incidentClient(f IncidentSource) lotsmanv1connect.IncidentServiceClient {
	src := newFakeSource(daemon.Status{})
	src.IncidentSource = f
	return lotsmanv1connect.NewIncidentServiceClient(connect.NewClient(connectinprocess.New(NewServer(src))))
}

func TestIncidentList(t *testing.T) {
	f := &fakeIncidents{incidents: []store.Incident{
		{ID: 3, Upstream: "nl", Kind: store.IncidentSendErrors, Started: t0, Detail: "6 errors in a minute", Devices: 2},
		{ID: 2, Upstream: "nl", Kind: store.IncidentUpload, Started: t0.Add(-time.Hour), Ended: t0.Add(-time.Minute), Detail: "upload to a.test: 413"},
		{ID: 1, Upstream: "de", Kind: store.IncidentDown, Started: t0.Add(-2 * time.Hour), Ended: t0.Add(-time.Hour)},
		{ID: 0, Upstream: "de", Kind: store.IncidentDownload, Started: t0.Add(-3 * time.Hour), Ended: t0.Add(-2 * time.Hour)},
	}}
	client := incidentClient(f)
	got, err := client.List(t.Context(), &lotsmanv1.ListIncidentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := &lotsmanv1.ListIncidentsResponse{Incidents: []*lotsmanv1.Incident{
		// Open: no end.
		{Id: "i_3", Upstream: "nl", Kind: lotsmanv1.IncidentKind_INCIDENT_KIND_SEND_ERRORS, Started: timestamppb.New(t0), Detail: "6 errors in a minute", Devices: 2},
		{Id: "i_2", Upstream: "nl", Kind: lotsmanv1.IncidentKind_INCIDENT_KIND_UPLOAD, Started: timestamppb.New(t0.Add(-time.Hour)),
			Ended: timestamppb.New(t0.Add(-time.Minute)), Detail: "upload to a.test: 413"},
		{Id: "i_1", Upstream: "de", Kind: lotsmanv1.IncidentKind_INCIDENT_KIND_DOWN, Started: timestamppb.New(t0.Add(-2 * time.Hour)),
			Ended: timestamppb.New(t0.Add(-time.Hour))},
		{Id: "i_0", Upstream: "de", Kind: lotsmanv1.IncidentKind_INCIDENT_KIND_DOWNLOAD, Started: timestamppb.New(t0.Add(-3 * time.Hour)),
			Ended: timestamppb.New(t0.Add(-2 * time.Hour))},
	}}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	if f.upstream != "" || f.limit != DefaultIncidents+1 || f.offset != 0 {
		t.Errorf("asked for %q, limit %d; want every upstream, the default limit", f.upstream, f.limit)
	}

	page, err := client.List(t.Context(), &lotsmanv1.ListIncidentsRequest{Upstream: "nl", Limit: 2, Offset: 2})
	if err != nil || f.upstream != "nl" || f.limit != 3 || f.offset != 2 || !page.GetHasMore() || len(page.GetIncidents()) != 2 {
		t.Errorf("asked for %q, limit %d, offset %d, response %v, error %v", f.upstream, f.limit, f.offset, page, err)
	}

	// The store's error stays inside the daemon.
	f.err = errors.New("disk I/O error")
	_, err = client.List(t.Context(), &lotsmanv1.ListIncidentsRequest{})
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "disk") {
		t.Errorf("store failure: %v", err)
	}
}
