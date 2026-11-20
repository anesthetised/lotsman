package store

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func mustOpenIncident(t *testing.T, s *Store, upstream string, kind IncidentKind, started time.Time, detail string, devices int) int64 {
	t.Helper()
	id, err := s.OpenIncident(upstream, kind, started, detail, devices)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIncidents(t *testing.T) {
	s := open(t)
	down := mustOpenIncident(t, s, "nl", IncidentDown, at(1, 0), "probe failed", 3)
	mustOpenIncident(t, s, "nl", IncidentUpload, at(1, 5), "upload to a.test: 413", 3)
	old := mustOpenIncident(t, s, "de", IncidentDownload, at(0, 30), "16384 of 65536 bytes", 1)

	// One of a kind is open per upstream; another kind or upstream is fine.
	if _, err := s.OpenIncident("nl", IncidentDown, at(1, 1), "again", 3); err == nil {
		t.Error("a second open down incident for nl was accepted")
	}
	if err := s.SetIncidentDetail(down, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIncident(down, at(1, 10)); err != nil {
		t.Fatal(err)
	}
	// Once closed, it neither closes again nor changes.
	s.CloseIncident(down, at(2, 0))
	s.SetIncidentDetail(down, "later")
	again := mustOpenIncident(t, s, "nl", IncidentDown, at(1, 20), "probe failed", 0)
	s.CloseIncident(old, at(0, 45))

	got, err := s.Incidents("", 10)
	if err != nil {
		t.Fatal(err)
	}
	// Open first, then the newest.
	want := []Incident{
		{ID: again, Upstream: "nl", Kind: IncidentDown, Started: at(1, 20), Detail: "probe failed"},
		{ID: 2, Upstream: "nl", Kind: IncidentUpload, Started: at(1, 5), Detail: "upload to a.test: 413", Devices: 3},
		{ID: down, Upstream: "nl", Kind: IncidentDown, Started: at(1, 0), Ended: at(1, 10), Detail: "connection refused", Devices: 3},
		{ID: old, Upstream: "de", Kind: IncidentDownload, Started: at(0, 30), Ended: at(0, 45), Detail: "16384 of 65536 bytes", Devices: 1},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("incidents (-want +got):\n%s", diff)
	}
	if got, _ := s.Incidents("de", 10); len(got) != 1 || got[0].ID != old {
		t.Errorf("de's incidents = %+v", got)
	}
	if got, _ := s.Incidents("", 2); len(got) != 2 || got[1].Kind != IncidentUpload {
		t.Errorf("limit 2 = %+v", got)
	}
	if !want[0].Open() || want[2].Open() {
		t.Error("Open is wrong")
	}

	// A restart closes what was left open and says so.
	if err := s.CloseOpenIncidents(at(3, 0), "closed when the daemon started"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Incidents("nl", 10)
	for _, in := range got {
		if in.Open() {
			t.Errorf("still open after restart: %+v", in)
		}
	}
	// Now all closed, the newest first: the second down, then the upload.
	if got[1].Ended != at(3, 0) || got[1].Detail != "upload to a.test: 413; closed when the daemon started" {
		t.Errorf("closed by a restart: %+v", got[1])
	}
	// A new one of the same kind may open after that.
	mustOpenIncident(t, s, "nl", IncidentDown, at(3, 1), "", 0)
}

// Closed incidents expire with the hour samples; open ones stay however old.
func TestCompactExpiresIncidents(t *testing.T) {
	s := open(t)
	ended := mustOpenIncident(t, s, "nl", IncidentDown, at(0, 0), "", 0)
	s.CloseIncident(ended, at(0, 30))
	recent := mustOpenIncident(t, s, "nl", IncidentDownload, at(0, 0), "", 0)
	s.CloseIncident(recent, at(2, 0))
	open := mustOpenIncident(t, s, "nl", IncidentUpload, at(0, 0), "", 0)
	if err := s.Compact(at(11, 0), time.Hour, 10*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Incidents("", 10)
	var ids []int64
	for _, in := range got {
		ids = append(ids, in.ID)
	}
	if diff := cmp.Diff([]int64{open, recent}, ids); diff != "" {
		t.Errorf("incidents left (-want +got):\n%s", diff)
	}
}
