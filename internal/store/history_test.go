package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// A day that is a multiple of the hour, so the boundaries in the tests are
// easy to read.
var day = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

func at(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustAdd(t *testing.T, s *Store, ts time.Time, ups []UpstreamSample, peers []PeerSample) {
	t.Helper()
	if err := s.AddSamples(ts, ups, peers); err != nil {
		t.Fatal(err)
	}
}

func TestSamplesRoundTrip(t *testing.T) {
	s := open(t)
	mustAdd(t, s, at(1, 0),
		[]UpstreamSample{{Name: "nl", UpRatio: 1, RTT: 25 * time.Millisecond, Down: 100, Up: 10}, {Name: "de", UpRatio: 0.5, Down: 7, Up: 3}},
		[]PeerSample{{PeerID: 4, Down: 90, Up: 9}, {PeerID: 5, Down: 1, Up: 2}})
	mustAdd(t, s, at(1, 1), []UpstreamSample{{Name: "nl", UpRatio: 0.5, RTT: 35 * time.Millisecond, Down: 200, Up: 20}}, []PeerSample{{PeerID: 4, Down: 10, Up: 1}})

	got, err := s.UpstreamSamples("nl", at(1, 0), at(1, 2), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want := []Sample{
		{Time: at(1, 0), UpRatio: 1, Latency: 25 * time.Millisecond, Down: 100, Up: 10},
		{Time: at(1, 1), UpRatio: 0.5, Latency: 35 * time.Millisecond, Down: 200, Up: 20},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("nl by the minute (-want +got):\n%s", diff)
	}

	// A coarser step sums the bytes and averages the rest. The upstream that
	// was never up has no latency, not zero milliseconds that would drag a mean down.
	got, err = s.UpstreamSamples("nl", at(1, 0), at(1, 2), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	want = []Sample{{Time: at(1, 0), UpRatio: 0.75, Latency: 30 * time.Millisecond, Down: 300, Up: 30}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("nl by two minutes (-want +got):\n%s", diff)
	}
	got, _ = s.UpstreamSamples("de", at(1, 0), at(1, 2), time.Minute)
	if diff := cmp.Diff([]Sample{{Time: at(1, 0), UpRatio: 0.5, Down: 7, Up: 3}}, got); diff != "" {
		t.Errorf("de (-want +got):\n%s", diff)
	}

	peers, err := s.PeerSamples(4, at(1, 0), at(1, 2), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Sample{{Time: at(1, 0), Down: 100, Up: 10}}, peers); diff != "" {
		t.Errorf("peer 4 (-want +got):\n%s", diff)
	}

	// The range is half open and bounds the rows, not the steps.
	got, _ = s.UpstreamSamples("nl", at(1, 1), at(1, 2), time.Minute)
	if len(got) != 1 || got[0].Time != at(1, 1) {
		t.Errorf("[1:01, 1:02) = %+v", got)
	}
	if got, err := s.UpstreamSamples("nope", at(0, 0), at(2, 0), time.Minute); err != nil || len(got) != 0 {
		t.Errorf("unknown upstream = %+v, %v", got, err)
	}
}

// A minute written twice, as after the clock stepped back, keeps both bytes.
func TestSamplesWrittenTwiceAddUp(t *testing.T) {
	s := open(t)
	mustAdd(t, s, at(1, 0), []UpstreamSample{{Name: "nl", UpRatio: 1, Down: 5, Up: 1}}, []PeerSample{{PeerID: 1, Down: 5, Up: 1}})
	mustAdd(t, s, at(1, 0), []UpstreamSample{{Name: "nl", UpRatio: 0.5, Down: 7, Up: 2}}, []PeerSample{{PeerID: 1, Down: 7, Up: 2}})
	got, _ := s.UpstreamSamples("nl", at(1, 0), at(1, 1), time.Minute)
	if diff := cmp.Diff([]Sample{{Time: at(1, 0), UpRatio: 0.5, Down: 12, Up: 3}}, got); diff != "" {
		t.Errorf("upstream (-want +got):\n%s", diff)
	}
	peers, _ := s.PeerSamples(1, at(1, 0), at(1, 1), time.Minute)
	if diff := cmp.Diff([]Sample{{Time: at(1, 0), Down: 12, Up: 3}}, peers); diff != "" {
		t.Errorf("peer (-want +got):\n%s", diff)
	}
}

// seedMinutes fills hours [from, to) with a minute row each: the upstream is
// up the first half of every hour, with a latency of 10 ms + the hour, and
// moves 3 bytes down and 1 up a minute, the peer 1 down.
func seedMinutes(t *testing.T, s *Store, from, to int) {
	t.Helper()
	for h := from; h < to; h++ {
		for m := 0; m < 60; m++ {
			ratio := 0.0
			if m < 30 {
				ratio = 1
			}
			mustAdd(t, s, at(h, m), []UpstreamSample{{Name: "nl", UpRatio: ratio, RTT: time.Duration(10+h) * time.Millisecond, Down: 3, Up: 1}},
				[]PeerSample{{PeerID: 1, Down: 1}})
		}
	}
}

// Whole hours that ended before now − minuteKeep fold; the hour that holds
// the boundary stays minute by minute, so a query never counts a minute twice.
func TestCompactFoldsWholeHours(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 2, 5)

	// now − minuteKeep is 3:30: hour 2 ended before it, hour 3 has not.
	if err := s.Compact(at(5, 0), 90*time.Minute, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if got, want := countRows(t, s, "upstream_samples"), 120; got != want {
		t.Errorf("upstream minute rows = %d, want %d", got, want)
	}
	if got, want := countRows(t, s, "peer_samples"), 120; got != want {
		t.Errorf("peer minute rows = %d, want %d", got, want)
	}
	hour, err := s.UpstreamSamples("nl", at(2, 0), at(3, 0), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]Sample{{Time: at(2, 0), UpRatio: 0.5, Latency: 12 * time.Millisecond, Down: 180, Up: 60}}, hour); diff != "" {
		t.Errorf("folded hour (-want +got):\n%s", diff)
	}
	peer, _ := s.PeerSamples(1, at(2, 0), at(3, 0), time.Hour)
	if diff := cmp.Diff([]Sample{{Time: at(2, 0), Down: 60}}, peer); diff != "" {
		t.Errorf("folded peer hour (-want +got):\n%s", diff)
	}

	// Hours that are folded and hours that are not read as one series, with
	// every minute counted once.
	all, _ := s.UpstreamSamples("nl", at(2, 0), at(5, 0), time.Hour)
	var down uint64
	for _, x := range all {
		down += x.Down
	}
	if len(all) != 3 || down != 3*180 || all[0].Time != at(2, 0) || all[2].Time != at(4, 0) {
		t.Errorf("hours 2–4 = %+v", all)
	}
}

// The boundary is exact: a minute at the cut stays, the one before it folds.
func TestCompactBoundary(t *testing.T) {
	s := open(t)
	cut := at(3, 0)
	mustAdd(t, s, cut.Add(-time.Minute), nil, []PeerSample{{PeerID: 1, Down: 1}})
	mustAdd(t, s, cut, nil, []PeerSample{{PeerID: 1, Down: 2}})
	// Not on the hour: now − minuteKeep = 3:00:30 floors to 3:00.
	if err := s.Compact(cut.Add(24*time.Hour+30*time.Second), 24*time.Hour, 1000*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "peer_samples"); n != 1 {
		t.Errorf("minute rows = %d, want only the one at the cut", n)
	}
	var folded, ts int64
	if err := s.db.QueryRow(`SELECT down_bytes, ts FROM peer_hour_samples`).Scan(&folded, &ts); err != nil || folded != 1 || ts != at(2, 0).Unix() {
		t.Errorf("hour row = %d bytes at %d, %v", folded, ts, err)
	}
}

func snapshot(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, table := range []string{"upstream_samples", "upstream_hour_samples", "peer_samples", "peer_hour_samples"} {
		rows, err := s.db.Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], vals)
		}
		rows.Close()
	}
	return out
}

