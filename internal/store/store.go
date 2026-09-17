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

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS peers (
	id          INTEGER PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	profile     TEXT NOT NULL,
	private_key TEXT NOT NULL,
	public_key  TEXT NOT NULL UNIQUE,
	ip          TEXT NOT NULL UNIQUE,
	created_at  TEXT NOT NULL,
	UNIQUE (user_id, profile)
);`

type User struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// Peer is one (user, profile) tunnel. The private key is kept so the client
// config can be shown again later; anyone with read access to the database can
// impersonate the peer, so the database must be protected like the server key.
type Peer struct {
	ID         int64
	User       string
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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
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
func (s *Store) AddPeer(user, profile string, subnet netip.Prefix) (Peer, error) {
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
	res, err := tx.Exec(`INSERT INTO peers (user_id, profile, private_key, public_key, ip, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, profile, priv.String(), priv.Public().String(), ip.String(), now.Format(time.RFC3339))
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
	return Peer{ID: id, User: user, Profile: profile, PrivateKey: priv, PublicKey: priv.Public(), IP: ip, CreatedAt: now}, nil
}

func (s *Store) DeletePeer(user, profile string) error {
	res, err := s.db.Exec(`DELETE FROM peers WHERE profile = ? AND user_id = (SELECT id FROM users WHERE name = ?)`, profile, user)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Peer(user, profile string) (Peer, error) {
	peers, err := s.queryPeers(`WHERE u.name = ? AND p.profile = ?`, user, profile)
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
	rows, err := s.db.Query(`SELECT p.id, u.name, p.profile, p.private_key, p.public_key, p.ip, p.created_at
		FROM peers p JOIN users u ON u.id = p.user_id `+where+` ORDER BY u.name, p.profile`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var p Peer
		var priv, pub, ip, created string
		if err := rows.Scan(&p.ID, &p.User, &p.Profile, &priv, &pub, &ip, &created); err != nil {
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
