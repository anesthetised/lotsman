package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/policy"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "lotsman.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var subnet = netip.MustParsePrefix("10.77.0.0/16")

// nl is a route of one country, as a device might have.
var nl = policy.Route{Strict: true, Countries: []policy.Country{{Geo: "NL"}}}

func TestUsers(t *testing.T) {
	s := open(t)
	if _, err := s.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("alice"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate user: %v", err)
	}
	bob, err := s.CreateUser("bob")
	if err != nil || !reflect.DeepEqual(bob.Rights, DefaultRights()) {
		t.Fatalf("CreateUser = %+v, %v; want the default rights", bob, err)
	}
	users, err := s.ListUsers()
	if err != nil || len(users) != 2 || users[0].Name != "alice" || users[1].Name != "bob" {
		t.Fatalf("ListUsers = %+v, %v", users, err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: %v", err)
	}
}

func TestRights(t *testing.T) {
	s := open(t)
	if _, err := s.SetRights("alice", DefaultRights()); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRights of a missing user: %v", err)
	}
	s.CreateUser("alice")
	if u, err := s.User("alice"); err != nil || !reflect.DeepEqual(u.Rights, DefaultRights()) {
		t.Errorf("a new user's rights = %+v, %v; want %+v", u.Rights, err, DefaultRights())
	}
	for _, r := range []Rights{
		{EditRoutes: false, ShowProviders: true, Exits: []string{"nl", "de"}},
		{EditRoutes: true, ShowProviders: false},
		DefaultRights(),
	} {
		u, err := s.SetRights("alice", r)
		if err != nil || !reflect.DeepEqual(u.Rights, r) {
			t.Errorf("SetRights(%+v) = %+v, %v", r, u.Rights, err)
		}
		if u, err := s.User("alice"); err != nil || !reflect.DeepEqual(u.Rights, r) {
			t.Errorf("rights read back = %+v, %v; want %+v", u.Rights, err, r)
		}
	}
}

func TestPeers(t *testing.T) {
	s := open(t)
	if _, err := s.AddPeer("alice", "phone", nl, subnet); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer for missing user: %v", err)
	}
	s.CreateUser("alice")

	p1, err := s.AddPeer("alice", "phone", nl, subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p1.IP != netip.MustParseAddr("10.77.0.2") {
		t.Errorf("first address = %s, want 10.77.0.2 (network and gateway skipped)", p1.IP)
	}
	if p1.PublicKey != p1.PrivateKey.Public() || p1.PrivateKey.IsZero() {
		t.Error("key pair inconsistent")
	}
	if _, err := s.AddPeer("alice", "phone", policy.DefaultRoute(), subnet); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate device: %v", err)
	}
	p2, err := s.AddPeer("alice", "laptop", policy.DefaultRoute(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p2.IP != netip.MustParseAddr("10.77.0.3") {
		t.Errorf("second address = %s", p2.IP)
	}
	if _, err := s.AddUserPeer("bob", "phone", nl, subnet); err != nil {
		t.Errorf("another user's device of the same name must be allowed: %v", err)
	}

	got, err := s.Peer("alice", "phone")
	if err != nil || got.PrivateKey != p1.PrivateKey || got.IP != p1.IP || got.User != "alice" || !reflect.DeepEqual(got.Route, nl) {
		t.Errorf("Peer = %+v, %v", got, err)
	}
	all, err := s.ListPeers()
	if err != nil || len(all) != 3 || all[0].Device != "laptop" || all[1].Device != "phone" || all[2].User != "bob" {
		t.Fatalf("ListPeers = %+v, %v", all, err)
	}

	if err := s.DeletePeer("alice", "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Peer("alice", "phone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted peer still found: %v", err)
	}
	if err := s.DeletePeer("alice", "phone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing device: %v", err)
	}
	p3, _ := s.AddPeer("alice", "phone", nl, subnet)
	if p3.IP != p1.IP {
		t.Errorf("freed address %s not reused, got %s", p1.IP, p3.IP)
	}

	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListPeers(); len(all) != 1 || all[0].User != "bob" {
		t.Errorf("peers survived user deletion: %+v", all)
	}
}

