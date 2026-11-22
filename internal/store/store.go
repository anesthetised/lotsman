// Package store persists users, their rights and their devices' peers in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/anesthetised/lotsman/internal/awgconf"
	"github.com/anesthetised/lotsman/internal/policy"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrSubnetFull = errors.New("no free address in subnet")
)

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
		SELECT id, user_id, 'default', profile, private_key, public_key, ip, created_at FROM peers;
	DROP TABLE peers;
	ALTER TABLE peers_new RENAME TO peers;`,
	// Profiles moved here from lotsman.yaml, so they can change at runtime.
	// Peers still name their profile; the store checks the reference, as
	// earlier databases hold peers whose profiles are imported only later.
	`CREATE TABLE profiles (
		name TEXT PRIMARY KEY
	);
	CREATE TABLE profile_tiers (
		profile  TEXT NOT NULL REFERENCES profiles(name) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		geo      TEXT NOT NULL,
		provider TEXT NOT NULL,
		tag      TEXT NOT NULL,
		PRIMARY KEY (profile, position)
	);`,
	// A peer's id is its API identifier (peer_id) and keys its history, so
	// a deleted peer's id must never go to a new one: AUTOINCREMENT.
	`CREATE TABLE peers_new (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device      TEXT NOT NULL,
		profile     TEXT NOT NULL,
		private_key TEXT NOT NULL,
		public_key  TEXT NOT NULL UNIQUE,
		ip          TEXT NOT NULL UNIQUE,
		created_at  TEXT NOT NULL,
		UNIQUE (user_id, device, profile)
	);
	INSERT INTO peers_new SELECT id, user_id, device, profile, private_key, public_key, ip, created_at FROM peers;
	DROP TABLE peers;
	ALTER TABLE peers_new RENAME TO peers;`,
	// Availability and traffic history, see history.go. One table per
	// resolution: the minute tables hold what is recent, a fold turns older
	// minutes into the hour tables, and each is clustered by series and time
	// (WITHOUT ROWID), so a range of one series is one contiguous scan and no
	// other index is kept. The peer tables name no peers(id): the peers
	// migrations above rebuild that table, and a removed peer's rows only
	// wait for their retention to pass.
	`CREATE TABLE upstream_samples (
		upstream   TEXT NOT NULL,
		ts         INTEGER NOT NULL,
		up_ratio   REAL NOT NULL,
		rtt_ms     REAL,
		down_bytes INTEGER NOT NULL,
		up_bytes   INTEGER NOT NULL,
		PRIMARY KEY (upstream, ts)
	) WITHOUT ROWID;
	CREATE TABLE upstream_hour_samples (
		upstream   TEXT NOT NULL,
		ts         INTEGER NOT NULL,
		up_ratio   REAL NOT NULL,
		rtt_ms     REAL,
		down_bytes INTEGER NOT NULL,
		up_bytes   INTEGER NOT NULL,
		PRIMARY KEY (upstream, ts)
	) WITHOUT ROWID;
	CREATE TABLE peer_samples (
		peer_id    INTEGER NOT NULL,
		ts         INTEGER NOT NULL,
		down_bytes INTEGER NOT NULL,
		up_bytes   INTEGER NOT NULL,
		PRIMARY KEY (peer_id, ts)
	) WITHOUT ROWID;
	CREATE TABLE peer_hour_samples (
		peer_id    INTEGER NOT NULL,
		ts         INTEGER NOT NULL,
		down_bytes INTEGER NOT NULL,
		up_bytes   INTEGER NOT NULL,
		PRIMARY KEY (peer_id, ts)
	) WITHOUT ROWID;`,
	// User links (#134): only a token's SHA-256 is kept. Rotating revokes
	// the old row and adds one, so a user has at most one live link.
	`CREATE TABLE user_links (
		id         INTEGER PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		token_hash BLOB NOT NULL UNIQUE,
		created_at TEXT NOT NULL,
		revoked_at TEXT
	);
	CREATE INDEX user_links_user ON user_links (user_id);
	CREATE UNIQUE INDEX user_links_live ON user_links (user_id) WHERE revoked_at IS NULL;`,
	// Per-device routes and user rights replace profiles (#161): a peer is
	// one device of a user, with the route as JSON, and users get their
	// rights. Nothing was in production, so the old peers and their history
	// are dropped rather than converted; users and their links stay. Ids go
	// on counting from the old table's, so a peer_id still in a browser or
	// a log never names a new device.
	`DROP TABLE profile_tiers;
	DROP TABLE profiles;
	DELETE FROM peer_samples;
	DELETE FROM peer_hour_samples;
	CREATE TABLE peers_new (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device      TEXT NOT NULL,
		route       TEXT NOT NULL,
		private_key TEXT NOT NULL,
		public_key  TEXT NOT NULL UNIQUE,
		ip          TEXT NOT NULL UNIQUE,
		created_at  TEXT NOT NULL,
		UNIQUE (user_id, device)
	);
	INSERT INTO sqlite_sequence (name, seq) SELECT 'peers_new', seq FROM sqlite_sequence WHERE name = 'peers';
	DROP TABLE peers;
	ALTER TABLE peers_new RENAME TO peers;
	ALTER TABLE users ADD COLUMN edit_routes INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE users ADD COLUMN show_providers INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE users ADD COLUMN exits TEXT NOT NULL DEFAULT '[]';`,
	// Speed tests and incidents (#167). A history row keeps the sum of the
	// rates its speed tests measured and their count, so rows add up when
	// they fold and a step averages over the tests, not over the rows. An
	// incident is one stretch of one kind of trouble with one upstream; the
	// partial index keeps one of a kind open per upstream.
	`ALTER TABLE upstream_samples ADD COLUMN speed_tests INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE upstream_samples ADD COLUMN down_rate_sum INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE upstream_samples ADD COLUMN up_rate_sum INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE upstream_hour_samples ADD COLUMN speed_tests INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE upstream_hour_samples ADD COLUMN down_rate_sum INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE upstream_hour_samples ADD COLUMN up_rate_sum INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE incidents (
		id       INTEGER PRIMARY KEY,
		upstream TEXT NOT NULL,
		kind     TEXT NOT NULL,
		started  INTEGER NOT NULL,
		ended    INTEGER,
		detail   TEXT NOT NULL,
		devices  INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX incidents_open ON incidents (upstream, kind) WHERE ended IS NULL;`,
	`CREATE TABLE upstream_subscriptions (
		upstream   TEXT NOT NULL PRIMARY KEY,
		expires_at TEXT NOT NULL
	) WITHOUT ROWID;`,
}

type User struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	HasLink   bool // a live link to the user's page; see IssueLink
	Rights    Rights
}

// Rights are what the operator lets a user do on their page, and which
// exits the user's devices may use.
type Rights struct {
	EditRoutes    bool     // false: only the operator sets the user's routes
	ShowProviders bool     // false: the user's page and API show countries only
	Exits         []string // upstream names the devices may use; empty: every upstream (policy.Allows)
}

// DefaultRights are a new user's: they edit their routes, see providers and
// may use every upstream.
func DefaultRights() Rights { return Rights{EditRoutes: true, ShowProviders: true} }

// Peer is one device of a user: one tunnel, with its route. The private key
// is kept so the client config can be shown again later; anyone with read
// access to the database can impersonate the peer, so the database must be
// protected like the server key.
type Peer struct {
	ID         int64
	User       string
	Device     string
	Route      policy.Route
	PrivateKey awgconf.Key
	PublicKey  awgconf.Key
	IP         netip.Addr
	CreatedAt  time.Time
}

type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it if needed. The database, its
// WAL/SHM sidecars and migration backups hold client private keys, so they are
// kept 0600. The directory is left alone: the caller owns its mode.
func Open(path string) (*Store, error) {
	if err := makePrivate(path); err != nil {
		return nil, err
	}
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

// makePrivate creates path as 0600 before SQLite writes anything (an empty file
// is a valid empty database; SQLite gives the -wal/-shm sidecars the mode of the
// main file) and tightens files left over from earlier versions, including
// migration backups made before they were created private.
func makePrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	old, err := filepath.Glob(glob(path) + ".v*.bak")
	if err != nil {
		return err
	}
	for _, p := range append([]string{path, path + "-wal", path + "-shm"}, old...) {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// glob escapes the glob metacharacters of a literal path.
func glob(path string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(path)
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
		// VACUUM INTO accepts an empty existing file, so the backup is born 0600.
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
		f.Close()
		if _, err := db.Exec(`VACUUM INTO ?`, backup); err != nil {
			os.Remove(backup)
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
	return User{ID: id, Name: name, CreatedAt: now, Rights: DefaultRights()}, nil
}

// SetRights replaces the user's rights; ErrNotFound if there is no such
// user. The caller validates the exits against the upstreams.
func (s *Store) SetRights(name string, r Rights) (User, error) {
	exits, err := json.Marshal(nonNil(r.Exits))
	if err != nil {
		return User{}, err
	}
	res, err := s.db.Exec(`UPDATE users SET edit_routes = ?, show_providers = ?, exits = ? WHERE name = ?`,
		r.EditRoutes, r.ShowProviders, string(exits), name)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	return s.User(name)
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
	return queryUsers(s.db, "ORDER BY u.name")
}

// UsersPage reads the selected users and only their peers, in one snapshot.
// An empty name includes every user. A limit of -1 includes every match.
func (s *Store) UsersPage(name string, limit, offset int) ([]User, []Peer, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	clause := "ORDER BY u.name LIMIT ? OFFSET ?"
	args := []any{limit, offset}
	if name != "" {
		clause = "WHERE u.name = ? " + clause
		args = []any{name, limit, offset}
	}
	users, err := queryUsers(tx, clause, args...)
	if err != nil || len(users) == 0 {
		return users, nil, err
	}
	peers, err := queryPeers(tx, "WHERE p.user_id IN (SELECT u.id FROM users u "+clause+")", args...)
	return users, peers, err
}

// User returns the user of that name; ErrNotFound if there is none.
func (s *Store) User(name string) (User, error) {
	users, err := queryUsers(s.db, `WHERE u.name = ?`, name)
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrNotFound
	}
	return users[0], nil
}

func queryUsers(q querier, clause string, args ...any) ([]User, error) {
	rows, err := q.Query(`SELECT u.id, u.name, u.created_at, u.edit_routes, u.show_providers, u.exits,
		EXISTS (SELECT 1 FROM user_links l WHERE l.user_id = u.id AND l.revoked_at IS NULL)
		FROM users u `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var created, exits string
		if err := rows.Scan(&u.ID, &u.Name, &created, &u.Rights.EditRoutes, &u.Rights.ShowProviders, &exits, &u.HasLink); err != nil {
			return nil, err
		}
		if u.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
			return nil, fmt.Errorf("user %d: %w", u.ID, err)
		}
		if err := json.Unmarshal([]byte(exits), &u.Rights.Exits); err != nil {
			return nil, fmt.Errorf("user %d: exits: %w", u.ID, err)
		}
		if len(u.Rights.Exits) == 0 {
			u.Rights.Exits = nil
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// AddPeer adds a device with its route: it generates a key pair and
// allocates the next free address in subnet. The network address and the
// first host (the gateway) are never handed out. The user must exist;
// ErrExists if they have a device of that name. The caller validates the
// route against the upstreams.
func (s *Store) AddPeer(user, device string, route policy.Route, subnet netip.Prefix) (Peer, error) {
	return s.addPeer(user, device, route, subnet, false)
}

// AddUserPeer is AddPeer for a user that may not exist yet: the user and the
// peer are created in one transaction, so a failed allocation leaves no user.
func (s *Store) AddUserPeer(user, device string, route policy.Route, subnet netip.Prefix) (Peer, error) {
	return s.addPeer(user, device, route, subnet, true)
}

func (s *Store) addPeer(user, device string, route policy.Route, subnet netip.Prefix, createUser bool) (Peer, error) {
	priv, err := awgconf.GeneratePrivateKey()
	if err != nil {
		return Peer{}, err
	}
	routeJSON, err := json.Marshal(route)
	if err != nil {
		return Peer{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Peer{}, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if createUser {
		if _, err := tx.Exec(`INSERT INTO users (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`, user, now.Format(time.RFC3339)); err != nil {
			return Peer{}, err
		}
	}
	userID, err := userID(tx, user)
	if err != nil {
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
	res, err := tx.Exec(`INSERT INTO peers (user_id, device, route, private_key, public_key, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, device, string(routeJSON), priv.String(), priv.Public().String(), ip.String(), now.Format(time.RFC3339))
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
	return Peer{ID: id, User: user, Device: device, Route: route, PrivateKey: priv, PublicKey: priv.Public(), IP: ip, CreatedAt: now}, nil
}

// SetRoute replaces the route of one of the user's devices; ErrNotFound if
// there is no such device. The key and the address stay, so the client
// config does too.
func (s *Store) SetRoute(user, device string, route policy.Route) (Peer, error) {
	routeJSON, err := json.Marshal(route)
	if err != nil {
		return Peer{}, err
	}
	res, err := s.db.Exec(`UPDATE peers SET route = ? WHERE device = ? AND user_id = (SELECT id FROM users WHERE name = ?)`,
		string(routeJSON), device, user)
	if err != nil {
		return Peer{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Peer{}, ErrNotFound
	}
	return s.Peer(user, device)
}

// DeletePeer removes one of the user's devices.
func (s *Store) DeletePeer(user, device string) error {
	res, err := s.db.Exec(`DELETE FROM peers WHERE device = ? AND user_id = (SELECT id FROM users WHERE name = ?)`, device, user)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Peer returns the user's device of that name; ErrNotFound if there is none.
func (s *Store) Peer(user, device string) (Peer, error) {
	peers, err := s.queryPeers(`WHERE u.name = ? AND p.device = ?`, user, device)
	if err != nil {
		return Peer{}, err
	}
	if len(peers) == 0 {
		return Peer{}, ErrNotFound
	}
	return peers[0], nil
}

// PeerByID returns the peer with that row id; ErrNotFound if there is none.
func (s *Store) PeerByID(id int64) (Peer, error) {
	peers, err := s.queryPeers(`WHERE p.id = ?`, id)
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

// PeersAndUsers reads every peer and every user in one transaction, so each
// peer's user is in the list, with the rights that were in effect.
func (s *Store) PeersAndUsers() ([]Peer, []User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	peers, err := queryPeers(tx, "")
	if err != nil {
		return nil, nil, err
	}
	users, err := queryUsers(tx, "ORDER BY u.name")
	if err != nil {
		return nil, nil, err
	}
	return peers, users, nil
}

// UserPeers returns every peer of one user, by device.
func (s *Store) UserPeers(user string) ([]Peer, error) {
	return s.queryPeers(`WHERE u.name = ?`, user)
}

func (s *Store) queryPeers(where string, args ...any) ([]Peer, error) {
	return queryPeers(s.db, where, args...)
}

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryPeers(q querier, where string, args ...any) ([]Peer, error) {
	rows, err := q.Query(`SELECT p.id, u.name, p.device, p.route, p.private_key, p.public_key, p.ip, p.created_at
		FROM peers p JOIN users u ON u.id = p.user_id `+where+` ORDER BY u.name, p.device`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Peer
	for rows.Next() {
		var p Peer
		var route, priv, pub, ip, created string
		if err := rows.Scan(&p.ID, &p.User, &p.Device, &route, &priv, &pub, &ip, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(route), &p.Route); err != nil {
			return nil, fmt.Errorf("peer %d: route: %w", p.ID, err)
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
		if p.CreatedAt, err = time.Parse(time.RFC3339, created); err != nil {
			return nil, fmt.Errorf("peer %d: %w", p.ID, err)
		}
		peers = append(peers, p)
	}
	return peers, rows.Err()
}

// nonNil keeps an empty list a JSON array rather than null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
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