func TestCompactIsIdempotent(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 0, 6)
	now := at(6, 0)
	if err := s.Compact(now, 2*time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	once := snapshot(t, s)
	if len(once["upstream_hour_samples"]) != 4 || len(once["upstream_samples"]) != 120 {
		t.Fatalf("after one run: %d hour rows, %d minute rows", len(once["upstream_hour_samples"]), len(once["upstream_samples"]))
	}
	if err := s.Compact(now, 2*time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(once, snapshot(t, s)); diff != "" {
		t.Errorf("a second run changed the rows (-first +second):\n%s", diff)
	}
}

// A failing compaction leaves the minutes where they are: the fold and the
// delete are one transaction.
func TestCompactFailureChangesNothing(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 0, 3)
	// The peer tables are compacted after the upstream ones; a missing table
	// fails the statement that follows an upstream fold that already ran.
	if _, err := s.db.Exec(`DROP TABLE peer_hour_samples`); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(at(6, 0), time.Hour, 100*time.Hour); err == nil {
		t.Fatal("Compact succeeded without a table")
	}
	if n, h := countRows(t, s, "upstream_samples"), countRows(t, s, "upstream_hour_samples"); n != 180 || h != 0 {
		t.Errorf("after the failure: %d minute rows, %d hour rows", n, h)
	}
}