// A new route keeps the device's key and address, so its config stays valid.
func TestSetRoute(t *testing.T) {
	s := open(t)
	if _, err := s.SetRoute("alice", "phone", nl); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRoute of a missing device: %v", err)
	}
	p, err := s.AddUserPeer("alice", "phone", policy.DefaultRoute(), subnet)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.AddUserPeer("bob", "phone", policy.DefaultRoute(), subnet)
	route := policy.Route{Fastest: true, Countries: []policy.Country{{Geo: "DE", Providers: []string{"acme", "beta"}}, {Geo: "NL"}}}
	got, err := s.SetRoute("alice", "phone", route)
	if err != nil || !reflect.DeepEqual(got.Route, route) || got.ID != p.ID || got.PrivateKey != p.PrivateKey || got.IP != p.IP {
		t.Errorf("SetRoute = %+v, %v", got, err)
	}
	if got, _ := s.Peer("alice", "phone"); !reflect.DeepEqual(got.Route, route) {
		t.Errorf("route read back = %+v", got.Route)
	}
	if got, _ := s.Peer("bob", "phone"); !reflect.DeepEqual(got.Route, other.Route) {
		t.Errorf("another user's device of the same name changed: %+v", got.Route)
	}
}

// seedVersion1 writes a first-release database with one peer and returns its key.
func seedVersion1(t *testing.T, path string) awgconf.Key {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatal(err)
	}
	key, _ := awgconf.GeneratePrivateKey()
	if _, err := db.Exec(`INSERT INTO users (id, name, created_at) VALUES (1, 'alice', '2026-09-17T00:00:00Z');
		INSERT INTO peers (user_id, profile, private_key, public_key, ip, created_at)
		VALUES (1, 'nl', ?, ?, '10.77.0.2', '2026-09-17T00:00:00Z');`,
		key.String(), key.Public().String()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return key
}

// The first release had no device column and never set user_version; its
// databases must migrate in place. Its users stay; its peers go with the
// profiles (#161).
func TestMigrateFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	seedVersion1(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if u, err := s.User("alice"); err != nil || !reflect.DeepEqual(u.Rights, DefaultRights()) {
		t.Fatalf("migrated user = %+v, %v", u, err)
	}
	if peers, err := s.ListPeers(); err != nil || len(peers) != 0 {
		t.Fatalf("migrated peers = %+v, %v; want none", peers, err)
	}
	if _, err := s.AddPeer("alice", "phone", nl, subnet); err != nil {
		t.Errorf("new schema not in effect after migration: %v", err)
	}
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}

	// The pre-migration database was snapshotted and still has the old schema.
	backups, _ := filepath.Glob(path + ".v1-*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	old, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var cols int
	old.QueryRow(`SELECT count(*) FROM pragma_table_info('peers') WHERE name = 'device'`).Scan(&cols)
	var rows int
	old.QueryRow(`SELECT count(*) FROM peers`).Scan(&rows)
	if cols != 0 || rows != 1 {
		t.Errorf("backup has device column: %v, peers: %d", cols != 0, rows)
	}
}

// A database as the profiles release left it (schema 6) keeps its users and
// their links and loses its peers, their history and the profiles (#161).
// New peers keep counting from the old ids.
func TestMigrateFromVersion6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lotsman.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:6] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 6;
		INSERT INTO users (id, name, created_at) VALUES (1, 'alice', '2026-11-01T00:00:00Z'), (2, 'bob', '2026-11-01T00:00:00Z');
		INSERT INTO user_links (user_id, token_hash, created_at) VALUES (1, x'00', '2026-11-01T00:00:00Z');
		INSERT INTO profiles VALUES ('nl');
		INSERT INTO profile_tiers VALUES ('nl', 0, 'NL', '', '');
		INSERT INTO peers (id, user_id, device, profile, private_key, public_key, ip, created_at) VALUES
			(7, 1, 'phone', 'nl', 'priv1', 'pub1', '10.77.0.2', '2026-11-01T00:00:00Z'),
			(8, 1, 'phone', 'eu', 'priv2', 'pub2', '10.77.0.3', '2026-11-01T00:00:00Z');
		INSERT INTO peer_samples VALUES (7, 1763985600, 10, 1);
		INSERT INTO peer_hour_samples VALUES (7, 1763982000, 10, 1);
		INSERT INTO upstream_samples VALUES ('nl', 1763985600, 1, 20, 1000, 100);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

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
	users, err := s.ListUsers()
	if err != nil || len(users) != 2 || !users[0].HasLink || users[1].HasLink {
		t.Fatalf("users after migration = %+v, %v; want alice with her link, then bob", users, err)
	}
	for _, u := range users {
		if !reflect.DeepEqual(u.Rights, DefaultRights()) {
			t.Errorf("%s's rights = %+v, want the defaults", u.Name, u.Rights)
		}
	}
	if peers, err := s.ListPeers(); err != nil || len(peers) != 0 {
		t.Errorf("peers after migration = %+v, %v; want none", peers, err)
	}
	for table, want := range map[string]int{"peer_samples": 0, "peer_hour_samples": 0, "upstream_samples": 1} {
		if n := countRows(t, s, table); n != want {
			t.Errorf("%s after migration: %d rows, want %d", table, n, want)
		}
	}
	var profileTables int
	s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'profile%'`).Scan(&profileTables)
	if profileTables != 0 {
		t.Errorf("%d profile tables left", profileTables)
	}
	p, err := s.AddPeer("alice", "phone", nl, subnet)
	if err != nil || p.ID <= 8 || p.IP.String() != "10.77.0.2" {
		t.Errorf("first new peer = %+v, %v; want an id after 8 and the first address", p, err)
	}
	if _, err := s.AddPeer("alice", "phone", nl, subnet); !errors.Is(err, ErrExists) {
		t.Errorf("a second phone for alice: %v", err)
	}
}

// A database as the routes release left it (schema 7) keeps its history;
// its upstream rows read as having had no speed test (#167).
func TestMigrateFromVersion7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lotsman.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:7] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	minute, hour := time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC), time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = 7;
		INSERT INTO upstream_samples VALUES ('nl', %d, 1, 20, 1000, 100);
		INSERT INTO upstream_hour_samples VALUES ('nl', %d, 0.5, 30, 5000, 500);`, minute.Unix(), hour.Unix())); err != nil {
		t.Fatal(err)
	}
	db.Close()

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
	got, err := s.UpstreamSamples("nl", hour, minute.Add(time.Minute), time.Hour)
	want := []Sample{
		{Time: hour, UpRatio: 0.5, Latency: 30 * time.Millisecond, Down: 5000, Up: 500},
		{Time: minute, UpRatio: 1, Latency: 20 * time.Millisecond, Down: 1000, Up: 100},
	}
	if diff := cmp.Diff(want, got); err != nil || diff != "" {
		t.Errorf("history after migration: %v (-want +got):\n%s", err, diff)
	}
	if _, err := s.OpenIncident("nl", IncidentDown, minute, "", 0); err != nil {
		t.Errorf("incidents after migration: %v", err)
	}
}

