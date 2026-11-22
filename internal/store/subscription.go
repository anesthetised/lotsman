package store

import (
	"bytes"
	"database/sql"
	"time"
)

type OutlineUsage struct {
	KeyHash    []byte
	ObservedAt time.Time
	Exhausted  bool
	ExpiresAt  time.Time
}

// SetUpstreamExpiry records the expiry in UTC. A zero time clears it.
func (s *Store) SetUpstreamExpiry(upstream string, expiry time.Time) error {
	if expiry.IsZero() {
		_, err := s.db.Exec(`DELETE FROM upstream_subscriptions WHERE upstream = ?`, upstream)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO upstream_subscriptions (upstream, expires_at) VALUES (?, ?)
		ON CONFLICT (upstream) DO UPDATE SET expires_at = excluded.expires_at`,
		upstream, expiry.UTC().Format(time.RFC3339Nano))
	return err
}

// UpstreamExpiries returns the manually recorded expiry by upstream name.
func (s *Store) UpstreamExpiries() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT upstream, expires_at FROM upstream_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	expiries := make(map[string]time.Time)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		expiry, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, err
		}
		expiries[name] = expiry
	}
	return expiries, rows.Err()
}

// SaveOutlineUsage keeps the last confirmed reading for mapped keys. Changing
// or removing a key discards its old reading, even when the API is down.
func (s *Store) SaveOutlineUsage(readings map[string]OutlineUsage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT upstream, key_hash FROM outline_usage`)
	if err != nil {
		return err
	}
	var remove []string
	for rows.Next() {
		var name string
		var hash []byte
		if err := rows.Scan(&name, &hash); err != nil {
			rows.Close()
			return err
		}
		if !bytes.Equal(hash, readings[name].KeyHash) {
			remove = append(remove, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range remove {
		if _, err := tx.Exec(`DELETE FROM outline_usage WHERE upstream = ?`, name); err != nil {
			return err
		}
	}
	for name, reading := range readings {
		if reading.ObservedAt.IsZero() {
			continue
		}
		var expiry sql.NullString
		if !reading.ExpiresAt.IsZero() {
			expiry = sql.NullString{String: reading.ExpiresAt.UTC().Format(time.RFC3339Nano), Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO outline_usage (upstream, key_hash, observed_at, exhausted, expires_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (upstream) DO UPDATE SET observed_at = excluded.observed_at, exhausted = excluded.exhausted,
				expires_at = COALESCE(excluded.expires_at, outline_usage.expires_at)
			WHERE excluded.observed_at >= outline_usage.observed_at`,
			name, reading.KeyHash, reading.ObservedAt.UnixMicro(), reading.Exhausted, expiry); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) OutlineUsage() (map[string]OutlineUsage, error) {
	rows, err := s.db.Query(`SELECT upstream, exhausted, expires_at FROM outline_usage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	usage := make(map[string]OutlineUsage)
	for rows.Next() {
		var name string
		var reading OutlineUsage
		var expiry sql.NullString
		if err := rows.Scan(&name, &reading.Exhausted, &expiry); err != nil {
			return nil, err
		}
		if expiry.Valid {
			reading.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiry.String)
			if err != nil {
				return nil, err
			}
		}
		usage[name] = reading
	}
	return usage, rows.Err()
}