func TestCompactExpiresHours(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 0, 4)
	// Fold hours 0–2; hour 3 is still minutes.
	if err := s.Compact(at(4, 0), time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "upstream_hour_samples"); n != 3 { // 0:00, 1:00, 2:00; 3:00 is still in the minute rows
		t.Fatalf("hour rows = %d", n)
	}
	// An hour row expires once it started before now − hourKeep: at 3:00
	// a hourKeep of 2h30 is 0:30, which keeps 1:00 and 2:00, not 0:00.
	if err := s.Compact(at(3, 0), time.Hour, 150*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UpstreamSamples("nl", at(0, 0), at(3, 0), time.Hour)
	if len(got) != 2 || got[0].Time != at(1, 0) {
		t.Errorf("after expiry: %+v", got)
	}
	if n := countRows(t, s, "peer_hour_samples"); n != 2 {
		t.Errorf("peer hour rows = %d, want 2", n)
	}
	// A row that started exactly at the limit stays.
	if err := s.Compact(at(3, 0), time.Hour, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, s, "upstream_hour_samples"); n != 2 {
		t.Errorf("hour row at the limit: %d rows, want 2", n)
	}
}

// A minute that turns up after its hour was folded, such as after a clock
// step, joins the hour instead of replacing it.
func TestCompactMergesIntoFoldedHour(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 2, 3)
	now := at(10, 0)
	if err := s.Compact(now, time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, at(2, 5), []UpstreamSample{{Name: "nl", UpRatio: 0, Down: 100, Up: 100}}, []PeerSample{{PeerID: 1, Down: 100}})
	if err := s.Compact(now, time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UpstreamSamples("nl", at(2, 0), at(3, 0), time.Hour)
	if len(got) != 1 || got[0].Down != 280 || got[0].Up != 160 || got[0].UpRatio != 0.25 {
		t.Errorf("merged hour = %+v", got)
	}
	peer, _ := s.PeerSamples(1, at(2, 0), at(3, 0), time.Hour)
	if len(peer) != 1 || peer[0].Down != 160 {
		t.Errorf("merged peer hour = %+v", peer)
	}
}

// An hour row stands for sixty minutes next to minute rows in a coarser step.
func TestStepAcrossHourAndMinuteRows(t *testing.T) {
	s := open(t)
	seedMinutes(t, s, 0, 2) // hour 0 folds, hour 1 stays minutes
	if err := s.Compact(at(2, 0), time.Hour, 100*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpstreamSamples("nl", at(0, 0), at(2, 0), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Both hours are half up: the mean is 0.5 whichever way they are stored.
	// Their latencies, 10 and 11 ms, average to 10.5.
	want := []Sample{{Time: at(0, 0), UpRatio: 0.5, Latency: 10500 * time.Microsecond, Down: 360, Up: 120}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestHasPeer(t *testing.T) {
	s := open(t)
	addProfiles(t, s, "nl")
	p, err := s.AddUserPeer("alice", DefaultDevice, "nl", subnet)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPeer(p.ID); !ok || err != nil {
		t.Errorf("HasPeer(%d) = %v, %v", p.ID, ok, err)
	}
	if err := s.DeletePeer("alice", DefaultDevice, "nl"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPeer(p.ID); ok || err != nil {
		t.Errorf("HasPeer of a deleted peer = %v, %v", ok, err)
	}
}

// seedVersion4 writes a database as the release before history left it, with
// a peer, and returns the path.
func seedVersion4(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lotsman.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (id, name, created_at) VALUES (1, 'alice', '2026-11-01T00:00:00Z');
		INSERT INTO peers (user_id, device, profile, private_key, public_key, ip, created_at)
		VALUES (1, 'phone', 'nl', 'priv', 'pub', '10.77.0.2', '2026-11-01T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrateFromVersion4(t *testing.T) {
	path := seedVersion4(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
	// The peers survive and the history tables work.
	if ok, err := s.HasPeer(1); !ok || err != nil {
		t.Errorf("peer 1 after migration: %v, %v", ok, err)
	}
	mustAdd(t, s, at(1, 0), []UpstreamSample{{Name: "nl", UpRatio: 1, Down: 1}}, []PeerSample{{PeerID: 1, Down: 1}})
	if err := s.Compact(at(2, 0), time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	// The snapshot taken before the migration has the old schema.
	backups, _ := filepath.Glob(path + ".v4-*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	old, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var tables int
	old.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE '%samples'`).Scan(&tables)
	if tables != 0 {
		t.Errorf("backup has %d history tables", tables)
	}
}
