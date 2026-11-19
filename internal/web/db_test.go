package web

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpenDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	if want := []string{"enrollments", "passkeys", "sessions"}; !slices.Equal(tables, want) {
		t.Errorf("tables = %v, want %v", tables, want)
	}
	db.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("web.db mode %v, %v; want 0600", fi.Mode(), err)
	}

	// Opening again applies nothing twice.
	db, err = OpenDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	db.Close()
}

// A database from a newer panel is left alone rather than misread.
func TestOpenDBRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`PRAGMA user_version = 99`)
	db.Close()
	if _, err := OpenDB(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("open: %v, want a newer-schema error", err)
	}
}
