package store

import (
	"database/sql"
	"errors"

	"github.com/anesthetised/lotsman/internal/config"
)

// The store keeps profiles as given; callers validate them against the
// upstreams first (config.ValidateProfile), as only they know the upstreams.

// ListProfiles returns every profile by name, each with its tiers in order.
func (s *Store) ListProfiles() ([]config.Profile, error) {
	rows, err := s.db.Query(`SELECT p.name, t.geo, t.provider, t.tag
		FROM profiles p LEFT JOIN profile_tiers t ON t.profile = p.name
		ORDER BY p.name, t.position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var profiles []config.Profile
	for rows.Next() {
		var name string
		var geo, provider, tag sql.NullString // NULL for a profile without tiers
		if err := rows.Scan(&name, &geo, &provider, &tag); err != nil {
			return nil, err
		}
		if len(profiles) == 0 || profiles[len(profiles)-1].Name != name {
			profiles = append(profiles, config.Profile{Name: name})
		}
		if geo.Valid {
			p := &profiles[len(profiles)-1]
			p.Prefer = append(p.Prefer, config.Match{Geo: geo.String, Provider: provider.String, Tag: tag.String})
		}
	}
	return profiles, rows.Err()
}

// AddProfile stores a new profile; ErrExists if the name is taken.
func (s *Store) AddProfile(p config.Profile) error { return s.putProfile(p, true, false) }

// SetProfile replaces the tiers of an existing profile; ErrNotFound if there is none.
func (s *Store) SetProfile(p config.Profile) error { return s.putProfile(p, false, true) }

// PutProfile creates p or replaces the profile of that name.
func (s *Store) PutProfile(p config.Profile) error { return s.putProfile(p, true, true) }

func (s *Store) putProfile(p config.Profile, create, replace bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO profiles (name) VALUES (?) ON CONFLICT (name) DO NOTHING`, p.Name)
	if err != nil {
		return err
	}
	created, _ := res.RowsAffected()
	switch {
	case created == 1 && !create:
		return ErrNotFound
	case created == 0 && !replace:
		return ErrExists
	}
	if _, err := tx.Exec(`DELETE FROM profile_tiers WHERE profile = ?`, p.Name); err != nil {
		return err
	}
	for i, m := range p.Prefer {
		if _, err := tx.Exec(`INSERT INTO profile_tiers (profile, position, geo, provider, tag) VALUES (?, ?, ?, ?, ?)`,
			p.Name, i, m.Geo, m.Provider, m.Tag); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteProfile removes a profile no peer uses; ErrInUse while one does.
func (s *Store) DeleteProfile(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM profiles WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	var used int
	if err := tx.QueryRow(`SELECT count(*) FROM peers WHERE profile = ?`, name).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return ErrInUse
	}
	return tx.Commit()
}

func profileExists(tx *sql.Tx, name string) error {
	var found string
	err := tx.QueryRow(`SELECT name FROM profiles WHERE name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoProfile
	}
	return err
}
