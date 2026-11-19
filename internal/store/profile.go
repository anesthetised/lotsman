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
	return listProfiles(s.db)
}

func listProfiles(q querier) ([]config.Profile, error) {
	rows, err := q.Query(`SELECT p.name, t.geo, t.provider, t.tag
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
	if created == 1 {
		if err := nameFree(tx, p.Name, p.Name); err != nil {
			return err
		}
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

// RenameProfile gives profile from the name to, and its peers with it, in
// one transaction. The peers keep their keys and addresses, so their clients
// notice nothing, and their history is kept by peer id. ErrNotFound without
// the profile; ErrExists when to belongs to another profile.
func (s *Store) RenameProfile(from, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := profileExists(tx, from); errors.Is(err, ErrNoProfile) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	if err := nameFree(tx, to, from); err != nil {
		return err
	}
	for _, q := range []string{
		`INSERT INTO profiles (name) VALUES (?2)`,
		`UPDATE profile_tiers SET profile = ?2 WHERE profile = ?1`,
		`UPDATE peers SET profile = ?2 WHERE profile = ?1`,
		`DELETE FROM profiles WHERE name = ?1`,
	} {
		if _, err := tx.Exec(q, from, to); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// nameFree is ErrExists when a profile other than except, or a peer of
// another profile, already uses name, ignoring case: Albania and albania
// would be told apart only by a typo. Peers count because older databases
// may hold peers whose profile was never imported.
func nameFree(tx *sql.Tx, name, except string) error {
	var taken int
	err := tx.QueryRow(`SELECT (SELECT count(*) FROM profiles WHERE name = ?1 COLLATE NOCASE AND name != ?2)
		+ (SELECT count(*) FROM peers WHERE profile = ?1 COLLATE NOCASE AND profile != ?2)`, name, except).Scan(&taken)
	if err != nil {
		return err
	}
	if taken > 0 {
		return ErrExists
	}
	return nil
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
