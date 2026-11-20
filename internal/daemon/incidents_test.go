package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
	"github.com/anesthetised/lotsman/internal/metrics"
	"github.com/anesthetised/lotsman/internal/store"
)

// incidentsOf lists what the store holds for the upstream, open first.
func incidentsOf(t *testing.T, h *harness, upstream string) []store.Incident {
	t.Helper()
	got, err := h.d.Incidents(upstream, 100)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// statusOf is the upstream's line in the daemon's status.
func statusOf(h *harness, name string) UpstreamStatus {
	for _, u := range h.d.Status().Upstreams {
		if u.Name == name {
			return u
		}
	}
	panic("no upstream " + name)
}

// A failing upload probe degrades an upstream as a failing download does,
// and opens an upload incident, not a download one, for as long as it lasts.
func TestUploadDegradesAndRecovers(t *testing.T) {
	h, f, round, _ := transferHarness(t, 2)
	h.cfg.Health.Transfer.UploadURL, h.cfg.Health.Transfer.UploadBytes = "https://up.test/", 1000
	h.st.CreateUser("alice")
	euPeer, _ := h.st.AddPeer("alice", "eu", euRoute, h.cfg.Subnet)
	routeOf := func() string { r, _ := h.dp.RouteOf(euPeer.IP); return r }
	nl, upload := metrics.Label{Name: "upstream", Value: "nl"}, metrics.Label{Name: "probe", Value: "upload"}

	round()
	if f.callsTo("lm-up-nl up") != 1 || !f.urls["https://up.test/"] {
		t.Fatalf("upload probes through nl: %d, URLs %v", f.callsTo("lm-up-nl up"), f.urls)
	}
	f.set("lm-up-nl up", true)
	round()
	if degradedOf(h, "nl") || len(incidentsOf(t, h, "nl")) != 0 {
		t.Fatal("degraded, or an incident, after a single failed upload")
	}
	round()
	if !degradedOf(h, "nl") || routeOf() != "lm-up-de" {
		t.Fatalf("after down_after failed uploads: degraded %v, eu via %q", degradedOf(h, "nl"), routeOf())
	}
	got := incidentsOf(t, h, "nl")
	if len(got) != 1 || got[0].Kind != store.IncidentUpload || !got[0].Open() || got[0].Devices != 1 || !strings.Contains(got[0].Detail, "16384 of 65536") {
		t.Fatalf("incidents = %+v; want one open upload incident, with eu's device", got)
	}
	if kinds := statusOf(h, "nl").Incidents; !slices.Equal(kinds, []store.IncidentKind{store.IncidentUpload}) {
		t.Errorf("status incidents = %v", kinds)
	}
	if v, _ := metric(h.d.Metrics(), "lotsman_upstream_transfer_probes_total", nl, upload, metrics.Label{Name: "result", Value: "fail"}); v != 2 {
		t.Errorf("transfer_probes_total{nl,upload,fail} = %v", v)
	}
	if v, _ := metric(h.d.Metrics(), "lotsman_upstream_transfer_probes_total", nl, metrics.Label{Name: "probe", Value: "download"}, metrics.Label{Name: "result", Value: "fail"}); v != 0 {
		t.Errorf("transfer_probes_total{nl,download,fail} = %v", v)
	}

	f.set("lm-up-nl up", false)
	round()
	round()
	if degradedOf(h, "nl") || routeOf() != "lm-up-nl" {
		t.Fatalf("after recovery: degraded %v, eu via %q", degradedOf(h, "nl"), routeOf())
	}
	if got := incidentsOf(t, h, "nl"); len(got) != 1 || got[0].Open() || len(statusOf(h, "nl").Incidents) != 0 {
		t.Errorf("after recovery: incidents %+v, status %v", got, statusOf(h, "nl").Incidents)
	}
}

// Both ways failing are two incidents; turning the upload probe off by a
// reload closes its incident and leaves the download's.
func TestUploadOffOnReload(t *testing.T) {
	h, f, round, _ := transferHarness(t, 1)
	h.cfg.Health.Transfer.UploadURL, h.cfg.Health.Transfer.UploadBytes = "https://up.test/", 1000
	f.set("lm-up-nl", true)
	f.set("lm-up-nl up", true)
	round()
	round()
	if kinds := statusOf(h, "nl").Incidents; !slices.Equal(kinds, []store.IncidentKind{store.IncidentDownload, store.IncidentUpload}) {
		t.Fatalf("incidents = %v, want download and upload", kinds)
	}
	f.set("lm-up-nl", false)
	cfg, err := config.Parse(fmt.Appendf(nil, harnessConfig, h.dir, h.dir, h.dir))
	if err != nil {
		t.Fatal(err)
	}
	noUpload := *h.cfg.Health.Transfer
	noUpload.UploadURL = ""
	cfg.Health.Transfer, cfg.Health.UpAfter = &noUpload, 1
	if err := h.d.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if kinds := statusOf(h, "nl").Incidents; !slices.Equal(kinds, []store.IncidentKind{store.IncidentDownload}) {
		t.Errorf("upload turned off: incidents %v, want download only", kinds)
	}
	calls := f.callsTo("lm-up-nl up")
	round()
	if f.callsTo("lm-up-nl up") != calls {
		t.Error("upload probe ran after it was turned off")
	}
	if degradedOf(h, "nl") || len(statusOf(h, "nl").Incidents) != 0 {
		t.Errorf("download passes again: degraded %v, incidents %v", degradedOf(h, "nl"), statusOf(h, "nl").Incidents)
	}
}

// An upstream that fails its reachability probes has a down incident from
// the round that marks it down to the one that marks it up; its detail
// follows the latest error. Removing the upstream closes what it had open.
func TestDownIncident(t *testing.T) {
	h, _, round, _ := transferHarness(t, 1) // down_after 2
	h.st.CreateUser("alice")
	h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	round()
	h.setHealthy("lm-up-nl", false)
	round()
	if got := incidentsOf(t, h, "nl"); len(got) != 0 {
		t.Fatalf("incident after one failed round: %+v", got)
	}
	round()
	got := incidentsOf(t, h, "nl")
	if len(got) != 1 || got[0].Kind != store.IncidentDown || !got[0].Open() || got[0].Devices != 1 || got[0].Detail != "probe failed" {
		t.Fatalf("incidents = %+v; want an open down incident with nl's device", got)
	}
	round()
	if got := incidentsOf(t, h, "nl"); len(got) != 1 {
		t.Errorf("still down opened another: %+v", got)
	}
	h.setHealthy("lm-up-nl", true)
	round()
	if got := incidentsOf(t, h, "nl"); got[0].Open() || got[0].Ended.Before(got[0].Started) {
		t.Errorf("up again: %+v", got[0])
	}

	h.setHealthy("lm-up-de", false)
	round()
	round()
	if kinds := statusOf(h, "de").Incidents; !slices.Equal(kinds, []store.IncidentKind{store.IncidentDown}) {
		t.Fatalf("de: %v", kinds)
	}
	cfg, err := config.Parse(fmt.Appendf(nil, strings.Replace(harnessConfig, "  - {name: de, conf: %s/de.conf, geo: DE, provider: acme}\n", "", 1), h.dir, h.dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.Reload(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := incidentsOf(t, h, "de"); len(got) != 1 || got[0].Open() {
		t.Errorf("de removed: %+v", got)
	}
}

// Incidents an earlier run left open are closed when the daemon starts.
func TestLeftoverIncidentsClosedOnStart(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.OpenIncident("nl", store.IncidentDown, time.Now().Add(-time.Hour), "probe failed", 2); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Truncate(time.Second)
	h.run(t)
	<-h.d.Ready()
	got := incidentsOf(t, h, "nl")
	if len(got) != 1 || got[0].Open() || got[0].Ended.Before(before) || got[0].Detail != "probe failed; closed when the daemon started" {
		t.Errorf("left over = %+v", got)
	}
}

// More errors than the threshold in a minute open a send_errors incident;
// five minutes in a row at or below it close it. The clock is the test's.
func TestSendErrorsIncident(t *testing.T) {
	h := newHarness(t)
	h.st.CreateUser("alice")
	h.st.AddPeer("alice", "nl", nlRoute, h.cfg.Subnet)
	h.run(t)
	<-h.d.Ready()
	h.d.mu.Lock()
	nl := h.d.ups[0]
	h.d.mu.Unlock()
	errs := func(n int) {
		for i := range n {
			nl.errs.add(fmt.Sprintf("peer(abc) - Failed to send data packets: sendmmsg: message too long (%d)", i))
		}
	}
	clock := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	minute := func(n int) {
		errs(n)
		h.d.checkSendErrors(clock)
		clock = clock.Add(time.Minute)
	}
	open := func() bool { return slices.Contains(statusOf(h, "nl").Incidents, store.IncidentSendErrors) }

	minute(5) // the threshold is 5: at it is fine
	if open() {
		t.Fatal("open at the threshold")
	}
	opened := clock
	minute(6)
	got := incidentsOf(t, h, "nl")
	if !open() || len(got) != 1 || !got[0].Started.Equal(opened) || got[0].Devices != 1 ||
		got[0].Detail != "6 errors in a minute, the latest: peer(abc) - Failed to send data packets: sendmmsg: message too long (5)" {
		t.Fatalf("after 6 errors in a minute: %+v", got)
	}
	// A quiet stretch that is broken starts again.
	for range 4 {
		minute(5)
	}
	minute(100)
	for range 4 {
		minute(0)
	}
	if !open() {
		t.Fatal("closed after 4 quiet minutes")
	}
	closed := clock
	minute(0)
	got = incidentsOf(t, h, "nl")
	if open() || len(got) != 1 || !got[0].Ended.Equal(closed) || !strings.HasPrefix(got[0].Detail, "100 errors in a minute") {
		t.Errorf("after 5 quiet minutes: %+v", got)
	}
	if v, _ := metric(h.d.Metrics(), "lotsman_upstream_send_errors_total", metrics.Label{Name: "upstream", Value: "nl"}); v != 131 {
		t.Errorf("send_errors_total{nl} = %v, want 131", v)
	}
	if _, ok := metric(h.d.Metrics(), "lotsman_upstream_send_errors_total", metrics.Label{Name: "upstream", Value: "de"}); !ok {
		t.Error("no send_errors_total for de")
	}
}

func TestSendErrorWindow(t *testing.T) {
	var w sendErrorWindow
	var count uint64
	step := func(n uint64) bool {
		count += n
		_, trouble := w.observe(count, 2)
		return trouble
	}
	for i, tc := range []struct {
		errors  uint64
		trouble bool
	}{{2, false}, {3, true}, {0, true}, {2, true}, {1, true}, {0, true}, {0, false}, {0, false}, {3, true}} {
		if got := step(tc.errors); got != tc.trouble {
			t.Errorf("minute %d with %d errors: trouble %v, want %v", i, tc.errors, got, tc.trouble)
		}
	}
}
