// Package store persists users and their peers in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/anesthetised/lotsman/internal/awgconf"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrSubnetFull = errors.New("no free address in subnet")
)

// DefaultDevice is used when a user does not name their device.
const DefaultDevice = "default"

// migrations run in order; PRAGMA user_version records how many have been applied.
var migrations = []string{
	`CREATE TABLE users (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	);
	CREATE TABLE peers (
		id          INTEGER PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		profile     TEXT NOT NULL,
		private_key TEXT NOT NULL,
		public_key  TEXT NOT NULL UNIQUE,
		ip          TEXT NOT NULL UNIQUE,
		created_at  TEXT NOT NULL,
		UNIQUE (user_id, profile)
	);`,
	// A user may have several devices; WireGuard needs one key per device.
	`CREATE TABLE peers_new (
		id          INTEGER PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device      TEXT NOT NULL,
		profile     TEXT NOT NULL,
		private_key TEXT NOT NULL,
		public_key  TEXT NOT NULL UNIQUE,
		ip          TEXT NOT NULL UNIQUE,
		created_at  TEXT NOT NULL,
		UNIQUE (user_id, device, profile)
	);
	INSERT INTO peers_new (id, user_id, device, profile, private_key, public_key, ip, created_at)
		SELECT id, user_id, '` + DefaultDevice + `', profile, private_key, public_key, ip, created_at FROM peers;
	DROP TABLE peers;
	ALTER TABLE peers_new RENAME TO peers;`,
}

type User struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Peer is one (user, device, profile) tunnel. The private key is kept so the
// client config can be shown again later; anyone with read access to the
// database can impersonate the peer, so the database must be protected like
// the server key.
type Peer struct {
	ID         int64
	User       string
	Device     string
	Profile    string
	PrivateKey awgconf.Key
	PublicKey  awgconf.Key
	IP         netip.Addr
	CreatedAt  time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := migrate(db, path); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate brings the schema up to date. When there is anything to apply it
// first snapshots the database next to itself, so a bad upgrade can be undone
// by stopping the daemon and moving the backup back.
func migrate(db *sql.DB, path string) error {
	version, err := schemaVersion(db)
	if err != nil {
		return err
	}
	if version >= len(migrations) {
		return nil
	}
	if version > 0 {
		backup := fmt.Sprintf("%s.v%d-%s.bak", path, version, time.Now().UTC().Format("20060102-150405"))
		if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
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

// schemaVersion reads PRAGMA user_version. The first release created the
// schema without recording one, so a version-0 database with tables is version 1.
func schemaVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, err
	}
	if version != 0 {
		return version, nil
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'users'`).Scan(&tables); err != nil {
		return 0, err
	}
	if tables == 1 {
		return 1, nil
	}
	return 0, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateUser(name string) (User, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`INSERT INTO users (name, created_at) VALUES (?, ?)`, name, now.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return User{}, ErrExists
		}
		return User{}, err
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Name: name, CreatedAt: now}, nil
}

func (s *Store) DeleteUser(name string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name, created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var created string
		if err := rows.Scan(&u.ID, &u.Name, &created); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = time.Parse(time.RFC3339, created)
		users = append(users, u)
	}
	return users, rows.Err()
}

// AddPeer generates a key pair and allocates the next free address in subnet.
// The network address and the first host (the gateway) are never handed out.
func (s *Store) AddPeer(user, device, profile string, subnet netip.Prefix) (Peer, error) {
	priv, err := awgconf.GeneratePrivateKey()
	if err != nil {
		return Peer{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Peer{}, err
	}
	defer tx.Rollback()

	var userID int64
	if err := tx.QueryRow(`SELECT id FROM users WHERE name = ?`, user).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Peer{}, ErrNotFound
		}
		return Peer{}, err
	}
	used, err := usedAddresses(tx)
	if err != nil {
		return Peer{}, err
	}
	ip, ok := firstFree(subnet, used)
	if !ok {
		return Peer{}, ErrSubnetFull
	}
	now := time.Now().UTC()
	res, err := tx.Exec(`INSERT INTO peers (user_id, device, profile, private_key, public_key, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, device, profile, priv.String(), priv.Public().String(), ip.String(), now.Format(time.RFC3339))
	if err != nil {
		if isUnique(err) {
			return Peer{}, ErrExists
		}
		return Peer{}, err
	}
	if err := tx.Commit(); err != nil {
		return Peer{}, err
	}
	id, _ := res.LastInsertId()
	return Peer{ID: id, User: user, Device: device, Profile: profile, PrivateKey: priv, PublicKey: priv.Public(), IP: ip, CreatedAt: now}, nil
}

func (s *Store) DeletePeer(user, device, profile string) error {
	return s.deletePeers(`profile = ? AND device = ? AND user_id = (SELECT id FROM users WHERE name = ?)`, profile, device, user)
}

// DeleteDevice removes every profile of one of the user's devices.
func (s *Store) DeleteDevice(user, device string) error {
	return s.deletePeers(`device = ? AND user_id = (SELECT id FROM users WHERE name = ?)`, device, user)
}

func (s *Store) deletePeers(where string, args ...any) error {
	res, err := s.db.Exec(`DELETE FROM peers WHERE `+where, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Peer(user, device, profile string) (Peer, error) {
	peers, err := s.queryPeers(`WHERE u.name = ? AND p.device = ? AND p.profile = ?`, user, device, profile)
	if err != nil {
		return Peer{}, err
	}
	if len(peers) == 0 {
		return Peer{}, ErrNotFound
	}
	return peers[0], nil
}

func (s *Store) ListPeers() ([]Peer, error) {
	return s.queryPeers("")
}

func (s *Store) queryPeers(where string, args ...any) ([]Peer, error) {
	rows, err := s.db.Query(`SELECT p.id, u.name, p.device, p.profile, p.private_key, p.public_key, p.ip, p.created_at
		FROM peers p JOIN users u ON u.id = p.user_id `+where+` ORDER BY u.name, p.device, p.profile`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var p Peer
		var priv, pub, ip, created string
		if err := rows.Scan(&p.ID, &p.User, &p.Device, &p.Profile, &priv, &pub, &ip, &created); err != nil {
			return nil, err
		}
		if p.PrivateKey, err = awgconf.ParseKey(priv); err != nil {
			return nil, fmt.Errorf("peer %d: %w", p.ID, err)
		}
		if p.PublicKey, err = awgconf.ParseKey(pub); err != nil {
			return nil, fmt.Errorf("peer %d: %w", p.ID, err)
		}
		if p.IP, err = netip.ParseAddr(ip); err != nil {
			return nil, fmt.Errorf("peer %d: %w", p.ID, err)
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

func usedAddresses(tx *sql.Tx) (map[netip.Addr]bool, error) {
	rows, err := tx.Query(`SELECT ip FROM peers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[netip.Addr]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		if a, err := netip.ParseAddr(s); err == nil {
			used[a] = true
		}
	}
	return used, rows.Err()
}

func firstFree(subnet netip.Prefix, used map[netip.Addr]bool) (netip.Addr, bool) {
	a := subnet.Addr().Next().Next() // skip network address and gateway
	for ; subnet.Contains(a); a = a.Next() {
		if !used[a] && !isBroadcast(subnet, a) {
			return a, true
		}
	}
	return netip.Addr{}, false
}

func isBroadcast(subnet netip.Prefix, a netip.Addr) bool {
	return !subnet.Contains(a.Next())
}

func isUnique(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
