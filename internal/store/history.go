package store

import (
	"database/sql"
	"time"
)

// Availability and traffic history. A sample is one minute of a series, or,
// once the minute is old enough, one hour of it; ts is the unix second the
// period starts at, a multiple of 60 or 3600. Bytes are what the series
// carried in the period, not a counter, and down is towards the client. An
// upstream's speed tests are kept as their count and the sums of their
// rates, so periods add up and a query averages over tests.

// hourSeconds is the length of a downsampled period, and the alignment of
// its ts.
const hourSeconds = 3600

// UpstreamSample is an upstream's minute.
type UpstreamSample struct {
	Name     string
	UpRatio  float64       // share of the minute it was up, 0..1
	RTT      time.Duration // mean probe time while up; 0 when it never was
	Down, Up uint64
	// The speed tests that finished in the minute, and the sums of the
	// rates they measured, bytes per second.
	SpeedTests             int
	DownRateSum, UpRateSum uint64
}

// PeerSample is a peer's minute, keyed by its row id.
type PeerSample struct {
	PeerID   int64
	Down, Up uint64
}

// Sample is one step of a query. UpRatio, Latency and the rates are for
// upstreams only. Latency is 0 when the upstream was never up in the step,
// the rates when no speed test ran in it.
type Sample struct {
	Time             time.Time
	UpRatio          float64
	Latency          time.Duration
	Down, Up         uint64
	DownRate, UpRate uint64 // the step's speed tests, averaged, bytes per second
}

