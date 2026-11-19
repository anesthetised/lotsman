// Package policy picks an upstream for a device's route from what is
// currently healthy.
package policy

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anesthetised/lotsman/internal/config"
)

type Upstream struct {
	config.Upstream
	Healthy  bool
	Degraded bool // reachable, but too slow for real traffic: a last resort
	Latency  time.Duration
}

// Match selects upstreams by their labels. An empty field matches anything,
// so an empty Match matches every upstream.
type Match struct {
	Geo      string
	Provider string
}

func (m Match) Matches(u config.Upstream) bool {
	return (m.Geo == "" || m.Geo == u.Geo) && (m.Provider == "" || m.Provider == u.Provider)
}

// Tier is one preference level: the upstreams any of its matches selects.
type Tier []Match

func (t Tier) Matches(u config.Upstream) bool {
	return slices.ContainsFunc(t, func(m Match) bool { return m.Matches(u) })
}

// Select returns the upstream a device whose route compiles to tiers should
// use, among upstreams, which the caller has narrowed to the user's allowed
// exits (see Allows).
//
// Only healthy upstreams in the best available tier are candidates; an
// upstream belongs to the first tier that matches it. Degraded ones count
// only when no other healthy upstream is in any tier: slow connectivity
// beats none.
//
// The choice is sticky: if current is a candidate it is kept even when a
// sibling now has lower latency, because switching costs the user every open
// connection. Otherwise the lowest latency wins, then the name, so the result
// is deterministic.
//
// ok is false when nothing usable exists; the caller must then fail closed.
func Select(tiers []Tier, upstreams []Upstream, current string) (name string, ok bool) {
	if name, ok := pick(tiers, upstreams, current, false); ok {
		return name, true
	}
	return pick(tiers, upstreams, current, true)
}

// pick applies Select's rules to the healthy upstreams, degraded ones
// included only if lastResort is set.
func pick(tiers []Tier, upstreams []Upstream, current string, lastResort bool) (name string, ok bool) {
	var candidates []Upstream
	bestTier := -1
	for _, u := range upstreams {
		tier := slices.IndexFunc(tiers, func(t Tier) bool { return t.Matches(u.Upstream) })
		if !u.Healthy || (u.Degraded && !lastResort) || tier < 0 {
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

// Allows reports whether exits, a user's allowed exits, include the upstream
// of that name. No exits means every upstream. A name the config no longer
// has matches nothing, so a list of only such names allows nothing: a
// restriction never turns into "every upstream" by itself.
func Allows(exits []string, upstream string) bool {
	return len(exits) == 0 || slices.Contains(exits, upstream)
}

// ValidateExits checks exits being written: each must name one of the
// upstreams, once. Like a route, a stored list may go stale later.
func ValidateExits(exits []string, upstreams []config.Upstream) error {
	for i, name := range exits {
		if slices.Contains(exits[:i], name) {
			return fmt.Errorf("exits: %q is listed twice", name)
		}
		if !slices.ContainsFunc(upstreams, func(u config.Upstream) bool { return u.Name == name }) {
			return fmt.Errorf("exits: no upstream %q", name)
		}
	}
	return nil
}

// Exits narrows upstreams to those exits allow.
func Exits(upstreams []Upstream, exits []string) []Upstream {
	var out []Upstream
	for _, u := range upstreams {
		if Allows(exits, u.Name) {
			out = append(out, u)
		}
	}
	return out
}