func TestFreshDatabaseMakesNoBackup(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if backups, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(backups) != 0 {
		t.Errorf("fresh database produced backups: %v", backups)
	}
	if _, err := Open(filepath.Join(dir, "new.db")); err != nil {
		t.Fatal(err)
	}
	if backups, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(backups) != 0 {
		t.Errorf("reopening an up-to-date database produced backups: %v", backups)
	}
}

func TestSubnetFull(t *testing.T) {
	s := open(t)
	s.CreateUser("u")
	tiny := netip.MustParsePrefix("10.0.0.0/29") // .0 net, .1 gw, .2-.6 usable, .7 broadcast
	for i, device := range []string{"a", "b", "c", "d", "e"} {
		p, err := s.AddPeer("u", device, nl, tiny)
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if p.IP.String() != "10.0.0."+string(rune('2'+i)) {
			t.Errorf("peer %d got %s", i, p.IP)
		}
	}
	if _, err := s.AddPeer("u", "f", nl, tiny); !errors.Is(err, ErrSubnetFull) {
		t.Errorf("expected ErrSubnetFull, got %v", err)
	}
}

func TestCanonicalSubnetAllocationBoundaries(t *testing.T) {
	for _, prefix := range []string{"10.77.42.0/24", "255.255.255.0/24"} {
		t.Run(prefix, func(t *testing.T) {
			network := netip.MustParsePrefix(prefix)
			used := map[netip.Addr]bool{}
			want := network.Addr().Next().Next()
			for i := 0; i < 253; i++ {
				got, ok := firstFree(network, used)
				if !ok || got != want || !network.Contains(got) {
					t.Fatalf("allocation %d = %s, %v; want %s inside %s", i, got, ok, want, network)
				}
				used[got] = true
				want = want.Next()
			}
			// Only the network, gateway and broadcast remain unused.
			if got, ok := firstFree(network, used); ok {
				t.Fatalf("exhausted network allocated reserved address %s", got)
			}
		})
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lotsman.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateUser("alice")
	p, _ := s.AddPeer("alice", "phone", nl, subnet)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Peer("alice", "phone")
	if err != nil || got.PrivateKey != p.PrivateKey || got.PublicKey != p.PublicKey || got.IP != p.IP || !reflect.DeepEqual(got.Route, nl) {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
}

// withUmask022 sets the process-wide umask for one test. Tests in this package
// do not run in parallel, so nothing else observes it.
func withUmask022(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s: %v", filepath.Base(path), err)
		return
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", filepath.Base(path), got, want)
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	withUmask022(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "lotsman.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateUser("alice")
	if _, err := s.AddPeer("alice", "phone", nl, subnet); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		assertMode(t, p, 0o600)
	}
	assertMode(t, dir, 0o755) // directories are not ours to change
}

func TestOpenTightensExistingFiles(t *testing.T) {
	withUmask022(t)
	path := filepath.Join(t.TempDir(), "lotsman.db")
	s, err := Open(path) // stays open so the sidecars exist
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateUser("alice")
	oldBackup := path + ".v1-20260101-000000.bak"
	if err := os.WriteFile(oldBackup, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []string{path, path + "-wal", path + "-shm", oldBackup}
	for _, p := range files {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for _, p := range files {
		assertMode(t, p, 0o600)
	}
	if users, err := s2.ListUsers(); err != nil || len(users) != 1 {
		t.Errorf("data after tightening: %v, %v", users, err)
	}
}

func TestMigrationBackupIsPrivateAndRestorable(t *testing.T) {
	withUmask022(t)
	path := filepath.Join(t.TempDir(), "lotsman.db")
	key := seedVersion1(t, path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	backups, _ := filepath.Glob(path + ".v1-*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	assertMode(t, backups[0], 0o600)
	assertMode(t, path, 0o600)

	// Rollback: the backup is the old database, keys included; put back
	// with an older binary, it is that binary's again.
	old, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	var priv string
	err = old.QueryRow(`SELECT private_key FROM peers`).Scan(&priv)
	old.Close()
	if err != nil || priv != key.String() {
		t.Errorf("peer key in the backup: %v", err)
	}
	if err := os.Rename(backups[0], path); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.User("alice"); err != nil {
		t.Errorf("user after rollback: %v", err)
	}
	assertMode(t, path, 0o600)
}

func TestCorruptTimestampIsReported(t *testing.T) {
	s := open(t)
	if _, err := s.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPeer("alice", "phone", nl, subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET created_at = 'garbage'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE peers SET created_at = 'garbage'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListUsers(); err == nil {
		t.Error("ListUsers accepted a corrupt created_at")
	}
	if _, err := s.ListPeers(); err == nil {
		t.Error("ListPeers accepted a corrupt created_at")
	}
}

func TestAddUserPeerIsAtomic(t *testing.T) {
	s := open(t)
	tiny := netip.MustParsePrefix("10.0.0.0/30") // .0 net, .1 gw, .2 usable, .3 broadcast
	if _, err := s.AddUserPeer("alice", "laptop", nl, tiny); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUserPeer("alice", "phone", nl, tiny); !errors.Is(err, ErrSubnetFull) {
		t.Fatalf("full subnet: %v", err)
	}
	if _, err := s.AddUserPeer("bob", "phone", nl, tiny); !errors.Is(err, ErrSubnetFull) {
		t.Fatalf("full subnet: %v", err)
	}
	users, err := s.ListUsers()
	if err != nil || len(users) != 1 || users[0].Name != "alice" {
		t.Errorf("a failed AddUserPeer left a user behind: %+v, %v", users, err)
	}
}

// A peer's id is its API identifier, so deleting the newest peer must not
// hand its id to the next one.
func TestPeerIDsAreNeverReused(t *testing.T) {
	s := open(t)
	subnet := netip.MustParsePrefix("10.77.0.0/24")
	a, err := s.AddUserPeer("alice", "phone", nl, subnet)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.AddUserPeer("alice", "laptop", nl, subnet)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePeer("alice", "laptop"); err != nil {
		t.Fatal(err)
	}
	c, err := s.AddUserPeer("alice", "tablet", nl, subnet)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID <= b.ID || c.ID == a.ID {
		t.Errorf("ids %d, %d (deleted), then %d: a deleted id was reused", a.ID, b.ID, c.ID)
	}
}

func TestPeerLookups(t *testing.T) {
	s := open(t)
	a, _ := s.AddUserPeer("alice", "phone", nl, subnet)
	b, _ := s.AddUserPeer("alice", "laptop", nl, subnet)
	c, _ := s.AddUserPeer("bob", "phone", nl, subnet)
	if p, err := s.PeerByID(c.ID); err != nil || p.User != "bob" || p.PrivateKey != c.PrivateKey {
		t.Errorf("PeerByID(%d) = %+v, %v", c.ID, p, err)
	}
	if _, err := s.PeerByID(c.ID + 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("PeerByID of a missing peer: %v", err)
	}
	peers, err := s.UserPeers("alice")
	if err != nil || len(peers) != 2 || peers[0].ID != b.ID || peers[1].ID != a.ID {
		t.Errorf("UserPeers(alice) = %+v, %v; want laptop, then phone", peers, err)
	}
	if peers, err := s.UserPeers("carol"); err != nil || len(peers) != 0 {
		t.Errorf("UserPeers(carol) = %+v, %v", peers, err)
	}
	if _, err := s.User("carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("User(carol): %v", err)
	}
}
