package store

import "time"

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
