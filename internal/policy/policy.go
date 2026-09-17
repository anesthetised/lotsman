// Package policy picks an upstream for a profile from what is currently healthy.
package policy

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

type Upstream struct {
	config.Upstream
	Healthy bool
	Latency time.Duration
}

// Select returns the upstream a peer with the given profile should use.
//
// Preference tiers come from profile.Prefer in order; an empty Prefer is one
// tier matching everything. Only healthy upstreams in the best available tier
// are candidates.
//
// The choice is sticky: if current is a candidate it is kept even when a
// sibling now has lower latency, because switching costs the user every open
// connection. Otherwise the lowest latency wins, then the name, so the result
// is deterministic.
//
// ok is false when nothing usable exists; the caller must then fail closed.
func Select(profile config.Profile, upstreams []Upstream, current string) (name string, ok bool) {
	var candidates []Upstream
	bestTier := -1
	for _, u := range upstreams {
		tier := tierOf(profile, u.Upstream)
		if !u.Healthy || tier < 0 {
			continue
		}
		switch {
		case bestTier < 0 || tier < bestTier:
			bestTier = tier
			candidates = []Upstream{u}
		case tier == bestTier:
			candidates = append(candidates, u)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	for _, u := range candidates {
		if u.Name == current {
			return current, true
		}
	}
	best := slices.MinFunc(candidates, func(a, b Upstream) int {
		return cmp.Or(cmp.Compare(a.Latency, b.Latency), strings.Compare(a.Name, b.Name))
	})
	return best.Name, true
}

// tierOf returns the index of the first preference that matches, or -1.
func tierOf(p config.Profile, u config.Upstream) int {
	if len(p.Prefer) == 0 {
		return 0
	}
	for i, m := range p.Prefer {
		if m.Matches(u) {
			return i
		}
	}
	return -1
}
