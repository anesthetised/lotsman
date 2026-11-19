package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/config"
)

func TestProfiles(t *testing.T) {
	s := open(t)
	if got, err := s.ListProfiles(); err != nil || len(got) != 0 {
		t.Fatalf("empty store: %+v, %v", got, err)
	}
	// Tiers keep their order, which is not the order of their fields.
	eu := config.Profile{Name: "eu", Prefer: []config.Match{{Tag: "near"}, {Geo: "DE", Provider: "acme"}, {Geo: "AT"}, {}}}
	auto := config.Profile{Name: "auto"}
	for _, p := range []config.Profile{eu, auto} {
		if err := s.AddProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddProfile(config.Profile{Name: "eu"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate AddProfile: %v", err)
	}
	want := []config.Profile{auto, eu}
	if got, err := s.ListProfiles(); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListProfiles = %+v, %v; want %+v", got, err, want)
	}

	// Set replaces the whole tier list; a shorter one leaves nothing behind.
	eu.Prefer = []config.Match{{Geo: "AT"}, {Tag: "near"}}
	if err := s.SetProfile(eu); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProfile(config.Profile{Name: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetProfile of a missing profile: %v", err)
	}
	auto.Prefer = []config.Match{{Provider: "acme"}}
	if err := s.PutProfile(auto); err != nil {
		t.Fatal(err)
	}
	nl := config.Profile{Name: "nl", Prefer: []config.Match{{Geo: "NL"}}}
	if err := s.PutProfile(nl); err != nil {
		t.Fatal(err)
	}
	want = []config.Profile{auto, eu, nl}
	if got, err := s.ListProfiles(); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("after set and put: %+v, %v; want %+v", got, err, want)
	}

	if err := s.DeleteProfile("auto"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("auto"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing profile: %v", err)
	}
	var tiers int
	s.db.QueryRow(`SELECT count(*) FROM profile_tiers WHERE profile = 'auto'`).Scan(&tiers)
	if tiers != 0 {
		t.Errorf("%d tiers outlived their profile", tiers)
	}
	if got, _ := s.ListProfiles(); len(got) != 2 || got[0].Name != "eu" {
		t.Errorf("after delete: %+v", got)
	}
}

// A profile cannot go while peers use it, and a peer cannot name a profile
// that does not exist.
func TestProfileReferences(t *testing.T) {
	s := open(t)
	if _, err := s.AddUserPeer("alice", DefaultDevice, "nl", subnet); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("peer with a missing profile: %v", err)
	}
	if users, _ := s.ListUsers(); len(users) != 0 {
		t.Errorf("a rejected AddUserPeer left a user behind: %+v", users)
	}
	addProfiles(t, s, "nl")
	if _, err := s.AddUserPeer("alice", DefaultDevice, "nl", subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUserPeer("alice", "phone", "nl", subnet); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("nl"); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting a used profile: %v", err)
	}
	if got, _ := s.ListProfiles(); len(got) != 1 {
		t.Errorf("a refused delete removed the profile: %+v", got)
	}
	s.DeleteDevice("alice", DefaultDevice)
	if err := s.DeleteProfile("nl"); !errors.Is(err, ErrInUse) {
		t.Errorf("deleting a profile one device still uses: %v", err)
	}
	s.DeleteUser("alice")
	if err := s.DeleteProfile("nl"); err != nil {
		t.Errorf("deleting an unused profile: %v", err)
	}
}

// A rename moves the tiers and the peers, which keep their ids, keys and
// addresses, and is seen whole by PeersAndProfiles.
func TestRenameProfile(t *testing.T) {
	s := open(t)
	nl := config.Profile{Name: "nl", Prefer: []config.Match{{Geo: "NL"}, {}}}
	for _, p := range []config.Profile{nl, {Name: "auto"}} {
		if err := s.AddProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddUserPeer("alice", "phone", "nl", subnet); err != nil {
		t.Fatal(err)
	}
	peer, err := s.Peer("alice", "phone", "nl") // with the stored, rounded time
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameProfile("nl", "Netherlands"); err != nil {
		t.Fatal(err)
	}
	peers, profiles, err := s.PeersAndProfiles()
	if err != nil {
		t.Fatal(err)
	}
	peer.Profile = "Netherlands"
	if len(peers) != 1 || !reflect.DeepEqual(peers[0], peer) {
		t.Errorf("peers = %+v, want %+v", peers, peer)
	}
	want := []config.Profile{{Name: "Netherlands", Prefer: nl.Prefer}, {Name: "auto"}}
	if !reflect.DeepEqual(profiles, want) {
		t.Errorf("profiles = %+v, want %+v", profiles, want)
	}
	var tiers int
	s.db.QueryRow(`SELECT count(*) FROM profile_tiers WHERE profile = 'nl'`).Scan(&tiers)
	if tiers != 0 {
		t.Errorf("%d tiers kept the old name", tiers)
	}

	// Only the case may change; another profile's name, in any case, is taken.
	if err := s.RenameProfile("Netherlands", "NETHERLANDS"); err != nil {
		t.Errorf("rename to another case: %v", err)
	}
	for _, tc := range []struct {
		from, to string
		want     error
	}{
		{"nl", "fi", ErrNotFound},
		{"auto", "netherlands", ErrExists},
		{"auto", "NETHERLANDS", ErrExists},
	} {
		if err := s.RenameProfile(tc.from, tc.to); !errors.Is(err, tc.want) {
			t.Errorf("RenameProfile(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
		}
	}
	if err := s.RenameProfile("auto", "auto"); err != nil {
		t.Errorf("rename to itself: %v", err)
	}
	if err := s.AddProfile(config.Profile{Name: "Auto"}); !errors.Is(err, ErrExists) {
		t.Errorf("AddProfile of a name taken in another case: %v", err)
	}
	if got, _ := s.ListProfiles(); len(got) != 2 {
		t.Errorf("a refused add stored a profile: %+v", got)
	}
}

// The release before profiles moved into the store is schema version 2. Its
// peers keep their profile names; the profiles themselves are imported later.
func TestMigrateFromVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:2] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	key, _ := awgconf.GeneratePrivateKey()
	if _, err := db.Exec(`PRAGMA user_version = 2;
		INSERT INTO users (id, name, created_at) VALUES (1, 'alice', '2026-11-01T00:00:00Z');
		INSERT INTO peers (user_id, device, profile, private_key, public_key, ip, created_at)
		VALUES (1, 'phone', 'eu', ?, ?, '10.77.0.2', '2026-11-01T00:00:00Z');`,
		key.String(), key.Public().String()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, err := s.Peer("alice", "phone", "eu"); err != nil || p.PrivateKey != key {
		t.Fatalf("migrated peer = %+v, %v", p, err)
	}
	if got, err := s.ListProfiles(); err != nil || len(got) != 0 {
		t.Fatalf("profiles after migration = %+v, %v", got, err)
	}
	if backups, _ := filepath.Glob(path + ".v2-*.bak"); len(backups) != 1 {
		t.Errorf("backups = %v", backups)
	}
	// Importing the profile its peers name makes it a normal, guarded profile.
	if err := s.AddProfile(config.Profile{Name: "eu", Prefer: []config.Match{{Geo: "NL"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProfile("eu"); !errors.Is(err, ErrInUse) {
		t.Errorf("deleting the imported profile its peers use: %v", err)
	}
}
