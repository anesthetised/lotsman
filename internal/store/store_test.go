package store

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
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
	if _, err := s.AddPeer("alice", "nl", subnet); !errors.Is(err, ErrNotFound) {
		t.Errorf("peer for missing user: %v", err)
	}
	s.CreateUser("alice")

	p1, err := s.AddPeer("alice", "nl", subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p1.IP != netip.MustParseAddr("10.77.0.2") {
		t.Errorf("first address = %s, want 10.77.0.2 (network and gateway skipped)", p1.IP)
	}
	if p1.PublicKey != p1.PrivateKey.Public() || p1.PrivateKey.IsZero() {
		t.Error("key pair inconsistent")
	}
	if _, err := s.AddPeer("alice", "nl", subnet); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate profile: %v", err)
	}
	p2, err := s.AddPeer("alice", "de", subnet)
	if err != nil {
		t.Fatal(err)
	}
	if p2.IP != netip.MustParseAddr("10.77.0.3") {
		t.Errorf("second address = %s", p2.IP)
	}

	got, err := s.Peer("alice", "nl")
	if err != nil || got.PrivateKey != p1.PrivateKey || got.IP != p1.IP || got.User != "alice" {
		t.Errorf("Peer = %+v, %v", got, err)
	}
	all, err := s.ListPeers()
	if err != nil || len(all) != 2 || all[0].Profile != "de" || all[1].Profile != "nl" {
		t.Fatalf("ListPeers = %+v, %v", all, err)
	}

	if err := s.DeletePeer("alice", "nl"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Peer("alice", "nl"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted peer still found: %v", err)
	}
	p3, _ := s.AddPeer("alice", "nl", subnet)
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

func TestSubnetFull(t *testing.T) {
	s := open(t)
	s.CreateUser("u")
	tiny := netip.MustParsePrefix("10.0.0.0/29") // .0 net, .1 gw, .2-.6 usable, .7 broadcast
	for i, profile := range []string{"a", "b", "c", "d", "e"} {
		p, err := s.AddPeer("u", profile, tiny)
		if err != nil {
			t.Fatalf("peer %d: %v", i, err)
		}
		if p.IP.String() != "10.0.0."+string(rune('2'+i)) {
			t.Errorf("peer %d got %s", i, p.IP)
		}
	}
	if _, err := s.AddPeer("u", "f", tiny); !errors.Is(err, ErrSubnetFull) {
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
	p, _ := s.AddPeer("alice", "nl", subnet)
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Peer("alice", "nl")
	if err != nil || got.PrivateKey != p.PrivateKey {
		t.Errorf("after reopen: %+v, %v", got, err)
	}
}
