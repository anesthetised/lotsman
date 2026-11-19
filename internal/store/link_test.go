package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinks(t *testing.T) {
	s := open(t)
	if _, err := s.IssueLink("alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("link for a missing user: %v", err)
	}
	s.CreateUser("alice")
	s.CreateUser("bob")

	first, err := s.IssueLink("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidToken(first) {
		t.Errorf("token %q is not of the issued form", first)
	}
	if user, err := s.ResolveLink(first); err != nil || user != "alice" {
		t.Errorf("resolve = %q, %v", user, err)
	}
	if u, err := s.User("alice"); err != nil || !u.HasLink {
		t.Errorf("alice after issue: %+v, %v", u, err)
	}
	if u, err := s.User("bob"); err != nil || u.HasLink {
		t.Errorf("bob without a link: %+v, %v", u, err)
	}

	// Rotation: the old token dies with the new one's birth.
	second, err := s.IssueLink("alice")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("rotation returned the same token")
	}
	if _, err := s.ResolveLink(first); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotated token still resolves: %v", err)
	}
	if user, err := s.ResolveLink(second); err != nil || user != "alice" {
		t.Errorf("new token: %q, %v", user, err)
	}

	bobs, err := s.IssueLink("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeLink("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveLink(second); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still resolves: %v", err)
	}
	if u, _ := s.User("alice"); u.HasLink {
		t.Error("alice still has a link after revoke")
	}
	if err := s.RevokeLink("alice"); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if err := s.RevokeLink("carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke for a missing user: %v", err)
	}
	if user, err := s.ResolveLink(bobs); err != nil || user != "bob" {
		t.Errorf("another user's revoke touched bob's link: %q, %v", user, err)
	}

	// A removed user's link dies with them, and the name reused later
	// does not inherit it.
	if err := s.DeleteUser("bob"); err != nil {
		t.Fatal(err)
	}
	s.CreateUser("bob")
	if _, err := s.ResolveLink(bobs); !errors.Is(err, ErrNotFound) {
		t.Errorf("a removed user's token resolves: %v", err)
	}

	for _, bad := range []string{"", "x", strings.Repeat("A", TokenLen-1), strings.Repeat("A", TokenLen+1),
		strings.Repeat("+", TokenLen), strings.Repeat("A", TokenLen-1) + "="} {
		if _, err := s.ResolveLink(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("resolve %q: %v", bad, err)
		}
	}
}

// Only a token's hash is stored: neither the database nor its WAL holds
// the token, in any encoding a reader of the files might try.
func TestLinkTokenIsNotStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lotsman.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.CreateUser("alice")
	token, err := s.IssueLink("alice")
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := s.db.QueryRow(`SELECT token_hash FROM user_links`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, hashToken(token)) || len(stored) != 32 {
		t.Errorf("stored %x, want the token's SHA-256", stored)
	}
	for _, f := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(token)) {
			t.Errorf("%s holds the token", filepath.Base(f))
		}
	}
}
