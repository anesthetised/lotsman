package store

import (
	"database/sql"
	"time"
)

// An incident is one stretch of one kind of trouble with one upstream: it
// opens when the trouble starts and closes when it ends, and at most one of
// each kind is open per upstream. Times are unix seconds; Compact expires
// closed incidents with the hour samples.

// IncidentKind is what went wrong.
type IncidentKind string

const (
	IncidentDown       IncidentKind = "down"        // the reachability probes fail
	IncidentDownload   IncidentKind = "download"    // the download probe fails: degraded
	IncidentUpload     IncidentKind = "upload"      // the upload probe fails: degraded
	IncidentSendErrors IncidentKind = "send_errors" // the AWG device logs errors above the threshold
)

// IncidentKinds lists every kind, in the order they are shown.
var IncidentKinds = []IncidentKind{IncidentDown, IncidentDownload, IncidentUpload, IncidentSendErrors}

type Incident struct {
	ID       int64
	Upstream string
	Kind     IncidentKind
	Started  time.Time
	Ended    time.Time // zero while open
	Detail   string    // the latest error
	Devices  int       // devices routed through the upstream when it started
}

// Open reports whether the incident is still going on.
func (i Incident) Open() bool { return i.Ended.IsZero() }

// OpenIncident records the start of an incident and returns its id. It
// fails if one of the same kind is open for the upstream.
func (s *Store) OpenIncident(upstream string, kind IncidentKind, started time.Time, detail string, devices int) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO incidents (upstream, kind, started, detail, devices) VALUES (?, ?, ?, ?, ?)`,
		upstream, string(kind), started.Unix(), detail, devices)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetIncidentDetail replaces an open incident's detail with a later error.
func (s *Store) SetIncidentDetail(id int64, detail string) error {
	_, err := s.db.Exec(`UPDATE incidents SET detail = ? WHERE id = ? AND ended IS NULL`, detail, id)
	return err
}

// CloseIncident ends an open incident.
func (s *Store) CloseIncident(id int64, ended time.Time) error {
	_, err := s.db.Exec(`UPDATE incidents SET ended = ? WHERE id = ? AND ended IS NULL`, ended.Unix(), id)
	return err
}

// CloseOpenIncidents ends every open incident at ended and appends note to
// its detail. The daemon calls it when it starts: what it left open, it can
// no longer vouch for.
func (s *Store) CloseOpenIncidents(ended time.Time, note string) error {
	_, err := s.db.Exec(`UPDATE incidents SET ended = ?1, detail = CASE WHEN detail = '' THEN ?2 ELSE detail || '; ' || ?2 END
		WHERE ended IS NULL`, ended.Unix(), note)
	return err
}

// Incidents returns up to limit incidents of the upstream, or of every
// upstream when it is "": the open ones first, then the newest.
func (s *Store) Incidents(upstream string, limit int) ([]Incident, error) {
	rows, err := s.db.Query(`SELECT id, upstream, kind, started, ended, detail, devices FROM incidents
		WHERE ?1 = '' OR upstream = ?1
		ORDER BY ended IS NOT NULL, started DESC, id DESC LIMIT ?2`, upstream, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var in Incident
		var kind string
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&in.ID, &in.Upstream, &kind, &started, &ended, &in.Detail, &in.Devices); err != nil {
			return nil, err
		}
		in.Kind, in.Started = IncidentKind(kind), time.Unix(started, 0).UTC()
		if ended.Valid {
			in.Ended = time.Unix(ended.Int64, 0).UTC()
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
