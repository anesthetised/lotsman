package store

import (
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/anesthetised/lotsman/internal/awgconf"
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

func TestUsers(t *testing.T) {
	s := open(t)
	if _, err := s.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("alice"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate user: %v", err)
	}
	if _, err := s.CreateUser("bob"); err != nil {
		t.Fatal(err)
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

func TestPeers(t *testing.T) {
	s := open(t)
	if _, err := s.AddPeer("alice", DefaultDevice, "nl", subnet); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer for missing user: %v", err)
	}
	s.CreateUser("alice")

	p1, err := s.AddPeer("alice", DefaultDevice, "nl", subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p1.IP != netip.MustParseAddr("10.77.0.2") {
		t.Errorf("first address = %s, want 10.77.0.2 (network and gateway skipped)", p1.IP)
	}
	if p1.PublicKey != p1.PrivateKey.Public() || p1.PrivateKey.IsZero() {
		t.Error("key pair inconsistent")
	}
	if _, err := s.AddPeer("alice", DefaultDevice, "nl", subnet); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate profile: %v", err)
	}
	p2, err := s.AddPeer("alice", DefaultDevice, "de", subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p2.IP != netip.MustParseAddr("10.77.0.3") {
		t.Errorf("second address = %s", p2.IP)
	}

	got, err := s.Peer("alice", DefaultDevice, "nl")
	if err != nil || got.PrivateKey != p1.PrivateKey || got.IP != p1.IP || got.User != "alice" {
		t.Errorf("Peer = %+v, %v", got, err)
	}
	all, err := s.ListPeers()
	if err != nil || len(all) != 2 || all[0].Profile != "de" || all[1].Profile != "nl" {
		t.Fatalf("ListPeers = %+v, %v", all, err)
	}

	if err := s.DeletePeer("alice", DefaultDevice, "nl"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Peer("alice", DefaultDevice, "nl"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted peer still found: %v", err)
	}
	p3, _ := s.AddPeer("alice", DefaultDevice, "nl", subnet)
	if p3.IP != p1.IP {
		t.Errorf("freed address %s not reused, got %s", p1.IP, p3.IP)
	}

	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListPeers(); len(all) != 0 {
		t.Errorf("peers survived user deletion: %+v", all)
	}
}

func TestDevices(t *testing.T) {
	s := open(t)
	s.CreateUser("alice")
	if _, err := s.AddPeer("alice", "phone", "nl", subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPeer("alice", "laptop", "nl", subnet); err != nil {
		t.Fatalf("same profile on another device must be allowed: %v", err)
	}
	if _, err := s.AddPeer("alice", "laptop", "eu", subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPeer("alice", "laptop", "eu", subnet); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate device+profile: %v", err)
	}
	p, err := s.Peer("alice", "laptop", "eu")
	if err != nil || p.Device != "laptop" {
		t.Errorf("Peer = %+v, %v", p, err)
	}
	if err := s.DeleteDevice("alice", "laptop"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListPeers()
	if len(all) != 1 || all[0].Device != "phone" {
		t.Errorf("after DeleteDevice: %+v", all)
	}
	if err := s.DeleteDevice("alice", "laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting a missing device: %v", err)
	}
}

// The first release had no device column; its databases must migrate in place.
func TestMigrateFromVersion1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
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
		VALUES (1, 'nl', ?, ?, '10.77.0.2', '2026-09-17T00:00:00Z'); PRAGMA user_version = 1;`,
		key.String(), key.Public().String()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.Peer("alice", DefaultDevice, "nl")
	if err != nil || p.PrivateKey != key || p.IP.String() != "10.77.0.2" {
		t.Fatalf("migrated peer = %+v, %v", p, err)
	}
	if _, err := s.AddPeer("alice", "phone", "nl", subnet); err != nil {
		t.Errorf("new schema not in effect after migration: %v", err)
	}
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != len(migrations) {
		t.Errorf("user_version = %d, want %d", version, len(migrations))
	}
}

func TestSubnetFull(t *testing.T) {
	s := open(t)
	s.CreateUser("u")
	tiny := netip.MustParsePrefix("10.0.0.0/29") // .0 net, .1 gw, .2-.6 usable, .7 broadcast
	for i, profile := range []string{"a", "b", "c", "d", "e"} {
		p, err := s.AddPeer("u", DefaultDevice, profile, tiny)
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if p.IP.String() != "10.0.0."+string(rune('2'+i)) {
			t.Errorf("peer %d got %s", i, p.IP)
		}
	}
	if _, err := s.AddPeer("u", DefaultDevice, "f", tiny); !errors.Is(err, ErrSubnetFull) {
		t.Errorf("expected ErrSubnetFull, got %v", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lotsman.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateUser("alice")
	p, _ := s.AddPeer("alice", DefaultDevice, "nl", subnet)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Peer("alice", DefaultDevice, "nl")
	if err != nil || got.PrivateKey != p.PrivateKey {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
}
