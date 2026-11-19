package web

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // the "sqlite" driver
)

// migrations build web.db, the panel's own state; PRAGMA user_version
// records how many have run. Secrets (session IDs, enrollment tokens) are
// stored only as SHA-256 hashes; times are Unix seconds.
var migrations = []string{
	// passkeys: each is an operator of its own, with its own WebAuthn user
	// handle. credential is the library's webauthn.Credential as JSON.
	`CREATE TABLE passkeys (
		id            INTEGER PRIMARY KEY,
		credential_id BLOB NOT NULL UNIQUE,
		user_handle   BLOB NOT NULL,
		credential    TEXT NOT NULL,
		name          TEXT NOT NULL,
		created_at    INTEGER NOT NULL,
		used_at       INTEGER
	);
	CREATE TABLE sessions (
		id_hash    BLOB PRIMARY KEY,
		passkey_id INTEGER NOT NULL REFERENCES passkeys(id) ON DELETE CASCADE,
		created_at INTEGER NOT NULL,
		seen_at    INTEGER NOT NULL
	);
	CREATE TABLE enrollments (
		token_hash BLOB PRIMARY KEY,
		expires_at INTEGER NOT NULL,
		used_at    INTEGER
	);`,
}

// OpenDB opens web.db at path, creating it 0600 if needed, and brings its
// schema up to date.
func OpenDB(path string) (*sql.DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this binary's %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
