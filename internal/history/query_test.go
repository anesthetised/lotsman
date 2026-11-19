package history

import (
	"errors"
	"testing"
	"time"
)

// A Thursday noon, so the minute and hour boundaries are easy to follow.
var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const week = 7 * 24 * time.Hour

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		r        Range
		wantStep time.Duration
		from, to time.Time // widened; zero: the same as asked
	}{
		{"unset step is the finest kept: a minute", Range{From: now.Add(-time.Hour), To: now}, time.Minute, time.Time{}, time.Time{}},
		{"an hour back from now, to unset", Range{From: now.Add(-time.Hour)}, time.Minute, time.Time{}, time.Time{}},
		{"exactly 7 d back is still minutes", Range{From: now.Add(-week), To: now.Add(-week + time.Hour)}, time.Minute, time.Time{}, time.Time{}},
		{"older than 7 d: hours", Range{From: now.Add(-week - time.Hour), To: now}, time.Hour, time.Time{}, time.Time{}},
		{"a range that only starts old gets hours throughout", Range{From: now.Add(-30 * 24 * time.Hour), To: now.Add(-29 * 24 * time.Hour)}, time.Hour, time.Time{}, time.Time{}},
		{"a step of the kept size is kept", Range{From: now.Add(-time.Hour), To: now, Step: time.Minute}, time.Minute, time.Time{}, time.Time{}},
		{"a step rounds up to a multiple", Range{From: now.Add(-6 * time.Hour), To: now, Step: 90 * time.Second}, 2 * time.Minute, time.Time{}, time.Time{}},
		{"a multiple stays", Range{From: now.Add(-6 * time.Hour), To: now, Step: 5 * time.Minute}, 5 * time.Minute, time.Time{}, time.Time{}},
		{"a step under the kept one rises to it", Range{From: now.Add(-24 * time.Hour), To: now, Step: time.Second}, time.Minute, time.Time{}, time.Time{}},
		{"hours: a step of 90 min is 2 h", Range{From: now.Add(-30 * 24 * time.Hour), To: now, Step: 90 * time.Minute}, 2 * time.Hour, time.Time{}, time.Time{}},
		{"hours: 10 min is an hour", Range{From: now.Add(-30 * 24 * time.Hour), To: now, Step: 10 * time.Minute}, time.Hour, time.Time{}, time.Time{}},
		// 7 d of minutes is 10080 samples: the cap makes the step the next
		// multiple of a minute above 10080/2000 = 5.04 minutes.
		{"the cap: a week by the minute is every 6 minutes", Range{From: now.Add(-week), To: now}, 6 * time.Minute, time.Time{}, time.Time{}},
		// 90 d of hours is 2160 samples: 2 h.
		{"the cap: 90 d by the hour is every 2 hours", Range{From: now.Add(-90 * 24 * time.Hour), To: now}, 2 * time.Hour, time.Time{}, time.Time{}},
		{"the cap rises over a step that is too fine", Range{From: now.Add(-week), To: now, Step: time.Minute}, 6 * time.Minute, time.Time{}, time.Time{}},
		{"the cap leaves a coarser step alone", Range{From: now.Add(-week), To: now, Step: 30 * time.Minute}, 30 * time.Minute, time.Time{}, time.Time{}},
		// Widening: 2000 minutes that start and end mid-minute need 2001 of
		// them, so the step grows past the cap's first answer, a minute.
		{"the widening counts against the cap", Range{From: now.Add(-2000*time.Minute + 30*time.Second), To: now.Add(30 * time.Second)}, 2 * time.Minute, time.Time{}, time.Time{}},
		{"bounds widen to whole steps", Range{From: now.Add(-time.Hour - 20*time.Second), To: now.Add(-10 * time.Second)}, time.Minute,
			now.Add(-time.Hour - time.Minute), now.Add(-0 * time.Second)},
		{"bounds widen to whole hours", Range{From: now.Add(-10*24*time.Hour - 20*time.Minute), To: now.Add(-9 * 24 * time.Hour)}, time.Hour,
			now.Add(-10*24*time.Hour - time.Hour), time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Resolve(c.r, now, week)
			if err != nil {
				t.Fatal(err)
			}
			if p.Step != c.wantStep {
				t.Errorf("step = %v, want %v", p.Step, c.wantStep)
			}
			to := c.r.To
			if to.IsZero() {
				to = now
			}
			// The cap and the alignment hold whatever the case says.
			if n := p.To.Sub(p.From) / p.Step; n > MaxSamples {
				t.Errorf("%d samples, over the cap", n)
			}
			if p.From.Unix()%int64(p.Step/time.Second) != 0 || p.To.Unix()%int64(p.Step/time.Second) != 0 {
				t.Errorf("[%v, %v) is not aligned to %v", p.From, p.To, p.Step)
			}
			if p.From.After(c.r.From) || p.To.Before(to) || p.From.Sub(c.r.From) <= -p.Step || p.To.Sub(to) >= p.Step {
				t.Errorf("[%v, %v) is not the tightest widening of [%v, %v)", p.From, p.To, c.r.From, to)
			}
			if !c.from.IsZero() && !p.From.Equal(c.from) || !c.to.IsZero() && !p.To.Equal(c.to) {
				t.Errorf("range = [%v, %v), want [%v, %v)", p.From, p.To, c.from, c.to)
			}
		})
	}
}

// The boundary is the configured minute retention, not a constant 7 days.
func TestResolveFollowsTheRetention(t *testing.T) {
	r := Range{From: now.Add(-3 * 24 * time.Hour), To: now}
	if p, _ := Resolve(r, now, 2*24*time.Hour); p.Step != time.Hour {
		t.Errorf("3 d back with 2 d of minutes: step %v, want 1h", p.Step)
	}
	if p, _ := Resolve(r, now, 4*24*time.Hour); p.Step < time.Minute || p.Step >= time.Hour {
		t.Errorf("3 d back with 4 d of minutes: step %v, want minutes", p.Step)
	}
}

func TestResolveRejectsBadRanges(t *testing.T) {
	for name, r := range map[string]Range{
		"no from":        {To: now},
		"to == from":     {From: now, To: now},
		"to before":      {From: now, To: now.Add(-time.Minute)},
		"from is now":    {From: now},
		"from in future": {From: now.Add(time.Hour)},
		"negative step":  {From: now.Add(-time.Hour), Step: -time.Minute},
	} {
		if _, err := Resolve(r, now, week); !errors.Is(err, ErrRange) {
			t.Errorf("%s: err = %v, want ErrRange", name, err)
		}
	}
}

// Absurd ranges are served coarsely and finish.
func TestResolveHugeRange(t *testing.T) {
	r := Range{From: time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC), To: time.Date(9999, 12, 30, 0, 0, 0, 0, time.UTC)}
	p, err := Resolve(r, now, week)
	if err != nil {
		t.Fatal(err)
	}
	if n := p.To.Sub(p.From) / p.Step; n > MaxSamples && p.To.Sub(p.From) > 0 { // Sub saturates at 292 years
		t.Errorf("%d samples", n)
	}
	if p.Step%time.Hour != 0 {
		t.Errorf("step = %v", p.Step)
	}
}
