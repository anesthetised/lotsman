package history

import (
	"errors"
	"fmt"
	"time"

	"github.com/anesthetised/lotsman/internal/store"
)

const (
	// MaxSamples caps what one query returns: a chart has no use for more,
	// and the response stays small whatever the range.
	MaxSamples = 2000
	// CoarseResolution is the step of what has aged out of the minute rows.
	CoarseResolution = time.Hour
)

// ErrRange reports a range or step no query can answer.
var ErrRange = errors.New("invalid range")

// Range is what a client asks for. To zero means now; Step zero, the finest
// step kept for the range.
type Range struct {
	From, To time.Time
	Step     time.Duration
}

// Plan is a Range made answerable: the bounds widened to whole steps and
// the step the data is served in.
type Plan struct {
	From, To time.Time // [From, To), multiples of Step since the epoch
	Step     time.Duration
}

// Result is a query's answer. The samples are in time order.
type Result struct {
	Step    time.Duration
	Samples []store.Sample
}

// Resolve plans a query at now. The finest step kept for the range is one
// minute while it starts within minuteKeep of now and an hour once it
// reaches further back, where only hour rows stay. The requested step is
// rounded up to a multiple of that, and further until the range fits in
// MaxSamples; the bounds widen to whole steps, so every sample covers a full
// step.
func Resolve(r Range, now time.Time, minuteKeep time.Duration) (Plan, error) {
	if r.From.IsZero() {
		return Plan{}, fmt.Errorf("%w: from is required", ErrRange)
	}
	to := r.To
	if to.IsZero() {
		to = now
	}
	from, until := r.From.Unix(), to.Unix() // seconds: a range of years cannot overflow
	if until <= from {
		return Plan{}, fmt.Errorf("%w: to must be after from", ErrRange)
	}
	if r.Step < 0 {
		return Plan{}, fmt.Errorf("%w: step must not be negative", ErrRange)
	}
	kept := int64(Resolution / time.Second)
	if r.From.Before(now.Add(-minuteKeep)) {
		kept = int64(CoarseResolution / time.Second)
	}
	// The step the client asked for, or the span split in MaxSamples parts,
	// whichever is longer.
	step := roundUp(max(int64(r.Step/time.Second), kept, ceilDiv(until-from, MaxSamples)), kept)
	for {
		start, end := floor(from, step), ceil(until, step)
		if (end-start)/step <= MaxSamples {
			return Plan{From: time.Unix(start, 0).UTC(), To: time.Unix(end, 0).UTC(), Step: time.Duration(step) * time.Second}, nil
		}
		step += kept // widening to whole steps added one
	}
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

func roundUp(a, unit int64) int64 { return ceilDiv(a, unit) * unit }

// floor and ceil align unix seconds to multiples of step since the epoch.
func floor(t, step int64) int64 { return t - ((t%step)+step)%step }

func ceil(t, step int64) int64 {
	if f := floor(t, step); f == t {
		return f
	}
	return floor(t, step) + step
}