// AddSamples writes one minute of every series in one transaction. A minute
// that is written twice, such as after the clock stepped back, keeps the
// bytes and speed tests of both and the later ratio and latency.
func (s *Store) AddSamples(ts time.Time, ups []UpstreamSample, peers []PeerSample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := ts.Unix()
	for _, u := range ups {
		var rtt sql.NullFloat64
		if u.RTT > 0 {
			rtt = sql.NullFloat64{Float64: float64(u.RTT) / float64(time.Millisecond), Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO upstream_samples (upstream, ts, up_ratio, rtt_ms, down_bytes, up_bytes, speed_tests, down_rate_sum, up_rate_sum)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (upstream, ts) DO UPDATE SET up_ratio = excluded.up_ratio, rtt_ms = excluded.rtt_ms,
				down_bytes = down_bytes + excluded.down_bytes, up_bytes = up_bytes + excluded.up_bytes,
				speed_tests = speed_tests + excluded.speed_tests, down_rate_sum = down_rate_sum + excluded.down_rate_sum,
				up_rate_sum = up_rate_sum + excluded.up_rate_sum`,
			u.Name, at, u.UpRatio, rtt, int64(u.Down), int64(u.Up), u.SpeedTests, int64(u.DownRateSum), int64(u.UpRateSum)); err != nil {
			return err
		}
	}
	for _, p := range peers {
		if _, err := tx.Exec(`INSERT INTO peer_samples (peer_id, ts, down_bytes, up_bytes) VALUES (?, ?, ?, ?)
			ON CONFLICT (peer_id, ts) DO UPDATE SET down_bytes = down_bytes + excluded.down_bytes, up_bytes = up_bytes + excluded.up_bytes`,
			p.PeerID, at, int64(p.Down), int64(p.Up)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Compact downsamples and expires history in one transaction, so a crash
// leaves either the old rows or the new ones, and running it again changes
// nothing. Minute rows of hours that ended before now − minuteKeep fold into
// hour rows: bytes and speed tests add up, the up ratio and the latency
// average. Hour rows that started before now − hourKeep are dropped, and so
// are incidents that ended before then.
func (s *Store) Compact(now time.Time, minuteKeep, hourKeep time.Duration) error {
	cut := now.Add(-minuteKeep).Unix()
	cut -= floorMod(cut, hourSeconds) // folds whole hours only
	expire := now.Add(-hourKeep).Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		// An hour that already has a row, such as when a minute was written
		// late, merges into it: the sums add and the averages meet halfway.
		`INSERT INTO upstream_hour_samples (upstream, ts, up_ratio, rtt_ms, down_bytes, up_bytes, speed_tests, down_rate_sum, up_rate_sum)
			SELECT upstream, ts - ts % 3600, AVG(up_ratio), AVG(rtt_ms), SUM(down_bytes), SUM(up_bytes),
				SUM(speed_tests), SUM(down_rate_sum), SUM(up_rate_sum)
			FROM upstream_samples WHERE ts < ?1 GROUP BY upstream, ts - ts % 3600
			ON CONFLICT (upstream, ts) DO UPDATE SET up_ratio = (up_ratio + excluded.up_ratio) / 2,
				rtt_ms = COALESCE((rtt_ms + excluded.rtt_ms) / 2, rtt_ms, excluded.rtt_ms),
				down_bytes = down_bytes + excluded.down_bytes, up_bytes = up_bytes + excluded.up_bytes,
				speed_tests = speed_tests + excluded.speed_tests, down_rate_sum = down_rate_sum + excluded.down_rate_sum,
				up_rate_sum = up_rate_sum + excluded.up_rate_sum`,
		`DELETE FROM upstream_samples WHERE ts < ?1`,
		`INSERT INTO peer_hour_samples (peer_id, ts, down_bytes, up_bytes)
			SELECT peer_id, ts - ts % 3600, SUM(down_bytes), SUM(up_bytes)
			FROM peer_samples WHERE ts < ?1 GROUP BY peer_id, ts - ts % 3600
			ON CONFLICT (peer_id, ts) DO UPDATE SET down_bytes = down_bytes + excluded.down_bytes, up_bytes = up_bytes + excluded.up_bytes`,
		`DELETE FROM peer_samples WHERE ts < ?1`,
	} {
		if _, err := tx.Exec(q, cut); err != nil {
			return err
		}
	}
	for _, q := range []string{`DELETE FROM upstream_hour_samples WHERE ts < ?`, `DELETE FROM peer_hour_samples WHERE ts < ?`,
		`DELETE FROM incidents WHERE ended < ?`} {
		if _, err := tx.Exec(q, expire); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpstreamSamples returns an upstream's history in [from, to) as samples of
// step, which must be a multiple of the minute and align from and to to it.
// Steps with no sample are left out. A step is the bytes' sum and the up
// ratio's and latency's mean over the rows in it, an hour row counting as
// the sixty minutes it folded, and the rates' mean over its speed tests.
func (s *Store) UpstreamSamples(name string, from, to time.Time, step time.Duration) ([]Sample, error) {
	rows, err := s.db.Query(`SELECT bucket, SUM(down_bytes), SUM(up_bytes), SUM(up_ratio * w) / SUM(w),
			SUM(rtt_ms * w) / SUM(CASE WHEN rtt_ms IS NOT NULL THEN w END),
			COALESCE(SUM(down_rate_sum) / NULLIF(SUM(speed_tests), 0), 0),
			COALESCE(SUM(up_rate_sum) / NULLIF(SUM(speed_tests), 0), 0)
		FROM (
			SELECT ts - ts % ?1 AS bucket, up_ratio, rtt_ms, down_bytes, up_bytes, speed_tests, down_rate_sum, up_rate_sum, 1 AS w
				FROM upstream_samples WHERE upstream = ?2 AND ts >= ?3 AND ts < ?4
			UNION ALL
			SELECT ts - ts % ?1, up_ratio, rtt_ms, down_bytes, up_bytes, speed_tests, down_rate_sum, up_rate_sum, 60
				FROM upstream_hour_samples WHERE upstream = ?2 AND ts >= ?3 AND ts < ?4
		) GROUP BY bucket ORDER BY bucket`, int64(step/time.Second), name, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var bucket, down, up, downRate, upRate int64
		var ratio float64
		var rtt sql.NullFloat64
		if err := rows.Scan(&bucket, &down, &up, &ratio, &rtt, &downRate, &upRate); err != nil {
			return nil, err
		}
		out = append(out, Sample{Time: time.Unix(bucket, 0).UTC(), UpRatio: ratio,
			Latency: time.Duration(rtt.Float64 * float64(time.Millisecond)), Down: uint64(down), Up: uint64(up),
			DownRate: uint64(downRate), UpRate: uint64(upRate)})
	}
	return out, rows.Err()
}

// PeerSamples is UpstreamSamples for the peer with this row id; the ratio and
// the latency stay zero.
func (s *Store) PeerSamples(peerID int64, from, to time.Time, step time.Duration) ([]Sample, error) {
	rows, err := s.db.Query(`SELECT bucket, SUM(down_bytes), SUM(up_bytes)
		FROM (
			SELECT ts - ts % ?1 AS bucket, down_bytes, up_bytes
				FROM peer_samples WHERE peer_id = ?2 AND ts >= ?3 AND ts < ?4
			UNION ALL
			SELECT ts - ts % ?1, down_bytes, up_bytes
				FROM peer_hour_samples WHERE peer_id = ?2 AND ts >= ?3 AND ts < ?4
		) GROUP BY bucket ORDER BY bucket`, int64(step/time.Second), peerID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var bucket, down, up int64
		if err := rows.Scan(&bucket, &down, &up); err != nil {
			return nil, err
		}
		out = append(out, Sample{Time: time.Unix(bucket, 0).UTC(), Down: uint64(down), Up: uint64(up)})
	}
	return out, rows.Err()
}

// HasPeer reports whether a peer with this row id exists.
func (s *Store) HasPeer(id int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM peers WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

// floorMod is a mod b for a positive b, never negative.
func floorMod(a, b int64) int64 { return (a%b + b) % b }
